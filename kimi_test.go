package main

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
)

func swapKimiLM(t *testing.T, lm fantasy.LanguageModel) {
	t.Helper()
	prev := kimiLanguageModel
	kimiLanguageModel = func(apiProviderConfig, string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	t.Cleanup(func() { kimiLanguageModel = prev })
}

func TestKimiProvider_Metadata(t *testing.T) {
	p := kimiAgentProvider()
	if p.ID() != "kimi" || p.DisplayName() != "Kimi" {
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
	if len(picker.Options) != 3 || picker.Options[0] != "kimi-k2.7-code" || !picker.AllowCustom {
		t.Errorf("model picker wrong: %+v", picker)
	}
	if efforts := p.EffortOptions(); len(efforts) != 2 || efforts[0] != "off" {
		t.Errorf("effort options wrong: %v", efforts)
	}
	if id := p.PreMintSessionID(ProviderSessionArgs{}); id == "" {
		t.Error("kimi must pre-mint session ids")
	}

	// Registered and addressable for workflow steps.
	got, ok := providerByIDStrict("kimi")
	if !ok || got.ID() != "kimi" {
		t.Fatal("kimi must be in the provider registry")
	}
	if err := validateProviderID("kimi"); err != nil {
		t.Errorf("workflow provider validation must accept kimi: %v", err)
	}
}

func TestKimiProvider_SettingsRoundTrip(t *testing.T) {
	isolateHome(t)
	p := kimiAgentProvider()
	if s := p.LoadSettings(); s.Model != "" || s.Effort != "" {
		t.Errorf("fresh settings must be zero: %+v", s)
	}
	want := ProviderSettings{Model: "kimi-k2-thinking", Effort: "high",
		SlashCommands: []providerSlashEntry{{Name: "x", Description: "y"}}}
	if err := p.SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got := p.LoadSettings()
	if got.Model != want.Model || got.Effort != want.Effort || len(got.SlashCommands) != 1 {
		t.Errorf("settings lost: %+v", got)
	}
}

func TestKimiProviderOptions(t *testing.T) {
	opts, temp := kimiProviderOptions("off")
	ds := opts["kimi"].(*openaicompat.ProviderOptions)
	if ds.ExtraBody == nil || temp == nil || *temp != 0.0 {
		t.Errorf("off must disable thinking and pin temperature 0: %+v temp=%v", ds, temp)
	}
	thinking, _ := ds.ExtraBody["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Errorf("thinking extra_body wrong: %+v", ds.ExtraBody)
	}

	opts, temp = kimiProviderOptions("high")
	ds = opts["kimi"].(*openaicompat.ProviderOptions)
	if ds.ReasoningEffort == nil || string(*ds.ReasoningEffort) != "high" || temp != nil {
		t.Errorf("high mapping wrong: %+v temp=%v", ds, temp)
	}

	opts, temp = kimiProviderOptions("")
	ds = opts["kimi"].(*openaicompat.ProviderOptions)
	if ds.ReasoningEffort == nil || string(*ds.ReasoningEffort) != "high" || temp != nil {
		t.Errorf("default effort must be high thinking: %+v", ds)
	}
}

func TestKimiProvider_NoAPIKeyFailsFast(t *testing.T) {
	isolateHome(t)
	t.Setenv(moonshotEnvAPIKey, "")
	p := kimiAgentProvider()
	_, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), NewSessionID: "s1"})
	if err == nil {
		t.Fatal("StartSession without a key must fail")
	}
	if !strings.Contains(err.Error(), "model picker") || !strings.Contains(err.Error(), moonshotEnvAPIKey) {
		t.Errorf("error must point at both config and env: %v", err)
	}
}

func TestKimiProvider_SessionLifecycle(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("answer one", fantasy.Usage{InputTokens: 10}),
	}}
	swapKimiLM(t, lm)

	p := kimiAgentProvider()
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

	if err := p.Send(proc, "question", nil); err != nil {
		t.Fatalf("Send: %v", err)
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
	// Kimi models have pricing from the lookaside table — the
	// usageMsg carries costKnown and a dollar amount.
	if usage == nil || !usage.costKnown {
		t.Errorf("usageMsg must exist and be cost-known: %+v", usage)
	}
	// 10 input tokens at $0.95/1M = $0.0000095.
	if usage.costUSD < 0.0000094 || usage.costUSD > 0.0000096 {
		t.Errorf("usage cost = %v, want ~0.0000095", usage.costUSD)
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
	// Kimi always uses the fallback max output tokens.
	if want := kimiSpec.maxOutputTokens("kimi-k2.7-code"); calls[0].MaxOutputTokens == nil ||
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
	if sessions[0].preview != "question" {
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
	swapKimiLM(t, lm2)
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
		if m.Role == fantasy.MessageRoleUser && strings.Contains(messageText(m), "question") {
			sawPriorTurn = true
		}
	}
	if !sawPriorTurn {
		t.Error("resumed session must replay prior turns on the wire")
	}
}

func TestKimiProvider_ResumeUnknownSessionErrors(t *testing.T) {
	isolateHome(t)
	swapKimiLM(t, &fakeLM{})
	p := kimiAgentProvider()
	if _, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), SessionID: "missing"}); err == nil {
		t.Fatal("resuming an unknown session must fail")
	}
}

