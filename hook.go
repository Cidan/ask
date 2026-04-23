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
// CLI used as the command for claude's SubagentStart / SubagentStop
// hooks. It reads the hook event JSON from stdin and POSTs it to the
// running MCP bridge so the TUI can update its in-memory state.
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return nil
}
