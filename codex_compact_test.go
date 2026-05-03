package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestCodexBaseSlashCommands_IncludesCompact pins the contract the
// /compact slash autocomplete relies on: the entry must be present
// in codex's BaseSlashCommands so the popover suggests it.
func TestCodexBaseSlashCommands_IncludesCompact(t *testing.T) {
	var cp codexProvider
	var found bool
	for _, s := range cp.BaseSlashCommands() {
		if s.name == "/compact" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("codex BaseSlashCommands missing /compact: %+v", cp.BaseSlashCommands())
	}
}

// /compact is codex-specific; claude has no equivalent protocol so
// the slash must NOT appear in claude's autocomplete.
func TestClaudeBaseSlashCommands_OmitsCompact(t *testing.T) {
	var cp claudeProvider
	for _, s := range cp.BaseSlashCommands() {
		if s.name == "/compact" {
			t.Errorf("claude BaseSlashCommands must not include /compact: %+v",
				cp.BaseSlashCommands())
		}
	}
}

// Happy path: codexCompact serialises a JSON-RPC thread/compact/start
// frame containing the active threadId and bumps nextID under the
// state mutex.
func TestCodexCompact_SendsThreadCompactStartWithThreadID(t *testing.T) {
	buf := &bufferCloser{Buffer: &bytes.Buffer{}}
	state := &codexState{
		stdin:    buf,
		threadID: "tid-active",
		nextID:   42,
	}
	p := &providerProc{stdin: buf, payload: state}

	sent, err := codexCompact(p)
	if err != nil {
		t.Fatalf("codexCompact: %v", err)
	}
	if !sent {
		t.Errorf("sent=false with threadID populated; want true")
	}
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &env); err != nil {
		t.Fatalf("invalid JSON %q: %v", buf.String(), err)
	}
	if env["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc=%v want 2.0", env["jsonrpc"])
	}
	if env["method"] != "thread/compact/start" {
		t.Errorf("method=%v want thread/compact/start", env["method"])
	}
	// id must come from the captured nextID before the bump (42),
	// and state.nextID must now be 43 — same handshake-id pattern
	// the rest of codex uses.
	if got, ok := env["id"].(float64); !ok || int(got) != 42 {
		t.Errorf("id=%v (%T) want 42", env["id"], env["id"])
	}
	if state.nextID != 43 {
		t.Errorf("state.nextID=%d want 43 after compact send", state.nextID)
	}
	params, _ := env["params"].(map[string]any)
	if params == nil {
		t.Fatalf("params missing: %v", env)
	}
	if params["threadId"] != "tid-active" {
		t.Errorf("threadId=%v want tid-active", params["threadId"])
	}
}

// No threadID → codex hasn't completed the handshake yet (or we're
// looking at a stale proc). Caller decides what to surface, but
// the helper must not write a malformed compact frame to the wire.
func TestCodexCompact_NoThreadIDReturnsFalseWithoutWrite(t *testing.T) {
	buf := &bufferCloser{Buffer: &bytes.Buffer{}}
	state := &codexState{stdin: buf}
	p := &providerProc{stdin: buf, payload: state}

	sent, err := codexCompact(p)
	if err != nil {
		t.Fatalf("codexCompact: %v", err)
	}
	if sent {
		t.Errorf("sent=true with empty threadID")
	}
	if buf.Len() != 0 {
		t.Errorf("no frames should be written; got %q", buf.String())
	}
}

// Defensive: a proc handed over without a codexState payload must
// not panic. This shouldn't happen in production (only codex's
// StartSession constructs payload) but covers test fakes and any
// future provider-swap refactor that hands a different payload type.
func TestCodexCompact_NilPayloadReturnsFalse(t *testing.T) {
	sent, err := codexCompact(&providerProc{})
	if err != nil || sent {
		t.Errorf("nil payload must short-circuit: sent=%v err=%v", sent, err)
	}
}

// Wrong-provider gate: /compact submitted by a user on the claude
// provider must report the limitation rather than appearing to do
// nothing. The slash popover doesn't surface /compact for claude,
// but a user can still type it manually.
func TestHandleCodexCompact_WrongProviderAppendsExplanation(t *testing.T) {
	fp := newFakeProvider()
	fp.id = "claude" // anything that isn't "codex"
	m := newTestModel(t, fp)

	m2, _ := m.handleCodexCompact()
	mm := m2.(model)
	if len(mm.history) == 0 {
		t.Fatal("wrong-provider /compact must append a history entry")
	}
	if !strings.Contains(mm.history[0].text, "Codex") {
		t.Errorf("history msg should mention Codex; got %q", mm.history[0].text)
	}
}

