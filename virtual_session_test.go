package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVirtualSessions_RoundTrip(t *testing.T) {
	isolateHome(t)

	now := time.Now().UTC().Truncate(time.Second)
	store := &virtualSessionStore{
		Version: virtualSessionStoreVersion,
		Sessions: []VirtualSession{
			{
				ID:           "vs-1",
				Workspace:    "/tmp/ws",
				CreatedAt:    now,
				UpdatedAt:    now,
				Preview:      "hello",
				LastProvider: "claude",
				ProviderSessions: map[string]ProviderSessionRef{
					"claude": {SessionID: "native-claude", Cwd: "/tmp/ws"},
					"codex":  {SessionID: "native-codex"},
				},
			},
		},
	}
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(got.Sessions))
	}
	vs := got.Sessions[0]
	if vs.ID != "vs-1" || vs.Workspace != "/tmp/ws" || vs.Preview != "hello" {
		t.Errorf("round-trip mismatch: %+v", vs)
	}
	if vs.ProviderSessions["claude"].SessionID != "native-claude" ||
		vs.ProviderSessions["claude"].Cwd != "/tmp/ws" {
		t.Errorf("claude provider ref wrong: %+v", vs.ProviderSessions["claude"])
	}
	if vs.ProviderSessions["codex"].SessionID != "native-codex" {
		t.Errorf("codex provider ref wrong: %+v", vs.ProviderSessions["codex"])
	}
	if vs.LastProvider != "claude" {
		t.Errorf("lastProvider=%q want claude", vs.LastProvider)
	}
}

func TestVirtualSessions_MissingFileIsEmpty(t *testing.T) {
	isolateHome(t)
	got, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load on missing: %v", err)
	}
	if got == nil {
		t.Fatal("nil store on missing file")
	}
	if len(got.Sessions) != 0 {
		t.Errorf("want empty sessions, got %d", len(got.Sessions))
	}
}

func TestVirtualSessions_CorruptJSONErrors(t *testing.T) {
	isolateHome(t)
	path, err := virtualSessionsPath()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{garbage"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if _, err := loadVirtualSessions(); err == nil {
		t.Error("corrupt JSON should surface an error, got nil")
	}
}

func TestVirtualSessions_FilePerms(t *testing.T) {
	isolateHome(t)
	store := &virtualSessionStore{Version: 1}
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	path, err := virtualSessionsPath()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("perms=%o want 0600", mode)
	}
}

func TestUpsertVirtualSession_Creates(t *testing.T) {
	store := &virtualSessionStore{Version: 1}
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	id := upsertVirtualSession(store, "", "/ws", "claude", "native-1", "/ws", "hi there", now)
	if id == "" {
		t.Fatal("upsert returned empty id")
	}
	if len(store.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(store.Sessions))
	}
	vs := store.Sessions[0]
	if vs.ID != id {
		t.Errorf("stored id=%q returned id=%q", vs.ID, id)
	}
	if vs.ProviderSessions["claude"].SessionID != "native-1" {
		t.Errorf("provider mapping wrong: %+v", vs.ProviderSessions)
	}
	if !vs.CreatedAt.Equal(now) || !vs.UpdatedAt.Equal(now) {
		t.Errorf("timestamps wrong: created=%v updated=%v", vs.CreatedAt, vs.UpdatedAt)
	}
	if vs.LastProvider != "claude" {
		t.Errorf("lastProvider=%q want claude", vs.LastProvider)
	}
	if vs.Preview != "hi there" {
		t.Errorf("preview=%q", vs.Preview)
	}
}

func TestUpsertVirtualSession_AddsSecondProvider(t *testing.T) {
	store := &virtualSessionStore{Version: 1}
	t0 := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	id := upsertVirtualSession(store, "", "/ws", "claude", "native-cla", "/ws", "hi", t0)
	// Same VS, second provider lands later.
	t1 := t0.Add(5 * time.Minute)
	got := upsertVirtualSession(store, id, "/ws", "codex", "native-cdx", "/ws", "hi", t1)
	if got != id {
		t.Fatalf("upsert returned %q, want %q", got, id)
	}
	vs := store.Sessions[0]
	if vs.ProviderSessions["claude"].SessionID != "native-cla" {
		t.Errorf("claude mapping lost: %+v", vs.ProviderSessions)
	}
	if vs.ProviderSessions["codex"].SessionID != "native-cdx" {
		t.Errorf("codex mapping missing: %+v", vs.ProviderSessions)
	}
	if !vs.UpdatedAt.Equal(t1) {
		t.Errorf("UpdatedAt not bumped; got %v want %v", vs.UpdatedAt, t1)
	}
	if !vs.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt changed; got %v want %v", vs.CreatedAt, t0)
	}
	if vs.LastProvider != "codex" {
		t.Errorf("lastProvider=%q want codex after second upsert", vs.LastProvider)
	}
}

func TestUpsertVirtualSession_FindByProviderNativeReattaches(t *testing.T) {
	store := &virtualSessionStore{Version: 1}
	t0 := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	id := upsertVirtualSession(store, "", "/ws", "claude", "native-1", "/ws", "hi", t0)
	// Caller forgot the vsID but passes the same native id; should reattach.
	got := upsertVirtualSession(store, "", "/ws", "claude", "native-1", "/ws", "hi", t0.Add(time.Minute))
	if got != id {
		t.Errorf("expected reattach to %q, got %q (duplicated)", id, got)
	}
	if len(store.Sessions) != 1 {
		t.Errorf("duplicate VS created: %d", len(store.Sessions))
	}
}

func TestFirstUserPreview_FindsFirstUserEntry(t *testing.T) {
	history := []historyEntry{
		{kind: histPrerendered, text: "tool output"},
		{kind: histUser, text: "first user"},
		{kind: histResponse, text: "assistant"},
		{kind: histUser, text: "second user"},
	}
	got := firstUserPreview(history)
	if got != "first user" {
		t.Errorf("preview=%q want 'first user'", got)
	}
}

func TestFirstUserPreview_FlattensNewlines(t *testing.T) {
	got := firstUserPreview([]historyEntry{{kind: histUser, text: "line1\nline2\nline3"}})
	if got != "line1 line2 line3" {
		t.Errorf("preview=%q", got)
	}
}

func TestFirstUserPreview_EmptyWhenNoUserEntries(t *testing.T) {
	got := firstUserPreview([]historyEntry{{kind: histResponse, text: "only assistant"}})
	if got != "" {
		t.Errorf("preview=%q want empty", got)
	}
}

func TestPreMintNativeSession_RecordsVSBeforeFork(t *testing.T) {
	// Provider that pre-mints (claude-style) — preMintNativeSessionIfNeeded
	// must stamp m.sessionID, flip m.sessionMinted, and persist a VS row
	// before the subprocess could possibly start.
	isolateHome(t)
	p := newFakeProvider()
	p.id = "claude"
	p.preMintFn = func(ProviderSessionArgs) string { return "minted-uuid-123" }
	m := newTestModel(t, p)
	m.cwd = "/ws"
	m.history = append(m.history, historyEntry{kind: histUser, text: "first turn"})

	(&m).preMintNativeSessionIfNeeded()

	if m.sessionID != "minted-uuid-123" {
		t.Errorf("sessionID=%q want minted-uuid-123", m.sessionID)
	}
	if !m.sessionMinted {
		t.Error("sessionMinted should be true after pre-mint")
	}
	if m.virtualSessionID == "" {
		t.Fatal("virtualSessionID must be recorded before fork")
	}
	store, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(store.Sessions) != 1 {
		t.Fatalf("want 1 VS persisted, got %d", len(store.Sessions))
	}
	ref, ok := store.Sessions[0].ProviderSessions["claude"]
	if !ok || ref.SessionID != "minted-uuid-123" {
		t.Errorf("VS mapping wrong: %+v", store.Sessions[0].ProviderSessions)
	}
}

func TestPreMintNativeSession_CodexNoOps(t *testing.T) {
	// Codex-style provider returns "" — no mint, no VS row.
	isolateHome(t)
	p := newFakeProvider()
	p.preMintFn = func(ProviderSessionArgs) string { return "" }
	m := newTestModel(t, p)
	m.history = append(m.history, historyEntry{kind: histUser, text: "x"})

	(&m).preMintNativeSessionIfNeeded()

	if m.sessionID != "" {
		t.Errorf("sessionID=%q must stay empty when provider doesn't pre-mint", m.sessionID)
	}
	if m.sessionMinted {
		t.Error("sessionMinted must stay false")
	}
	store, _ := loadVirtualSessions()
	if len(store.Sessions) != 0 {
		t.Errorf("no VS should be recorded for non-pre-minting provider; got %d", len(store.Sessions))
	}
}