func TestKimiProvider_MaterializeRoundTrip(t *testing.T) {
	isolateHome(t)
	p := kimiAgentProvider()
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
func TestKimiStoreUsesHome(t *testing.T) {
	home := isolateHome(t)
	st := kimiStore()
	root, err := st.root()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(root, home) {
		t.Errorf("store root %q must live under isolated home %q", root, home)
	}
}

func TestKimiMaxOutputTokens(t *testing.T) {
	if got := kimiSpec.maxOutputTokens("kimi-k2.7-code"); got != kimiFallbackMaxOutputTokens {
		t.Errorf("kimi-k2.7-code budget = %d want %d", got, kimiFallbackMaxOutputTokens)
	}
	if got := kimiSpec.maxOutputTokens("kimi-k2.5"); got != kimiFallbackMaxOutputTokens {
		t.Errorf("kimi-k2.5 budget = %d want %d", got, kimiFallbackMaxOutputTokens)
	}
	if got := kimiSpec.maxOutputTokens("kimi-k2-thinking"); got != kimiFallbackMaxOutputTokens {
		t.Errorf("kimi-k2-thinking budget = %d want %d", got, kimiFallbackMaxOutputTokens)
	}
	if got := kimiSpec.maxOutputTokens("custom-model"); got != kimiFallbackMaxOutputTokens {
		t.Errorf("unknown model must use the fallback budget: %d", got)
	}
}

func TestKimiSupportsImages(t *testing.T) {
	// kimi-k2.7-code supports vision.
	if !kimiSpec.supportsImages("kimi-k2.7-code") {
		t.Error("kimi-k2.7-code must support images")
	}
	// kimi-k2.5 supports vision.
	if !kimiSpec.supportsImages("kimi-k2.5") {
		t.Error("kimi-k2.5 must support images")
	}
	// kimi-k2-thinking does not support vision.
	if kimiSpec.supportsImages("kimi-k2-thinking") {
		t.Error("kimi-k2-thinking must NOT support images")
	}
	// Unknown models fall back to true.
	if !kimiSpec.supportsImages("unknown-model") {
		t.Error("unknown model should default to image-capable")
	}
}

func TestKimiProvider_K2ThinkingRejectsImages(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("answer", fantasy.Usage{}),
	}}
	swapKimiLM(t, lm)

	p := kimiAgentProvider()
	args := ProviderSessionArgs{
		Cwd: t.TempDir(), NewSessionID: "ses-think", SkipAllPermissions: true,
		Model: "kimi-k2-thinking",
	}
	proc, ch, err := p.StartSession(args)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer func() { proc.kill(); drainProviderStream(ch) }()

	// kimi-k2-thinking must reject images.
	if err := p.Send(proc, "look", []pendingAttachment{{mime: "image/png"}}); err == nil ||
		!strings.Contains(err.Error(), "image") {
		t.Errorf("kimi-k2-thinking must reject images: %v", err)
	}
}

func TestModelContextLimit_Kimi(t *testing.T) {
	if got := modelContextLimit("kimi-k2.7-code"); got != kimiContextWindow {
		t.Errorf("kimi-k2.7-code limit = %d want %d", got, kimiContextWindow)
	}
	if got := modelContextLimit("kimi-k2.5"); got != kimiContextWindow {
		t.Errorf("kimi-k2.5 limit = %d want %d", got, kimiContextWindow)
	}
	if got := modelContextLimit("kimi-k2-thinking"); got != kimiContextWindow {
		t.Errorf("kimi-k2-thinking limit = %d want %d", got, kimiContextWindow)
	}
	// Guard: other providers still resolve correctly.
	if got := modelContextLimit("opus[1m]"); got != 1_000_000 {
		t.Errorf("claude 1m tier broken by kimi case: %d", got)
	}
	if got := modelContextLimit("sonnet"); got != 200_000 {
		t.Errorf("claude default tier broken: %d", got)
	}
	if got := modelContextLimit("deepseek-v4-pro"); got != deepseekContextWindow {
		t.Errorf("deepseek limit broken: %d", got)
	}
}
