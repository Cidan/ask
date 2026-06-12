package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