func TestPreMintNativeSession_SkippedWhenSessionIDAlreadySet(t *testing.T) {
	// A resumed conversation already has m.sessionID; pre-mint must not
	// overwrite it (that would orphan the prior session jsonl).
	isolateHome(t)
	p := newFakeProvider()
	p.preMintFn = func(ProviderSessionArgs) string { return "wrong-fresh-uuid" }
	m := newTestModel(t, p)
	m.sessionID = "existing-resume-uuid"

	(&m).preMintNativeSessionIfNeeded()

	if m.sessionID != "existing-resume-uuid" {
		t.Errorf("sessionID overwritten: %q", m.sessionID)
	}
	if m.sessionMinted {
		t.Error("sessionMinted must stay false on resume")
	}
}

func TestSessionArgs_RoutesMintedToNewSessionID(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "uuid-fresh"
	m.sessionMinted = true
	args := m.sessionArgs()
	if args.NewSessionID != "uuid-fresh" {
		t.Errorf("NewSessionID=%q want uuid-fresh", args.NewSessionID)
	}
	if args.SessionID != "" {
		t.Errorf("SessionID should be empty when minted; got %q", args.SessionID)
	}
}

func TestSessionArgs_RoutesUnmintedToSessionID(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "uuid-resume"
	m.sessionMinted = false
	args := m.sessionArgs()
	if args.SessionID != "uuid-resume" {
		t.Errorf("SessionID=%q want uuid-resume", args.SessionID)
	}
	if args.NewSessionID != "" {
		t.Errorf("NewSessionID should be empty when not minted; got %q", args.NewSessionID)
	}
}

func TestSendToProvider_PreMintsAndRecordsVSBeforeStart(t *testing.T) {
	// End-to-end: a fresh tab on a claude-style provider sends the first
	// turn. After sendToProvider returns synchronously (before the async
	// start cmd runs), the VS must already exist on disk and m.sessionID
	// must be populated. This is the cancel-safety guarantee — if the
	// user hits ESC right now, the worktree is still findable via the
	// /resume picker.
	isolateHome(t)
	p := newFakeProvider()
	p.id = "claude"
	p.preMintFn = func(ProviderSessionArgs) string { return "fresh-mint-xyz" }
	m := newTestModel(t, p)
	mm, cmd := m.sendToProvider("hello")
	m2 := mm.(model)

	if m2.sessionID != "fresh-mint-xyz" {
		t.Errorf("m.sessionID=%q want fresh-mint-xyz", m2.sessionID)
	}
	if m2.sessionMinted {
		t.Error("sessionMinted should be cleared after dispatch so retries take --resume")
	}
	if m2.virtualSessionID == "" {
		t.Error("virtualSessionID should be set before fork")
	}
	store, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(store.Sessions) != 1 {
		t.Fatalf("VS row must exist before async start runs; got %d", len(store.Sessions))
	}
	ref := store.Sessions[0].ProviderSessions["claude"]
	if ref.SessionID != "fresh-mint-xyz" {
		t.Errorf("VS native id=%q want fresh-mint-xyz", ref.SessionID)
	}
	// The dispatched start cmd must have captured args carrying the
	// minted id in NewSessionID (so claudeCLIArgs picks --session-id).
	done := runProviderStartCmd(t, cmd)
	if done.providerID != "claude" {
		t.Errorf("providerID=%q", done.providerID)
	}
	if len(p.startArgs) != 1 {
		t.Fatalf("StartSession should be called once; got %d", len(p.startArgs))
	}
	if p.startArgs[0].NewSessionID != "fresh-mint-xyz" {
		t.Errorf("StartSession args.NewSessionID=%q want fresh-mint-xyz",
			p.startArgs[0].NewSessionID)
	}
	if p.startArgs[0].SessionID != "" {
		t.Errorf("StartSession args.SessionID should be empty for fresh mint; got %q",
			p.startArgs[0].SessionID)
	}
}

func TestKillProc_ClearsSessionMinted(t *testing.T) {
	// killProc fires on ESC-confirm; after it, any later fork in the
	// same conversation must take --resume because either claude wrote
	// the jsonl already (the file exists) or didn't (in which case
	// re-using --session-id with a stale ack would still misbehave).
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "uuid"
	m.sessionMinted = true
	(&m).killProc()
	if m.sessionMinted {
		t.Error("killProc must clear sessionMinted")
	}
	if m.sessionID != "uuid" {
		t.Errorf("killProc must NOT clear sessionID; got %q", m.sessionID)
	}
}

func TestRecordVirtualSession_NewSessionCreatesVS(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = "/ws"
	m.history = append(m.history, historyEntry{kind: histUser, text: "hi there"})
	m.recordVirtualSession("native-1")
	if m.virtualSessionID == "" {
		t.Fatal("virtualSessionID should be set after recordVirtualSession")
	}
	store, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(store.Sessions) != 1 {
		t.Fatalf("want 1 VS persisted, got %d", len(store.Sessions))
	}
	vs := store.Sessions[0]
	if vs.ID != m.virtualSessionID {
		t.Errorf("persisted id=%q vs model id=%q", vs.ID, m.virtualSessionID)
	}
	if vs.Workspace != "/ws" {
		t.Errorf("workspace=%q want /ws", vs.Workspace)
	}
	if vs.Preview != "hi there" {
		t.Errorf("preview=%q", vs.Preview)
	}
	ref, ok := vs.ProviderSessions["fake"]
	if !ok || ref.SessionID != "native-1" {
		t.Errorf("provider mapping wrong: %+v", vs.ProviderSessions)
	}
	if ref.Cwd != "/ws" {
		t.Errorf("native cwd=%q want /ws", ref.Cwd)
	}
}

func TestRecordVirtualSession_SameProviderSecondTurnReusesVS(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = "/ws"
	m.history = append(m.history, historyEntry{kind: histUser, text: "hi"})
	m.recordVirtualSession("native-1")
	firstID := m.virtualSessionID
	// Second turn same provider, same VS. Native id might update (e.g.
	// claude rewrites session id on compaction), so we pass a fresh one.
	m.recordVirtualSession("native-1-v2")
	if m.virtualSessionID != firstID {
		t.Errorf("VS id changed across turns: %q → %q", firstID, m.virtualSessionID)
	}
	store, _ := loadVirtualSessions()
	if len(store.Sessions) != 1 {
		t.Fatalf("want 1 VS, got %d", len(store.Sessions))
	}
	if got := store.Sessions[0].ProviderSessions["fake"].SessionID; got != "native-1-v2" {
		t.Errorf("native id not updated: got %q want native-1-v2", got)
	}
}

func TestRecordVirtualSession_SecondProviderAddsMapping(t *testing.T) {
	isolateHome(t)
	p1 := newFakeProvider()
	p1.id = "claude"
	m := newTestModel(t, p1)
	m.cwd = "/ws"
	m.history = append(m.history, historyEntry{kind: histUser, text: "hi"})
	m.recordVirtualSession("native-claude")
	vsID := m.virtualSessionID

	// Swap to a different provider but preserve the VS id.
	p2 := newFakeProvider()
	p2.id = "codex"
	m.provider = p2
	m.recordVirtualSession("native-codex")
	if m.virtualSessionID != vsID {
		t.Errorf("VS id changed on provider swap: %q → %q", vsID, m.virtualSessionID)
	}
	store, _ := loadVirtualSessions()
	if len(store.Sessions) != 1 {
		t.Fatalf("want 1 VS, got %d", len(store.Sessions))
	}
	ps := store.Sessions[0].ProviderSessions
	if ps["claude"].SessionID != "native-claude" {
		t.Errorf("claude mapping lost: %+v", ps)
	}
	if ps["codex"].SessionID != "native-codex" {
		t.Errorf("codex mapping missing: %+v", ps)
	}
}

