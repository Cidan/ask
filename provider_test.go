package main

import (
	"bytes"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestProviderRegistry_ReturnsAnthropicByID(t *testing.T) {
	// The registry is populated by provider.go's init(); verify lookup
	// works end-to-end and the default (first registered) is anthropic.
	p := providerByID("anthropic")
	if p == nil {
		t.Fatalf("providerByID(\"anthropic\") returned nil; registry=%d", len(providerRegistry))
	}
	if p.ID() != "anthropic" {
		t.Fatalf("want ID=anthropic, got %q", p.ID())
	}
	if def := providerByID(""); def == nil || def.ID() != "anthropic" {
		t.Fatalf("empty id must fall back to anthropic, got %v", def)
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

// providerByIDStrict must NEVER fall back to the first registered
// provider. The resume-side LastProvider override depends on this:
// silently swapping providers when the recorded id is missing would
// either reopen the conversation under the wrong backend (defeating
// the purpose) or hide the rename/removal from the user.
func TestProviderByIDStrict_HitReturnsTrue(t *testing.T) {
	f1 := newFakeProvider()
	f1.id = "alpha"
	f2 := newFakeProvider()
	f2.id = "beta"
	withRegisteredProviders(t, f1, f2)
	got, ok := providerByIDStrict("beta")
	if !ok {
		t.Fatal("strict lookup should find beta")
	}
	if got != f2 {
		t.Errorf("strict beta returned %v want f2", got)
	}
}

func TestProviderByIDStrict_MissReturnsNilFalse(t *testing.T) {
	f1 := newFakeProvider()
	f1.id = "alpha"
	withRegisteredProviders(t, f1)
	got, ok := providerByIDStrict("not-real")
	if ok {
		t.Error("strict miss must return false, not the first-fallback")
	}
	if got != nil {
		t.Errorf("strict miss should return nil, got %v", got)
	}
}

func TestProviderByIDStrict_EmptyIDReturnsNilFalse(t *testing.T) {
	f1 := newFakeProvider()
	f1.id = "alpha"
	withRegisteredProviders(t, f1)
	got, ok := providerByIDStrict("")
	if ok || got != nil {
		t.Errorf("strict empty id must miss; got=%v ok=%v", got, ok)
	}
}

func TestProviderByIDStrict_EmptyRegistryMisses(t *testing.T) {
	withRegisteredProviders(t)
	got, ok := providerByIDStrict("anything")
	if ok || got != nil {
		t.Errorf("strict empty registry must miss; got=%v ok=%v", got, ok)
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
