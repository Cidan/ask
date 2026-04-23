package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

// virtualSessionStoreVersion is the on-disk schema version for
// ~/.config/ask/sessions.json. Bump when the VirtualSession shape
// changes so loadVirtualSessions can migrate older payloads.
const virtualSessionStoreVersion = 1

// VirtualSession is ask's provider-agnostic session abstraction: one
// logical conversation whose ProviderSessions maps provider IDs to
// the native session ids those providers persist on disk. The UI
// resumes a VirtualSession; the native id for the current provider
// is looked up at resume time.
type VirtualSession struct {
	ID               string                        `json:"id"`
	Workspace        string                        `json:"workspace"`
	CreatedAt        time.Time                     `json:"createdAt"`
	UpdatedAt        time.Time                     `json:"updatedAt"`
	Preview          string                        `json:"preview,omitempty"`
	LastProvider     string                        `json:"lastProvider,omitempty"`
	ProviderSessions map[string]ProviderSessionRef `json:"providerSessions"`
}

// ProviderSessionRef points at a provider's native session. Cwd
// matters for providers (claude) whose on-disk project dir is keyed
// off cwd.
type ProviderSessionRef struct {
	SessionID string `json:"sessionID"`
	Cwd       string `json:"cwd,omitempty"`
}

// virtualSessionStore is the versioned on-disk envelope so schema
// changes can migrate in place.
type virtualSessionStore struct {
	Version  int              `json:"version"`
	Sessions []VirtualSession `json:"sessions"`
}

// virtualSessionsPath returns the canonical location of the store —
// ~/.config/ask/sessions.json — sitting next to ask.json so the two
// user-state files live together.
func virtualSessionsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ask", "sessions.json"), nil
}

// loadVirtualSessions reads and decodes the store. A missing file
// yields an empty (but non-nil) store so callers can upsert without
// worrying about first-run bootstrap. Corrupt JSON surfaces an error
// rather than being silently wiped — we never destroy user data.
func loadVirtualSessions() (*virtualSessionStore, error) {
	path, err := virtualSessionsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &virtualSessionStore{Version: virtualSessionStoreVersion}, nil
		}
		return nil, err
	}
	var store virtualSessionStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if store.Version == 0 {
		store.Version = virtualSessionStoreVersion
	}
	return &store, nil
}

// saveVirtualSessions writes store atomically: tmp-file + rename so a
// crash mid-write can't leave a half-written sessions.json on disk.
// Mode 0600 matches ask.json; the file can carry provider session
// ids that connect the user's machine to cloud transcripts.
func saveVirtualSessions(store *virtualSessionStore) error {
	if store == nil {
		return errors.New("nil store")
	}
	path, err := virtualSessionsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if store.Version == 0 {
		store.Version = virtualSessionStoreVersion
	}
	if store.Sessions == nil {
		store.Sessions = []VirtualSession{}
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sessions-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// newVirtualSessionID returns a fresh opaque identifier for a VS.
// 16 random bytes hex-encoded (32 chars) is enough to avoid collisions
// without taking on a ULID dependency.
func newVirtualSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read on linux effectively never fails; if it does
		// fall back to a deterministic id seeded from time so we don't
		// panic a TUI over sessions.json tracking.
		return fmt.Sprintf("vs-%d", time.Now().UnixNano())
	}
	return "vs-" + hex.EncodeToString(b[:])
}

// newUUIDv4 returns a fresh RFC-4122 v4 UUID string ("8-4-4-4-12" hex
// form). Used when synthesising native session files whose ids must
// match each provider's UUID-shaped expectations (claude's
// session-file name, codex's thread id).
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back on a time-seeded pseudo-uuid so we never panic.
		ns := time.Now().UnixNano()
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", ns&0xFFFFFFFFFFFF)
	}
	b[6] = (b[6] & 0x0F) | 0x40 // version 4
	b[8] = (b[8] & 0x3F) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// findVirtualSessionByID returns a pointer to the matching VS in
// store.Sessions, or nil when absent.
func (store *virtualSessionStore) findByID(id string) *VirtualSession {
	if store == nil || id == "" {
		return nil
	}
	for i := range store.Sessions {
		if store.Sessions[i].ID == id {
			return &store.Sessions[i]
		}
	}
	return nil
}