func TestResumeVirtualSession_CurrentProviderMappingUsesNativeID(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	p.id = "claude"
	p.loadHistoryFn = func(id string, _ HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{{kind: histUser, text: "loaded-for:" + id}}, nil
	}
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = "/ws"

	// Seed a VS with a claude mapping.
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "claude", "native-42", "/ws-cwd", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	entry := sessionEntry{id: vsID, virtualSessionID: vsID}
	newM, cmd := m.resumeVirtualSession(entry)
	mm := newM.(model)
	if mm.virtualSessionID != vsID {
		t.Errorf("virtualSessionID=%q want %q", mm.virtualSessionID, vsID)
	}
	if mm.sessionID != "native-42" {
		t.Errorf("sessionID=%q want native-42 (the native id for current provider)", mm.sessionID)
	}
	if mm.resumeCwd != "/ws-cwd" {
		t.Errorf("resumeCwd=%q want /ws-cwd", mm.resumeCwd)
	}
	if cmd == nil {
		t.Fatal("expected loadHistoryCmd, got nil")
	}
	msg := cmd()
	hl, ok := msg.(historyLoadedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want historyLoadedMsg", msg)
	}
	if hl.sessionID != "native-42" {
		t.Errorf("history loaded for sessionID=%q want native-42", hl.sessionID)
	}
	if hl.virtualSessionID != vsID {
		t.Errorf("historyLoadedMsg missing VS id tag: %+v", hl)
	}

	// Run the message through Update to confirm the gate accepts it
	// and the translated history lands on m.history.
	mm2, _ := runUpdate(t, mm, hl)
	if len(mm2.history) == 0 {
		t.Fatal("translated history must render through Update")
	}
	var found bool
	for _, e := range mm2.history {
		if strings.Contains(e.text, "loaded-for:native-42") {
			found = true
		}
	}
	if !found {
		t.Errorf("history missing loaded entries: %+v", mm2.history)
	}
}

func TestResumeVirtualSession_NoMappingForCurrentProviderTranslatesFromSource(t *testing.T) {
	isolateHome(t)
	claude := newFakeProvider()
	claude.id = "claude"
	claude.loadHistoryFn = func(id string, _ HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{
			{kind: histUser, text: "from-claude:" + id},
			{kind: histResponse, text: "assistant reply"},
		}, nil
	}
	codex := newFakeProvider()
	codex.id = "codex"
	var gotTurns []NeutralTurn
	codex.materializeFn = func(ws string, turns []NeutralTurn) (string, string, error) {
		gotTurns = append([]NeutralTurn(nil), turns...)
		return "codex-synth", ws, nil
	}
	withRegisteredProviders(t, claude, codex)

	// VS has only a claude mapping; the tab's provider is codex.
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "claude", "c-sess", "/ws", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	m := newTestModel(t, codex)
	m.cwd = "/ws"
	entry := sessionEntry{id: vsID, virtualSessionID: vsID}
	newM, cmd := m.resumeVirtualSession(entry)
	mm := newM.(model)
	if mm.virtualSessionID != vsID {
		t.Errorf("virtualSessionID not set: %q", mm.virtualSessionID)
	}
	if !mm.busy {
		t.Error("busy must be true while translation runs")
	}
	if cmd == nil {
		t.Fatal("expected translate command, got nil")
	}
	mat, ok := cmd().(virtualSessionMaterializedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want virtualSessionMaterializedMsg", cmd())
	}
	if mat.err != nil {
		t.Fatalf("translate err: %v", mat.err)
	}
	if mat.nativeSessionID != "codex-synth" {
		t.Errorf("nativeSessionID=%q want codex-synth", mat.nativeSessionID)
	}
	// Source (claude) was asked for its history; turns passed to codex are the neutral form.
	if len(gotTurns) != 2 ||
		gotTurns[0] != (NeutralTurn{Role: "user", Text: "from-claude:c-sess"}) ||
		gotTurns[1] != (NeutralTurn{Role: "assistant", Text: "assistant reply"}) {
		t.Errorf("target.Materialize received wrong turns: %+v", gotTurns)
	}
	// Run the msg through Update: sessionID should now point at the synthesized codex id.
	mm2, _ := runUpdate(t, mm, mat)
	if mm2.sessionID != "codex-synth" {
		t.Errorf("m.sessionID=%q want codex-synth after translate", mm2.sessionID)
	}
	if mm2.busy {
		t.Error("busy must clear after translate completes")
	}
	if len(mm2.history) == 0 {
		t.Error("history entries from source should be surfaced on the UI")
	}
	// VS store now has a codex mapping too.
	got, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Sessions[0].ProviderSessions["codex"].SessionID != "codex-synth" {
		t.Errorf("VS codex mapping not upserted: %+v", got.Sessions[0].ProviderSessions)
	}
}

func TestApplyProviderSwitch_PreservesVirtualSessionID(t *testing.T) {
	isolateHome(t)
	// Register two distinct providers so a swap means "cross-provider".
	p1 := newFakeProvider()
	p1.id = "fakeA"
	p1.displayName = "Fake A"
	p2 := newFakeProvider()
	p2.id = "fakeB"
	p2.displayName = "Fake B"
	withRegisteredProviders(t, p1, p2)

	m := newTestModel(t, p1)
	m.virtualSessionID = "vs-carry"
	m.sessionID = "native-from-A"
	m.resumeCwd = "/ws"
	m.providerSwitchProvIdx = 1 // target is B

	newM, _ := m.applyProviderSwitch("")
	mm := newM.(model)
	if mm.provider.ID() != "fakeB" {
		t.Fatalf("swap failed: provider=%s", mm.provider.ID())
	}
	// Cross-provider swap drops native id (correct) but the VS id
	// must survive so the next providerDoneMsg's upsert wires the
	// new provider's native id onto the same VS.
	if mm.sessionID != "" {
		t.Errorf("cross-provider swap should clear sessionID, got %q", mm.sessionID)
	}
	if mm.virtualSessionID != "vs-carry" {
		t.Errorf("virtualSessionID dropped on cross-provider swap: got %q want vs-carry", mm.virtualSessionID)
	}
}

func TestResumeVirtualSession_MissingVSInStoreErrorsGracefully(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	// Empty store, but entry points at a phantom VS.
	entry := sessionEntry{id: "vs-ghost", virtualSessionID: "vs-ghost"}
	newM, cmd := m.resumeVirtualSession(entry)
	mm := newM.(model)
	if mm.mode != modeInput {
		t.Errorf("mode=%v want modeInput after missing VS", mm.mode)
	}
	if cmd != nil {
		t.Errorf("expected nil cmd, got %T", cmd())
	}
	if len(mm.history) == 0 {
		t.Error("expected error message appended to history")
	}
}

func TestResumeVirtualSession_RoundTripUpsertPersistsCodexNativeID(t *testing.T) {
	isolateHome(t)
	claude := newFakeProvider()
	claude.id = "claude"
	claude.loadHistoryFn = func(_ string, _ HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{{kind: histUser, text: "hi"}}, nil
	}
	codex := newFakeProvider()
	codex.id = "codex"
	withRegisteredProviders(t, claude, codex)

	// Seed with claude-only mapping.
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "claude", "c-1", "/ws", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Resume into a codex-tabbed model.
	m := newTestModel(t, codex)
	m.cwd = "/ws"
	newM, _ := m.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	mm := newM.(model)

	// Simulate the user completing a codex turn: providerDoneMsg with
	// a fresh codex native id. recordVirtualSession must route that id
	// onto the same VS and populate a codex mapping.
	mm.recordVirtualSession("cdx-42")

	got, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("want 1 VS persisted, got %d: %+v", len(got.Sessions), got.Sessions)
	}
	vs := got.Sessions[0]
	if vs.ID != vsID {
		t.Errorf("VS id=%q want %q (should have reused the existing VS id)", vs.ID, vsID)
	}
	if vs.ProviderSessions["claude"].SessionID != "c-1" {
		t.Errorf("claude mapping lost: %+v", vs.ProviderSessions)
	}
	if vs.ProviderSessions["codex"].SessionID != "cdx-42" {
		t.Errorf("codex mapping not added: %+v", vs.ProviderSessions)
	}
}

