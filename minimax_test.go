package main

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
)

func swapMiniMaxLM(t *testing.T, lm fantasy.LanguageModel) {
	t.Helper()
	prev := minimaxLanguageModel
	minimaxLanguageModel = func(apiProviderConfig, string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	t.Cleanup(func() { minimaxLanguageModel = prev })
}

func TestMiniMaxProvider_Metadata(t *testing.T) {
	p := minimaxAgentProvider()
	if p.ID() != "minimax" || p.DisplayName() != "MiniMax" {
		t.Errorf("identity wrong: %q %q", p.ID(), p.DisplayName())
	}
	caps := p.Capabilities()
	if !caps.Resume || !caps.ModelPicker || !caps.EffortPicker {
		t.Errorf("capabilities wrong: %+v", caps)
	}
	if caps.AskUserQuestionMCP || caps.PermissionPromptMCP {
		t.Errorf("MCP redirect capabilities must be off (native tools): %+v", caps)
	}
	picker := p.ModelPicker()
	if len(picker.Options) != 1 || picker.Options[0] != "MiniMax-M3" || !picker.AllowCustom {
		t.Errorf("model picker wrong: %+v", picker)
	}
	if efforts := p.EffortOptions(); len(efforts) != 2 || efforts[0] != "off" {
		t.Errorf("effort options wrong: %v", efforts)
	}
	if id := p.PreMintSessionID(ProviderSessionArgs{}); id == "" {
		t.Error("minimax must pre-mint session ids")
	}

	// Registered and addressable for workflow steps.
	got, ok := providerByIDStrict("minimax")
	if !ok || got.ID() != "minimax" {
		t.Fatal("minimax must be in the provider registry")
	}
	if err := validateProviderID("minimax"); err != nil {
		t.Errorf("workflow provider validation must accept minimax: %v", err)
	}
}

func TestMiniMaxProvider_SettingsRoundTrip(t *testing.T) {
	isolateHome(t)
	p := minimaxAgentProvider()
	if s := p.LoadSettings(); s.Model != "" || s.Effort != "" {
		t.Errorf("fresh settings must be zero: %+v", s)
	}
	want := ProviderSettings{Model: "MiniMax-M3", Effort: "high",
		SlashCommands: []providerSlashEntry{{Name: "x", Description: "y"}}}
	if err := p.SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got := p.LoadSettings()
	if got.Model != want.Model || got.Effort != want.Effort || len(got.SlashCommands) != 1 {
		t.Errorf("settings lost: %+v", got)
	}
}

func TestMiniMaxProviderOptions(t *testing.T) {
	opts, temp := minimaxProviderOptions("off")
	ds := opts["minimax"].(*openaicompat.ProviderOptions)
	if ds.ExtraBody == nil || temp == nil || *temp != 0.0 {
		t.Errorf("off must disable thinking and pin temperature 0: %+v temp=%v", ds, temp)
	}
	thinking, _ := ds.ExtraBody["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Errorf("thinking extra_body wrong: %+v", ds.ExtraBody)
	}

	opts, temp = minimaxProviderOptions("high")
	ds = opts["minimax"].(*openaicompat.ProviderOptions)
	thinking, _ = ds.ExtraBody["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || temp != nil {
		t.Errorf("high mapping wrong: %+v temp=%v", ds, temp)
	}
	if ds.ExtraBody["reasoning_split"] != true {
		t.Errorf("high must request reasoning_split: %+v", ds.ExtraBody)
	}

	opts, temp = minimaxProviderOptions("")
	ds = opts["minimax"].(*openaicompat.ProviderOptions)
	thinking, _ = ds.ExtraBody["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || temp != nil {
		t.Errorf("default effort must be high thinking: %+v", ds)
	}
	if ds.ExtraBody["reasoning_split"] != true {
		t.Errorf("default must include reasoning_split: %+v", ds.ExtraBody)
	}
}

func TestMiniMaxProvider_NoAPIKeyFailsFast(t *testing.T) {
	isolateHome(t)
	t.Setenv(minimaxEnvAPIKey, "")
	p := minimaxAgentProvider()
	_, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), NewSessionID: "s1"})
	if err == nil {
		t.Fatal("StartSession without a key must fail")
	}
	if !strings.Contains(err.Error(), "model picker") || !strings.Contains(err.Error(), minimaxEnvAPIKey) {
		t.Errorf("error must point at both config and env: %v", err)
	}
}

