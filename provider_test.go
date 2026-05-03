package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestProviderRegistry_ReturnsClaudeByID(t *testing.T) {
	// claudeProvider registers itself in init(); verify lookup works end-to-end.
	p := providerByID("claude")
	if p == nil {
		t.Fatalf("providerByID(\"claude\") returned nil; registry=%d", len(providerRegistry))
	}
	if p.ID() != "claude" {
		t.Fatalf("want ID=claude, got %q", p.ID())
	}
}

func TestProviderRegistry_UnknownIDFallsBack(t *testing.T) {
	p := providerByID("not-a-real-provider")
	if p == nil {
		t.Fatal("expected fallback to first registered provider, got nil")
	}
	if len(providerRegistry) > 0 && p != providerRegistry[0] {
		t.Fatalf("fallback returned %q, want first registered %q", p.ID(), providerRegistry[0].ID())
	}
}

func TestProviderRegistry_EmptyReturnsNil(t *testing.T) {
	withRegisteredProviders(t)
	if providerByID("") != nil {
		t.Fatal("expected nil when registry is empty")
	}
	if providerByID("claude") != nil {
		t.Fatal("expected nil when registry is empty")
	}
}

func TestRegisterProvider_AppendsWithoutDedup(t *testing.T) {
	withRegisteredProviders(t, newFakeProvider())
	before := len(providerRegistry)
	registerProvider(newFakeProvider())
	registerProvider(newFakeProvider())
	if got := len(providerRegistry); got != before+2 {
		t.Fatalf("registerProvider dedups; len before=%d after=%d", before, got)
	}
}

func TestProviderByID_EmptyIDReturnsFirst(t *testing.T) {
	f1 := newFakeProvider()
	f1.id = "alpha"
	f2 := newFakeProvider()
	f2.id = "beta"
	withRegisteredProviders(t, f1, f2)
	if p := providerByID(""); p != f1 {
		t.Fatalf("empty id: want first=alpha, got %q", p.ID())
	}
	if p := providerByID("beta"); p != f2 {
		t.Fatalf("beta lookup: got %q", p.ID())
	}
}

func TestClaudeProvider_Metadata(t *testing.T) {
	var p claudeProvider
	if got := p.ID(); got != "claude" {
		t.Errorf("ID=%q want claude", got)
	}
	if got := p.DisplayName(); got != "Claude" {
		t.Errorf("DisplayName=%q want Claude", got)
	}
	caps := p.Capabilities()
	if !caps.Resume || !caps.ModelPicker || !caps.EffortPicker ||
		!caps.AskUserQuestionMCP || !caps.PermissionPromptMCP {
		t.Errorf("Capabilities missing expected flags: %+v", caps)
	}
	mp := p.ModelPicker()
	if mp.Prompt == "" {
		t.Error("ModelPicker prompt is empty")
	}
	if !mp.AllowCustom {
		t.Error("ModelPicker AllowCustom should be true")
	}
	if v, ok := mp.SubConfig[ollamaModelOption]; !ok || v != "ollama" {
		t.Errorf("ModelPicker SubConfig[ollama] = %q, want \"ollama\"", v)
	}
	var sawOllama bool
	for _, opt := range mp.Options {
		if opt == ollamaModelOption {
			sawOllama = true
		}
	}
	if !sawOllama {
		t.Errorf("ModelPicker options missing ollama entry: %v", mp.Options)
	}

	efforts := p.EffortOptions()
	wantEffort := map[string]bool{
		"default": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
	}
	if len(efforts) != len(wantEffort) {
		t.Errorf("EffortOptions len=%d want %d (%v)", len(efforts), len(wantEffort), efforts)
	}
	for _, e := range efforts {
		if !wantEffort[e] {
			t.Errorf("unexpected effort option %q", e)
		}
	}

	slashes := p.BaseSlashCommands()
	names := map[string]bool{}
	for _, s := range slashes {
		names[s.name] = true
	}
	for _, want := range []string{"/resume", "/new", "/clear", "/model", "/effort"} {
		if !names[want] {
			t.Errorf("BaseSlashCommands missing %q", want)
		}
	}
}

func TestProviderProc_KillSafeOnNil(t *testing.T) {
	var p *providerProc
	p.kill() // must not panic
}

func TestProviderProc_KillWithNilStdinAndCmd(t *testing.T) {
	p := &providerProc{}
	p.kill() // must not panic on nil cmd and nil stdin
}

func TestProviderProc_KillClosesStdin(t *testing.T) {
	tc := &trackCloser{Buffer: &bytes.Buffer{}}
	p := &providerProc{stdin: tc}
	p.kill()
	if !tc.closed {
		t.Errorf("kill() must call Close on stdin; close not observed")
	}
}

