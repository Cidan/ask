package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// InnerCall is the scope of the tool runs one decide call makes on the
// model's behalf. Every inner call of a decide shares its rule delivery, so
// the decision model is handed each project rule and instruction file once;
// each call keeps the images its tool produced, which go to the decision
// model instead of the session's model.
type InnerCall struct {
	delivery *ruleDelivery

	mu     sync.Mutex
	images []*genai.Part
}

// NewInnerCall starts the scope of one decide call. Its delivery starts
// empty: the decision model has seen none of what the session's model was
// told.
func NewInnerCall() *InnerCall {
	return &InnerCall{delivery: newRuleDelivery()}
}

// ForCall is the scope of one inner call: the decide's delivery, its own
// images.
func (s *InnerCall) ForCall() *InnerCall {
	return &InnerCall{delivery: s.delivery}
}

// Context marks ctx as an inner call in this scope.
func (s *InnerCall) Context(ctx agent.Context) agent.Context {
	return innerCallContext{Context: ctx, scope: s}
}

// AddImage keeps an image an inner call's tool produced.
func (s *InnerCall) AddImage(p *genai.Part) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images = append(s.images, p)
}

// Images returns the images the call's tool produced.
func (s *InnerCall) Images() []*genai.Part {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*genai.Part(nil), s.images...)
}

type innerCallKey struct{}

type innerCallContext struct {
	agent.Context
	scope *InnerCall
}

func (c innerCallContext) Value(key any) any {
	if key == (innerCallKey{}) {
		return c.scope
	}
	return c.Context.Value(key)
}

// InnerCallOf returns the decide scope ctx runs in, nil for a call the model
// made itself. A read in an inner call does not count as the model having
// read the file.
func InnerCallOf(ctx context.Context) *InnerCall {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(innerCallKey{}).(*InnerCall)
	return s
}

// decisionStatePrefix keys a decision record in session state. Session
// state is saved with the session and restored when it is loaded, and never
// reaches the model.
const decisionStatePrefix = "ask:decision:"

