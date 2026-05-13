package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestNewMCPBridge_AllocatesEphemeralPort(t *testing.T) {
	b, err := newMCPBridge(42)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}
	defer b.stop()
	if b.port <= 0 {
		t.Errorf("port should be > 0: %d", b.port)
	}
	if b.tabID != 42 {
		t.Errorf("tabID=%d want 42", b.tabID)
	}
	if b.ln == nil || b.server == nil {
		t.Errorf("bridge unfinished: ln=%v server=%v", b.ln, b.server)
	}
	// The listener must be accepting on the reported port.
	addr := "127.0.0.1:" + strconv.Itoa(b.port)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial bridge at %s: %v", addr, err)
	}
	_ = conn.Close()
}

func TestMCPBridge_StopIsIdempotent(t *testing.T) {
	b, err := newMCPBridge(1)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	b.stop()
	b.stop() // should not panic on a closed listener
	var nilB *mcpBridge
	nilB.stop() // nil-safety
}

func TestConvertMCPQuestions_KindMapping(t *testing.T) {
	qs := []mcpQuestion{
		{Kind: "pick_one", Prompt: "one", Options: []mcpOption{{Label: "a"}, {Label: "b"}}, AllowCustom: true},
		{Kind: "pick_many", Prompt: "many", Options: []mcpOption{{Label: "x"}}, AllowCustom: true},
		{Kind: "pick_diagram", Prompt: "dia", Options: []mcpOption{{Label: "d", Diagram: "▓"}}, AllowCustom: true},
		{Kind: "unknown", Prompt: "fallback", Options: []mcpOption{{Label: "z"}}},
	}
	out := convertMCPQuestions(qs)
	if len(out) != 4 {
		t.Fatalf("len=%d want 4", len(out))
	}
	if out[0].kind != qPickOne {
		t.Errorf("[0] kind=%v want qPickOne", out[0].kind)
	}
	if out[1].kind != qPickMany {
		t.Errorf("[1] kind=%v want qPickMany", out[1].kind)
	}
	if out[2].kind != qPickDiagram {
		t.Errorf("[2] kind=%v want qPickDiagram", out[2].kind)
	}
	if out[3].kind != qPickOne {
		t.Errorf("[3] unknown kind should fall back to qPickOne, got %v", out[3].kind)
	}
}

func TestConvertMCPQuestions_AllowCustomAppendsEnterYourOwn(t *testing.T) {
	// pick_one + AllowCustom → options has trailing "Enter your own"
	qs := []mcpQuestion{{Kind: "pick_one", Options: []mcpOption{{Label: "a"}}, AllowCustom: true}}
	out := convertMCPQuestions(qs)
	if len(out[0].options) != 2 || out[0].options[1] != "Enter your own" {
		t.Errorf("pick_one AllowCustom options=%v", out[0].options)
	}

	// pick_many same
	qs = []mcpQuestion{{Kind: "pick_many", Options: []mcpOption{{Label: "a"}}, AllowCustom: true}}
	out = convertMCPQuestions(qs)
	if len(out[0].options) != 2 || out[0].options[1] != "Enter your own" {
		t.Errorf("pick_many AllowCustom options=%v", out[0].options)
	}

	// pick_diagram + AllowCustom → still no custom trailer
	qs = []mcpQuestion{{Kind: "pick_diagram", Options: []mcpOption{{Label: "d"}}, AllowCustom: true}}
	out = convertMCPQuestions(qs)
	if len(out[0].options) != 1 {
		t.Errorf("pick_diagram AllowCustom must not add Enter your own; options=%v", out[0].options)
	}
}

func TestConvertMCPAnswers_EmptyPicksReturnsEmptySlice(t *testing.T) {
	qs := []mcpQuestion{{Kind: "pick_one", Options: []mcpOption{{Label: "a"}, {Label: "b"}}}}
	answers := []qAnswer{{picks: map[int]bool{}}}
	out := convertMCPAnswers(qs, answers)
	if len(out) != 1 {
		t.Fatalf("len=%d want 1", len(out))
	}
	if out[0].Picks == nil {
		t.Errorf("empty picks should produce []string{} not nil")
	}
	if len(out[0].Picks) != 0 {
		t.Errorf("empty picks should be empty, got %v", out[0].Picks)
	}
}

func TestConvertMCPAnswers_PassesCustomAndNote(t *testing.T) {
	qs := []mcpQuestion{{
		Kind: "pick_one", Options: []mcpOption{{Label: "a"}}, AllowCustom: true,
	}}
	// picks: index 0 = "a", index 1 = Enter your own
	answers := []qAnswer{{picks: map[int]bool{1: true}, custom: "freeform", note: "ditto"}}
	out := convertMCPAnswers(qs, answers)
	if len(out[0].Picks) != 0 {
		t.Errorf("only custom selected; Picks should be empty: %v", out[0].Picks)
	}
	if out[0].Custom != "freeform" {
		t.Errorf("Custom=%q want freeform", out[0].Custom)
	}
	if out[0].Note != "ditto" {
		t.Errorf("Note=%q want ditto", out[0].Note)
	}
}

