package main

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
)

func swapAnthropicLM(t *testing.T, lm fantasy.LanguageModel) {
	t.Helper()
	prev := anthropicLanguageModel
	anthropicLanguageModel = func(apiProviderConfig, string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	t.Cleanup(func() { anthropicLanguageModel = prev })
}

func TestAnthropicProvider_Metadata(t *testing.T) {
	p := anthropicAgentProvider()
	if p.ID() != "anthropic" || p.DisplayName() != "Anthropic" {
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
	if len(picker.Options) == 0 || !picker.AllowCustom {
		t.Errorf("model picker must list catalog models with a custom row: %+v", picker)
	}
	var hasDefault bool
	for _, opt := range picker.Options {
		if opt == anthropicDefaultModel {
			hasDefault = true
		}
	}
	if !hasDefault {
		t.Errorf("picker must include the default model %q: %v", anthropicDefaultModel, picker.Options)
	}
	if efforts := p.EffortOptions(); len(efforts) != 5 || efforts[0] != "low" || efforts[4] != "max" {
		t.Errorf("effort options wrong: %v", efforts)
	}
	if id := p.PreMintSessionID(ProviderSessionArgs{}); id == "" {
		t.Error("anthropic must pre-mint session ids")
	}

	got, ok := providerByIDStrict("anthropic")
	if !ok || got.ID() != "anthropic" {
		t.Fatal("anthropic must be in the provider registry")
	}
	if err := validateProviderID("anthropic"); err != nil {
		t.Errorf("workflow provider validation must accept anthropic: %v", err)
	}
}

func TestAnthropicProviderOptions(t *testing.T) {
	opts, temp := anthropicProviderOptions("claude-fable-5", "max")
	ao := opts[anthropic.Name].(*anthropic.ProviderOptions)
	if ao.Effort == nil || *ao.Effort != anthropic.EffortMax || temp != nil {
		t.Errorf("max mapping wrong: %+v temp=%v", ao, temp)
	}

	opts, _ = anthropicProviderOptions("claude-fable-5", "")
	ao = opts[anthropic.Name].(*anthropic.ProviderOptions)
	if ao.Effort != nil {
		t.Errorf("empty effort must leave the API default: %+v", ao)
	}

	// claude-sonnet-4-6 publishes no xhigh level — the pick clamps to
	// high instead of erroring mid-session.
	if m, ok := catalogModel("anthropic", "claude-sonnet-4-6"); ok {
		hasXHigh := false
		for _, l := range m.ReasoningLevels {
			if l == "xhigh" {
				hasXHigh = true
			}
		}
		if !hasXHigh {
			opts, _ = anthropicProviderOptions("claude-sonnet-4-6", "xhigh")
			ao = opts[anthropic.Name].(*anthropic.ProviderOptions)
			if ao.Effort == nil || *ao.Effort != anthropic.EffortHigh {
				t.Errorf("xhigh on sonnet-4-6 must clamp to high: %+v", ao)
			}
		}
	}
}

func TestAnthropicPrepareStep_CacheBreakpoints(t *testing.T) {
	system := fantasy.NewSystemMessage("sys")
	// Simulate a stale marker on an old message: it must be stripped so
	// breakpoints never accumulate past the API's 4-block limit.
	stale := fantasy.NewUserMessage("old turn")
	stale.ProviderOptions = anthropicCacheOptions()
	input := []fantasy.Message{
		system,
		stale,
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "old answer"}}},
		fantasy.NewUserMessage("new turn"),
	}

	_, prepared, err := anthropicPrepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: input})
	if err != nil {
		t.Fatal(err)
	}
	msgs := prepared.Messages
	if len(msgs) != 4 {
		t.Fatalf("message count changed: %d", len(msgs))
	}
	marked := func(m fantasy.Message) bool {
		if m.ProviderOptions == nil {
			return false
		}
		_, ok := m.ProviderOptions[anthropic.Name].(*anthropic.ProviderCacheControlOptions)
		return ok
	}
	if !marked(msgs[0]) {
		t.Error("system message must carry a cache breakpoint")
	}
	if marked(msgs[1]) {
		t.Error("stale marker on an old message must be stripped")
	}
	if !marked(msgs[2]) || !marked(msgs[3]) {
		t.Error("the last two messages must carry cache breakpoints")
	}
	// Mutation isolation: the caller's slice (which is what gets
	// persisted) must stay untouched.
	if input[0].ProviderOptions != nil {
		t.Error("prepare step must not mutate the caller's messages")
	}
	if input[3].ProviderOptions != nil {
		t.Error("prepare step must not mutate the caller's messages")
	}
}

