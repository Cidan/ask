package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/adk/v2/agent"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"golang.org/x/net/html"
)

const (
	FetchMaxBytes       = 100_000
	FetchDefaultTimeout = 30 * time.Second
	FetchMaxTimeout     = 120 * time.Second

	BraveSearchEndpoint   = "https://api.search.brave.com/res/v1/web/search"
	BraveSearchDefaultN   = 8
	BraveSearchMaxResults = 20
	BraveSearchTimeout    = 20 * time.Second

	WebSearchNoKeyNotice = `web_search is not configured: no Brave Search API key is set. Do not retry web_search this turn. Continue with the rest of the task using what you already know, and when you finish, clearly tell the user that web search is unavailable and that they should add a Brave Search API key under /config → Web Search (or set the BRAVE_API_KEY environment variable) to enable it.`
)

const FetchToolDescription = `Fetch a URL over HTTP GET and return its content. HTML pages are reduced to readable text; other content types return raw (capped at 100KB). Use for documentation, APIs, and references the task points at.`

type FetchParams struct {
	URL         string `json:"url" jsonschema:"the http(s) URL to fetch"`
	Timeout     int    `json:"timeout,omitempty" jsonschema:"max seconds to wait (default 30, max 120)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// FetchClient is swappable in tests.
var FetchClient = &http.Client{}

// FetchTool returns the native fetch tool.
// FetchResult is the fetch tool's response.
type FetchResult struct {
	URL         string `json:"url,omitempty"`
	Status      int    `json:"status,omitempty" jsonschema:"HTTP status code"`
	ContentType string `json:"content_type,omitempty"`
	Body        string `json:"body,omitempty" jsonschema:"extracted text of the page"`
	Truncated   bool   `json:"truncated,omitempty" jsonschema:"true when the body exceeded the fetch cap"`
}

func FetchTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"fetch",
		FetchToolDescription,
		func(ctx agent.Context, p FetchParams) (FetchResult, error) {
			raw := strings.TrimSpace(p.URL)
			if raw == "" {
				return FetchResult{}, errors.New("url is required")
			}
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return FetchResult{}, errors.New("only http and https URLs are supported: " + raw)
			}
			if denied := env.ApprovalDenied(ctx, "fetch", map[string]any{"url": raw, "description": p.Description}); denied != "" {
				return FetchResult{}, errors.New(denied)
			}

			timeout := FetchDefaultTimeout
			if p.Timeout > 0 {
				timeout = min(time.Duration(p.Timeout)*time.Second, FetchMaxTimeout)
			}
			reqCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, raw, nil)
			if err != nil {
				return FetchResult{}, errors.New("bad request: " + err.Error())
			}
			req.Header.Set("User-Agent", "ask-agent/1.0")
			resp, err := FetchClient.Do(req)
			if err != nil {
				return FetchResult{}, errors.New("fetch failed: " + err.Error())
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(io.LimitReader(resp.Body, FetchMaxBytes+1))
			if err != nil {
				return FetchResult{}, errors.New("read body: " + err.Error())
			}
			truncated := len(body) > FetchMaxBytes
			if truncated {
				body = body[:FetchMaxBytes]
			}

			contentType := resp.Header.Get("Content-Type")
			text := string(body)
			if strings.Contains(contentType, "text/html") {
				text = HTMLToText(text)
			}
			if LooksBinary([]byte(text[:min(len(text), 8192)])) {
				return FetchResult{}, fmt.Errorf(
					"%s returned binary content (%s) — not useful as text", raw, contentType)
			}

			res := FetchResult{
				URL:         raw,
				Status:      resp.StatusCode,
				ContentType: contentType,
				Body:        TruncateMiddle(text),
				Truncated:   truncated,
			}
			if resp.StatusCode >= 400 {
				return res, fmt.Errorf("%s returned HTTP %d", raw, resp.StatusCode)
			}
			return res, nil
		},
	)
}

// HTMLToText reduces an HTML document to readable text.
func HTMLToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return src
	}
	var out strings.Builder
	var walk func(*html.Node)
	skip := map[string]bool{"script": true, "style": true, "noscript": true, "head": true, "svg": true, "iframe": true}
	block := map[string]bool{
		"p": true, "div": true, "br": true, "li": true, "ul": true, "ol": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"tr": true, "table": true, "section": true, "article": true, "header": true,
		"footer": true, "pre": true, "blockquote": true,
	}
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skip[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			out.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			if block[n.Data] {
				out.WriteByte('\n')
			}
			if n.Data == "a" {
				for _, a := range n.Attr {
					if a.Key == "href" && a.Val != "" && !strings.HasPrefix(a.Val, "#") {
						fmt.Fprintf(&out, " (%s)", a.Val)
						break
					}
				}
			}
		}
	}
	walk(doc)
	return CollapseBlankLines(out.String())
}

// CollapseBlankLines trims trailing spaces and squeezes consecutive blank lines.
func CollapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

const WebSearchToolDescription = `Search the web and return ranked results (title, URL, and snippet) for a query. Use this to find current information, documentation, releases, or anything outside your training data — then follow up with the fetch tool to read a promising result in full.`

type WebSearchParams struct {
	Query       string `json:"query" jsonschema:"the search query"`
	Count       int    `json:"count,omitempty" jsonschema:"max number of results to return (default 8, max 20)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// BraveSearchClient is swappable in tests.
var BraveSearchClient = &http.Client{}

// BraveResult represents one search result item.
type BraveResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

type braveSearchResponse struct {
	Web struct {
		Results []BraveResult `json:"results"`
	} `json:"web"`
}

// BraveSearch performs a Brave Web Search query.
func BraveSearch(ctx context.Context, apiKey, query string, count int) ([]BraveResult, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(count))
	reqURL := BraveSearchEndpoint + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)
	req.Header.Set("User-Agent", "ask-agent/1.0")
	resp, err := BraveSearchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, FetchMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("brave search HTTP %d: %s", resp.StatusCode, TruncateLine(strings.TrimSpace(string(body))))
	}
	var parsed braveSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("brave search: decode response: %w", err)
	}
	return parsed.Web.Results, nil
}