// TestKillProc_DrainsStreamChannelSoWriterCanExit pins the fix for the
// workflow-step freeze: when the model abandons a stream channel
// (workflow advancing to the next step, /clear, provider switch),
// the read side must keep draining until the writer's `defer close(ch)`
// runs. Without the drainer, a writer with stdout still backlogged
// blocks at `ch <- msg` after the 32-slot buffer fills, never reaches
// `cmd.Wait()`, and wedges the whole pipeline.
func TestKillProc_DrainsStreamChannelSoWriterCanExit(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	// Channel size mirrors the 32 used by readClaudeStream/readCodexStream.
	ch := make(chan tea.Msg, 32)
	m.proc = &providerProc{}
	m.streamCh = ch

	writerDone := make(chan struct{})
	// Writer mimics readClaudeStream's tail: many backlogged events
	// followed by `defer close(ch)`. With a 32-slot buffer and no
	// reader, a non-drained channel pins this goroutine forever.
	go func() {
		defer close(ch)
		defer close(writerDone)
		for i := 0; i < 200; i++ {
			ch <- assistantTextMsg{text: "x"}
		}
	}()

	(&m).killProc()

	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("writer goroutine did not exit; killProc must drain the abandoned stream channel")
	}
	if m.streamCh != nil {
		t.Errorf("killProc must clear m.streamCh; got %v", m.streamCh)
	}
	if m.proc != nil {
		t.Errorf("killProc must clear m.proc; got %v", m.proc)
	}
}

// TestDrainProviderStream_NilChannelSafe guards against a nil-deref
// regression — callers (eventually proc.go's error paths) may pass a
// nil channel when StartSession failed before allocating one.
func TestDrainProviderStream_NilChannelSafe(t *testing.T) {
	drainProviderStream(nil) // must not panic
}

// trackCloser is a bufferCloser variant that records whether Close was
// ever invoked. Used to verify kill() actually drives the io.WriteCloser.
type trackCloser struct {
	*bytes.Buffer
	closed bool
}

func (t *trackCloser) Close() error {
	t.closed = true
	return nil
}

func TestUserContent_StringWhenNoAttachments(t *testing.T) {
	got := userContent("hello", nil)
	if s, ok := got.(string); !ok || s != "hello" {
		t.Fatalf("userContent empty atts: got %T(%v), want string \"hello\"", got, got)
	}
}

func TestUserContent_BlocksWithAttachments(t *testing.T) {
	raw := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	got := userContent("look", []pendingAttachment{{data: raw, mime: "image/png"}})
	blocks, ok := got.([]map[string]any)
	if !ok {
		t.Fatalf("want []map[string]any, got %T", got)
	}
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks (text+image), got %d", len(blocks))
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "look" {
		t.Errorf("block[0] unexpected: %v", blocks[0])
	}
	if blocks[1]["type"] != "image" {
		t.Errorf("block[1] type=%v want image", blocks[1]["type"])
	}
	src, _ := blocks[1]["source"].(map[string]any)
	if src == nil {
		t.Fatal("block[1].source missing")
	}
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Errorf("source fields wrong: %v", src)
	}
	if src["data"] != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("source data not base64 of raw bytes")
	}
}

func TestUserContent_OmitsEmptyTextBlock(t *testing.T) {
	got := userContent("", []pendingAttachment{{mime: "image/png"}})
	blocks, _ := got.([]map[string]any)
	if len(blocks) != 1 {
		t.Fatalf("empty text + 1 image: want 1 block, got %d: %v", len(blocks), blocks)
	}
	if blocks[0]["type"] != "image" {
		t.Errorf("only block should be image, got %v", blocks[0]["type"])
	}
}

func TestUserContent_JSONSerialisable(t *testing.T) {
	raw := []byte("bytes")
	got := userContent("x", []pendingAttachment{{data: raw, mime: "image/jpeg"}})
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("userContent should be JSON serialisable: %v", err)
	}
}

func TestUserBarText(t *testing.T) {
	cases := []struct {
		line string
		n    int
		want string
	}{
		{"hi", 0, "hi"},
		{"", 1, "[image attached]"},
		{"hi", 1, "hi  [image attached]"},
		{"", 3, "[3 images attached]"},
		{"hi", 3, "hi  [3 images attached]"},
	}
	for _, c := range cases {
		if got := userBarText(c.line, c.n); got != c.want {
			t.Errorf("userBarText(%q, %d)=%q want %q", c.line, c.n, got, c.want)
		}
	}
}

func TestClaudeSend_WritesStreamJSON(t *testing.T) {
	buf := &bufferCloser{Buffer: &bytes.Buffer{}}
	p := &providerProc{stdin: buf}
	var cp claudeProvider
	if err := cp.Send(p, "hello world", nil); err != nil {
		t.Fatalf("Send err: %v", err)
	}
	line := bytes.TrimRight(buf.Bytes(), "\n")
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Errorf("Send output missing trailing newline")
	}
	var env map[string]any
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("invalid JSON %q: %v", buf.String(), err)
	}
	if env["type"] != "user" {
		t.Errorf("type=%v want user", env["type"])
	}
	msg, _ := env["message"].(map[string]any)
	if msg == nil {
		t.Fatalf("message field missing: %v", env)
	}
	if msg["role"] != "user" {
		t.Errorf("role=%v want user", msg["role"])
	}
	if msg["content"] != "hello world" {
		t.Errorf("content=%v want hello world", msg["content"])
	}
}

func TestClaudeSend_AttachmentsProduceBlocks(t *testing.T) {
	buf := &bufferCloser{Buffer: &bytes.Buffer{}}
	p := &providerProc{stdin: buf}
	var cp claudeProvider
	err := cp.Send(p, "look", []pendingAttachment{{data: []byte("png"), mime: "image/png"}})
	if err != nil {
		t.Fatalf("Send err: %v", err)
	}
	var env map[string]any
	_ = json.Unmarshal(buf.Bytes(), &env)
	msg := env["message"].(map[string]any)
	content, ok := msg["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content should be 2-element block list, got %T %v", msg["content"], msg["content"])
	}
}
