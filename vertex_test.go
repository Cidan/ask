package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/google"
)

func swapVertexLM(t *testing.T, lm fantasy.LanguageModel) {
	t.Helper()
	prev := vertexLanguageModel
	vertexLanguageModel = func(vertexConfig, string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	t.Cleanup(func() { vertexLanguageModel = prev })
}

func TestVertexProvider_Metadata(t *testing.T) {
	p := vertexAgentProvider()
	if p.ID() != "vertex" || p.DisplayName() != "Vertex AI" {
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
	if len(picker.Options) == 0 || picker.Options[0] != vertexDefaultModel || !picker.AllowCustom {
		t.Errorf("model picker wrong: %+v", picker)
	}
	// The Vertex catwalk lists Claude models alongside Gemini; the
	// spec filters them out so the picker shows only the Gemini line.
	for _, id := range picker.Options {
		low := strings.ToLower(id)
		if strings.Contains(low, "claude") || strings.Contains(low, "anthropic") {
			t.Errorf("picker must filter Claude / anthropic ids, got %q", id)
		}
	}
	if efforts := p.EffortOptions(); len(efforts) != 3 || efforts[0] != "low" || efforts[2] != "high" {
		t.Errorf("effort options wrong: %v", efforts)
	}
	if id := p.PreMintSessionID(ProviderSessionArgs{}); id == "" {
		t.Error("vertex must pre-mint session ids")
	}

	got, ok := providerByIDStrict("vertex")
	if !ok || got.ID() != "vertex" {
		t.Fatal("vertex must be in the provider registry")
	}
	if err := validateProviderID("vertex"); err != nil {
		t.Errorf("workflow provider validation must accept vertex: %v", err)
	}

	// Vertex is NOT in providerKeySpecs: its auth is configured via
	// the /config → Vertex AI submenu, not via the model picker's
	// inline API-key prompt. The picker therefore never gates on a
	// key for vertex.
	if _, ok := providerKeySpecByID("vertex"); ok {
		t.Error("vertex must NOT be in providerKeySpecs (auth via submenu)")
	}
}

func TestVertexProvider_SettingsRoundTrip(t *testing.T) {
	isolateHome(t)
	p := vertexAgentProvider()
	if s := p.LoadSettings(); s.Model != "" || s.Effort != "" {
		t.Errorf("fresh settings must be zero: %+v", s)
	}
	want := ProviderSettings{Model: "gemini-3-flash-preview", Effort: "high",
		SlashCommands: []providerSlashEntry{{Name: "x", Description: "y"}}}
	if err := p.SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got := p.LoadSettings()
	if got.Model != want.Model || got.Effort != want.Effort || len(got.SlashCommands) != 1 {
		t.Errorf("settings lost: %+v", got)
	}
}

func TestVertexProviderOptions(t *testing.T) {
	opts, temp := vertexProviderOptions(vertexDefaultModel, "low")
	if temp != nil {
		t.Errorf("vertex must not set temperature: %v", temp)
	}
	goo, ok := opts["google"].(*google.ProviderOptions)
	if !ok || goo.ThinkingConfig == nil || goo.ThinkingConfig.ThinkingLevel == nil {
		t.Fatalf("low must produce ThinkingLevel=LOW: %+v", goo)
	}
	if *goo.ThinkingConfig.ThinkingLevel != "LOW" {
		t.Errorf("level=%q want LOW", *goo.ThinkingConfig.ThinkingLevel)
	}

	// "low" clamps to "low" on gemini-3.1-pro.
	opts, _ = vertexProviderOptions(vertexDefaultModel, "low")
	goo = opts["google"].(*google.ProviderOptions)
	if *goo.ThinkingConfig.ThinkingLevel != "LOW" {
		t.Errorf("low should clamp to LOW on 3.1 Pro, got %q", *goo.ThinkingConfig.ThinkingLevel)
	}

	// "high" passes through.
	opts, _ = vertexProviderOptions(vertexDefaultModel, "high")
	goo = opts["google"].(*google.ProviderOptions)
	if *goo.ThinkingConfig.ThinkingLevel != "HIGH" {
		t.Errorf("high should pass through, got %q", *goo.ThinkingConfig.ThinkingLevel)
	}

	// Empty effort is a no-op.
	opts, _ = vertexProviderOptions(vertexDefaultModel, "")
	if opts != nil {
		t.Errorf("empty effort must return nil options, got %+v", opts)
	}
}

func TestVertexResolveLocation(t *testing.T) {
	// Configured location wins.
	if got := vertexResolveLocation(vertexConfig{Location: "us-central1"}); got != "us-central1" {
		t.Errorf("configured location = %q want us-central1", got)
	}
	// Empty → global default.
	if got := vertexResolveLocation(vertexConfig{}); got != vertexDefaultLocation {
		t.Errorf("default location = %q want %q", got, vertexDefaultLocation)
	}
}

func TestVertexResolveProject(t *testing.T) {
	// Configured project wins.
	t.Setenv(vertexEnvCloudProject, "env-proj")
	if got := vertexResolveProject(vertexConfig{Project: "cfg-proj"}); got != "cfg-proj" {
		t.Errorf("configured project = %q want cfg-proj", got)
	}
	// Empty config → env fallback.
	if got := vertexResolveProject(vertexConfig{}); got != "env-proj" {
		t.Errorf("env fallback = %q want env-proj", got)
	}
	// Both empty → empty (session start will fail-fast with a pointed error).
	t.Setenv(vertexEnvCloudProject, "")
	if got := vertexResolveProject(vertexConfig{}); got != "" {
		t.Errorf("both empty = %q want \"\"", got)
	}
}

func TestVertexLanguageModel_MissingProjectErrors(t *testing.T) {
	isolateHome(t)
	t.Setenv(vertexEnvCloudProject, "")
	t.Setenv(vertexEnvApplicationCredentials, "")
	p := vertexAgentProvider()
	_, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), NewSessionID: "s1"})
	if err == nil {
		t.Fatal("StartSession without a project must fail")
	}
	if !strings.Contains(err.Error(), "vertex") || !strings.Contains(err.Error(), "project is required") {
		t.Errorf("error must point at vertex + project: %v", err)
	}
	if !strings.Contains(err.Error(), vertexEnvCloudProject) {
		t.Errorf("error must mention the env fallback: %v", err)
	}
}

