package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// fakeHandshake wires a test scanner + write buffer and feeds them to
// codexHandshake. The returned state carries the handshake result; the
// returned stdin buffer lets tests assert the exact outgoing JSON frames.
func fakeHandshake(t *testing.T, serverOutput string, args ProviderSessionArgs) (*codexState, *bytes.Buffer, error) {
	t.Helper()
	var stdinBuf bytes.Buffer
	sc := bufio.NewScanner(strings.NewReader(serverOutput))
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	state := &codexState{nextID: codexTurnStartBaseID}
	err := codexHandshake(&stdinBuf, sc, state, args)
	return state, &stdinBuf, err
}

func TestCodexHandshake_SendsInitializeThenThreadStart(t *testing.T) {
	// A minimal, valid server response that carries thread.id.
	serverOut := `{"id":1,"result":{"userAgent":"codex/0.1"}}
{"id":2,"result":{"thread":{"id":"tid-123"}}}
`
	state, stdin, err := fakeHandshake(t, serverOut, ProviderSessionArgs{Cwd: "/work"})
	if err != nil {
		t.Fatalf("handshake err: %v", err)
	}
	if state.threadID != "tid-123" {
		t.Errorf("state.threadID=%q want tid-123", state.threadID)
	}
	// Client side must have written three frames in order: initialize,
	// initialized, thread/start.
	frames := decodeFrames(t, stdin.Bytes())
	if len(frames) != 3 {
		t.Fatalf("want 3 client frames, got %d: %v", len(frames), frames)
	}
	if frames[0]["method"] != "initialize" {
		t.Errorf("frame[0].method=%v want initialize", frames[0]["method"])
	}
	if _, hasID := frames[0]["id"]; !hasID {
		t.Errorf("initialize must carry id: %v", frames[0])
	}
	if frames[1]["method"] != "initialized" {
		t.Errorf("frame[1].method=%v want initialized", frames[1]["method"])
	}
	if _, hasID := frames[1]["id"]; hasID {
		t.Errorf("initialized is a notification — must not have id: %v", frames[1])
	}
	if frames[2]["method"] != "thread/start" {
		t.Errorf("frame[2].method=%v want thread/start", frames[2]["method"])
	}
	params, _ := frames[2]["params"].(map[string]any)
	if params["cwd"] != "/work" {
		t.Errorf("thread/start.params.cwd=%v want /work", params["cwd"])
	}
}

func TestCodexHandshake_OmitsCwdWhenEmpty(t *testing.T) {
	// Don't pin the thread to "" — let codex pick a reasonable default.
	serverOut := `{"id":2,"result":{"thread":{"id":"tid-x"}}}
`
	_, stdin, err := fakeHandshake(t, serverOut, ProviderSessionArgs{})
	if err != nil {
		t.Fatalf("handshake err: %v", err)
	}
	frames := decodeFrames(t, stdin.Bytes())
	params, _ := frames[2]["params"].(map[string]any)
	if _, hasCwd := params["cwd"]; hasCwd {
		t.Errorf("empty Cwd should omit cwd key: %v", params)
	}
}

func TestCodexHandshake_SkipsInterleavedNotifications(t *testing.T) {
	// The real server emits the initialize response, then startup
	// notifications, then the thread/start response. The handshake must
	// ignore the noise and pluck out id=2.
	serverOut := `{"id":1,"result":{"userAgent":"codex/0.1"}}
{"method":"mcpServer/startupStatus/updated","params":{"name":"codex_apps","status":"starting"}}
{"method":"thread/started","params":{"thread":{"id":"tid-early"}}}
{"id":2,"result":{"thread":{"id":"tid-real"}}}
`
	state, _, err := fakeHandshake(t, serverOut, ProviderSessionArgs{})
	if err != nil {
		t.Fatalf("handshake err: %v", err)
	}
	if state.threadID != "tid-real" {
		t.Errorf("handshake should read thread id from the id=2 response, got %q", state.threadID)
	}
}