func TestConvertMCPAnswers_DropsCustomWhenNotAllowed(t *testing.T) {
	qs := []mcpQuestion{{Kind: "pick_one", Options: []mcpOption{{Label: "a"}}}} // AllowCustom=false
	answers := []qAnswer{{picks: map[int]bool{0: true}, custom: "ignored"}}
	out := convertMCPAnswers(qs, answers)
	if out[0].Custom != "" {
		t.Errorf("custom should be dropped when AllowCustom=false, got %q", out[0].Custom)
	}
	if len(out[0].Picks) != 1 || out[0].Picks[0] != "a" {
		t.Errorf("Picks=%v want [a]", out[0].Picks)
	}
}

func TestConvertMCPAnswers_RoundTripJSON(t *testing.T) {
	qs := []mcpQuestion{{Kind: "pick_many", Options: []mcpOption{{Label: "a"}, {Label: "b"}}, AllowCustom: true}}
	answers := []qAnswer{{picks: map[int]bool{0: true, 1: true, 2: true}, custom: "cust", note: "hello"}}
	out := convertMCPAnswers(qs, answers)
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("convertMCPAnswers output must marshal: %v", err)
	}
	if len(out[0].Picks) != 2 {
		t.Errorf("Picks=%v want [a b]", out[0].Picks)
	}
}

func TestPermissionRuleFor_FileTools(t *testing.T) {
	for _, tool := range []string{"Edit", "Write", "MultiEdit", "NotebookEdit", "Read"} {
		r := permissionRuleFor(tool, map[string]any{"file_path": "/a/b"})
		if r.toolName != tool || r.ruleContent != "/a/b" {
			t.Errorf("%s: rule=%+v", tool, r)
		}
	}
	r := permissionRuleFor("Edit", map[string]any{})
	if r.toolName != "Edit" || r.ruleContent != "" {
		t.Errorf("missing file_path should yield empty ruleContent, got %+v", r)
	}
}

func TestPermissionRuleFor_Bash(t *testing.T) {
	r := permissionRuleFor("Bash", map[string]any{"command": "ls -la"})
	if r.toolName != "Bash" || r.ruleContent != "ls -la" {
		t.Errorf("bash rule=%+v", r)
	}
}

func TestPermissionRuleFor_OtherTool(t *testing.T) {
	r := permissionRuleFor("Glob", map[string]any{"pattern": "*.go"})
	if r.toolName != "Glob" || r.ruleContent != "" {
		t.Errorf("non-file/bash tools should leave ruleContent empty, got %+v", r)
	}
}

func TestBuildApprovalBody_Deny(t *testing.T) {
	body := buildApprovalBody(false, map[string]any{"command": "rm -rf"}, nil)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if parsed["behavior"] != "deny" {
		t.Errorf("behavior=%v want deny", parsed["behavior"])
	}
	if _, ok := parsed["message"]; !ok {
		t.Error("deny body should include message")
	}
	if _, ok := parsed["updatedInput"]; ok {
		t.Error("deny body should NOT include updatedInput")
	}
}

func TestBuildApprovalBody_AllowWithoutRemember(t *testing.T) {
	in := map[string]any{"command": "ls"}
	body := buildApprovalBody(true, in, nil)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if parsed["behavior"] != "allow" {
		t.Errorf("behavior=%v want allow", parsed["behavior"])
	}
	upd, ok := parsed["updatedInput"].(map[string]any)
	if !ok || upd["command"] != "ls" {
		t.Errorf("updatedInput missing/wrong: %v", parsed["updatedInput"])
	}
	if _, ok := parsed["updatedPermissions"]; ok {
		t.Error("no remember → should not include updatedPermissions")
	}
}

