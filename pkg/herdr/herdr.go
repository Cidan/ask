// Package herdr reports ask's agent state to herdr (https://herdr.dev), the
// agent-aware terminal multiplexer, when ask runs inside a herdr pane:
// whether ask is idle, working, or blocked on the user, the command that
// resumes its session after a herdr restart, and display metadata for the
// pane. Reports travel over herdr's socket API (newline-delimited JSON)
// from one background goroutine; they are best-effort and never block the
// caller.
package herdr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// Source identifies ask's reports. herdr reserves the "herdr:" prefix
	// for its own integrations.
	Source = "ask"
	// Agent is the agent name herdr shows for the pane.
	Agent = "ask"
)

// The variables herdr sets for every process in a pane.
const (
	EnvMarker = "HERDR_ENV"
	EnvPaneID = "HERDR_PANE_ID"
	EnvSocket = "HERDR_SOCKET_PATH"
)

// State is herdr's semantic agent state. herdr derives "done" (finished
// and not yet looked at) on its own.
type State string

const (
	Idle    State = "idle"
	Working State = "working"
	Blocked State = "blocked"
)

// Status is everything ask tells herdr about its pane. Every Report
// carries the whole status; the client sends only the parts herdr has not
// yet accepted.
type Status struct {
	State State
	// Message says what a blocked agent is waiting for.
	Message string
	// SessionID names the session Resume reopens. Resume is the argv herdr
	// runs in the pane after a server restart; its first element is a
	// plain command name.
	SessionID string
	Resume    []string
	// Title and Tokens are display-only: the pane title, and named values
	// herdr's sidebar renders as $name.
	Title  string
	Tokens map[string]string
}

// DebugLog is the package's debug sink; the TUI points it at its ASK_DEBUG
// logger. A no-op by default.
var DebugLog = func(format string, args ...any) {}

// Client reports one pane's status to herdr. A nil *Client is valid and
// does nothing, which is what FromEnv returns outside herdr.
type Client struct {
	socket  string
	pane    string
	timeout time.Duration
	backoff time.Duration

	mu   sync.Mutex
	seq  uint64
	want *Status
	sent *Status
	// epoch counts Reclaims; a flush that started before one must not
	// record what it sent, since herdr has since dropped it.
	epoch    uint64
	retryAt  time.Time
	released bool

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// FromEnv returns a client for the pane described by herdr's variables, or
// nil when the process is not running inside a herdr pane.
func FromEnv(getenv func(string) string) *Client {
	if getenv(EnvMarker) != "1" {
		return nil
	}
	socket, pane := getenv(EnvSocket), getenv(EnvPaneID)
	if socket == "" || pane == "" {
		return nil
	}
	return New(socket, pane)
}

// New returns a client reporting pane over the herdr socket at socket.
func New(socket, pane string) *Client {
	return newClient(socket, pane, 500*time.Millisecond, 2*time.Second)
}

// newClient bounds each request by timeout and waits backoff after a
// transport failure.
func newClient(socket, pane string, timeout, backoff time.Duration) *Client {
	c := &Client{
		socket:  socket,
		pane:    pane,
		timeout: timeout,
		backoff: backoff,
		// herdr ignores a report whose seq is not above the last one it
		// accepted from this source over the pane's whole life, so a new
		// ask in the same pane must start above every earlier one.
		seq:  uint64(time.Now().UnixNano()),
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go c.run()
	return c
}

// Report makes s the pane's status. It returns at once; a report still in
// flight is superseded rather than queued behind.
func (c *Client) Report(s Status) {
	if c == nil {
		return
	}
	s = s.clone()
	c.mu.Lock()
	if c.released || (c.want != nil && c.want.equal(s)) {
		c.mu.Unlock()
		return
	}
	c.want = &s
	c.mu.Unlock()
	c.signal()
}

// Reclaim resends the whole status. herdr drops an agent once its pane is
// back at a shell prompt, as it is while ask is suspended, so ask reclaims
// the pane when it resumes.
func (c *Client) Reclaim() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.sent = nil
	c.epoch++
	c.retryAt = time.Time{}
	c.mu.Unlock()
	c.signal()
}

// Release stops reporting and hands the pane back to herdr, which clears
// ask's name, state, and resume command from it.
func (c *Client) Release() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		return
	}
	c.released = true
	req := c.requestLocked("pane.release_agent", func(seq uint64) any {
		return releaseParams{PaneID: c.pane, Source: Source, Agent: Agent, Seq: seq}
	})
	c.mu.Unlock()
	close(c.stop)
	<-c.done
	if err := c.call(req); err != nil {
		DebugLog("herdr: release failed: %v", err)
	}
}