func TestCodexHandshake_PropagatesRPCError(t *testing.T) {
	serverOut := `{"id":2,"error":{"code":-32000,"message":"bad cwd"}}
`
	state, _, err := fakeHandshake(t, serverOut, ProviderSessionArgs{Cwd: "/bogus"})
	if err == nil {
		t.Fatal("expected handshake error when thread/start returns JSON-RPC error")
	}
	if !strings.Contains(err.Error(), "bad cwd") {
		t.Errorf("error should surface server message; got %v", err)
	}
	if state.threadID != "" {
		t.Errorf("threadID must stay empty on error, got %q", state.threadID)
	}
}

func TestCodexHandshake_EmptyThreadIDIsError(t *testing.T) {
	serverOut := `{"id":2,"result":{"thread":{"id":""}}}
`
	_, _, err := fakeHandshake(t, serverOut, ProviderSessionArgs{})
	if err == nil {
		t.Fatal("expected error when thread.id is empty")
	}
}

func TestCodexHandshake_EOFBeforeResponseIsError(t *testing.T) {
	// Simulates codex exiting during startup (auth failure, bad config).
	_, _, err := fakeHandshake(t, "", ProviderSessionArgs{})
	if err == nil {
		t.Fatal("expected error when server pipe closes before response")
	}
}

func TestCodexHandshake_IgnoresMalformedLines(t *testing.T) {
	// A rogue stderr-into-stdout write or partial flush shouldn't kill the
	// handshake — scanner must skip and keep reading.
	serverOut := "garbage\n" +
		`{"id":2,"result":{"thread":{"id":"tid-ok"}}}` + "\n"
	state, _, err := fakeHandshake(t, serverOut, ProviderSessionArgs{})
	if err != nil {
		t.Fatalf("handshake err: %v", err)
	}
	if state.threadID != "tid-ok" {
		t.Errorf("threadID=%q want tid-ok", state.threadID)
	}
}

func TestCodexHandshake_ResumeSwapsMethodAndPassesThreadID(t *testing.T) {
	serverOut := `{"id":2,"result":{"thread":{"id":"prior-id"}}}
`
	state, stdin, err := fakeHandshake(t, serverOut, ProviderSessionArgs{
		SessionID: "prior-id",
		Cwd:       "/work",
	})
	if err != nil {
		t.Fatalf("handshake err: %v", err)
	}
	if state.threadID != "prior-id" {
		t.Errorf("threadID=%q want prior-id", state.threadID)
	}
	frames := decodeFrames(t, stdin.Bytes())
	if len(frames) != 3 {
		t.Fatalf("want 3 client frames, got %d: %v", len(frames), frames)
	}
	if frames[2]["method"] != "thread/resume" {
		t.Errorf("resume handshake must use thread/resume, got method=%v", frames[2]["method"])
	}
	params, _ := frames[2]["params"].(map[string]any)
	if params["threadId"] != "prior-id" {
		t.Errorf("params.threadId=%v want prior-id", params["threadId"])
	}
	if params["cwd"] != "/work" {
		t.Errorf("params.cwd=%v want /work", params["cwd"])
	}
}

func TestCodexHandshake_FreshSessionStillUsesThreadStart(t *testing.T) {
	// Guard the non-resume path: empty SessionID must keep sending
	// thread/start.
	serverOut := `{"id":2,"result":{"thread":{"id":"new-id"}}}
`
	_, stdin, err := fakeHandshake(t, serverOut, ProviderSessionArgs{})
	if err != nil {
		t.Fatalf("handshake err: %v", err)
	}
	frames := decodeFrames(t, stdin.Bytes())
	if frames[2]["method"] != "thread/start" {
		t.Errorf("fresh handshake must use thread/start, got %v", frames[2]["method"])
	}
}

// decodeFrames splits the stdin byte stream at newlines and JSON-parses each
// frame.
func decodeFrames(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad frame %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// Compile-time check that the stdin shim satisfies the interface the real
// handshake writes to.
var _ io.Writer = (*bytes.Buffer)(nil)
