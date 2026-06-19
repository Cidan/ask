package main

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
)

func swapOpenAILM(t *testing.T, lm fantasy.LanguageModel) {
	t.Helper()
	prev := openaiLanguageModel
	openaiLanguageModel = func(apiProviderConfig, string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	t.Cleanup(func() { openaiLanguageModel = prev })
}

func TestOpenAIProvider_Metadata(t *testing.T) {
	p := openaiAgentProvider()
	if p.ID() != "openai" || p.DisplayName() != "OpenAI" {
		t.Errorf("identity wrong: %q %q", p.ID(), p.DisplayName())
	}
	caps := p.Capabilities()
	if !caps.Resume || !caps.ModelPicker || !caps.EffortPicker {
		t.Errorf("capabilities wrong: %+v", caps)
	}
	picker := p.ModelPicker()
	if len(picker.Options) == 0 || !picker.AllowCustom {
		t.Errorf("model picker must list catalog models with a custom row: %+v", picker)
	}
	var hasDefault bool
	for _, opt := range picker.Options {
		if opt == openaiDefaultModel {
			hasDefault = true
		}
	}
	if !hasDefault {
		t.Errorf("picker must include the default model %q", openaiDefaultModel)
	}
	if efforts := p.EffortOptions(); len(efforts) != 3 || efforts[0] != "low" || efforts[2] != "high" {
		t.Errorf("effort options wrong: %v", efforts)
	}

	got, ok := providerByIDStrict("openai")
	if !ok || got.ID() != "openai" {
		t.Fatal("openai must be in the provider registry")
	}
	if err := validateProviderID("openai"); err != nil {
		t.Errorf("workflow provider validation must accept openai: %v", err)
	}
}

func TestOpenAIUseResponsesAPI(t *testing.T) {
	cases := map[string]bool{
		"gpt-5.5":           true,
		"gpt-5.4-codex":     true,
		"gpt-5.1-codex-max": true,
		"gpt-5.9-future":    true, // prefix predicate must outlive fantasy's exact-id list
		"o3":                true,
		"codex-mini-latest": true,
		"gpt-oss-120b":      true,
		"gpt-4o":            false,
		"gpt-4.1":           false,
	}
	for id, want := range cases {
		if got := openaiUseResponsesAPI(id); got != want {
			t.Errorf("openaiUseResponsesAPI(%q) = %v want %v", id, got, want)
		}
	}
}

func TestOpenAIProviderOptions(t *testing.T) {
	opts, temp := openaiProviderOptions("gpt-5.5", "high")
	oo := opts[openai.Name].(*openai.ResponsesProviderOptions)
	if temp != nil {
		t.Errorf("openai options must not pin temperature: %v", temp)
	}
	if oo.ReasoningEffort == nil || *oo.ReasoningEffort != openai.ReasoningEffortXHigh {
		t.Errorf("high mapping wrong: %+v", oo)
	}
	if oo.ReasoningSummary == nil || *oo.ReasoningSummary != "auto" {
		t.Errorf("reasoning summaries must be on: %+v", oo)
	}
	var hasEncrypted bool
	for _, inc := range oo.Include {
		if inc == openai.IncludeReasoningEncryptedContent {
			hasEncrypted = true
		}
	}
	if !hasEncrypted {
		t.Error("encrypted reasoning content must be requested — stateless resume depends on it")
	}

	opts, _ = openaiProviderOptions("gpt-5.5", "")
	oo = opts[openai.Name].(*openai.ResponsesProviderOptions)
	if oo.ReasoningEffort != nil {
		t.Errorf("empty effort must leave the API default: %+v", oo)
	}
}

func TestOpenAIProvider_NoAPIKeyFailsFast(t *testing.T) {
	isolateHome(t)
	t.Setenv(openaiEnvAPIKey, "")
	p := openaiAgentProvider()
	_, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), NewSessionID: "s1"})
	if err == nil {
		t.Fatal("StartSession without a key must fail")
	}
	if !strings.Contains(err.Error(), "model picker") || !strings.Contains(err.Error(), openaiEnvAPIKey) {
		t.Errorf("error must point at both config and env: %v", err)
	}
}

func TestOpenAIProvider_SessionLifecycle(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("done", fantasy.Usage{InputTokens: 5}),
	}}
	swapOpenAILM(t, lm)

	p := openaiAgentProvider()
	cwd := t.TempDir()
	proc, ch, err := p.StartSession(ProviderSessionArgs{
		Cwd: cwd, TabID: 3, NewSessionID: "ses-openai", SkipAllPermissions: true,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	// Images are accepted on the gpt-5 lineup.
	if err := p.Send(proc, "look", []pendingAttachment{{data: []byte("x"), mime: "image/png"}}); err != nil {
		t.Fatalf("Send with attachment must be accepted: %v", err)
	}
	msgs := readSessionMsgs(t, ch, isTurnComplete)
	var done providerDoneMsg
	for _, m := range msgs {
		if d, ok := m.(providerDoneMsg); ok {
			done = d
		}
	}
	if done.res.SessionID != "ses-openai" || done.res.Result != "done" {
		t.Errorf("done msg wrong: %+v", done.res)
	}

	sessions, err := p.ListSessions(cwd)
	if err != nil || len(sessions) != 1 || sessions[0].id != "ses-openai" {
		t.Fatalf("ListSessions: %v %+v", err, sessions)
	}

	proc.kill()
	drainProviderStream(ch)
}

func TestOpenAIContextWindow(t *testing.T) {
	if got := openaiSpec.contextWindow("gpt-5"); got != 400_000 {
		t.Errorf("gpt-5 window = %d want 400k", got)
	}
	if got := openaiSpec.contextWindow("custom-model"); got != openaiFallbackContextWindow {
		t.Errorf("unknown model must use the conservative fallback: %d", got)
	}
}

func TestOpenAIMaxOutputTokens(t *testing.T) {
	if got := openaiSpec.maxOutputTokens("gpt-5.5"); got != 128_000 {
		t.Errorf("gpt-5.5 budget = %d want 128k", got)
	}
	if got := openaiSpec.maxOutputTokens("custom-model"); got != openaiFallbackMaxOutputTokens {
		t.Errorf("unknown model must use the fallback budget: %d", got)
	}
}

func TestOpenAINativeWebSearch(t *testing.T) {
	if openaiSpec.nativeWebSearch == nil {
		t.Fatal("openai must provide a native web_search tool")
	}
	tool := openaiSpec.nativeWebSearch("gpt-5.5")
	if tool == nil {
		t.Fatal("nativeWebSearch returned nil")
	}
	pdt, ok := tool.(fantasy.ProviderDefinedTool)
	if !ok {
		t.Fatalf("expected ProviderDefinedTool, got %T", tool)
	}
	if pdt.Name != "web_search" {
		t.Errorf("provider tool name = %q, want web_search", pdt.Name)
	}
}