func TestBuildApprovalBody_AllowWithRememberSession(t *testing.T) {
	rule := permissionRule{toolName: "Edit", ruleContent: "/a/b"}
	body := buildApprovalBody(true, nil, &rule)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if parsed["behavior"] != "allow" {
		t.Errorf("behavior=%v want allow", parsed["behavior"])
	}
	upd, _ := parsed["updatedInput"].(map[string]any)
	if upd == nil {
		t.Errorf("updatedInput should be a non-nil map (empty ok): %v", parsed["updatedInput"])
	}
	permsAny, ok := parsed["updatedPermissions"].([]any)
	if !ok || len(permsAny) != 1 {
		t.Fatalf("updatedPermissions missing or wrong shape: %v", parsed["updatedPermissions"])
	}
	p := permsAny[0].(map[string]any)
	if p["type"] != "addRules" {
		t.Errorf("type=%v want addRules", p["type"])
	}
	if p["destination"] != "session" {
		t.Errorf("destination=%v want session", p["destination"])
	}
	if p["behavior"] != "allow" {
		t.Errorf("inner behavior=%v want allow", p["behavior"])
	}
	rules := p["rules"].([]any)
	r0 := rules[0].(map[string]any)
	if r0["toolName"] != "Edit" || r0["ruleContent"] != "/a/b" {
		t.Errorf("inner rule=%+v", r0)
	}
}

func TestBuildApprovalBody_EmptyRuleContentBecomesNull(t *testing.T) {
	rule := permissionRule{toolName: "Glob"} // no ruleContent
	body := buildApprovalBody(true, map[string]any{"pattern": "*.md"}, &rule)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	perms := parsed["updatedPermissions"].([]any)
	r0 := perms[0].(map[string]any)["rules"].([]any)[0].(map[string]any)
	if _, ok := r0["ruleContent"]; ok {
		// ruleContent should marshal as JSON null and thus be present but nil.
		if v := r0["ruleContent"]; v != nil {
			t.Errorf("ruleContent=%v want null, JSON nil", v)
		}
	}
}

func TestBridge_RememberAndRuleAlwaysAllowed(t *testing.T) {
	b := &mcpBridge{alwaysAllow: map[permissionRule]struct{}{}}
	rule := permissionRule{toolName: "Read", ruleContent: "/x"}
	if b.ruleAlwaysAllowed(rule) {
		t.Error("empty allowlist must not claim allowed")
	}
	b.rememberAlwaysAllow(rule)
	if !b.ruleAlwaysAllowed(rule) {
		t.Error("rule should be allowed after remember")
	}
	empty := permissionRule{}
	b.rememberAlwaysAllow(empty)
	if b.ruleAlwaysAllowed(empty) {
		t.Error("empty toolName must never be allowed")
	}
}

func TestBridge_AlwaysAllowIsGoroutineSafe(t *testing.T) {
	b := &mcpBridge{alwaysAllow: map[permissionRule]struct{}{}}
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := permissionRule{toolName: "Bash", ruleContent: strconv.Itoa(i)}
			b.rememberAlwaysAllow(r)
			b.ruleAlwaysAllowed(r)
		}(i)
	}
	wg.Wait()
}

func TestConvertMCPAnswers_PicksMappedByIndex(t *testing.T) {
	qs := []mcpQuestion{{
		Kind: "pick_many",
		Options: []mcpOption{
			{Label: "red"}, {Label: "green"}, {Label: "blue"},
		},
		AllowCustom: false,
	}}
	answers := []qAnswer{{picks: map[int]bool{0: true, 2: true}}}
	out := convertMCPAnswers(qs, answers)
	if len(out[0].Picks) != 2 {
		t.Fatalf("Picks=%v want 2", out[0].Picks)
	}
	if out[0].Picks[0] != "red" || out[0].Picks[1] != "blue" {
		t.Errorf("order should preserve option order; got %v", out[0].Picks)
	}
}

func TestPermissionRuleFor_UnknownToolEmptyContent(t *testing.T) {
	r := permissionRuleFor("CustomTool", map[string]any{"anything": "x"})
	if r.ruleContent != "" {
		t.Errorf("unknown tool should leave ruleContent empty, got %+v", r)
	}
}

// postHook POSTs a hook event body to the bridge and returns the status
// code. The bridge is spun up for real (real listener, real mux) so the
// routing through http.ServeMux is covered end-to-end; we just don't
// care about tea.Program delivery here (teaProgramPtr may be nil in
// tests, the handler guards against it).
func postHook(t *testing.T, port int, event string, body any) int {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/hooks/%s", port, event)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestMCPBridge_HookEndpointsReturn200(t *testing.T) {
	b, err := newMCPBridge(7)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	defer b.stop()

	start := map[string]any{
		"session_id":      "s1",
		"hook_event_name": "SubagentStart",
		"agent_id":        "agent_123",
		"agent_type":      "general-purpose",
	}
	if code := postHook(t, b.port, "subagent-start", start); code != http.StatusOK {
		t.Errorf("subagent-start POST got %d want 200", code)
	}

	stop := map[string]any{
		"session_id":      "s1",
		"hook_event_name": "SubagentStop",
		"agent_id":        "agent_123",
		"agent_type":      "general-purpose",
	}
	if code := postHook(t, b.port, "subagent-stop", stop); code != http.StatusOK {
		t.Errorf("subagent-stop POST got %d want 200", code)
	}
}