func TestListForWorkspace_FiltersAndSorts(t *testing.T) {
	store := &virtualSessionStore{Version: 1}
	t0 := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	upsertVirtualSession(store, "", "/a", "claude", "a1", "/a", "A1", t0)
	upsertVirtualSession(store, "", "/b", "claude", "b1", "/b", "B1", t0.Add(time.Hour))
	upsertVirtualSession(store, "", "/a", "claude", "a2", "/a", "A2", t0.Add(2*time.Hour))

	listA := store.listForWorkspace("/a")
	if len(listA) != 2 {
		t.Fatalf("/a listing got %d, want 2", len(listA))
	}
	// Newest first: A2 before A1.
	if listA[0].Preview != "A2" || listA[1].Preview != "A1" {
		t.Errorf("sort wrong: %+v", listA)
	}
	listB := store.listForWorkspace("/b")
	if len(listB) != 1 || listB[0].Preview != "B1" {
		t.Errorf("/b listing wrong: %+v", listB)
	}
}

// ---- US-018: stale mapping must not be reused when VS.LastProvider differs ----

func TestResumeVirtualSession_StaleMappingForCurrentProviderTriggersTranslate(t *testing.T) {
	isolateHome(t)
	claude := newFakeProvider()
	claude.id = "claude"
	claude.displayName = "Claude"
	codex := newFakeProvider()
	codex.id = "codex"
	codex.displayName = "Codex"
	codex.loadHistoryFn = func(_ string, _ HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{
			{kind: histUser, text: "question in codex"},
			{kind: histResponse, text: "codex answered"},
		}, nil
	}
	var claudeMaterialized bool
	claude.materializeFn = func(workspace string, turns []NeutralTurn) (string, string, error) {
		claudeMaterialized = true
		// Assert we're seeing codex's turns (not some stale claude snapshot).
		if len(turns) != 2 || turns[0].Text != "question in codex" {
			t.Errorf("claude.Materialize should receive codex's turns; got %+v", turns)
		}
		return "claude-fresh", workspace, nil
	}
	withRegisteredProviders(t, claude, codex)

	// VS has both mappings, but codex wrote more recently (LastProvider=codex).
	store := &virtualSessionStore{Version: 1}
	now := time.Now().UTC()
	vsID := upsertVirtualSession(store, "", "/ws", "claude", "stale-claude", "/ws", "hi", now.Add(-time.Hour))
	upsertVirtualSession(store, vsID, "/ws", "codex", "fresh-codex", "/ws", "hi", now)
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Tab is on claude; the cached claude mapping is stale (codex wrote after).
	m := newTestModel(t, claude)
	m.cwd = "/ws"
	newM, cmd := m.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	mm := newM.(model)
	if mm.sessionID != "" {
		t.Errorf("sessionID=%q — must be empty until translation completes (cannot reuse stale claude mapping)", mm.sessionID)
	}
	if !mm.busy {
		t.Error("busy must be true during translation")
	}
	if cmd == nil {
		t.Fatal("expected translateVSCmd, got nil")
	}
	mat, ok := cmd().(virtualSessionMaterializedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want virtualSessionMaterializedMsg (stale mapping must trigger translate)", cmd())
	}
	if !claudeMaterialized {
		t.Error("claude.Materialize must be called to refresh the stale mapping")
	}
	if mat.err != nil {
		t.Fatalf("translate err: %v", mat.err)
	}
	if mat.nativeSessionID != "claude-fresh" {
		t.Errorf("nativeSessionID=%q want claude-fresh (materialized)", mat.nativeSessionID)
	}
	mm2, _ := runUpdate(t, mm, mat)
	if mm2.sessionID != "claude-fresh" {
		t.Errorf("m.sessionID=%q want claude-fresh after translate", mm2.sessionID)
	}
	// VS store: claude mapping is overwritten, LastProvider flipped to claude.
	got, _ := loadVirtualSessions()
	if got.Sessions[0].ProviderSessions["claude"].SessionID != "claude-fresh" {
		t.Errorf("stale claude mapping not overwritten: %+v", got.Sessions[0].ProviderSessions)
	}
	if got.Sessions[0].LastProvider != "claude" {
		t.Errorf("LastProvider=%q want claude after translate-back", got.Sessions[0].LastProvider)
	}
}

func TestApplyProviderSwitch_StaleMappingTriggersTranslate(t *testing.T) {
	isolateHome(t)
	// fakeA will be the swap target; fakeB is where we're coming from (the
	// provider that wrote most recently).
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	var aGotTurns []NeutralTurn
	pA.materializeFn = func(ws string, turns []NeutralTurn) (string, string, error) {
		aGotTurns = append([]NeutralTurn(nil), turns...)
		return "fresh-A", ws, nil
	}
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	withRegisteredProviders(t, pA, pB)

	// VS has both mappings; fakeB is LastProvider (the latest writer).
	store := &virtualSessionStore{Version: 1}
	now := time.Now().UTC()
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "stale-A", "/ws", "hi", now.Add(-time.Hour))
	upsertVirtualSession(store, vsID, "/ws", "fakeB", "current-B", "/ws", "hi", now)
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Model is on fakeB with the latest turns in-memory; swap to fakeA.
	m := newTestModel(t, pB)
	m.virtualSessionID = vsID
	m.cwd = "/ws"
	m.history = []historyEntry{
		{kind: histUser, text: "user-B"},
		{kind: histResponse, text: "assistant-B"},
	}
	m.providerSwitchProvIdx = 0 // target A
	newM, cmd := m.applyProviderSwitch("")
	mm := newM.(model)
	if mm.provider.ID() != "fakeA" {
		t.Fatalf("expected provider fakeA, got %s", mm.provider.ID())
	}
	if mm.sessionID != "" {
		t.Errorf("sessionID=%q — must be empty during translation (stale mapping cannot be reused)", mm.sessionID)
	}
	if !mm.busy {
		t.Error("busy should be true while translation runs")
	}
	// Drain the batched cmd and find the materialize msg.
	msgs := drainBatch(t, cmd)
	var matMsg *virtualSessionMaterializedMsg
	for _, msg := range msgs {
		if m, ok := msg.(virtualSessionMaterializedMsg); ok {
			matMsg = &m
		}
	}
	if matMsg == nil {
		t.Fatalf("stale mapping must trigger translate; got msgs %T", msgs)
	}
	if matMsg.nativeSessionID != "fresh-A" {
		t.Errorf("nativeSessionID=%q want fresh-A", matMsg.nativeSessionID)
	}
	if len(aGotTurns) != 2 || aGotTurns[0].Text != "user-B" || aGotTurns[1].Text != "assistant-B" {
		t.Errorf("fakeA.Materialize should receive the B-tab turns; got %+v", aGotTurns)
	}
}

func TestTranslate_PassesWorktreeCwdToMaterialize(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pB := newFakeProvider()
	pB.id = "fakeB"
	var gotCwd string
	pB.materializeFn = func(cwd string, _ []NeutralTurn) (string, string, error) {
		gotCwd = cwd
		return "synth-B", cwd, nil
	}
	withRegisteredProviders(t, pA, pB)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "nat-A", "/ws", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pA)
	m.virtualSessionID = vsID
	m.cwd = "/ws"
	m.worktreeName = "ask-fakeA-1"
	m.history = []historyEntry{
		{kind: histUser, text: "hi"},
		{kind: histResponse, text: "hello"},
	}
	m.providerSwitchProvIdx = 1
	newM, cmd := m.applyProviderSwitch("")
	mm := newM.(model)
	if mm.provider.ID() != "fakeB" {
		t.Fatalf("swap failed: %s", mm.provider.ID())
	}
	// Drain to find the translate cmd's msg.
	msgs := drainBatch(t, cmd)
	var mat *virtualSessionMaterializedMsg
	for _, msg := range msgs {
		if m, ok := msg.(virtualSessionMaterializedMsg); ok {
			mat = &m
		}
	}
	if mat == nil {
		t.Fatal("expected virtualSessionMaterializedMsg")
	}
	wantCwd := "/ws/.claude/worktrees/ask-fakeA-1"
	if gotCwd != wantCwd {
		t.Errorf("Materialize got cwd=%q want %q — worktree path must propagate so claude --resume finds the synthetic file", gotCwd, wantCwd)
	}
	// VS's mapping Cwd should also carry the worktree path so ensureProc
	// points the subprocess at the same directory on resume.
	got, _ := loadVirtualSessions()
	if ref := got.Sessions[0].ProviderSessions["fakeB"]; ref.Cwd != wantCwd {
		t.Errorf("VS mapping Cwd=%q want %q", ref.Cwd, wantCwd)
	}
}

// ---- US-013: claudeProvider.Materialize round-trip ----