func TestAnthropicDecorateTools_MarksLastTool(t *testing.T) {
	mk := func(name string) fantasy.AgentTool {
		return fantasy.NewAgentTool(name, "t",
			func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("ok"), nil
			})
	}
	tools := []fantasy.AgentTool{mk("a"), mk("b"), mk("c")}
	anthropicDecorateTools(tools)
	if tools[0].ProviderOptions() != nil || tools[1].ProviderOptions() != nil {
		t.Error("only the last tool may carry cache options")
	}
	last := tools[2].ProviderOptions()
	if last == nil {
		t.Fatal("last tool must carry cache options")
	}
	if _, ok := last[anthropic.Name].(*anthropic.ProviderCacheControlOptions); !ok {
		t.Errorf("last tool options wrong type: %+v", last)
	}
	anthropicDecorateTools(nil) // must not panic
}

func TestAnthropicProvider_NoAPIKeyFailsFast(t *testing.T) {
	isolateHome(t)
	t.Setenv(anthropicEnvAPIKey, "")
	p := anthropicAgentProvider()
	_, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), NewSessionID: "s1"})
	if err == nil {
		t.Fatal("StartSession without a key must fail")
	}
	if !strings.Contains(err.Error(), "/config") || !strings.Contains(err.Error(), anthropicEnvAPIKey) {
		t.Errorf("error must point at both config and env: %v", err)
	}
}

func TestAnthropicProvider_SessionLifecycleWithImages(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("looked at it", fantasy.Usage{InputTokens: 10}),
	}}
	swapAnthropicLM(t, lm)

	p := anthropicAgentProvider()
	cwd := t.TempDir()
	proc, ch, err := p.StartSession(ProviderSessionArgs{
		Cwd: cwd, TabID: 2, NewSessionID: "ses-anthropic", SkipAllPermissions: true,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if proc.cmd != nil {
		t.Error("in-process provider must not carry an exec.Cmd")
	}

	// Images are accepted (claude models are vision-capable) and reach
	// the wire as a file part on the user message.
	att := []pendingAttachment{{data: []byte("png-bytes"), mime: "image/png"}}
	if err := p.Send(proc, "what is in this image?", att); err != nil {
		t.Fatalf("Send with attachment: %v", err)
	}
	msgs := readSessionMsgs(t, ch, isTurnComplete)
	var done providerDoneMsg
	for _, m := range msgs {
		if d, ok := m.(providerDoneMsg); ok {
			done = d
		}
	}
	if done.res.SessionID != "ses-anthropic" || done.res.Result != "looked at it" {
		t.Errorf("done msg wrong: %+v", done.res)
	}

	wire := lm.streamCalls()[0].Prompt
	var sawFile bool
	for _, m := range wire {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if fp, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok && fp.MediaType == "image/png" {
				sawFile = true
			}
		}
	}
	if !sawFile {
		t.Error("attachment must reach the wire as an image file part")
	}

	// The prepare-step ran: the system message carries a breakpoint.
	if wire[0].Role != fantasy.MessageRoleSystem {
		t.Fatal("first wire message must be the system prompt")
	}
	if wire[0].ProviderOptions == nil {
		t.Error("cache breakpoints must be placed on the wire messages")
	}

	// The persisted transcript stays clean of cache markers and keeps
	// the image part for resume.
	file, err := (&agentSessionStore{provider: "anthropic"}).load("ses-anthropic")
	if err != nil {
		t.Fatalf("stored session: %v", err)
	}
	if len(file.Messages) == 0 {
		t.Fatal("transcript must persist")
	}
	for _, m := range file.Messages {
		if m.ProviderOptions != nil {
			t.Error("persisted messages must not carry cache markers")
		}
	}

	sessions, err := p.ListSessions(cwd)
	if err != nil || len(sessions) != 1 || sessions[0].id != "ses-anthropic" {
		t.Fatalf("ListSessions: %v %+v", err, sessions)
	}

	proc.kill()
	drainProviderStream(ch)
}

func TestAnthropicContextWindow(t *testing.T) {
	if got := anthropicSpec.contextWindow("claude-fable-5"); got != 1_000_000 {
		t.Errorf("claude-fable-5 window = %d want 1M", got)
	}
	if got := anthropicSpec.contextWindow("some-custom-model"); got != anthropicFallbackContextWindow {
		t.Errorf("unknown model must use the conservative fallback: %d", got)
	}
	if got := modelContextLimit("claude-fable-5"); got != 1_000_000 {
		t.Errorf("modelContextLimit must resolve catalog models: %d", got)
	}
}

func TestAnthropicSupportsImages(t *testing.T) {
	if !anthropicSpec.supportsImages("claude-fable-5") {
		t.Error("claude-fable-5 must accept images")
	}
	if !anthropicSpec.supportsImages("future-claude") {
		t.Error("unknown claude ids default to image-capable")
	}
}
