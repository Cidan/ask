package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := FetchTool(env)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><style>.x{}</style></head><body><h1>Title</h1><script>evil()</script><p>Body text <a href="/docs">docs</a></p></body></html>`)
		case "/plain":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "raw text body")
		case "/missing":
			http.NotFound(w, r)
		case "/binary":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0x7f, 0x45, 0x4c, 0x46, 0x00, 0x01})
		}
	}))
	defer srv.Close()

	resp := runTool(t, tool, FetchParams{URL: srv.URL + "/html"})
	if resp.IsError {
		t.Fatalf("html fetch: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "Title") || !strings.Contains(resp.Content, "Body text") {
		t.Errorf("html text extraction lost content:\n%s", resp.Content)
	}
	if strings.Contains(resp.Content, "evil()") || strings.Contains(resp.Content, ".x{}") {
		t.Errorf("script/style must be stripped:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, "(/docs)") {
		t.Errorf("link hrefs should be preserved:\n%s", resp.Content)
	}

	resp = runTool(t, tool, FetchParams{URL: srv.URL + "/plain"})
	if !strings.Contains(resp.Content, "raw text body") {
		t.Errorf("plain fetch: %q", resp.Content)
	}

	if resp = runTool(t, tool, FetchParams{URL: srv.URL + "/missing"}); !resp.IsError || !strings.Contains(resp.Content, "HTTP 404") {
		t.Errorf("404 should be an error result: %+v", resp)
	}
	if resp = runTool(t, tool, FetchParams{URL: srv.URL + "/binary"}); !resp.IsError || !strings.Contains(resp.Content, "binary") {
		t.Errorf("binary should be rejected: %+v", resp)
	}
	if resp = runTool(t, tool, FetchParams{URL: "ftp://nope"}); !resp.IsError {
		t.Error("non-http scheme must be rejected")
	}

	env.SkipPermissions = false
	env.Approve = func(context.Context, string, map[string]any) (bool, error) { return false, nil }
	if resp = runTool(t, tool, FetchParams{URL: srv.URL + "/plain"}); !resp.IsError || !resp.StopTurn {
		t.Errorf("denied fetch should stop turn: %+v", resp)
	}
}

func TestWebSearchToolNoKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BRAVE_API_KEY", "")
	env, _ := newTestToolEnv(t)
	tool := WebSearchTool(env)

	resp := runTool(t, tool, WebSearchParams{Query: "golang generics"})
	if resp.IsError || resp.StopTurn {
		t.Fatalf("no-key result must be a plain notice, got %+v", resp)
	}
	if !strings.Contains(resp.Content, "not configured") || !strings.Contains(resp.Content, "/config") {
		t.Errorf("no-key notice should point at /config: %q", resp.Content)
	}

	if r := runTool(t, tool, WebSearchParams{Query: "  "}); !r.IsError {
		t.Error("empty query must be an error")
	}
}
