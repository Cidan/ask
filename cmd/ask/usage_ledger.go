package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/session"
)

// A session's accounting lives in two places. Every model call of the
// conversation itself is an event in the session file, and carries its usage
// record (providers.Usage) there. Calls made on the session's behalf that never
// become events — the tab title, deslop rewrites, memory extraction,
// sub-agents — are appended to the usage ledger, <sessionID>.usage.json next
// to the session file, like the deslop sidecar. /resume reads both back.

const usageLedgerVersion = 1

// Ledger entry kinds: what an out-of-conversation call was for.
const (
	spendTitle    = "title"
	spendDeslop   = "deslop"
	spendMemory   = "memory"
	spendSubagent = "subagent"
)

type usageLedgerEntry struct {
	Kind  string          `json:"kind"`
	Usage providers.Usage `json:"usage"`
}

type usageLedger struct {
	Version int                `json:"version"`
	Entries []usageLedgerEntry `json:"entries"`
}

// usageLedgerMu serializes read-modify-write of a session's ledger file.
var usageLedgerMu sync.Mutex

func (st *agentSessionStore) usageLedgerPath(id, cwd string) (string, error) {
	dir, err := st.svc(cwd).DirFor(cwd)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".usage.json"), nil
}

// loadUsageLedger reads a session's ledger; a missing or unreadable one is
// empty.
func (st *agentSessionStore) loadUsageLedger(id, cwd string) []usageLedgerEntry {
	path, err := st.usageLedgerPath(id, cwd)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var l usageLedger
	if json.Unmarshal(data, &l) != nil {
		return nil
	}
	return l.Entries
}

// appendUsageLedger adds entries to a session's ledger, writing it back
// atomically.
func (st *agentSessionStore) appendUsageLedger(id, cwd string, entries ...usageLedgerEntry) error {
	if id == "" || len(entries) == 0 {
		return nil
	}
	usageLedgerMu.Lock()
	defer usageLedgerMu.Unlock()

	path, err := st.usageLedgerPath(id, cwd)
	if err != nil {
		return err
	}
	all := append(st.loadUsageLedger(id, cwd), entries...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(usageLedger{Version: usageLedgerVersion, Entries: all}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sessionUsage is what a stored session says about its context and spend:
// the latest context reading, the provider and model that made it, and the
// total cost of the conversation's calls and its ledgered ones. Only usage
// records count — a session saved before ask recorded them restores nothing,
// rather than a guess from its providers' raw counts.
type sessionUsage struct {
	contextTokens int
	provider      string
	model         string
	costUSD       float64
	costKnown     bool
}

func summarizeSessionUsage(events []*session.Event, ledger []usageLedgerEntry) sessionUsage {
	var out sessionUsage
	add := func(u providers.Usage) {
		if u.CostKnown() {
			out.costUSD += u.CostUSD
			out.costKnown = true
		}
	}
	for _, e := range events {
		if e == nil {
			continue
		}
		u, ok := providers.UsageOf(&e.LLMResponse)
		if !ok {
			continue
		}
		add(u)
		if u.ContextTokens > 0 {
			out.contextTokens, out.provider, out.model = u.ContextTokens, u.Provider, u.Model
		}
	}
	for _, e := range ledger {
		add(e.Usage)
	}
	return out
}

// loadUsage summarizes a stored session's usage.
func (st *agentSessionStore) loadUsage(id string) (sessionUsage, error) {
	file, err := st.load(id)
	if err != nil {
		return sessionUsage{}, err
	}
	return summarizeSessionUsage(file.Events, st.loadUsageLedger(id, file.Cwd)), nil
}

// usageLoader is the optional capability of a TUI provider whose sessions
// carry usage records, so /resume can restore the context meter and the cost.
type usageLoader interface {
	LoadUsage(sessionID string) (sessionUsage, error)
}

func (p agentAPIProvider) LoadUsage(sessionID string) (sessionUsage, error) {
	return p.store().loadUsage(sessionID)
}

// loadUsageFor returns p's stored usage for sessionID, when p records any.
func loadUsageFor(p Provider, sessionID string) *sessionUsage {
	ul, ok := p.(usageLoader)
	if !ok || sessionID == "" {
		return nil
	}
	u, err := ul.LoadUsage(sessionID)
	if err != nil {
		debugLog("load usage %s: %v", sessionID, err)
		return nil
	}
	return &u
}

// appendSpendCmd records an out-of-conversation call in the tab session's
// ledger off the update loop.
func appendSpendCmd(providerID, sessionID, cwd, kind string, u providers.Usage) tea.Cmd {
	return func() tea.Msg {
		st := &agentSessionStore{provider: providerID}
		if err := st.appendUsageLedger(sessionID, cwd, usageLedgerEntry{Kind: kind, Usage: u}); err != nil {
			debugLog("usage ledger %s: %v", sessionID, err)
		}
		return nil
	}
}