func TestClaudeMaterialize_RoundTripsViaLoadClaudeHistory(t *testing.T) {
	home := isolateHome(t)
	t.Chdir(t.TempDir())
	cwd, _ := os.Getwd()
	// Production canonicalizes the workspace via EvalSymlinks so the
	// encoded dir matches where claude itself reads via getcwd(2).
	// On macOS t.TempDir() lives under /var which symlinks to
	// /private/var, so we have to canonicalize the same way to
	// derive the expected encoded path.
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	turns := []NeutralTurn{
		{Role: "user", Text: "first user"},
		{Role: "assistant", Text: "first answer"},
		{Role: "user", Text: "second user"},
		{Role: "assistant", Text: "second answer"},
	}
	sid, nativeCwd, err := writeClaudeSyntheticSession(cwd, turns)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if sid == "" {
		t.Fatal("expected non-empty session id")
	}
	if nativeCwd != resolved {
		t.Errorf("nativeCwd=%q want %q", nativeCwd, resolved)
	}
	// File landed under HOME/.claude/projects/<enc>.
	enc := strings.ReplaceAll(resolved, "/", "-")
	enc = strings.ReplaceAll(enc, ".", "-")
	fp := filepath.Join(home, ".claude", "projects", enc, sid+".jsonl")
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("synthetic file missing at %s: %v", fp, err)
	}
	// loadClaudeHistory sees the same 4 turns in order.
	got, err := loadClaudeHistory(sid, HistoryOpts{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 entries, got %d: %+v", len(got), got)
	}
	want := []struct {
		kind historyKind
		text string
	}{
		{histUser, "first user"},
		{histResponse, "first answer"},
		{histUser, "second user"},
		{histResponse, "second answer"},
	}
	for i, w := range want {
		if got[i].kind != w.kind || got[i].text != w.text {
			t.Errorf("entry[%d] = {%v, %q}, want {%v, %q}", i, got[i].kind, got[i].text, w.kind, w.text)
		}
	}
}

// When the workspace passed in is a symlink, the synthetic file must land
// under the canonical-encoded dir (matching where claude itself reads via
// getcwd(2)) and the returned nativeCwd must point at the canonical path
// so subsequent --resume launches don't drift back to the symlink form.
func TestClaudeMaterialize_ResolvesSymlinkedWorkspace(t *testing.T) {
	home := isolateHome(t)
	canonical := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(canonical, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// EvalSymlinks resolves through every symlink, including macOS's
	// /var → /private/var. Production canonicalizes the same way, so
	// expected paths must use the fully-resolved form on every OS.
	resolvedCanonical, err := filepath.EvalSymlinks(canonical)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	turns := []NeutralTurn{{Role: "user", Text: "hi"}}
	sid, nativeCwd, err := writeClaudeSyntheticSession(link, turns)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if nativeCwd != resolvedCanonical {
		t.Errorf("nativeCwd=%q want canonical %q", nativeCwd, resolvedCanonical)
	}
	encCanon := strings.ReplaceAll(resolvedCanonical, "/", "-")
	encCanon = strings.ReplaceAll(encCanon, ".", "-")
	canonPath := filepath.Join(home, ".claude", "projects", encCanon, sid+".jsonl")
	if _, err := os.Stat(canonPath); err != nil {
		t.Fatalf("synthetic file should land under canonical encoding %s: %v", canonPath, err)
	}
	encLink := strings.ReplaceAll(link, "/", "-")
	encLink = strings.ReplaceAll(encLink, ".", "-")
	linkPath := filepath.Join(home, ".claude", "projects", encLink, sid+".jsonl")
	if _, err := os.Stat(linkPath); err == nil {
		t.Errorf("synthetic file should NOT exist under symlink encoding %s", linkPath)
	}
}

// ---- US-014: codexProvider.Materialize round-trip ----

func TestCodexMaterialize_RoundTripsViaLoadCodexHistory(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	turns := []NeutralTurn{
		{Role: "user", Text: "hi codex"},
		{Role: "assistant", Text: "hello human"},
	}
	threadID, nativeCwd, err := writeCodexSyntheticSession(cwd, turns)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if threadID == "" {
		t.Fatal("expected non-empty thread id")
	}
	if nativeCwd != cwd {
		t.Errorf("nativeCwd=%q want %q", nativeCwd, cwd)
	}
	got, err := loadCodexHistory(threadID, HistoryOpts{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(got), got)
	}
	if got[0].kind != histUser || got[0].text != "hi codex" {
		t.Errorf("entry[0] = {%v, %q}", got[0].kind, got[0].text)
	}
	if got[1].kind != histResponse || got[1].text != "hello human" {
		t.Errorf("entry[1] = {%v, %q}", got[1].kind, got[1].text)
	}
}

func TestCodexMaterialize_OmitsBaseInstructionsAndDeveloperPreambles(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	turns := []NeutralTurn{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "hello"},
	}
	threadID, _, err := writeCodexSyntheticSession(cwd, turns)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// Locate the rollout and inspect the raw bytes.
	var found string
	_ = filepath.Walk(filepath.Join(home, ".codex", "sessions"), func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, threadID+".jsonl") {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatal("rollout file not written")
	}
	data, err := os.ReadFile(found)
	if err != nil {
		t.Fatalf("read rollout: %v", err)
	}
	for _, forbidden := range []string{"base_instructions", "permissions instructions", "collaboration_mode", "environment_context", "AGENTS.md"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("synthetic rollout should not contain %q; file:\n%s", forbidden, string(data))
		}
	}
	// session_meta + 2 response_items expected; every line is a single JSON object
	// so splitting by newline gives us a predictable count.
	lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1
	if lines != 3 {
		t.Errorf("want 3 lines (meta + 2 turns), got %d: %s", lines, string(data))
	}
}

// ---- US-011: NeutralTurn extraction ----

