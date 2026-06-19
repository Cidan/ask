package main

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/google"
)

func swapGoogleAILM(t *testing.T, lm fantasy.LanguageModel) {
	t.Helper()
	prev := googleaiLanguageModel
	googleaiLanguageModel = func(apiProviderConfig, string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	t.Cleanup(func() { googleaiLanguageModel = prev })
}

func TestGoogleAIProvider_Metadata(t *testing.T) {
	p := googleaiAgentProvider()
	if p.ID() != "googleai" || p.DisplayName() != "Google AI Studio" {
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
	if len(picker.Options) == 0 || picker.Options[0] != googleaiDefaultModel || !picker.AllowCustom {
		t.Errorf("model picker wrong: %+v", picker)
	}
	if efforts := p.EffortOptions(); len(efforts) != 3 || efforts[0] != "low" || efforts[2] != "high" {
		t.Errorf("effort options wrong: %v", efforts)
	}
	if id := p.PreMintSessionID(ProviderSessionArgs{}); id == "" {
		t.Error("googleai must pre-mint session ids")
	}

	got, ok := providerByIDStrict("googleai")
	if !ok || got.ID() != "googleai" {
		t.Fatal("googleai must be in the provider registry")
	}
	if err := validateProviderID("googleai"); err != nil {
		t.Errorf("workflow provider validation must accept googleai: %v", err)
	}
}

func TestGoogleAIProvider_SettingsRoundTrip(t *testing.T) {
	isolateHome(t)
	p := googleaiAgentProvider()
	if s := p.LoadSettings(); s.Model != "" || s.Effort != "" {
		t.Errorf("fresh settings must be zero: %+v", s)
	}
	want := ProviderSettings{Model: "gemini-3.1-pro-preview", Effort: "low",
		SlashCommands: []providerSlashEntry{{Name: "x", Description: "y"}}}
	if err := p.SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got := p.LoadSettings()
	if got.Model != want.Model || got.Effort != want.Effort || len(got.SlashCommands) != 1 {
		t.Errorf("settings lost: %+v", got)
	}
}

func TestGoogleAIProviderOptions(t *testing.T) {
	opts, temp := googleaiProviderOptions(googleaiDefaultModel, "low")
	if temp != nil {
		t.Errorf("googleai must not set temperature: %v", temp)
	}
	goo, ok := opts["google"].(*google.ProviderOptions)
	if !ok || goo.ThinkingConfig == nil || goo.ThinkingConfig.ThinkingLevel == nil {
		t.Fatalf("low must produce ThinkingLevel=LOW: %+v", goo)
	}
	if *goo.ThinkingConfig.ThinkingLevel != "LOW" {
		t.Errorf("level=%q want LOW", *goo.ThinkingConfig.ThinkingLevel)
	}

	// "low" clamps to "low" on gemini-3.1-pro.
	opts, _ = googleaiProviderOptions(googleaiDefaultModel, "low")
	goo = opts["google"].(*google.ProviderOptions)
	if *goo.ThinkingConfig.ThinkingLevel != "LOW" {
		t.Errorf("low should clamp to LOW on 3.1 Pro, got %q", *goo.ThinkingConfig.ThinkingLevel)
	}

	// "high" passes through.
	opts, _ = googleaiProviderOptions(googleaiDefaultModel, "high")
	goo = opts["google"].(*google.ProviderOptions)
	if *goo.ThinkingConfig.ThinkingLevel != "HIGH" {
		t.Errorf("high should pass through, got %q", *goo.ThinkingConfig.ThinkingLevel)
	}

	// Empty effort is a no-op (no provider options returned).
	opts, _ = googleaiProviderOptions(googleaiDefaultModel, "")
	if opts != nil {
		t.Errorf("empty effort must return nil options, got %+v", opts)
	}
}

func TestGoogleAIProvider_NoAPIKeyFailsFast(t *testing.T) {
	isolateHome(t)
	t.Setenv(googleaiEnvAPIKey, "")
	p := googleaiAgentProvider()
	_, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), NewSessionID: "s1"})
	if err == nil {
		t.Fatal("StartSession without a key must fail")
	}
	if !strings.Contains(err.Error(), "model picker") || !strings.Contains(err.Error(), googleaiEnvAPIKey) {
		t.Errorf("error must point at both config and env: %v", err)
	}
}