func TestMiniMaxProvider_SessionLifecycle(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("answer one", fantasy.Usage{InputTokens: 10}),
	}}
	swapMiniMaxLM(t, lm)

	p := minimaxAgentProvider()
	cwd := t.TempDir()
	args := ProviderSessionArgs{Cwd: cwd, TabID: 4, NewSessionID: "ses-lifecycle", SkipAllPermissions: true}
	proc, ch, err := p.StartSession(args)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if proc.cmd != nil {
		t.Error("in-process provider must not carry an exec.Cmd")
	}
	if p.NativeSessionID(proc) != "" {
		t.Error("pre-minting provider must return empty NativeSessionID")
	}

	// MiniMax-M3 supports images — attachments should be accepted.
	if err := p.Send(proc, "look", []pendingAttachment{{mime: "image/png"}}); err != nil {
		t.Fatalf("MiniMax-M3 must accept image attachments: %v", err)
	}
	msgs := readSessionMsgs(t, ch, isTurnComplete)
	var done providerDoneMsg
	var usage *usageMsg
	for _, m := range msgs {
		switch v := m.(type) {
		case providerDoneMsg:
			done = v
		case usageMsg:
			u := v
			usage = &u
		}
	}
	if done.res.SessionID != "ses-lifecycle" || done.res.Result != "answer one" {
		t.Errorf("done msg wrong: %+v", done.res)
	}
	// MiniMax-M3 has catwalk pricing — usageMsg should carry costKnown.
	if usage == nil || !usage.costKnown {
		t.Errorf("usageMsg must exist and be cost-known: %+v", usage)
	}
	// 10 input tokens at $0.60/1M = $0.000006.
	if usage.costUSD < 0.0000059 || usage.costUSD > 0.0000061 {
		t.Errorf("usage cost = %v, want ~0.000006", usage.costUSD)
	}

	// The system prompt reached the wire as the first message.
	calls := lm.streamCalls()
	if len(calls) == 0 || calls[0].Prompt[0].Role != fantasy.MessageRoleSystem {
		t.Fatal("first wire message must be the system prompt")
	}
	if sys := messageText(calls[0].Prompt[0]); !strings.Contains(sys, "<critical_rules>") ||
		!strings.Contains(sys, "super human speeds") {
		t.Error("system prompt must include coder rules and ask steering")
	}
	// StartSession resolves the model's output budget onto the wire.
	if want := minimaxSpec.maxOutputTokens(minimaxDefaultModel); calls[0].MaxOutputTokens == nil ||
		*calls[0].MaxOutputTokens != want {
		t.Errorf("wire MaxOutputTokens = %v want %d", calls[0].MaxOutputTokens, want)
	}

	// Idle interrupt reports unhandled → killProc fallback path.
	if handled, _ := p.Interrupt(proc); handled {
		t.Error("idle interrupt must report handled=false")
	}

	// Session persisted → listable and resumable.
	sessions, err := p.ListSessions(cwd)
	if err != nil || len(sessions) != 1 || sessions[0].id != "ses-lifecycle" {
		t.Fatalf("ListSessions: %v %+v", err, sessions)
	}
	if sessions[0].preview != "look" {
		t.Errorf("preview %q", sessions[0].preview)
	}
	entries, err := p.LoadHistory("ses-lifecycle", HistoryOpts{ToolOutput: toolOutputFull})
	if err != nil || len(entries) != 2 {
		t.Fatalf("LoadHistory: %v %+v", err, entries)
	}

	// Teardown: kill → providerExitedMsg then channel close.
	proc.kill()
	sawExit := false
	for m := range ch {
		if _, ok := m.(providerExitedMsg); ok {
			sawExit = true
		}
	}
	if !sawExit {
		t.Error("kill must produce providerExitedMsg")
	}

	// Resume restores the transcript into a fresh session.
	lm2 := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("answer two", fantasy.Usage{}),
	}}
	swapMiniMaxLM(t, lm2)
	proc2, ch2, err := p.StartSession(ProviderSessionArgs{
		Cwd: cwd, TabID: 4, SessionID: "ses-lifecycle", SkipAllPermissions: true,
	})
	if err != nil {
		t.Fatalf("resume StartSession: %v", err)
	}
	defer func() { proc2.kill(); drainProviderStream(ch2) }()
	if err := p.Send(proc2, "follow-up", nil); err != nil {
		t.Fatal(err)
	}
	readSessionMsgs(t, ch2, isTurnComplete)
	wire := lm2.streamCalls()[0].Prompt
	var sawPriorTurn bool
	for _, m := range wire {
		if m.Role == fantasy.MessageRoleUser && strings.Contains(messageText(m), "look") {
			sawPriorTurn = true
		}
	}
	if !sawPriorTurn {
		t.Error("resumed session must replay prior turns on the wire")
	}
}