func TestNeutralTurnsFromHistory_MapsKindsAndSkipsTools(t *testing.T) {
	history := []historyEntry{
		{kind: histUser, text: "hi"},
		{kind: histPrerendered, text: "[tool call — skipped]"},
		{kind: histResponse, text: "hello"},
		{kind: histPrerendered, text: "[tool result — skipped]"},
		{kind: histUser, text: "more"},
	}
	got := neutralTurnsFromHistory(history)
	if len(got) != 3 {
		t.Fatalf("want 3 turns, got %d: %+v", len(got), got)
	}
	want := []NeutralTurn{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "hello"},
		{Role: "user", Text: "more"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("turn[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// ---- US-008: Ctrl+B mid-session swap ----

func TestApplyProviderSwitch_CrossProviderWithMappingLoadsHistory(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	pB.loadHistoryFn = func(id string, _ HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{{kind: histResponse, text: "loaded-from-B:" + id}}, nil
	}
	withRegisteredProviders(t, pA, pB)

	// Seed a VS with mappings for both providers so the swap target has one.
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "nat-A", "/ws", "hi", time.Now().UTC())
	upsertVirtualSession(store, vsID, "/ws", "fakeB", "nat-B", "/ws-B", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pA)
	m.virtualSessionID = vsID
	m.sessionID = "nat-A"
	m.cwd = "/ws"
	m.providerSwitchProvIdx = 1 // target B
	newM, cmd := m.applyProviderSwitch("")
	mm := newM.(model)
	if mm.provider.ID() != "fakeB" {
		t.Fatalf("expected provider fakeB, got %s", mm.provider.ID())
	}
	if mm.sessionID != "nat-B" {
		t.Errorf("sessionID=%q want nat-B (mapped from VS)", mm.sessionID)
	}
	if mm.resumeCwd != "/ws-B" {
		t.Errorf("resumeCwd=%q want /ws-B", mm.resumeCwd)
	}
	if cmd == nil {
		t.Fatal("expected batched cmd (probe + loadHistory)")
	}
}

func TestApplyProviderSwitch_CrossProviderWithoutMappingMaterializes(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	// Record what turns the target sees.
	var gotTurns []NeutralTurn
	pB.materializeFn = func(workspace string, turns []NeutralTurn) (string, string, error) {
		gotTurns = append([]NeutralTurn(nil), turns...)
		return "synth-B", workspace, nil
	}
	withRegisteredProviders(t, pA, pB)

	// VS has fakeA only.
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "nat-A", "/ws", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pA)
	m.virtualSessionID = vsID
	m.cwd = "/ws"
	m.history = []historyEntry{
		{kind: histUser, text: "prior user"},
		{kind: histResponse, text: "prior assistant"},
	}
	m.providerSwitchProvIdx = 1
	newM, cmd := m.applyProviderSwitch("")
	mm := newM.(model)
	if !mm.busy {
		t.Error("busy should be true while translation runs")
	}
	if mm.virtualSessionID != vsID {
		t.Errorf("VS id lost on swap: %q", mm.virtualSessionID)
	}
	if cmd == nil {
		t.Fatal("expected batched cmd")
	}
	// Drain the batch to find the translate cmd's message.
	msgs := drainBatch(t, cmd)
	var matMsg *virtualSessionMaterializedMsg
	for _, msg := range msgs {
		if m, ok := msg.(virtualSessionMaterializedMsg); ok {
			matMsg = &m
		}
	}
	if matMsg == nil {
		t.Fatalf("expected virtualSessionMaterializedMsg; got %T messages", msgs)
	}
	if matMsg.err != nil {
		t.Fatalf("materialize err: %v", matMsg.err)
	}
	if matMsg.nativeSessionID != "synth-B" {
		t.Errorf("nativeSessionID=%q want synth-B", matMsg.nativeSessionID)
	}
	// The target saw the two prior turns.
	if len(gotTurns) != 2 || gotTurns[0].Text != "prior user" || gotTurns[1].Text != "prior assistant" {
		t.Errorf("target.Materialize received wrong turns: %+v", gotTurns)
	}
	// Feed the msg back so state lands.
	mm2, _ := runUpdate(t, mm, *matMsg)
	if mm2.sessionID != "synth-B" {
		t.Errorf("sessionID not set post-materialize: %q", mm2.sessionID)
	}
	if mm2.busy {
		t.Error("busy should clear after materialize completes")
	}
}

func TestApplyProviderSwitch_SameProviderDoesNotTouchSession(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	p.id = "only"
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.virtualSessionID = "vs-keep"
	m.sessionID = "keep-session"
	m.resumeCwd = "/keep"
	m.providerSwitchProvIdx = 0
	newM, _ := m.applyProviderSwitch("new-model")
	mm := newM.(model)
	if mm.sessionID != "keep-session" {
		t.Errorf("same-provider swap dropped sessionID: %q", mm.sessionID)
	}
	if mm.virtualSessionID != "vs-keep" {
		t.Errorf("VS id dropped: %q", mm.virtualSessionID)
	}
}

// ---- US-009: concurrent-tab write safety ----

func TestMutateVirtualSessions_ConcurrentUpsertsAllPersist(t *testing.T) {
	isolateHome(t)
	const N = 10
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := mutateVirtualSessions(func(store *virtualSessionStore) error {
				upsertVirtualSession(store, "", "/ws",
					fmt.Sprintf("prov-%d", i),
					fmt.Sprintf("native-%d", i),
					"/ws",
					fmt.Sprintf("preview %d", i),
					time.Now().UTC())
				return nil
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("mutate failed: %v", err)
	}
	got, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Sessions) != N {
		t.Errorf("want %d sessions after concurrent upserts, got %d — locking failed to prevent lost writes",
			N, len(got.Sessions))
	}
}

// ---- Issue #19: worktree-cwd non-regression guard ----

// applyTurn must not regress a known-good worktree-rooted Cwd to the
// bare project root. A turn arriving with a project-root Cwd from a
// tab where m.worktreeName has been cleared (cross-provider swap
// before first fork, /config worktree toggle, etc.) would otherwise
// rewrite the canonical worktree path on the VS and strand later
// worktree-mode resumes. The SessionID still advances — it's only
// the Cwd that's protected.
func TestApplyTurn_KeepsWorktreeCwdWhenNewIsProjectRoot(t *testing.T) {
	vs := &VirtualSession{
		ProviderSessions: map[string]ProviderSessionRef{
			"claude": {SessionID: "old-id", Cwd: "/ws/.claude/worktrees/witty-napping-peach"},
		},
	}
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	vs.applyTurn("claude",
		ProviderSessionRef{SessionID: "new-id", Cwd: "/ws"}, "", now)
	ref := vs.ProviderSessions["claude"]
	if ref.Cwd != "/ws/.claude/worktrees/witty-napping-peach" {
		t.Errorf("Cwd regressed to %q; want preserved worktree path", ref.Cwd)
	}
	if ref.SessionID != "new-id" {
		t.Errorf("SessionID=%q want new-id (always advanced even when Cwd is held)", ref.SessionID)
	}
	if !vs.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt should bump even when Cwd is held; got %v", vs.UpdatedAt)
	}
}

// The guard only fires when the new Cwd is non-worktree. A legitimate
// worktree-to-worktree migration (the same conversation moving to a
// different ask-managed workspace) lets the new Cwd through.
func TestApplyTurn_WorktreeToWorktreeUpdatesNormally(t *testing.T) {
	vs := &VirtualSession{
		ProviderSessions: map[string]ProviderSessionRef{
			"claude": {SessionID: "id1", Cwd: "/ws/.claude/worktrees/old"},
		},
	}
	vs.applyTurn("claude",
		ProviderSessionRef{SessionID: "id2", Cwd: "/ws/.claude/worktrees/new"},
		"", time.Now().UTC())
	if vs.ProviderSessions["claude"].Cwd != "/ws/.claude/worktrees/new" {
		t.Errorf("Cwd should advance worktree→worktree, got %q",
			vs.ProviderSessions["claude"].Cwd)
	}
}

// Project-root → project-root is not a regression — the new Cwd is
// applied normally so symlink resolution differences don't get stuck.
func TestApplyTurn_ProjectRootToProjectRootUpdatesNormally(t *testing.T) {
	vs := &VirtualSession{
		ProviderSessions: map[string]ProviderSessionRef{
			"claude": {SessionID: "id1", Cwd: "/ws"},
		},
	}
	vs.applyTurn("claude",
		ProviderSessionRef{SessionID: "id2", Cwd: "/private/ws"},
		"", time.Now().UTC())
	if vs.ProviderSessions["claude"].Cwd != "/private/ws" {
		t.Errorf("Cwd should advance project-root→project-root, got %q",
			vs.ProviderSessions["claude"].Cwd)
	}
}

// First-time recording on a fresh ref (no prior entry for the
// provider) writes the new Cwd verbatim — the guard only kicks in
// when there's a prior ref to compare against.
func TestApplyTurn_FirstRefWritesProjectRootNormally(t *testing.T) {
	vs := &VirtualSession{
		ProviderSessions: map[string]ProviderSessionRef{},
	}
	vs.applyTurn("claude",
		ProviderSessionRef{SessionID: "id1", Cwd: "/ws"},
		"", time.Now().UTC())
	if vs.ProviderSessions["claude"].Cwd != "/ws" {
		t.Errorf("Cwd=%q want /ws on first ref", vs.ProviderSessions["claude"].Cwd)
	}
}

// upsertVirtualSession routes existing-VS updates through applyTurn,
// so the recording guard must hold end-to-end through that entry too.
// This is the public API recordVirtualSession actually calls.
func TestUpsertVirtualSession_GuardsAgainstWorktreeRegression(t *testing.T) {
	store := &virtualSessionStore{Version: 1}
	t0 := time.Now().UTC()
	id := upsertVirtualSession(store, "", "/ws", "claude", "id1",
		"/ws/.claude/worktrees/calm-resting-otter", "hi", t0)
	// Second turn with bare project-root cwd: simulates a turn from a
	// tab where m.worktreeName has been cleared between turns.
	upsertVirtualSession(store, id, "/ws", "claude", "id2", "/ws", "", t0.Add(time.Minute))
	ref := store.Sessions[0].ProviderSessions["claude"]
	if ref.Cwd != "/ws/.claude/worktrees/calm-resting-otter" {
		t.Errorf("upsert regressed Cwd to %q; guard should have kept the worktree path", ref.Cwd)
	}
	if ref.SessionID != "id2" {
		t.Errorf("SessionID=%q want id2 (advance even when Cwd is held)", ref.SessionID)
	}
}