func (c *Client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Client) run() {
	defer close(c.done)
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-c.wake:
		case <-retry.C:
		}
		if wait := c.flush(); wait > 0 {
			retry.Reset(wait)
		}
	}
}

// flush sends the parts of the wanted status herdr has not accepted. After
// a transport failure it backs off and returns how long to wait before
// trying again; a request herdr rejects is logged and not retried.
func (c *Client) flush() time.Duration {
	c.mu.Lock()
	if c.released || c.want == nil {
		c.mu.Unlock()
		return 0
	}
	if wait := time.Until(c.retryAt); wait > 0 {
		c.mu.Unlock()
		return wait
	}
	want := *c.want
	epoch := c.epoch
	var sent Status
	if c.sent != nil {
		sent = *c.sent
	}
	agentDue := c.sent == nil || !want.sameAgent(sent)
	displayDue := c.sent == nil || !want.sameDisplay(sent)
	var agentReq, displayReq request
	if agentDue {
		agentReq = c.requestLocked("pane.report_agent", func(seq uint64) any { return reportParams(c.pane, want, seq) })
	}
	if displayDue {
		prev := c.sent
		displayReq = c.requestLocked("pane.report_metadata", func(seq uint64) any { return metadataParams(c.pane, want, prev, seq) })
	}
	c.mu.Unlock()

	if agentDue {
		if c.failed(c.call(agentReq)) {
			return c.backoff
		}
		c.mu.Lock()
		sent.State, sent.Message, sent.SessionID, sent.Resume = want.State, want.Message, want.SessionID, want.Resume
		c.markSentLocked(sent, epoch)
		c.mu.Unlock()
	}
	if displayDue {
		if c.failed(c.call(displayReq)) {
			return c.backoff
		}
		c.mu.Lock()
		sent.Title, sent.Tokens = want.Title, want.Tokens
		c.markSentLocked(sent, epoch)
		c.mu.Unlock()
	}
	return 0
}

func (c *Client) markSentLocked(s Status, epoch uint64) {
	if !c.released && c.epoch == epoch {
		c.sent = &s
	}
}

// failed reports whether err is a transport failure worth retrying, and
// arms the backoff when it is.
func (c *Client) failed(err error) bool {
	if err == nil {
		return false
	}
	var rejected *RejectedError
	if errors.As(err, &rejected) {
		DebugLog("herdr: %v", err)
		return false
	}
	DebugLog("herdr: report failed, retrying in %s: %v", c.backoff, err)
	c.mu.Lock()
	c.retryAt = time.Now().Add(c.backoff)
	c.mu.Unlock()
	return true
}