func TestMiniMaxProvider_ResumeUnknownSessionErrors(t *testing.T) {
	isolateHome(t)
	swapMiniMaxLM(t, &fakeLM{})
	p := minimaxAgentProvider()
	if _, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), SessionID: "missing"}); err == nil {
		t.Fatal("resuming an unknown session must fail")
	}
}

func TestMiniMaxProvider_MaterializeRoundTrip(t *testing.T) {
	isolateHome(t)
	p := minimaxAgentProvider()
	workspace := t.TempDir()
	id, cwd, err := p.Materialize(workspace, []NeutralTurn{
		{Role: "user", Text: "ported question"},
		{Role: "assistant", Text: "ported answer"},
	})
	if err != nil || id == "" || cwd != workspace {
		t.Fatalf("Materialize: id=%q cwd=%q err=%v", id, cwd, err)
	}
	entries, err := p.LoadHistory(id, HistoryOpts{})
	if err != nil || len(entries) != 2 {
		t.Fatalf("materialized history: %v %+v", err, entries)
	}
	if entries[0].kind != histUser || entries[1].kind != histResponse {
		t.Errorf("materialized entry kinds wrong: %+v", entries)
	}
}

// Guard: the fake home isolation used above must keep agent sessions
// out of the real home. (Defensive — isolateHome already pins $HOME.)
func TestMiniMaxStoreUsesHome(t *testing.T) {
	home := isolateHome(t)
	st := minimaxStore()
	root, err := st.root()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(root, home) {
		t.Errorf("store root %q must live under isolated home %q", root, home)
	}
}

func TestMiniMaxMaxOutputTokens(t *testing.T) {
	if got := minimaxSpec.maxOutputTokens("MiniMax-M3"); got != 512_000 {
		t.Errorf("MiniMax-M3 budget = %d want 512k", got)
	}
	if got := minimaxSpec.maxOutputTokens("custom-model"); got != minimaxFallbackMaxOutputTokens {
		t.Errorf("unknown model must use the fallback budget: %d", got)
	}
}

func TestMiniMaxNoNativeWebSearch(t *testing.T) {
	// openaicompat backends have no first-party search; they fall back to
	// the Brave-backed core web_search tool, so nativeWebSearch stays nil.
	if minimaxSpec.nativeWebSearch != nil {
		t.Error("minimax must not declare a native web search")
	}
}

func TestModelContextLimit_MiniMax(t *testing.T) {
	if got := modelContextLimit("MiniMax-M3"); got != 1_000_000 {
		t.Errorf("MiniMax-M3 limit = %d want 1_000_000", got)
	}
	if got := modelContextLimit("minimax-some-other"); got != 200_000 {
		t.Errorf("unknown minimax model limit = %d want 200_000", got)
	}
	// Guard: other providers still resolve correctly.
	if got := modelContextLimit("deepseek-v4-pro"); got != deepseekContextWindow {
		t.Errorf("deepseek limit broken: %d", got)
	}
	if got := modelContextLimit("kimi-k2.7-code"); got != kimiContextWindow {
		t.Errorf("kimi limit broken: %d", got)
	}
	if got := modelContextLimit("opus[1m]"); got != 1_000_000 {
		t.Errorf("claude 1m tier broken by minimax case: %d", got)
	}
	if got := modelContextLimit("sonnet"); got != 200_000 {
		t.Errorf("claude default tier broken: %d", got)
	}
}
