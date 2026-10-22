package herdr

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type gotRequest struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// fakeHerdr is a herdr socket that records every request. reject makes it
// refuse a method; hold, when set, delays every reply until it is closed.
type fakeHerdr struct {
	path   string
	got    chan gotRequest
	reject map[string]string
	hold   chan struct{}
	l      net.Listener
}

// shortSocketPath keeps unix socket paths under the sun_path limit, which
// t.TempDir() paths (they embed the test name) can exceed.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "herdr.sock")
}

func startFakeHerdr(t *testing.T, path string, configure ...func(*fakeHerdr)) *fakeHerdr {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHerdr{path: path, got: make(chan gotRequest, 64), reject: map[string]string{}, l: l}
	for _, fn := range configure {
		fn(f)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeHerdr) serve(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req gotRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return
	}
	f.got <- req
	if f.hold != nil {
		<-f.hold
	}
	reply := map[string]any{"id": req.ID, "result": map[string]any{"type": "ok"}}
	if code, ok := f.reject[req.Method]; ok {
		reply = map[string]any{"id": req.ID, "error": map[string]any{"code": code, "message": "refused"}}
	}
	_ = json.NewEncoder(conn).Encode(reply)
}

func (f *fakeHerdr) next(t *testing.T) gotRequest {
	t.Helper()
	select {
	case r := <-f.got:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("herdr never received the expected request")
		return gotRequest{}
	}
}

func (f *fakeHerdr) nextMethod(t *testing.T, method string) gotRequest {
	t.Helper()
	r := f.next(t)
	if r.Method != method {
		t.Fatalf("request = %s %v, want %s", r.Method, r.Params, method)
	}
	return r
}

func newTestClient(t *testing.T, path string) *Client {
	t.Helper()
	c := newClient(path, "w1:p1", 200*time.Millisecond, 20*time.Millisecond)
	t.Cleanup(c.Release)
	return c
}

func seqOf(r gotRequest) uint64 {
	n, _ := strconv.ParseUint(string(r.Params["seq"].(json.Number)), 10, 64)
	return n
}

func TestFromEnv(t *testing.T) {
	path := shortSocketPath(t)
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"inside a herdr pane", map[string]string{EnvMarker: "1", EnvSocket: path, EnvPaneID: "w1:p1"}, true},
		{"outside herdr", map[string]string{}, false},
		{"marker not 1", map[string]string{EnvMarker: "0", EnvSocket: path, EnvPaneID: "w1:p1"}, false},
		{"no socket", map[string]string{EnvMarker: "1", EnvPaneID: "w1:p1"}, false},
		{"no pane", map[string]string{EnvMarker: "1", EnvSocket: path}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := FromEnv(func(k string) string { return tc.env[k] })
			defer c.Release()
			if (c != nil) != tc.want {
				t.Errorf("FromEnv() = %v, want a client: %v", c, tc.want)
			}
		})
	}
}

func TestReport_SendsStateResumeAndDisplayToTheOwnPane(t *testing.T) {
	path := shortSocketPath(t)
	f := startFakeHerdr(t, path)
	c := newTestClient(t, path)

	c.Report(Status{
		State:     Working,
		Message:   "ignored unless blocked",
		SessionID: "vs-abc",
		Resume:    []string{"ask", "resume", "vs-abc"},
		Title:     "Fix CI",
		Tokens:    map[string]string{"model": "vertex/gemini", "tabs": "2"},
	})

	agent := f.nextMethod(t, "pane.report_agent").Params
	for k, want := range map[string]any{"pane_id": "w1:p1", "source": "ask", "agent": "ask", "state": "working", "agent_session_id": "vs-abc"} {
		if agent[k] != want {
			t.Errorf("report_agent %s = %v, want %v", k, agent[k], want)
		}
	}
	if _, ok := agent["message"]; ok {
		t.Errorf("report_agent carried a message while working: %v", agent["message"])
	}
	if argv := agent["resume_argv"]; !slices.Equal(toStrings(argv), []string{"ask", "resume", "vs-abc"}) {
		t.Errorf("resume_argv = %v", argv)
	}

	meta := f.nextMethod(t, "pane.report_metadata").Params
	if meta["title"] != "Fix CI" || meta["agent"] != "ask" || meta["source"] != "ask" {
		t.Errorf("report_metadata = %v", meta)
	}
	if tokens := meta["tokens"].(map[string]any); tokens["model"] != "vertex/gemini" || tokens["tabs"] != "2" {
		t.Errorf("tokens = %v", tokens)
	}
}

