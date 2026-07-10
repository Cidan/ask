package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"golang.org/x/net/html"
)

const (
	agentFetchMaxBytes       = 100_000
	agentFetchDefaultTimeout = 30 * time.Second
	agentFetchMaxTimeout     = 120 * time.Second
)

const agentFetchToolDescription = `Fetch a URL over HTTP GET and return its content. HTML pages are reduced to readable text; other content types return raw (capped at 100KB). Use for documentation, APIs, and references the task points at.`

type agentFetchParams struct {
	URL         string `json:"url" description:"the http(s) URL to fetch"`
	Timeout     int    `json:"timeout,omitempty" description:"max seconds to wait (default 30, max 120)"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// agentFetchClient is swappable in tests; production uses a client
// with sane connection reuse and the per-call timeout from params.
var agentFetchClient = &http.Client{}

func agentFetchTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		"fetch",
		agentFetchToolDescription,
		func(ctx context.Context, p agentFetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			raw := strings.TrimSpace(p.URL)
			if raw == "" {
				return fantasy.NewTextErrorResponse("url is required"), nil
			}
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return fantasy.NewTextErrorResponse("only http and https URLs are supported: " + raw), nil
			}
			if denied := env.requestApproval(ctx, "fetch", map[string]any{"url": raw, "description": p.Description}); denied != nil {
				return *denied, nil
			}

			timeout := agentFetchDefaultTimeout
			if p.Timeout > 0 {
				timeout = min(time.Duration(p.Timeout)*time.Second, agentFetchMaxTimeout)
			}
			reqCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, raw, nil)
			if err != nil {
				return fantasy.NewTextErrorResponse("bad request: " + err.Error()), nil
			}
			req.Header.Set("User-Agent", "ask-agent/1.0")
			resp, err := agentFetchClient.Do(req)
			if err != nil {
				return fantasy.NewTextErrorResponse("fetch failed: " + err.Error()), nil
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(io.LimitReader(resp.Body, agentFetchMaxBytes+1))
			if err != nil {
				return fantasy.NewTextErrorResponse("read body: " + err.Error()), nil
			}
			truncated := len(body) > agentFetchMaxBytes
			if truncated {
				body = body[:agentFetchMaxBytes]
			}

			contentType := resp.Header.Get("Content-Type")
			text := string(body)
			if strings.Contains(contentType, "text/html") {
				text = htmlToText(text)
			}
			if looksBinary([]byte(text[:min(len(text), 8192)])) {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"%s returned binary content (%s) — not useful as text", raw, contentType)), nil
			}

			var out strings.Builder
			fmt.Fprintf(&out, "[%s — HTTP %d, %s]\n", raw, resp.StatusCode, contentType)
			out.WriteString(truncateMiddle(text))
			if truncated {
				fmt.Fprintf(&out, "\n(body capped at %d bytes)", agentFetchMaxBytes)
			}
			if resp.StatusCode >= 400 {
				return fantasy.NewTextErrorResponse(out.String()), nil
			}
			return fantasy.NewTextResponse(out.String()), nil
		},
	)
}

// htmlToText reduces an HTML document to readable text: script/style
// subtrees are dropped, block elements become line breaks, and links
// keep their href so the agent can follow them.
func htmlToText(src string) string {
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
	return collapseBlankLines(out.String())
}

// collapseBlankLines trims trailing spaces and squeezes runs of blank
// lines down to one so extracted HTML reads compactly.
func collapseBlankLines(s string) string {
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

const (
	braveSearchEndpoint   = "https://api.search.brave.com/res/v1/web/search"
	braveSearchDefaultN   = 8
	braveSearchMaxResults = 20
	braveSearchTimeout    = 20 * time.Second
)

const agentWebSearchToolDescription = `Search the web and return ranked results (title, URL, and snippet) for a query. Use this to find current information, documentation, releases, or anything outside your training data — then follow up with the fetch tool to read a promising result in full.`

// agentWebSearchNoKeyNotice is returned (as a NON-error response) when
// the Brave key is unconfigured: the model is told to stop trying to
// search, finish the task as best it can, and tell the user to add a
// key. Returning a plain text response (not IsError / StopTurn) lets the
// model proceed on its own judgment instead of treating it as a failure
// to retry.
const agentWebSearchNoKeyNotice = `web_search is not configured: no Brave Search API key is set. Do not retry web_search this turn. Continue with the rest of the task using what you already know, and when you finish, clearly tell the user that web search is unavailable and that they should add a Brave Search API key under /config → Web Search (or set the BRAVE_API_KEY environment variable) to enable it.`

type agentWebSearchParams struct {
	Query       string `json:"query" description:"the search query"`
	Count       int    `json:"count,omitempty" description:"max number of results to return (default 8, max 20)"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// braveSearchClient is swappable in tests; production uses a client with
// the per-call timeout applied via context.
var braveSearchClient = &http.Client{}

// braveResult is one web result from the Brave Search API.
type braveResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// braveSearchResponse is the subset of the Brave Search API web-search
// response we consume.
type braveSearchResponse struct {
	Web struct {
		Results []braveResult `json:"results"`
	} `json:"web"`
}

// braveSearch performs one Brave web search and returns the parsed
// results. Errors are wire/HTTP failures; an empty slice with nil error
// means the query simply matched nothing.
func braveSearch(ctx context.Context, apiKey, query string, count int) ([]braveResult, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(count))
	reqURL := braveSearchEndpoint + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)
	req.Header.Set("User-Agent", "ask-agent/1.0")
	resp, err := braveSearchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, agentFetchMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("brave search HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), 300))
	}
	var parsed braveSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("brave search: decode response: %w", err)
	}
	return parsed.Web.Results, nil
}