func TestVertexLanguageModel_ServiceAccountKeyNotFound(t *testing.T) {
	isolateHome(t)
	// Don't swap vertexLanguageModel — we want the real SA-key
	// resolution to fire. The validation runs BEFORE google.New,
	// so we never reach a real network call.
	missing := "/nonexistent/path/that/does/not/exist.json"
	calls := []string{}
	prevApply := vertexApplyEnv
	vertexApplyEnv = func(p string) { calls = append(calls, p) }
	t.Cleanup(func() { vertexApplyEnv = prevApply })

	_, err := vertexPrepareCredentials(vertexConfig{ServiceAccountKey: missing})
	if err == nil {
		t.Fatal("missing SA key path must fail")
	}
	if !strings.Contains(err.Error(), "vertex") || !strings.Contains(err.Error(), missing) {
		t.Errorf("error must wrap the path: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("env must NOT be touched when the path is invalid: got %v", calls)
	}
}

func TestVertexLanguageModel_ServiceAccountKeyParsed(t *testing.T) {
	isolateHome(t)
	// Stand up a minimal valid SA key JSON. The simplest shape that
	// would pass credentials.DetectDefault is enough — we just want
	// to verify the path is propagated to vertexApplyEnv.
	dir := t.TempDir()
	saPath := filepath.Join(dir, "sa.json")
	saContent := `{
  "type": "service_account",
  "project_id": "test-proj",
  "private_key_id": "abc123",
  "private_key": "-----BEGIN PRIVATE KEY-----\nMIIBVgIBADANBgkqhkiG9w0BAQEFAASCAUAwggE8AgEAAkEAuFvwGm9Q+a7VxMvA\n-----END PRIVATE KEY-----\n",
  "client_email": "test@test-proj.iam.gserviceaccount.com",
  "client_id": "100000000000000000000",
  "auth_uri": "https://accounts.google.com/o/oauth2/auth",
  "token_uri": "https://oauth2.googleapis.com/token"
}`
	if err := os.WriteFile(saPath, []byte(saContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// Capture the env-mutation calls.
	calls := []string{}
	prevApply := vertexApplyEnv
	vertexApplyEnv = func(p string) { calls = append(calls, p) }
	t.Cleanup(func() { vertexApplyEnv = prevApply })

	// Drive the SA-key resolution directly — no need to invoke
	// vertexLanguageModel (which would call google.New and require
	// a real Vertex client).
	got, err := vertexPrepareCredentials(vertexConfig{ServiceAccountKey: saPath})
	if err != nil {
		t.Fatalf("vertexPrepareCredentials: %v", err)
	}
	if got != saPath {
		t.Errorf("returned path = %q, want %q", got, saPath)
	}
	if len(calls) != 1 || calls[0] != saPath {
		t.Errorf("vertexApplyEnv must be called with the SA key path once, got %v", calls)
	}
}

func TestVertexPrepareCredentials_SetsRealEnv(t *testing.T) {
	isolateHome(t)
	// No swap: the real vertexApplyEnv must mutate the process env
	// so the genai library's credentials.DetectDefault finds the
	// bytes on the next call.
	dir := t.TempDir()
	saPath := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(saPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := vertexPrepareCredentials(vertexConfig{ServiceAccountKey: saPath}); err != nil {
		t.Fatal(err)
	}
	if env := os.Getenv(vertexEnvApplicationCredentials); env != saPath {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q want %q", env, saPath)
	}
}

func TestVertexPrepareCredentials_EnvFallback(t *testing.T) {
	isolateHome(t)
	// When the config has no SA key, the function falls back to the
	// env var (already exported) and applies it through the same seam.
	dir := t.TempDir()
	saPath := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(saPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(vertexEnvApplicationCredentials, saPath)
	calls := []string{}
	prevApply := vertexApplyEnv
	vertexApplyEnv = func(p string) { calls = append(calls, p) }
	t.Cleanup(func() { vertexApplyEnv = prevApply })

	got, err := vertexPrepareCredentials(vertexConfig{})
	if err != nil {
		t.Fatalf("vertexPrepareCredentials: %v", err)
	}
	if got != saPath {
		t.Errorf("path = %q want %q", got, saPath)
	}
	if len(calls) != 1 || calls[0] != saPath {
		t.Errorf("vertexApplyEnv must be called with the env-var path once, got %v", calls)
	}
}

func TestVertexPrepareCredentials_ADCDiscovery(t *testing.T) {
	isolateHome(t)
	// When neither the config nor the env has a SA key, the function
	// returns ("", nil) and lets the genai library do ADC discovery
	// (gcloud / GCE metadata). vertexApplyEnv must NOT be called.
	t.Setenv(vertexEnvApplicationCredentials, "")
	calls := []string{}
	prevApply := vertexApplyEnv
	vertexApplyEnv = func(p string) { calls = append(calls, p) }
	t.Cleanup(func() { vertexApplyEnv = prevApply })

	got, err := vertexPrepareCredentials(vertexConfig{})
	if err != nil {
		t.Fatalf("vertexPrepareCredentials: %v", err)
	}
	if got != "" {
		t.Errorf("path = %q want \"\"", got)
	}
	if len(calls) != 0 {
		t.Errorf("vertexApplyEnv must NOT be called when no SA key is set: got %v", calls)
	}
}

func TestVertexProvider_SessionLifecycle(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("answer one", fantasy.Usage{InputTokens: 10}),
	}}
	swapVertexLM(t, lm)

	// Project + location configured.
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.Vertex.Project = "test-proj"
		cfg.Vertex.Location = "us-central1"
		return saveConfig(cfg)
	}); err != nil {
		t.Fatal(err)
	}

	p := vertexAgentProvider()
	cwd := t.TempDir()
	args := ProviderSessionArgs{Cwd: cwd, TabID: 4, NewSessionID: "ses-vtx", SkipAllPermissions: true}
	proc, ch, err := p.StartSession(args)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if proc.cmd != nil {
		t.Error("in-process provider must not carry an exec.Cmd")
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
	if done.res.SessionID != "ses-vtx" || done.res.Result != "answer one" {
		t.Errorf("done msg wrong: %+v", done.res)
	}
	if usage == nil || !usage.costKnown {
		t.Errorf("usageMsg must exist and be cost-known: %+v", usage)
	}

	calls := lm.streamCalls()
	if len(calls) == 0 || calls[0].Prompt[0].Role != fantasy.MessageRoleSystem {
		t.Fatal("first wire message must be the system prompt")
	}
	if want := vertexSpec.maxOutputTokens(vertexDefaultModel); calls[0].MaxOutputTokens == nil ||
		*calls[0].MaxOutputTokens != want {
		t.Errorf("wire MaxOutputTokens = %v want %d", calls[0].MaxOutputTokens, want)
	}

	if handled, _ := p.Interrupt(proc); handled {
		t.Error("idle interrupt must report handled=false")
	}

	sessions, err := p.ListSessions(cwd)
	if err != nil || len(sessions) != 1 || sessions[0].id != "ses-vtx" {
		t.Fatalf("ListSessions: %v %+v", err, sessions)
	}
	if sessions[0].preview != "question" {
		t.Errorf("preview %q", sessions[0].preview)
	}
	entries, err := p.LoadHistory("ses-vtx", HistoryOpts{ToolOutput: toolOutputFull})
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
	swapVertexLM(t, lm2)
	proc2, ch2, err := p.StartSession(ProviderSessionArgs{
		Cwd: cwd, TabID: 4, SessionID: "ses-vtx", SkipAllPermissions: true,
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

func TestVertexProvider_ResumeUnknownSessionErrors(t *testing.T) {
	isolateHome(t)
	swapVertexLM(t, &fakeLM{})
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.Vertex.Project = "test-proj"
		return saveConfig(cfg)
	}); err != nil {
		t.Fatal(err)
	}
	p := vertexAgentProvider()
	if _, _, err := p.StartSession(ProviderSessionArgs{Cwd: t.TempDir(), SessionID: "missing"}); err == nil {
		t.Fatal("resuming an unknown session must fail")
	}
}

func TestVertexProvider_MaterializeRoundTrip(t *testing.T) {
	isolateHome(t)
	p := vertexAgentProvider()
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

func TestVertexStoreUsesHome(t *testing.T) {
	home := isolateHome(t)
	st := vertexStore()
	root, err := st.root()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(root, home) {
		t.Errorf("store root %q must live under isolated home %q", root, home)
	}
}

func TestVertexMaxOutputTokens(t *testing.T) {
	// The default model is in the catwalk catalog; the published
	// default_max_tokens (64000) wins over the fallback.
	if got := vertexSpec.maxOutputTokens(vertexDefaultModel); got == vertexFallbackMaxOutputTokens {
		t.Errorf("default model should use the catwalk value, not the fallback: got %d", got)
	}
	// Unknown models fall back to the conservative default.
	if got := vertexSpec.maxOutputTokens("custom-unknown"); got != vertexFallbackMaxOutputTokens {
		t.Errorf("unknown model must use the fallback budget: %d", got)
	}
}

func TestVertexSupportsImages(t *testing.T) {
	if !vertexSpec.supportsImages(vertexDefaultModel) {
		t.Error("default model must support images")
	}
	if !vertexSpec.supportsImages("unknown-model") {
		t.Error("unknown model should default to image-capable")
	}
}

func TestModelContextLimit_Vertex(t *testing.T) {
	if got := modelContextLimit(vertexDefaultModel); got != vertexContextWindow {
		t.Errorf("vertex default limit = %d want %d", got, vertexContextWindow)
	}
	if got := modelContextLimit("gemini-3-flash-preview"); got != vertexContextWindow {
		t.Errorf("gemini-3-flash-preview limit = %d want %d", got, vertexContextWindow)
	}
}

func TestVertexModelOptions_FiltersClaude(t *testing.T) {
	// The Vertex catwalk ships Claude models; the spec must filter
	// them out so the picker doesn't offer ids with the wrong
	// reasoning enum.
	for _, id := range vertexModelOptions {
		low := strings.ToLower(id)
		if strings.Contains(low, "claude") || strings.Contains(low, "anthropic") {
			t.Errorf("vertexModelOptions must filter Claude / anthropic ids, got %q", id)
		}
	}
	// And the Gemini default must still be there.
	var found bool
	for _, id := range vertexModelOptions {
		if id == vertexDefaultModel {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("vertexModelOptions missing default %q: %v", vertexDefaultModel, vertexModelOptions)
	}
}