// recordVirtualSession is the in-tab entry: a tab whose
// m.worktreeName has been cleared between turns must not cause the
// VS row's worktree-rooted Cwd to be overwritten with the project
// root. Mirrors the real call site at update.go (providerDoneMsg
// handler).
func TestRecordVirtualSession_DoesNotRegressWorktreeCwd(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.cwd = "/ws"
	m.worktreeName = "shimmering-flying-crow"
	m.history = []historyEntry{{kind: histUser, text: "first"}}
	m.recordVirtualSession("native-1")
	vsID := m.virtualSessionID
	if vsID == "" {
		t.Fatal("recordVirtualSession should have created a VS")
	}

	// Now mimic the regression-causing path: m.worktreeName cleared
	// (e.g. /config worktree toggle), then another turn completes.
	m.worktreeName = ""
	m.recordVirtualSession("native-2")

	got, _ := loadVirtualSessions()
	vs := got.findByID(vsID)
	if vs == nil {
		t.Fatalf("VS missing after second turn: %+v", got.Sessions)
	}
	ref := vs.ProviderSessions["claude"]
	if ref.Cwd != "/ws/.claude/worktrees/shimmering-flying-crow" {
		t.Errorf("Cwd=%q regressed; guard should keep the worktree path across the m.worktreeName=\"\" turn",
			ref.Cwd)
	}
	if ref.SessionID != "native-2" {
		t.Errorf("SessionID=%q want native-2", ref.SessionID)
	}
}

// ---- Issue #19: cross-provider swap recovers worktree from VS ----

// Bug-B fix at the swap site: a cross-provider swap from a tab whose
// m.worktreeName is empty must still materialize the new provider's
// session inside the VS's worktree, recovered from any prior ref.
// Without recovery, the materialize would write the file at m.cwd,
// baking project-root state into the VS for the new provider — and
// every later resume would re-record the regression.
func TestApplyProviderSwitch_RecoversWorktreeFromVS_OnTranslate(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	var gotCwd string
	pB.materializeFn = func(cwd string, _ []NeutralTurn) (string, string, error) {
		gotCwd = cwd
		return "synth-B", cwd, nil
	}
	withRegisteredProviders(t, pA, pB)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "nat-A",
		"/ws/.claude/worktrees/witty-napping-peach", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pA)
	m.virtualSessionID = vsID
	m.cwd = "/ws"
	m.worktreeName = "" // mimics resume-then-swap before the first fork
	m.history = []historyEntry{
		{kind: histUser, text: "user-A"},
		{kind: histResponse, text: "assistant-A"},
	}
	m.providerSwitchProvIdx = 1
	newM, cmd := m.applyProviderSwitch("")
	mm := newM.(model)
	if mm.provider.ID() != "fakeB" {
		t.Fatalf("swap failed: %s", mm.provider.ID())
	}
	msgs := drainBatch(t, cmd)
	var mat *virtualSessionMaterializedMsg
	for _, msg := range msgs {
		if m, ok := msg.(virtualSessionMaterializedMsg); ok {
			mat = &m
		}
	}
	if mat == nil {
		t.Fatal("expected virtualSessionMaterializedMsg")
	}
	wantCwd := "/ws/.claude/worktrees/witty-napping-peach"
	if gotCwd != wantCwd {
		t.Errorf("Materialize cwd=%q want %q (recovered from prior VS ref)", gotCwd, wantCwd)
	}
	got, _ := loadVirtualSessions()
	if ref := got.Sessions[0].ProviderSessions["fakeB"]; ref.Cwd != wantCwd {
		t.Errorf("VS B-mapping Cwd=%q want %q", ref.Cwd, wantCwd)
	}
}

// When the live tab already has m.worktreeName set, the swap path
// uses that directly and does NOT consult the VS — preserves the
// existing fast path so the recovery logic only runs when needed.
func TestApplyProviderSwitch_PrefersLiveWorktreeNameOverVS(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	var gotCwd string
	pB.materializeFn = func(cwd string, _ []NeutralTurn) (string, string, error) {
		gotCwd = cwd
		return "synth-B", cwd, nil
	}
	withRegisteredProviders(t, pA, pB)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "nat-A",
		"/ws/.claude/worktrees/old-from-vs", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pA)
	m.virtualSessionID = vsID
	m.cwd = "/ws"
	m.worktreeName = "live-name" // live tab knows where it is
	m.history = []historyEntry{
		{kind: histUser, text: "u"},
		{kind: histResponse, text: "a"},
	}
	m.providerSwitchProvIdx = 1
	_, cmd := m.applyProviderSwitch("")
	_ = drainBatch(t, cmd)
	if gotCwd != "/ws/.claude/worktrees/live-name" {
		t.Errorf("Materialize cwd=%q want /ws/.claude/worktrees/live-name (live wins over VS)", gotCwd)
	}
}

// When the VS has only project-root refs (a genuinely worktree-less
// conversation), swap stays at project root — recovery doesn't
// invent a worktree out of thin air.
func TestApplyProviderSwitch_StaysAtProjectRootWhenNoVSWorktree(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	var gotCwd string
	pB.materializeFn = func(cwd string, _ []NeutralTurn) (string, string, error) {
		gotCwd = cwd
		return "synth-B", cwd, nil
	}
	withRegisteredProviders(t, pA, pB)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "nat-A",
		"/ws", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pA)
	m.virtualSessionID = vsID
	m.cwd = "/ws"
	m.worktreeName = ""
	m.history = []historyEntry{
		{kind: histUser, text: "u"},
		{kind: histResponse, text: "a"},
	}
	m.providerSwitchProvIdx = 1
	_, cmd := m.applyProviderSwitch("")
	_ = drainBatch(t, cmd)
	if gotCwd != "/ws" {
		t.Errorf("Materialize cwd=%q want /ws (no VS worktree to recover)", gotCwd)
	}
}

// A stale worktree ref on some other provider must not override the
// last writer's explicit project-root cwd. The direct-turns swap path
// is translating the current provider's canonical history; if that
// provider recorded project-root cwd, the new materialized session
// must stay at project root rather than reviving an older worktree.
func TestApplyProviderSwitch_PrefersLastProviderProjectRootOverStaleWorktree(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	pC := newFakeProvider()
	pC.id = "fakeC"
	pC.displayName = "Fake C"
	var gotCwd string
	pC.materializeFn = func(cwd string, _ []NeutralTurn) (string, string, error) {
		gotCwd = cwd
		return "synth-C", cwd, nil
	}
	withRegisteredProviders(t, pA, pB, pC)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeB", "nat-B",
		"/ws/.claude/worktrees/stale-worktree", "hi", time.Now().UTC())
	upsertVirtualSession(store, vsID, "/ws", "fakeA", "nat-A",
		"/ws", "", time.Now().UTC().Add(time.Minute))
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pA)
	m.virtualSessionID = vsID
	m.cwd = "/ws"
	m.worktreeName = ""
	m.history = []historyEntry{
		{kind: histUser, text: "u"},
		{kind: histResponse, text: "a"},
	}
	m.providerSwitchProvIdx = 2 // target fakeC
	_, cmd := m.applyProviderSwitch("")
	_ = drainBatch(t, cmd)
	if gotCwd != "/ws" {
		t.Errorf("Materialize cwd=%q want /ws (last provider's explicit project-root ref must win)", gotCwd)
	}
	got, _ := loadVirtualSessions()
	if ref := got.Sessions[0].ProviderSessions["fakeC"]; ref.Cwd != "/ws" {
		t.Errorf("VS C-mapping Cwd=%q want /ws", ref.Cwd)
	}
}

// Cached-path swap onto a worktree-rooted ref must hand the worktree
// name to m.worktreeName so an immediate second swap-before-fork
// translates at the right cwd. Without this, the second swap would
// fall back to the slow VS-recovery path on every chained Ctrl+B.
func TestApplyProviderSwitch_RealignsWorktreeNameOnCachedSwap(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	pB.loadHistoryFn = func(id string, _ HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{{kind: histResponse, text: "from B " + id}}, nil
	}
	withRegisteredProviders(t, pA, pB)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeB", "nat-B",
		"/ws/.claude/worktrees/blissful-skating-swan", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pA)
	m.virtualSessionID = vsID
	m.cwd = "/ws"
	m.worktreeName = ""
	m.providerSwitchProvIdx = 1
	newM, _ := m.applyProviderSwitch("")
	mm := newM.(model)
	if mm.worktreeName != "blissful-skating-swan" {
		t.Errorf("worktreeName=%q want blissful-skating-swan (recovered from cached B ref)",
			mm.worktreeName)
	}
}