// DecisionRecord is everything one decide call did: the raw results of its
// inner calls (which the model was never shown) and the answers it was.
type DecisionRecord struct {
	ID string `json:"id"`
	// CallID is the decide tool call's id, which ties the record to the
	// call in the transcript.
	CallID    string         `json:"callID,omitempty"`
	Model     string         `json:"model,omitempty"`
	Answers   string         `json:"answers,omitempty"`
	Calls     []DecisionCall `json:"calls,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// DecisionCall is one inner call: the tool, the arguments it ran with, and
// its result (or the error that stopped it).
type DecisionCall struct {
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args,omitempty"`
	Result map[string]any `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// Text is the call's result as a UI shows it, and whether it failed.
func (c DecisionCall) Text() (string, bool) {
	if c.Error != "" {
		return c.Error, true
	}
	return ToolResultText(c.Result)
}

// SaveDecision stores rec in the session's state.
func SaveDecision(ctx agent.Context, rec DecisionRecord) error {
	st := ctx.State()
	if st == nil {
		return errors.New("this session keeps no state")
	}
	return st.Set(decisionStatePrefix+rec.ID, rec)
}

// LoadDecision returns the record with id from the session's state.
func LoadDecision(ctx agent.Context, id string) (DecisionRecord, error) {
	st := ctx.State()
	if st == nil {
		return DecisionRecord{}, errors.New("this session keeps no decision results")
	}
	v, err := st.Get(decisionStatePrefix + id)
	if err != nil {
		return DecisionRecord{}, fmt.Errorf("no decision %s in this session", id)
	}
	rec, ok := decodeDecision(v)
	if !ok {
		return DecisionRecord{}, fmt.Errorf("decision %s is unreadable", id)
	}
	return rec, nil
}

// LastDecisionIndex is the highest decision number the session holds, 0
// for none — decision ids are d1, d2, ….
func LastDecisionIndex(st session.State) int {
	if st == nil {
		return 0
	}
	last := 0
	for k := range st.All() {
		if n := decisionIndex(strings.TrimPrefix(k, decisionStatePrefix)); strings.HasPrefix(k, decisionStatePrefix) && n > last {
			last = n
		}
	}
	return last
}

func decisionIndex(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "d"))
	if err != nil || !strings.HasPrefix(id, "d") {
		return 0
	}
	return n
}

// DecisionRecordsIn returns the decision records of a stored session's
// state, in the order they were made.
func DecisionRecordsIn(state map[string]any) []DecisionRecord {
	var out []DecisionRecord
	for k, v := range state {
		if !strings.HasPrefix(k, decisionStatePrefix) {
			continue
		}
		if rec, ok := decodeDecision(v); ok {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return decisionIndex(out[i].ID) < decisionIndex(out[j].ID) })
	return out
}

func decodeDecision(v any) (DecisionRecord, bool) {
	switch r := v.(type) {
	case DecisionRecord:
		return r, true
	case *DecisionRecord:
		return *r, r != nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return DecisionRecord{}, false
	}
	var rec DecisionRecord
	if json.Unmarshal(raw, &rec) != nil || rec.ID == "" {
		return DecisionRecord{}, false
	}
	return rec, true
}

// DecisionAddressPrefix starts the path read and grep take for a decision's
// raw results: decision:d4 for every inner call, decision:d4/3 for one.
const DecisionAddressPrefix = "decision:"

// DecisionAddress names a decision's raw results; Call 0 is every call.
type DecisionAddress struct {
	ID   string
	Call int
}

func (a DecisionAddress) String() string {
	if a.Call > 0 {
		return fmt.Sprintf("%s%s/%d", DecisionAddressPrefix, a.ID, a.Call)
	}
	return DecisionAddressPrefix + a.ID
}

// ParseDecisionAddress reads a decision:<id>[/<n>] path.
func ParseDecisionAddress(s string) (DecisionAddress, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), DecisionAddressPrefix)
	if !ok || rest == "" {
		return DecisionAddress{}, false
	}
	id, call, hasCall := strings.Cut(rest, "/")
	addr := DecisionAddress{ID: id}
	if decisionIndex(id) == 0 {
		return DecisionAddress{}, false
	}
	if hasCall {
		n, err := strconv.Atoi(call)
		if err != nil || n < 1 {
			return DecisionAddress{}, false
		}
		addr.Call = n
	}
	return addr, true
}

// IsDecisionAddress reports whether p names a decision's raw results
// rather than a file.
func IsDecisionAddress(p string) bool {
	_, ok := ParseDecisionAddress(p)
	return ok
}

// DecisionText is one inner call's raw result as text, named by its
// address.
type DecisionText struct {
	Name string
	Text string
}

// DecisionTexts resolves addr to the raw results it names, one per call.
func DecisionTexts(ctx agent.Context, addr DecisionAddress) ([]DecisionText, error) {
	rec, err := LoadDecision(ctx, addr.ID)
	if err != nil {
		return nil, err
	}
	if addr.Call > len(rec.Calls) {
		return nil, fmt.Errorf("decision %s made %d calls; there is no call %d", addr.ID, len(rec.Calls), addr.Call)
	}
	var out []DecisionText
	for i, c := range rec.Calls {
		if addr.Call > 0 && i+1 != addr.Call {
			continue
		}
		text, _ := c.Text()
		out = append(out, DecisionText{Name: DecisionAddress{ID: addr.ID, Call: i + 1}.String(), Text: text})
	}
	return out, nil
}

// DecisionCallLabel is the one-line name of an inner call: the tool and its
// arguments, without the description phrase.
func DecisionCallLabel(c DecisionCall) string {
	args := make(map[string]any, len(c.Args))
	for k, v := range c.Args {
		if k != "description" {
			args[k] = v
		}
	}
	if len(args) == 0 {
		return c.Tool
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return c.Tool
	}
	return c.Tool + " " + string(raw)
}