func TestReport_SendsOnlyWhatChanged(t *testing.T) {
	path := shortSocketPath(t)
	f := startFakeHerdr(t, path)
	c := newTestClient(t, path)

	base := Status{State: Idle, Title: "a", Tokens: map[string]string{"model": "m", "tabs": "1"}}
	c.Report(base)
	f.nextMethod(t, "pane.report_agent")
	f.nextMethod(t, "pane.report_metadata")

	c.Report(base)
	blocked := base
	blocked.State, blocked.Message = Blocked, "approve bash"
	c.Report(blocked)
	agent := f.nextMethod(t, "pane.report_agent").Params
	if agent["state"] != "blocked" || agent["message"] != "approve bash" {
		t.Errorf("report_agent = %v, want blocked with its reason", agent)
	}

	retitled := blocked
	retitled.Title = ""
	retitled.Tokens = map[string]string{"tabs": "1"}
	c.Report(retitled)
	meta := f.nextMethod(t, "pane.report_metadata").Params
	if meta["clear_title"] != true {
		t.Errorf("report_metadata = %v, want the title cleared", meta)
	}
	tokens := meta["tokens"].(map[string]any)
	if v, ok := tokens["model"]; !ok || v != nil {
		t.Errorf("tokens = %v, want model cleared with null", tokens)
	}

	c.Report(Status{State: Working, Title: "", Tokens: map[string]string{"tabs": "1"}})
	if r := f.next(t); r.Method != "pane.report_agent" || r.Params["state"] != "working" {
		t.Errorf("next request = %s %v, want only the state change", r.Method, r.Params)
	}
}

func TestReport_SeqIncreasesAcrossRequestsAndClients(t *testing.T) {
	path := shortSocketPath(t)
	f := startFakeHerdr(t, path)
	first := newTestClient(t, path)
	first.Report(Status{State: Idle})
	a := seqOf(f.nextMethod(t, "pane.report_agent"))
	b := seqOf(f.nextMethod(t, "pane.report_metadata"))
	first.Release()
	c := seqOf(f.nextMethod(t, "pane.release_agent"))

	second := newTestClient(t, path)
	second.Report(Status{State: Idle})
	d := seqOf(f.nextMethod(t, "pane.report_agent"))

	if !(a < b && b < c && c < d) {
		t.Errorf("seqs %d %d %d %d are not strictly increasing; herdr ignores a report that is not newer than the last from the source", a, b, c, d)
	}
}

func TestReport_LeavesOutAResumeCommandHerdrWouldReject(t *testing.T) {
	path := shortSocketPath(t)
	f := startFakeHerdr(t, path)
	c := newTestClient(t, path)

	c.Report(Status{State: Working, SessionID: "vs-1", Resume: []string{"/home/u/go/bin/ask", "resume", "vs-1"}})
	agent := f.nextMethod(t, "pane.report_agent").Params
	if _, ok := agent["resume_argv"]; ok {
		t.Errorf("sent resume_argv %v; herdr would refuse the whole report", agent["resume_argv"])
	}
	if agent["state"] != "working" {
		t.Errorf("state = %v, want the state reported anyway", agent["state"])
	}
}