// Cached-path swap onto a project-root ref MUST clear any stale
// worktreeName from a prior tab — otherwise the next fork would
// point at a worktree the resumed session was never written to,
// breaking claude --resume's cwd-keyed lookup.
func TestApplyProviderSwitch_ClearsWorktreeNameOnProjectRootCachedSwap(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	pB.loadHistoryFn = func(_ string, _ HistoryOpts) ([]historyEntry, error) {
		return nil, nil
	}
	withRegisteredProviders(t, pA, pB)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeB", "nat-B",
		"/ws", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pA)
	m.virtualSessionID = vsID
	m.cwd = "/ws"
	m.worktreeName = "stale-prior-tab-name"
	m.providerSwitchProvIdx = 1
	newM, _ := m.applyProviderSwitch("")
	mm := newM.(model)
	if mm.worktreeName != "" {
		t.Errorf("worktreeName=%q must be cleared when swapping to a project-root ref",
			mm.worktreeName)
	}
}

// /resume picker landing on a worktree-rooted ref hands the worktree
// name to m.worktreeName so a swap-before-first-turn (Ctrl+B) keeps
// the conversation in the right workspace.
func TestResumeVirtualSession_RealignsWorktreeNameFromCachedRef(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	p.id = "fakeA"
	p.loadHistoryFn = func(_ string, _ HistoryOpts) ([]historyEntry, error) {
		return nil, nil
	}
	withRegisteredProviders(t, p)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "nat-A",
		"/ws/.claude/worktrees/swift-dancing-glacier", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, p)
	m.cwd = "/ws"
	m.worktreeName = "stale"
	newM, _ := m.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	mm := newM.(model)
	if mm.worktreeName != "swift-dancing-glacier" {
		t.Errorf("worktreeName=%q want swift-dancing-glacier (from resumed ref)", mm.worktreeName)
	}
}

// /resume picker landing on a project-root ref MUST clear any stale
// worktreeName so the first fork honors the recorded location.
func TestResumeVirtualSession_ClearsWorktreeNameOnProjectRootRef(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	p.id = "fakeA"
	p.loadHistoryFn = func(_ string, _ HistoryOpts) ([]historyEntry, error) {
		return nil, nil
	}
	withRegisteredProviders(t, p)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "nat-A",
		"/ws", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, p)
	m.cwd = "/ws"
	m.worktreeName = "stale"
	newM, _ := m.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	mm := newM.(model)
	if mm.worktreeName != "" {
		t.Errorf("worktreeName=%q must be cleared on project-root resume", mm.worktreeName)
	}
}

// /resume into a translate path (current provider has no ref) must
// recover a worktree name from the source provider's ref so the
// new native session lands in the same worktree the conversation
// already lives in.
func TestResumeVirtualSession_TranslateRecoversWorktreeFromSourceRef(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pA.loadHistoryFn = func(_ string, _ HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{{kind: histUser, text: "u"}}, nil
	}
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	var gotCwd string
	pB.materializeFn = func(cwd string, _ []NeutralTurn) (string, string, error) {
		gotCwd = cwd
		return "nat-B-synth", cwd, nil
	}
	withRegisteredProviders(t, pA, pB)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeA", "nat-A",
		"/ws/.claude/worktrees/lazy-singing-fox", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pB)
	m.cwd = "/ws"
	m.worktreeName = ""
	newM, cmd := m.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	mm := newM.(model)
	if cmd == nil {
		t.Fatal("expected translate command")
	}
	mat, ok := cmd().(virtualSessionMaterializedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want virtualSessionMaterializedMsg", cmd())
	}
	if mat.err != nil {
		t.Fatalf("translate err: %v", mat.err)
	}
	wantCwd := "/ws/.claude/worktrees/lazy-singing-fox"
	if gotCwd != wantCwd {
		t.Errorf("Materialize cwd=%q want %q (recovered from source ref)", gotCwd, wantCwd)
	}
	if mm.worktreeName != "lazy-singing-fox" {
		t.Errorf("model worktreeName=%q want lazy-singing-fox", mm.worktreeName)
	}
}

// A stale worktree on some other provider must not override the
// source provider's explicit project-root cwd. The resume translate
// path is replaying the source provider's history, so project-root is
// authoritative here.
func TestResumeVirtualSession_TranslateKeepsProjectRootWhenSourceRefIsProjectRoot(t *testing.T) {
	isolateHome(t)
	pA := newFakeProvider()
	pA.id = "fakeA"
	pA.displayName = "Fake A"
	pA.loadHistoryFn = func(_ string, _ HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{{kind: histUser, text: "u"}}, nil
	}
	pB := newFakeProvider()
	pB.id = "fakeB"
	pB.displayName = "Fake B"
	pC := newFakeProvider()
	pC.id = "fakeC"
	pC.displayName = "Fake C"
	var gotCwd string
	pC.materializeFn = func(cwd string, _ []NeutralTurn) (string, string, error) {
		gotCwd = cwd
		return "nat-C-synth", cwd, nil
	}
	withRegisteredProviders(t, pA, pB, pC)

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "fakeB", "nat-B",
		"/ws/.claude/worktrees/stale-worktree", "hi", time.Now().UTC())
	upsertVirtualSession(store, vsID, "/ws", "fakeA", "nat-A",
		"/ws", "", time.Now().UTC().Add(time.Minute))
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, pC)
	m.cwd = "/ws"
	m.worktreeName = ""
	newM, cmd := m.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	mm := newM.(model)
	if cmd == nil {
		t.Fatal("expected translate command")
	}
	mat, ok := cmd().(virtualSessionMaterializedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want virtualSessionMaterializedMsg", cmd())
	}
	if mat.err != nil {
		t.Fatalf("translate err: %v", mat.err)
	}
	if gotCwd != "/ws" {
		t.Errorf("Materialize cwd=%q want /ws (source provider's project-root ref must win)", gotCwd)
	}
	if mm.worktreeName != "" {
		t.Errorf("model worktreeName=%q want empty for explicit project-root source", mm.worktreeName)
	}
}

// worktreeNameFromVS can recover from another provider's worktree
// when the caller has no authoritative preferred ref (or it is
// missing from the VS).
func TestWorktreeNameFromVS_FindsWorktreeAcrossProviderRefs(t *testing.T) {
	vs := &VirtualSession{
		ProviderSessions: map[string]ProviderSessionRef{
			"a": {Cwd: "/ws"},
			"b": {Cwd: "/ws/.claude/worktrees/merry-floating-loon"},
		},
	}
	if got := worktreeNameFromVS(vs, "missing"); got != "merry-floating-loon" {
		t.Errorf("worktreeNameFromVS=%q want merry-floating-loon", got)
	}
}

// An explicit project-root cwd on the preferred provider is
// authoritative and must not fall through to some other provider's
// older worktree.
func TestWorktreeNameFromVS_PrefersPreferredProjectRootRef(t *testing.T) {
	vs := &VirtualSession{
		ProviderSessions: map[string]ProviderSessionRef{
			"a": {Cwd: "/ws"},
			"b": {Cwd: "/ws/.claude/worktrees/merry-floating-loon"},
		},
	}
	if got := worktreeNameFromVS(vs, "a"); got != "" {
		t.Errorf("worktreeNameFromVS=%q want empty for explicit preferred project-root ref", got)
	}
}

// Only a missing/empty preferred cwd falls through to other refs.
func TestWorktreeNameFromVS_FallsBackWhenPreferredRefHasNoCwd(t *testing.T) {
	vs := &VirtualSession{
		ProviderSessions: map[string]ProviderSessionRef{
			"a": {},
			"b": {Cwd: "/ws/.claude/worktrees/merry-floating-loon"},
		},
	}
	if got := worktreeNameFromVS(vs, "a"); got != "merry-floating-loon" {
		t.Errorf("worktreeNameFromVS=%q want merry-floating-loon when preferred ref lost cwd", got)
	}
}

// When no ref carries a worktree path, returns empty (the VS is a
// genuinely project-root conversation).
func TestWorktreeNameFromVS_EmptyWhenAllProjectRoot(t *testing.T) {
	vs := &VirtualSession{
		ProviderSessions: map[string]ProviderSessionRef{
			"a": {Cwd: "/ws"},
			"b": {Cwd: "/ws"},
		},
	}
	if got := worktreeNameFromVS(vs, "missing"); got != "" {
		t.Errorf("worktreeNameFromVS=%q want empty for all-project-root VS", got)
	}
}

func TestWorktreeNameFromVS_NilSafe(t *testing.T) {
	if got := worktreeNameFromVS(nil, ""); got != "" {
		t.Errorf("worktreeNameFromVS(nil)=%q want empty", got)
	}
}
