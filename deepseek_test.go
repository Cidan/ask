package main

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
)

func swapDeepseekLM(t *testing.T, lm fantasy.LanguageModel) {
	t.Helper()
	prev := deepseekLanguageModel
	deepseekLanguageModel = func(apiProviderConfig, string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	t.Cleanup(func() { deepseekLanguageModel = prev })
}

func TestDeepseekProvider_Metadata(t *testing.T) {
	p := deepseekAgentProvider()
	if p.ID() != "deepseek" || p.DisplayName() != "DeepSeek" {
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
	if len(picker.Options) != 2 || picker.Options[0] != "deepseek-v4-pro" || !picker.AllowCustom {
		t.Errorf("model picker wrong: %+v", picker)
	}
	if efforts := p.EffortOptions(); len(efforts) != 3 || efforts[0] != "off" {
		t.Errorf("effort options wrong: %v", efforts)
	}
	if id := p.PreMintSessionID(ProviderSessionArgs{}); id == "" {
		t.Error("deepseek must pre-mint session ids")
	}

	// Registered and addressable for workflow steps.
	got, ok := providerByIDStrict("deepseek")
	if !ok || got.ID() != "deepseek" {
		t.Fatal("deepseek must be in the provider registry")
	}
	if err := validateProviderID("deepseek"); err != nil {
		t.Errorf("workflow provider validation must accept deepseek: %v", err)
	}
}

func TestDeepseekProvider_SettingsRoundTrip(t *testing.T) {
	isolateHome(t)
	p := deepseekAgentProvider()
	if s := p.LoadSettings(); s.Model != "" || s.Effort != "" {
		t.Errorf("fresh settings must be zero: %+v", s)
	}
	want := ProviderSettings{Model: "deepseek-v4-flash", Effort: "max",
		SlashCommands: []providerSlashEntry{{Name: "x", Description: "y"}}}
	if err := p.SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got := p.LoadSettings()
	if got.Model != want.Model || got.Effort != want.Effort || len(got.SlashCommands) != 1 {
		t.Errorf("settings lost: %+v", got)
	}
}

func TestDeepseekProviderOptions(t *testing.T) {
	opts, temp := deepseekProviderOptions("off")
	ds := opts["deepseek"].(*openaicompat.ProviderOptions)
	if ds.ExtraBody == nil || temp == nil || *temp != 0.0 {
		t.Errorf("off must disable thinking and pin temperature 0: %+v temp=%v", ds, temp)
	}
	thinking, _ := ds.ExtraBody["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Errorf("thinking extra_body wrong: %+v", ds.ExtraBody)
	}

	opts, temp = deepseekProviderOptions("high")
	ds = opts["deepseek"].(*openaicompat.ProviderOptions)
	if ds.ReasoningEffort == nil || string(*ds.ReasoningEffort) != "high" || temp != nil {
		t.Errorf("high mapping wrong: %+v temp=%v", ds, temp)
	}

	opts, _ = deepseekProviderOptions("max")
	ds = opts["deepseek"].(*openaicompat.ProviderOptions)
	if ds.ReasoningEffort == nil || string(*ds.ReasoningEffort) != "xhigh" {
		t.Errorf("max must map to xhigh: %+v", ds)
	}

	opts, temp = deepseekProviderOptions("")
	ds = opts["deepseek"].(*openaicompat.ProviderOptions)
	if ds.ReasoningEffort == nil || string(*ds.ReasoningEffort) != "high" || temp != nil {
		t.Errorf("default effort must be high thinking: %+v", ds)
	}
}

func TestDeepseekProvider_NoAPIKeyFailsFast(t *testing.T) {
	isolateHome(t)
	t.Setenv(deepseekEnvAPIKey, "")
	p := deepseekAgentProvider()
	_, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), NewSessionID: "s1"})
	if err == nil {
		t.Fatal("StartSession without a key must fail")
	}
	if !strings.Contains(err.Error(), "model picker") || !strings.Contains(err.Error(), deepseekEnvAPIKey) {
		t.Errorf("error must point at both config and env: %v", err)
	}
}

func TestDeepseekProvider_SessionLifecycle(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("answer one", fantasy.Usage{InputTokens: 10}),
	}}
	swapDeepseekLM(t, lm)

	p := deepseekAgentProvider()
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

	// Attachments are rejected with a useful message.
	if err := p.Send(proc, "look", []pendingAttachment{{mime: "image/png"}}); err == nil ||
		!strings.Contains(err.Error(), "image") {
		t.Errorf("attachment send must error helpfully: %v", err)
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
	// A real spec + catalog default model prices every step onto the
	// usageMsg (10 input tokens at deepseek-v4-pro's 0.435/1M rate).
	if usage == nil || !usage.costKnown || usage.costUSD != 10*0.435/1e6 {
		t.Errorf("usageMsg cost wrong: %+v", usage)
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
	if want := deepseekSpec.maxOutputTokens(deepseekDefaultModel); calls[0].MaxOutputTokens == nil ||
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
	swapDeepseekLM(t, lm2)
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

func TestDeepseekProvider_ResumeUnknownSessionErrors(t *testing.T) {
	isolateHome(t)
	swapDeepseekLM(t, &fakeLM{})
	p := deepseekAgentProvider()
	if _, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), SessionID: "missing"}); err == nil {
		t.Fatal("resuming an unknown session must fail")
	}
}

func TestDeepseekProvider_MaterializeRoundTrip(t *testing.T) {
	isolateHome(t)
	p := deepseekAgentProvider()
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
func TestDeepseekStoreUsesHome(t *testing.T) {
	home := isolateHome(t)
	st := deepseekStore()
	root, err := st.root()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(root, home) {
		t.Errorf("store root %q must live under isolated home %q", root, home)
	}
}

func TestDeepseekMaxOutputTokens(t *testing.T) {
	if got := deepseekSpec.maxOutputTokens("deepseek-v4-pro"); got != 384_000 {
		t.Errorf("deepseek-v4-pro budget = %d want 384k", got)
	}
	if got := deepseekSpec.maxOutputTokens("custom-model"); got != deepseekFallbackMaxOutputTokens {
		t.Errorf("unknown model must use the fallback budget: %d", got)
	}
}

func TestDeepseekNoNativeWebSearch(t *testing.T) {
	// openaicompat backends have no first-party search; they fall back to
	// the Brave-backed core web_search tool, so nativeWebSearch stays nil.
	if deepseekSpec.nativeWebSearch != nil {
		t.Error("deepseek must not declare a native web search")
	}
	if kimiSpec.nativeWebSearch != nil {
		t.Error("kimi must not declare a native web search")
	}
}

func TestModelContextLimit_DeepSeek(t *testing.T) {
	if got := modelContextLimit("deepseek-v4-pro"); got != deepseekContextWindow {
		t.Errorf("deepseek-v4-pro limit = %d want %d", got, deepseekContextWindow)
	}
	if got := modelContextLimit("deepseek-v4-flash"); got != deepseekContextWindow {
		t.Errorf("deepseek-v4-flash limit = %d want %d", got, deepseekContextWindow)
	}
	if got := modelContextLimit("opus[1m]"); got != 1_000_000 {
		t.Errorf("claude 1m tier broken by deepseek case: %d", got)
	}
	if got := modelContextLimit("sonnet"); got != 200_000 {
		t.Errorf("claude default tier broken: %d", got)
	}
}