func TestValidResumeArgv(t *testing.T) {
	long := strings.Repeat("x", 8192)
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"ask", "resume", "vs-0123abcd"}, true},
		{[]string{"my-agent.v2_x"}, true},
		{nil, false},
		{[]string{""}, false},
		{[]string{"-ask"}, false},
		{[]string{"./ask"}, false},
		{[]string{"/usr/bin/ask"}, false},
		{[]string{`C:\ask`}, false},
		{[]string{"ask resume"}, false},
		{[]string{"ask", "it's"}, false},
		{[]string{"ask", "a\nb"}, false},
		{[]string{"ask", "a\u0085b"}, false},
		{[]string{"ask", long}, false},
		{append([]string{"ask"}, make([]string, 64)...), false},
	}
	for _, tc := range cases {
		if got := validResumeArgv(tc.argv); got != tc.want {
			t.Errorf("validResumeArgv(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestReport_CoalescesWhileHerdrIsSlow(t *testing.T) {
	path := shortSocketPath(t)
	hold := make(chan struct{})
	f := startFakeHerdr(t, path, func(f *fakeHerdr) { f.hold = hold })
	c := newTestClient(t, path)

	c.Report(Status{State: Working})
	if r := f.nextMethod(t, "pane.report_agent"); r.Params["state"] != "working" {
		t.Fatalf("first report = %v", r.Params)
	}
	c.Report(Status{State: Blocked, Message: "approve bash"})
	c.Report(Status{State: Idle})
	close(hold)

	f.nextMethod(t, "pane.report_metadata")
	if r := f.nextMethod(t, "pane.report_agent"); r.Params["state"] != "idle" {
		t.Errorf("after a slow reply herdr got state %v, want only the latest (idle)", r.Params["state"])
	}
}

func TestReport_RetriesOnceHerdrIsReachable(t *testing.T) {
	path := shortSocketPath(t)
	c := newTestClient(t, path)

	c.Report(Status{State: Working})
	time.Sleep(30 * time.Millisecond)
	f := startFakeHerdr(t, path)

	if r := f.nextMethod(t, "pane.report_agent"); r.Params["state"] != "working" {
		t.Errorf("report after herdr came up = %v", r.Params)
	}
}

func TestReport_RejectedRequestIsNotRetried(t *testing.T) {
	path := shortSocketPath(t)
	f := startFakeHerdr(t, path, func(f *fakeHerdr) { f.reject["pane.report_metadata"] = "invalid_metadata" })
	c := newTestClient(t, path)

	c.Report(Status{State: Working, Title: "t"})
	f.nextMethod(t, "pane.report_agent")
	f.nextMethod(t, "pane.report_metadata")

	c.Report(Status{State: Idle, Title: "t"})
	if r := f.next(t); r.Method != "pane.report_agent" || r.Params["state"] != "idle" {
		t.Errorf("next request = %s %v, want the state change without resending the refused metadata", r.Method, r.Params)
	}
}

func TestReclaim_ResendsTheWholeStatus(t *testing.T) {
	path := shortSocketPath(t)
	f := startFakeHerdr(t, path)
	c := newTestClient(t, path)

	s := Status{State: Idle, SessionID: "vs-1", Resume: []string{"ask", "resume", "vs-1"}, Title: "t"}
	c.Report(s)
	f.nextMethod(t, "pane.report_agent")
	f.nextMethod(t, "pane.report_metadata")

	c.Reclaim()
	agent := f.nextMethod(t, "pane.report_agent").Params
	if agent["agent_session_id"] != "vs-1" || agent["resume_argv"] == nil {
		t.Errorf("reclaim sent %v, want the session and resume command again", agent)
	}
	f.nextMethod(t, "pane.report_metadata")
}

func TestRelease_HandsThePaneBackAndStopsReporting(t *testing.T) {
	path := shortSocketPath(t)
	f := startFakeHerdr(t, path)
	c := newTestClient(t, path)

	c.Report(Status{State: Working})
	f.nextMethod(t, "pane.report_agent")
	f.nextMethod(t, "pane.report_metadata")

	c.Release()
	rel := f.nextMethod(t, "pane.release_agent").Params
	if rel["pane_id"] != "w1:p1" || rel["source"] != "ask" || rel["agent"] != "ask" {
		t.Errorf("release_agent = %v", rel)
	}
	c.Release()
	c.Report(Status{State: Idle})
	c.Reclaim()
	select {
	case r := <-f.got:
		t.Errorf("request after release: %s %v", r.Method, r.Params)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNilClientIsANoOp(t *testing.T) {
	var c *Client
	c.Report(Status{State: Working})
	c.Reclaim()
	c.Release()
}

func TestWithoutPaneEnv(t *testing.T) {
	got := WithoutPaneEnv([]string{"PATH=/bin", "HERDR_ENV=1", "HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH=/s", "HERDR_BIN_PATH=/h", "HOME=/u"})
	if !slices.Equal(got, []string{"PATH=/bin", "HOME=/u"}) {
		t.Errorf("WithoutPaneEnv = %v", got)
	}
}

func TestTestHomeClearsPaneEnv(t *testing.T) {
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HERDR_") {
			t.Errorf("%s survived testhome.Main; a test could report into the developer's pane", kv)
		}
	}
}

func toStrings(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := it.(string)
		out = append(out, s)
	}
	return out
}