// RejectedError is herdr's answer to a request it refused.
type RejectedError struct {
	Method  string
	Code    string
	Message string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("%s rejected: %s: %s", e.Method, e.Code, e.Message)
}

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID    string `json:"id"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// requestLocked stamps the next sequence number on a request. One counter
// serves every method, so each request is newer than all before it.
func (c *Client) requestLocked(method string, params func(seq uint64) any) request {
	c.seq++
	return request{ID: fmt.Sprintf("%s:%d", Source, c.seq), Method: method, Params: params(c.seq)}
}

// call sends one request on a fresh connection and reads herdr's one-line
// answer.
func (c *Client) call(req request) error {
	conn, err := net.DialTimeout("unix", c.socket, c.timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return err
	}
	reply, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	var resp response
	if err := json.Unmarshal(reply, &resp); err != nil {
		return fmt.Errorf("%s: malformed reply: %w", req.Method, err)
	}
	if resp.Error != nil {
		return &RejectedError{Method: req.Method, Code: resp.Error.Code, Message: resp.Error.Message}
	}
	return nil
}

type agentParams struct {
	PaneID         string   `json:"pane_id"`
	Source         string   `json:"source"`
	Agent          string   `json:"agent"`
	State          State    `json:"state"`
	Message        string   `json:"message,omitempty"`
	Seq            uint64   `json:"seq"`
	AgentSessionID string   `json:"agent_session_id,omitempty"`
	ResumeArgv     []string `json:"resume_argv,omitempty"`
}

func reportParams(pane string, s Status, seq uint64) agentParams {
	p := agentParams{PaneID: pane, Source: Source, Agent: Agent, State: s.State, AgentSessionID: s.SessionID, Seq: seq}
	if s.State == Blocked {
		p.Message = s.Message
	}
	if validResumeArgv(s.Resume) {
		p.ResumeArgv = s.Resume
	}
	return p
}

type metaParams struct {
	PaneID     string             `json:"pane_id"`
	Source     string             `json:"source"`
	Agent      string             `json:"agent"`
	Title      string             `json:"title,omitempty"`
	ClearTitle bool               `json:"clear_title,omitempty"`
	Tokens     map[string]*string `json:"tokens,omitempty"`
	Seq        uint64             `json:"seq"`
}

// metadataParams patches herdr's display metadata from sent to s: tokens
// ask no longer reports are cleared with null.
func metadataParams(pane string, s Status, sent *Status, seq uint64) metaParams {
	p := metaParams{PaneID: pane, Source: Source, Agent: Agent, Title: s.Title, ClearTitle: s.Title == "", Seq: seq}
	tokens := make(map[string]*string, len(s.Tokens))
	for k, v := range s.Tokens {
		tokens[k] = &v
	}
	if sent != nil {
		for k := range sent.Tokens {
			if _, ok := s.Tokens[k]; !ok {
				tokens[k] = nil
			}
		}
	}
	if len(tokens) > 0 {
		p.Tokens = tokens
	}
	return p
}

type releaseParams struct {
	PaneID string `json:"pane_id"`
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Seq    uint64 `json:"seq"`
}

// validResumeArgv applies herdr's validate_resume_argv. herdr rejects the
// whole report, state included, when the command breaks it, so an invalid
// command is left out rather than sent.
func validResumeArgv(argv []string) bool {
	if len(argv) == 0 || len(argv) > 64 {
		return false
	}
	command := argv[0]
	if command == "" || command[0] == '-' {
		return false
	}
	for i := 0; i < len(command); i++ {
		b := command[i]
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-' || b == '.') {
			return false
		}
	}
	total := 0
	for _, a := range argv {
		total += len(a)
		if strings.ContainsRune(a, '\'') || strings.ContainsFunc(a, unicode.IsControl) {
			return false
		}
	}
	return total <= 8192
}

func (s Status) clone() Status {
	s.Resume = slices.Clone(s.Resume)
	s.Tokens = maps.Clone(s.Tokens)
	return s
}

func (s Status) equal(o Status) bool {
	return s.sameAgent(o) && s.sameDisplay(o)
}

func (s Status) sameAgent(o Status) bool {
	return s.State == o.State && s.Message == o.Message && s.SessionID == o.SessionID && slices.Equal(s.Resume, o.Resume)
}

func (s Status) sameDisplay(o Status) bool {
	return s.Title == o.Title && maps.Equal(s.Tokens, o.Tokens)
}

// WithoutPaneEnv returns env without herdr's pane variables. Processes ask
// starts on the agent's behalf get it, so a nested agent (an `ask`, a
// `claude` child with herdr's hooks installed, a test binary) can neither
// report over nor release ask's own status in the pane.
func WithoutPaneEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "HERDR_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