func TestGoogleAIProvider_SessionLifecycle(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("answer one", fantasy.Usage{InputTokens: 10}),
	}}
	swapGoogleAILM(t, lm)

	p := googleaiAgentProvider()
	cwd := t.TempDir()
	args := ProviderSessionArgs{Cwd: cwd, TabID: 4, NewSessionID: "ses-gai", SkipAllPermissions: true}
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
	if done.res.SessionID != "ses-gai" || done.res.Result != "answer one" {
		t.Errorf("done msg wrong: %+v", done.res)
	}
	// Gemini models have catalog pricing → costKnown + dollar amount.
	if usage == nil || !usage.costKnown {
		t.Errorf("usageMsg must exist and be cost-known: %+v", usage)
	}

	calls := lm.streamCalls()
	if len(calls) == 0 || calls[0].Prompt[0].Role != fantasy.MessageRoleSystem {
		t.Fatal("first wire message must be the system prompt")
	}
	if sys := messageText(calls[0].Prompt[0]); !strings.Contains(sys, "<critical_rules>") ||
		!strings.Contains(sys, "super human speeds") {
		t.Error("system prompt must include coder rules and ask steering")
	}
	if want := googleaiSpec.maxOutputTokens(googleaiDefaultModel); calls[0].MaxOutputTokens == nil ||
		*calls[0].MaxOutputTokens != want {
		t.Errorf("wire MaxOutputTokens = %v want %d", calls[0].MaxOutputTokens, want)
	}

	if handled, _ := p.Interrupt(proc); handled {
		t.Error("idle interrupt must report handled=false")
	}

	sessions, err := p.ListSessions(cwd)
	if err != nil || len(sessions) != 1 || sessions[0].id != "ses-gai" {
		t.Fatalf("ListSessions: %v %+v", err, sessions)
	}
	if sessions[0].preview != "question" {
		t.Errorf("preview %q", sessions[0].preview)
	}
	entries, err := p.LoadHistory("ses-gai", HistoryOpts{ToolOutput: toolOutputFull})
	if err != nil || len(entries) != 2 {
		t.Fatalf("LoadHistory: %v %+v", err, entries)
	}

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

	lm2 := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("answer two", fantasy.Usage{}),
	}}
	swapGoogleAILM(t, lm2)
	proc2, ch2, err := p.StartSession(ProviderSessionArgs{
		Cwd: cwd, TabID: 4, SessionID: "ses-gai", SkipAllPermissions: true,
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

func TestGoogleAIProvider_ResumeUnknownSessionErrors(t *testing.T) {
	isolateHome(t)
	swapGoogleAILM(t, &fakeLM{})
	p := googleaiAgentProvider()
	if _, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), SessionID: "missing"}); err == nil {
		t.Fatal("resuming an unknown session must fail")
	}
}

func TestGoogleAIProvider_MaterializeRoundTrip(t *testing.T) {
	isolateHome(t)
	p := googleaiAgentProvider()
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

func TestGoogleAIStoreUsesHome(t *testing.T) {
	home := isolateHome(t)
	st := googleaiStore()
	root, err := st.root()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(root, home) {
		t.Errorf("store root %q must live under isolated home %q", root, home)
	}
}

func TestGoogleAIMaxOutputTokens(t *testing.T) {
	// The default model is in the catwalk catalog; the published
	// default_max_tokens (64000) wins over the fallback.
	if got := googleaiSpec.maxOutputTokens(googleaiDefaultModel); got == googleaiFallbackMaxOutputTokens {
		t.Errorf("default model should use the catwalk value, not the fallback: got %d", got)
	}
	// Unknown models fall back to the conservative default.
	if got := googleaiSpec.maxOutputTokens("custom-unknown"); got != googleaiFallbackMaxOutputTokens {
		t.Errorf("unknown model must use the fallback budget: %d", got)
	}
}

func TestGoogleAISupportsImages(t *testing.T) {
	// gemini-3.1-pro-preview-customtools supports images.
	if !googleaiSpec.supportsImages(googleaiDefaultModel) {
		t.Error("default model must support images")
	}
	// Unknown models fall back to true.
	if !googleaiSpec.supportsImages("unknown-model") {
		t.Error("unknown model should default to image-capable")
	}
}

func TestModelContextLimit_GoogleAI(t *testing.T) {
	if got := modelContextLimit(googleaiDefaultModel); got != googleaiContextWindow {
		t.Errorf("googleai default limit = %d want %d", got, googleaiContextWindow)
	}
	if got := modelContextLimit("gemini-3-flash-preview"); got != googleaiContextWindow {
		t.Errorf("gemini-3-flash-preview limit = %d want %d", got, googleaiContextWindow)
	}
}
