package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// withStdin temporarily replaces os.Stdin with a pipe fed from s, so
// runHookSubcommand can read a canned payload. Restored on cleanup.
func withStdin(t *testing.T, s string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = w.Close()
}

// The `ask _hook` subcommand exists so claude's hook config can invoke
// ask itself rather than depending on curl being on PATH.
func TestHookSubcommand_RelaysStdinToBridge(t *testing.T) {
	var gotBody []byte
	var gotPath, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	payload := `{"session_id":"s1","hook_event_name":"SubagentStop","agent_id":"agent_42","agent_type":"general-purpose"}`
	withStdin(t, payload)

	if err := runHookSubcommand([]string{"subagent-stop", "--port", strconv.Itoa(port)}); err != nil {
		t.Fatalf("runHookSubcommand: %v", err)
	}

	if gotPath != "/hooks/subagent-stop" {
		t.Errorf("POST path=%q want /hooks/subagent-stop", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type=%q want application/json", gotContentType)
	}
	var ev hookInput
	if err := json.Unmarshal(gotBody, &ev); err != nil {
		t.Fatalf("body not JSON (%v): %s", err, gotBody)
	}
	if ev.AgentID != "agent_42" || ev.AgentType != "general-purpose" {
		t.Errorf("parsed event wrong: %+v", ev)
	}
}

func TestHookSubcommand_MissingPortErrors(t *testing.T) {
	withStdin(t, "{}")
	err := runHookSubcommand([]string{"subagent-stop"})
	if err == nil || !strings.Contains(err.Error(), "--port") {
		t.Errorf("missing --port should error, got %v", err)
	}
}

func TestHookSubcommand_MissingEventErrors(t *testing.T) {
	err := runHookSubcommand(nil)
	if err == nil {
		t.Errorf("missing event should error")
	}
}

// A hook POST that returns non-2xx must surface as an error so main
// can log it — but the CLI wrapper in main.go exits 0 regardless so
// claude never sees it as a hook failure.
func TestHookSubcommand_Non2xxSurfacesAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	withStdin(t, "{}")
	err := runHookSubcommand([]string{"subagent-start", "--port", strconv.Itoa(port)})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500 response should return error mentioning status; got %v", err)
	}
}