func TestMCPBridge_HookEndpointsRejectBadJSON(t *testing.T) {
	b, err := newMCPBridge(1)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	defer b.stop()

	url := fmt.Sprintf("http://127.0.0.1:%d/hooks/subagent-start", b.port)
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte("not-json")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad JSON should 400, got %d", resp.StatusCode)
	}
}

// The MCP streamable-HTTP endpoint must still be reachable after the
// mux is wired up — the catch-all "/" route has to keep working
// alongside /hooks/*.
func TestMCPBridge_MCPEndpointStillRoutes(t *testing.T) {
	b, err := newMCPBridge(1)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	defer b.stop()
	// A GET to "/" should reach the MCP handler; we don't care about the
	// body, just that we get an HTTP response (not a mux-level 404).
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", b.port))
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("MCP handler unreachable: got 404 at /")
	}
}

// newMCPBridge must wire up an http.Server (not bare http.Serve) so
// stop() can call Shutdown for graceful drain. Without this, the
// shutdown race against closeMemoryService re-opens.
func TestNewMCPBridge_BuildsHTTPServer(t *testing.T) {
	b, err := newMCPBridge(1)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	defer b.stop()
	if b.httpServer == nil {
		t.Fatalf("newMCPBridge should populate httpServer; got nil")
	}
	if b.httpServer.Handler == nil {
		t.Errorf("httpServer.Handler should be the mux, got nil")
	}
}

// After stop(), the listener is gone — a fresh dial against the
// previously-active port either errors immediately (RST) or fails
// the dial deadline. Either signals the bridge is done.
func TestMCPBridge_StopMakesPortUnreachable(t *testing.T) {
	b, err := newMCPBridge(1)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", b.port)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial before stop: %v", err)
	}
	_ = conn.Close()

	b.stop()

	// Allow a tiny moment for the listener close to propagate to the
	// kernel — on some platforms net.Listener.Close returns before the
	// socket actually leaves LISTEN state.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr != nil {
			return // expected: port is unreachable
		}
		_ = c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("port %s remained reachable after stop()", addr)
}

// stop() must drain in-flight HTTP handlers via http.Server.Shutdown
// before returning — that's the contract closeMemoryService relies on
// to avoid the neo4j nil-driver panic. We simulate an in-flight
// handler with a partial-body POST that parks json.Decode reading
// r.Body, then assert stop() blocks until we close the conn (which
// unparks Decode and lets the handler return).
func TestMCPBridge_StopDrainsInFlightHandler(t *testing.T) {
	b, err := newMCPBridge(2)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", b.port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// We deliberately keep `conn` open so the handler stays parked.
	defer func() { _ = conn.Close() }()

	// Declare a Content-Length larger than the bytes we're going to
	// write so json.NewDecoder(r.Body).Decode keeps reading from the
	// stream, parking the handler. The body is intentionally a JSON
	// fragment ("{\"source\":") with no closing brace; Decode will not
	// see a complete value until the conn drops.
	req := "POST /hooks/session-start HTTP/1.1\r\n" +
		"Host: 127.0.0.1\r\n" +
		"Content-Length: 4096\r\n" +
		"Content-Type: application/json\r\n" +
		"\r\n" +
		`{"source": "startup"`
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	// Give the server a beat to dispatch the request to the handler
	// goroutine. Local TCP loopback dispatch is sub-millisecond, so
	// 100ms is generous.
	time.Sleep(100 * time.Millisecond)

	stopDone := make(chan struct{})
	go func() {
		b.stop()
		close(stopDone)
	}()

	// While the handler is in-flight reading the body, stop() must NOT
	// have returned. If it does, Shutdown isn't actually being called,
	// or it's not waiting for handlers — the bug is back.
	select {
	case <-stopDone:
		t.Fatalf("stop() returned before in-flight handler completed")
	case <-time.After(150 * time.Millisecond):
		// expected: stop is parked draining
	}

	// Closing the client conn unblocks the handler's Decode (EOF / read
	// error), the handler returns, and Shutdown's poll loop sees no
	// active connections and returns.
	_ = conn.Close()

	select {
	case <-stopDone:
		// success
	case <-time.After(bridgeShutdownTimeout + time.Second):
		t.Fatalf("stop() did not complete after conn close (Shutdown deadline=%s)", bridgeShutdownTimeout)
	}
}

// stop() must remain idempotent under the new http.Server model — both
// because the existing test asserts it and because tabs.go calls it
// from both the per-tab close path and the app-wide shutdown path.
func TestMCPBridge_StopIsIdempotentWithHTTPServer(t *testing.T) {
	b, err := newMCPBridge(3)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	b.stop()
	b.stop() // must not panic / hang on a server that's already shut down
}