// No live proc → codex hasn't been started in this tab. Per the
// issue: "ask prints No Codex session to compact. and does not
// start a provider turn."
func TestHandleCodexCompact_NoProcAppendsNoSession(t *testing.T) {
	fp := newFakeProvider()
	fp.id = "codex"
	m := newTestModel(t, fp)
	m.proc = nil // explicit: no codex thread yet

	m2, _ := m.handleCodexCompact()
	mm := m2.(model)
	if len(mm.history) == 0 {
		t.Fatal("/compact with no proc must append a history entry")
	}
	if !strings.Contains(mm.history[0].text, "No Codex session") {
		t.Errorf("history msg should say no codex session; got %q", mm.history[0].text)
	}
	if mm.busy {
		t.Errorf("/compact with no proc must not flip busy")
	}
}

// Proc but no threadID — codex started but the handshake hasn't
// landed yet. codexCompact returns sent=false and the handler
// surfaces the same "no session" line as the no-proc case.
func TestHandleCodexCompact_ProcWithoutThreadIDAppendsNoSession(t *testing.T) {
	fp := newFakeProvider()
	fp.id = "codex"
	m := newTestModel(t, fp)
	buf := &bufferCloser{Buffer: &bytes.Buffer{}}
	m.proc = &providerProc{stdin: buf, payload: &codexState{stdin: buf}}

	m2, _ := m.handleCodexCompact()
	mm := m2.(model)
	if len(mm.history) == 0 {
		t.Fatal("/compact with no threadID must append a history entry")
	}
	if !strings.Contains(mm.history[0].text, "No Codex session") {
		t.Errorf("history msg should say no codex session; got %q", mm.history[0].text)
	}
	if buf.Len() != 0 {
		t.Errorf("no frames written without threadID; got %q", buf.String())
	}
}

// Mid-turn refusal: same shape as /provider's busy guard. Sending
// compact while a turn is in flight would race the stream reader
// and the next turn/completed.
func TestHandleCodexCompact_BusyIsNoOp(t *testing.T) {
	fp := newFakeProvider()
	fp.id = "codex"
	m := newTestModel(t, fp)
	buf := &bufferCloser{Buffer: &bytes.Buffer{}}
	m.proc = &providerProc{stdin: buf, payload: &codexState{
		stdin: buf, threadID: "tid",
	}}
	m.busy = true
	historyLen := len(m.history)

	m2, _ := m.handleCodexCompact()
	mm := m2.(model)
	if len(mm.history) != historyLen {
		t.Errorf("/compact while busy must not append history; len=%d want %d",
			len(mm.history), historyLen)
	}
	if buf.Len() != 0 {
		t.Errorf("/compact while busy must not write; got %q", buf.String())
	}
}

// End-to-end: codex provider + live proc + threadID → handler
// emits the JSON-RPC frame on stdin. This is the one test that
// proves the wiring across handleCodexCompact → codexCompact →
// codex stdin.
func TestHandleCodexCompact_HappyPathWritesThreadCompactStart(t *testing.T) {
	fp := newFakeProvider()
	fp.id = "codex"
	m := newTestModel(t, fp)
	buf := &bufferCloser{Buffer: &bytes.Buffer{}}
	m.proc = &providerProc{stdin: buf, payload: &codexState{
		stdin:    buf,
		threadID: "tid-live",
		nextID:   1,
	}}

	m2, _ := m.handleCodexCompact()
	mm := m2.(model)
	if len(mm.history) != 0 {
		t.Errorf("happy-path /compact should not append a history line; got %+v",
			mm.history)
	}
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &env); err != nil {
		t.Fatalf("invalid JSON %q: %v", buf.String(), err)
	}
	if env["method"] != "thread/compact/start" {
		t.Errorf("method=%v want thread/compact/start", env["method"])
	}
	params, _ := env["params"].(map[string]any)
	if params == nil || params["threadId"] != "tid-live" {
		t.Errorf("params=%v want threadId=tid-live", params)
	}
}

// /compact dispatched through handleCommand should land at
// handleCodexCompact, not "unknown command". Direct test through
// the slash dispatcher proves the case wiring in update.go.
func TestHandleCommand_CompactDispatchesToCodexHandler(t *testing.T) {
	fp := newFakeProvider()
	fp.id = "codex"
	m := newTestModel(t, fp)
	buf := &bufferCloser{Buffer: &bytes.Buffer{}}
	m.proc = &providerProc{stdin: buf, payload: &codexState{
		stdin:    buf,
		threadID: "tid",
		nextID:   1,
	}}

	m2, _ := m.handleCommand("/compact")
	mm := m2.(model)
	// Either the wire write happened (happy path) OR the no-session
	// branch was taken (would have appended history). Anything else
	// — "unknown command" — is the bug we're guarding against.
	if buf.Len() == 0 && len(mm.history) == 0 {
		t.Error("/compact reached neither the wire path nor the no-session path; case wiring missing?")
	}
	for _, h := range mm.history {
		if strings.Contains(h.text, "unknown command") {
			t.Errorf("/compact reported unknown command; case missing in handleCommand: %+v", h)
		}
	}
}