// findByProviderNative returns the VS whose ProviderSessions[providerID].SessionID
// matches nativeID, scoped to workspace. Used to reattach ongoing
// provider sessions to the VS that spawned them when ask restarts.
func (store *virtualSessionStore) findByProviderNative(workspace, providerID, nativeID string) *VirtualSession {
	if store == nil || providerID == "" || nativeID == "" {
		return nil
	}
	for i := range store.Sessions {
		if workspace != "" && store.Sessions[i].Workspace != workspace {
			continue
		}
		ref, ok := store.Sessions[i].ProviderSessions[providerID]
		if !ok {
			continue
		}
		if ref.SessionID == nativeID {
			return &store.Sessions[i]
		}
	}
	return nil
}

// listForWorkspace returns every VS whose Workspace matches, sorted
// newest first by UpdatedAt. Callers get a copy so mutating the
// result doesn't disturb the store.
func (store *virtualSessionStore) listForWorkspace(workspace string) []VirtualSession {
	if store == nil {
		return nil
	}
	var out []VirtualSession
	for _, vs := range store.Sessions {
		if vs.Workspace != workspace {
			continue
		}
		out = append(out, vs)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// translateVSReq bundles the inputs for a translate-VS async run.
// Exactly one of (source+sourceSessionID) or directTurns should be
// populated: source path loads history from the source provider's
// on-disk file and derives both UI entries and wire turns from it;
// direct path feeds pre-loaded turns straight to Materialize (used
// by Ctrl+B mid-session when m.history already has the turns).
type translateVSReq struct {
	target          Provider
	vsID            string
	workspace       string
	source          Provider
	sourceSessionID string
	directTurns     []NeutralTurn
	opts            HistoryOpts
}

// virtualSessionMaterializedMsg lands on Update when translateVSCmd
// finishes: nativeSessionID/nativeCwd point at the newly-written
// native session file the target provider can --resume; entries
// carries the source's rendered history for the UI when the source
// path was used; err != nil means translation failed.
type virtualSessionMaterializedMsg struct {
	vsID            string
	nativeSessionID string
	nativeCwd       string
	entries         []historyEntry
	err             error
}

// translateVSCmd runs cross-provider translation off the UI thread:
// loads source history (if source != nil), distills to []NeutralTurn,
// asks the target provider to Materialize a native session file, and
// upserts the new native id onto the VS. Returns
// virtualSessionMaterializedMsg that the Update handler plumbs back
// into model state.
func translateVSCmd(req translateVSReq) tea.Cmd {
	return func() tea.Msg {
		turns := req.directTurns
		var entries []historyEntry
		if req.source != nil && req.sourceSessionID != "" {
			loaded, err := req.source.LoadHistory(req.sourceSessionID, req.opts)
			if err != nil {
				return virtualSessionMaterializedMsg{vsID: req.vsID, err: err}
			}
			entries = loaded
			turns = neutralTurnsFromHistory(loaded)
		}
		if len(turns) == 0 {
			return virtualSessionMaterializedMsg{
				vsID:    req.vsID,
				entries: entries,
				err:     errors.New("no prior turns to translate"),
			}
		}
		newID, nativeCwd, err := req.target.Materialize(req.workspace, turns)
		if err != nil {
			return virtualSessionMaterializedMsg{vsID: req.vsID, entries: entries, err: err}
		}
		_ = mutateVirtualSessions(func(store *virtualSessionStore) error {
			upsertVirtualSession(store, req.vsID, req.workspace,
				req.target.ID(), newID, nativeCwd, "", time.Now().UTC())
			return nil
		})
		return virtualSessionMaterializedMsg{
			vsID:            req.vsID,
			nativeSessionID: newID,
			nativeCwd:       nativeCwd,
			entries:         entries,
		}
	}
}

// recordVirtualSession upserts the current tab's conversation into
// ~/.config/ask/sessions.json with the native id the provider just
// reported. The load-modify-save runs under an advisory lock so
// concurrent tabs can't drop each other's upserts. Errors are
// logged and swallowed — failing to persist must never block an
// in-flight turn.
func (m *model) recordVirtualSession(nativeID string) {
	if nativeID == "" || m.provider == nil {
		return
	}
	workspace := m.cwd
	nativeCwd := nativeCwdForUpsert(workspace, m.worktreeName)
	preview := firstUserPreview(m.history)
	providerID := m.provider.ID()
	vsID := m.virtualSessionID
	now := time.Now().UTC()
	var newID string
	err := mutateVirtualSessions(func(store *virtualSessionStore) error {
		newID = upsertVirtualSession(store, vsID, workspace, providerID, nativeID, nativeCwd, preview, now)
		return nil
	})
	if err != nil {
		debugLog("recordVirtualSession: %v", err)
		return
	}
	if newID != "" {
		m.virtualSessionID = newID
	}
}

// NeutralTurn is the provider-agnostic representation of a single
// user or assistant conversational turn. Used as the lingua franca
// for cross-provider session materialization: source provider's
// history is distilled to []NeutralTurn and fed to the target
// provider's Materialize to write a native session file it can
// then --resume natively.
type NeutralTurn struct {
	Role string // "user" | "assistant"
	Text string
}

// neutralTurnsFromHistory flattens a UI history slice to the
// provider-neutral turn list. histPrerendered entries (tool blocks,
// diff blocks) are deliberately skipped — they're noise when
// seeding the target provider with conversational context and the
// tools aren't portable across provider schemas anyway.
func neutralTurnsFromHistory(history []historyEntry) []NeutralTurn {
	out := make([]NeutralTurn, 0, len(history))
	for _, e := range history {
		switch e.kind {
		case histUser:
			out = append(out, NeutralTurn{Role: "user", Text: e.text})
		case histResponse:
			out = append(out, NeutralTurn{Role: "assistant", Text: e.text})
		}
	}
	return out
}

// mutateVirtualSessions serializes load-modify-save against
// ~/.config/ask/sessions.json across concurrent ask tabs via an
// advisory file lock on a sentinel lockfile. Without it two tabs
// finishing turns simultaneously would race: both read, both modify,
// last-writer-wins drops the other's upsert.
func mutateVirtualSessions(fn func(*virtualSessionStore) error) error {
	path, err := virtualSessionsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	store, err := loadVirtualSessions()
	if err != nil {
		return err
	}
	if err := fn(store); err != nil {
		return err
	}
	return saveVirtualSessions(store)
}

// firstUserPreview returns the first user entry's text with CR/LF
// collapsed to single spaces for one-line picker rendering.
func firstUserPreview(history []historyEntry) string {
	for _, e := range history {
		if e.kind == histUser {
			return flattenNewlines(e.text)
		}
	}
	return ""
}

func flattenNewlines(s string) string {
	b := make([]byte, 0, len(s))
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' {
			if !prevSpace {
				b = append(b, ' ')
				prevSpace = true
			}
			continue
		}
		b = append(b, c)
		prevSpace = c == ' '
	}
	return string(b)
}

