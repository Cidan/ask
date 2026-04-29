package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runHookSubcommand implements the hidden `ask _hook <event> --port <n>`
// CLI used as the command for claude's hooks. It reads the hook event
// JSON from stdin, POSTs it to the running MCP bridge, and forwards
// the bridge's response body to stdout so claude can parse it as a
// hookSpecificOutput JSON for events that inject context (SessionStart
// / UserPromptSubmit / PreToolUse). Subagent hooks reply with an empty
// body, which claude treats as "no action."
//
// memoryHookEvents is the set of events whose responses inject
// additionalContext into claude's prompt. When --port is unreachable
// or returns a non-2xx response for one of these, we still need to
// emit a syntactically-valid (empty) hookSpecificOutput on stdout so
// claude doesn't treat the silence as a hook failure that blocks the
// turn. Subagent hooks pre-date the recall surface and are silent on
// success, so they fall through the empty-body codepath untouched.
//
// Returns nil on full success, an error otherwise. The CLI entry in
// main unconditionally exits 0 regardless — hook telemetry is
// best-effort and a non-zero exit would be seen by claude as a hook
// failure (exit 2 blocks the event outright, any other non-zero is
// surfaced as an error).
func runHookSubcommand(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("expected event name")
	}
	event := args[0]
	fs := flag.NewFlagSet("_hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", 0, "mcp bridge port")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *port <= 0 || *port > 65535 {
		return fmt.Errorf("--port required (1..65535)")
	}
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/hooks/%s", *port, event)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	// Forward the response body verbatim to stdout. claude reads stdout
	// of hook subprocesses and interprets non-empty bodies as JSON for
	// hookSpecificOutput; an empty body is treated as a no-op. The
	// bridge handlers for recall hooks always emit a JSON envelope
	// (possibly with empty additionalContext); subagent capture hooks
	// emit nothing, which is also valid.
	_, _ = io.Copy(os.Stdout, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return nil
}