// WebSearchTool returns the Brave-backed web_search tool.
// WebSearchResult is the web_search tool's response.
type WebSearchResult struct {
	Query   string `json:"query,omitempty"`
	Results string `json:"results,omitempty" jsonschema:"formatted search results"`
	Count   int    `json:"count,omitempty" jsonschema:"number of results returned"`
	Notice  string `json:"notice,omitempty" jsonschema:"set when search is not configured"`
}

func WebSearchTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"web_search",
		WebSearchToolDescription,
		func(ctx agent.Context, p WebSearchParams) (WebSearchResult, error) {
			query := strings.TrimSpace(p.Query)
			if query == "" {
				return WebSearchResult{}, errors.New("query is required")
			}
			cfg, _ := config.Load()
			apiKey := config.ResolveBraveAPIKey(cfg.WebSearch)
			if apiKey == "" {
				return WebSearchResult{Notice: WebSearchNoKeyNotice}, nil
			}
			if denied := env.ApprovalDenied(ctx, "WebSearch", map[string]any{"query": query, "description": p.Description}); denied != "" {
				return WebSearchResult{}, errors.New(denied)
			}

			count := BraveSearchDefaultN
			if p.Count > 0 {
				count = min(p.Count, BraveSearchMaxResults)
			}
			reqCtx, cancel := context.WithTimeout(ctx, BraveSearchTimeout)
			defer cancel()
			results, err := BraveSearch(reqCtx, apiKey, query, count)
			if err != nil {
				return WebSearchResult{}, errors.New("web search failed: " + err.Error())
			}
			if len(results) == 0 {
				return WebSearchResult{Query: query, Count: 0}, nil
			}

			var out strings.Builder
			for i, r := range results {
				if i > 0 {
					out.WriteString("\n\n")
				}
				fmt.Fprintf(&out, "%d. %s\n   %s\n   %s", i+1, r.Title, r.URL, r.Description)
			}
			return WebSearchResult{Query: query, Results: out.String(), Count: len(results)}, nil
		},
	)
}