// nativeCwdForUpsert is the directory the provider actually runs
// under — the ask-managed worktree when one is live. Recording this
// keeps /resume's later LoadHistory lookup aligned with the project
// dir the provider wrote its session file into.
func nativeCwdForUpsert(workspace, worktreeName string) string {
	if worktreeName == "" {
		return workspace
	}
	return worktreePath(workspace, worktreeName)
}

// upsertVirtualSession mutates store in place: creates a new VS (when
// vsID is empty and no reattach match) or updates an existing one.
// Returns the VS's id — generated when creating, echoed otherwise.
//
// The fallback match by provider+native lets a restart reattach to
// an existing VS rather than duplicating when the caller has lost
// the vsID but the provider still reports the same native id.
func upsertVirtualSession(store *virtualSessionStore, vsID, workspace, providerID, nativeID, nativeCwd, preview string, now time.Time) string {
	if store == nil {
		return ""
	}
	if providerID == "" || nativeID == "" {
		return vsID
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ref := ProviderSessionRef{SessionID: nativeID, Cwd: nativeCwd}
	vs := store.findByID(vsID)
	if vs == nil {
		vs = store.findByProviderNative(workspace, providerID, nativeID)
	}
	if vs != nil {
		vs.applyTurn(providerID, ref, preview, now)
		return vs.ID
	}
	id := vsID
	if id == "" {
		id = newVirtualSessionID()
	}
	store.Sessions = append(store.Sessions, VirtualSession{
		ID:               id,
		Workspace:        workspace,
		CreatedAt:        now,
		UpdatedAt:        now,
		Preview:          preview,
		LastProvider:     providerID,
		ProviderSessions: map[string]ProviderSessionRef{providerID: ref},
	})
	return id
}

// applyTurn records a completed turn on vs: stamps UpdatedAt,
// refreshes LastProvider, sets the provider's ref, and fills preview
// if it was empty. Shared by the findByID and findByProviderNative
// update branches of upsertVirtualSession.
func (vs *VirtualSession) applyTurn(providerID string, ref ProviderSessionRef, preview string, now time.Time) {
	if vs.ProviderSessions == nil {
		vs.ProviderSessions = map[string]ProviderSessionRef{}
	}
	vs.ProviderSessions[providerID] = ref
	vs.UpdatedAt = now
	vs.LastProvider = providerID
	if vs.Preview == "" && preview != "" {
		vs.Preview = preview
	}
}