// agentWebSearchTool is the Brave-backed web_search core tool used by
// providers without first-party web search (DeepSeek and other
// openaicompat backends). Anthropic and OpenAI sessions register their
// provider-executed web search under the same name instead, so this tool
// is never attached for them (agent_provider.go). With no Brave key the
// tool returns a graceful notice rather than failing — see
// agentWebSearchNoKeyNotice.
func agentWebSearchTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		"web_search",
		agentWebSearchToolDescription,
		func(ctx context.Context, p agentWebSearchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			query := strings.TrimSpace(p.Query)
			if query == "" {
				return fantasy.NewTextErrorResponse("query is required"), nil
			}
			cfg, _ := loadConfig()
			apiKey := resolveBraveAPIKey(cfg.WebSearch)
			if apiKey == "" {
				return fantasy.NewTextResponse(agentWebSearchNoKeyNotice), nil
			}
			if denied := env.requestApproval(ctx, "WebSearch", map[string]any{"query": query, "description": p.Description}); denied != nil {
				return *denied, nil
			}

			count := braveSearchDefaultN
			if p.Count > 0 {
				count = min(p.Count, braveSearchMaxResults)
			}
			reqCtx, cancel := context.WithTimeout(ctx, braveSearchTimeout)
			defer cancel()
			results, err := braveSearch(reqCtx, apiKey, query, count)
			if err != nil {
				return fantasy.NewTextErrorResponse("web search failed: " + err.Error()), nil
			}
			if len(results) == 0 {
				return fantasy.NewTextResponse(fmt.Sprintf("No web results for %q.", query)), nil
			}

			var out strings.Builder
			fmt.Fprintf(&out, "Web results for %q:\n", query)
			for i, r := range results {
				fmt.Fprintf(&out, "\n%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL))
				if d := strings.TrimSpace(htmlToText(r.Description)); d != "" {
					fmt.Fprintf(&out, "   %s\n", d)
				}
			}
			return fantasy.NewTextResponse(strings.TrimSpace(out.String())), nil
		},
	)
}
