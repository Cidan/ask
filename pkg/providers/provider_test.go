package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// stubProvider is the smallest Provider a test can register.
type stubProvider struct {
	id       string
	settings []SettingField
}

func (s stubProvider) ID() string              { return s.id }
func (s stubProvider) DisplayName() string     { return "Stub " + s.id }
func (s stubProvider) DefaultModel() string    { return "stub-default" }
func (s stubProvider) ModelOptions() []string  { return []string{"stub-default", "stub-2"} }
func (s stubProvider) EffortOptions() []string { return GlobalEffortOptions }
func (s stubProvider) Settings() []SettingField {
	return s.settings
}
func (s stubProvider) Configured(pc config.ProviderConfig) bool { return pc.Field("token") != "" }
func (s stubProvider) BuildModel(context.Context, config.ProviderConfig, string) (model.LLM, error) {
	return nil, nil
}
func (s stubProvider) CanonicalModelID(modelID, fallback string) string {
	if strings.TrimSpace(modelID) == "" {
		if fallback == "" {
			return s.DefaultModel()
		}
		return fallback
	}
	return "canon:" + modelID
}
func (s stubProvider) CallOptions(string, string) (*genai.GenerateContentConfig, *float64) {
	return nil, nil
}
func (s stubProvider) SupportsImages(string) bool   { return false }
func (s stubProvider) ContextWindow(string) int64   { return 1 }
func (s stubProvider) MaxOutputTokens(string) int64 { return 1 }

// withRegistry swaps the package registry for the test's duration.
func withRegistry(t *testing.T, provs ...Provider) {
	t.Helper()
	registryMu.Lock()
	prev := registry
	registry = nil
	registryMu.Unlock()
	for _, p := range provs {
		Register(p)
	}
	t.Cleanup(func() {
		registryMu.Lock()
		registry = prev
		registryMu.Unlock()
	})
}

func TestRegistry_BuiltinOrderAndLookup(t *testing.T) {
	all := All()
	if len(all) != 3 || all[0].ID() != VertexProviderID || all[1].ID() != OpenRouterProviderID || all[2].ID() != ClaudeCodeProviderID {
		t.Fatalf("builtin registry = %v", ids(all))
	}
	if DefaultProviderID() != VertexProviderID {
		t.Errorf("default provider = %q want vertex", DefaultProviderID())
	}
	for _, id := range []string{VertexProviderID, OpenRouterProviderID, ClaudeCodeProviderID} {
		p, ok := Get(id)
		if !ok || p.ID() != id || p.DisplayName() == "" || p.DefaultModel() == "" {
			t.Errorf("Get(%q) = %v, %v", id, p, ok)
		}
	}
	if _, ok := Get("nosuch"); ok {
		t.Error("unknown id must miss")
	}
	// All returns a copy — mutating it must not touch the registry.
	all[0] = stubProvider{id: "mutated"}
	if All()[0].ID() != VertexProviderID {
		t.Error("All must return a copy")
	}
}

func ids(ps []Provider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID())
	}
	return out
}

func TestRegister_ReplacesSameIDInPlace(t *testing.T) {
	withRegistry(t, stubProvider{id: "a"}, stubProvider{id: "b"})
	Register(stubProvider{id: "a", settings: []SettingField{{Key: "token", Secret: true}}})
	all := All()
	if got := ids(all); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("re-registering must keep the slot: %v", got)
	}
	if f, ok := SecretField(all[0]); !ok || f.Key != "token" {
		t.Errorf("replacement must be the new value: %+v %v", f, ok)
	}
	if DefaultProviderID() != "a" {
		t.Errorf("default = %q", DefaultProviderID())
	}
}

func TestRegister_RejectsMalformedProviders(t *testing.T) {
	cases := map[string]Provider{
		"nil":            nil,
		"empty id":       stubProvider{id: " "},
		"empty key":      stubProvider{id: "x", settings: []SettingField{{Key: ""}}},
		"reserved model": stubProvider{id: "x", settings: []SettingField{{Key: "model"}}},
		"reserved slash": stubProvider{id: "x", settings: []SettingField{{Key: "slashCommands"}}},
		"duplicate key":  stubProvider{id: "x", settings: []SettingField{{Key: "k"}, {Key: "k"}}},
	}
	for name, p := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: Register must panic", name)
				}
			}()
			Register(p)
		}()
	}
	if _, ok := Get("x"); ok {
		t.Error("a rejected provider must not be registered")
	}
}

func TestBuiltinSettings_AreWellFormed(t *testing.T) {
	for _, p := range All() {
		if err := validateProvider(p); err != nil {
			t.Errorf("%s: %v", p.ID(), err)
		}
		for _, f := range p.Settings() {
			if f.Title == "" || f.Hint == "" {
				t.Errorf("%s.%s: title and hint are required for the config screen", p.ID(), f.Key)
			}
		}
	}
	if f, ok := SecretField(OpenRouter{}); !ok || f.Key != OpenRouterFieldAPIKey || f.EnvKey != OpenRouterEnvAPIKey {
		t.Errorf("OpenRouter secret field = %+v %v", f, ok)
	}
	if _, ok := SecretField(Vertex{}); ok {
		t.Error("Vertex has no secret field — it authenticates with ADC, not a key")
	}
	if f, ok := FieldByKey(Vertex{}, VertexFieldLocation); !ok || f.Default != VertexDefaultLocation {
		t.Errorf("FieldByKey(vertex, location) = %+v %v", f, ok)
	}
	if _, ok := FieldByKey(Vertex{}, "nosuch"); ok {
		t.Error("unknown key must miss")
	}
}

func TestSettingValue_StoredThenEnvThenDefault(t *testing.T) {
	f := SettingField{Key: "k", EnvKey: "ASK_TEST_SETTING", Default: "dflt"}
	t.Setenv("ASK_TEST_SETTING", "")
	if got := SettingValue(config.ProviderConfig{}, f); got != "dflt" {
		t.Errorf("default: %q", got)
	}
	t.Setenv("ASK_TEST_SETTING", " from-env ")
	if got := SettingValue(config.ProviderConfig{}, f); got != "from-env" {
		t.Errorf("env: %q", got)
	}
	pc := config.ProviderConfig{}.WithField("k", " stored ")
	if got := SettingValue(pc, f); got != "stored" {
		t.Errorf("stored wins: %q", got)
	}
}

func TestProviderConfigured(t *testing.T) {
	var cfg config.Config
	t.Setenv(OpenRouterEnvAPIKey, "")
	t.Setenv(VertexEnvCloudProject, "")
	if ProviderConfigured(cfg, VertexProviderID) || ProviderConfigured(cfg, OpenRouterProviderID) {
		t.Fatal("no credentials → not configured")
	}
	if ProviderConfigured(cfg, "nosuch") {
		t.Fatal("unknown provider → not configured")
	}
	cfg.SetProviderConfig(VertexProviderID, config.ProviderConfig{}.WithField(VertexFieldProject, "p"))
	cfg.SetProviderConfig(OpenRouterProviderID, config.ProviderConfig{}.WithField(OpenRouterFieldAPIKey, "k"))
	if !ProviderConfigured(cfg, VertexProviderID) || !ProviderConfigured(cfg, OpenRouterProviderID) {
		t.Fatal("credentials present → configured")
	}
	t.Setenv(OpenRouterEnvAPIKey, "env-key")
	if !ProviderConfigured(config.Config{}, OpenRouterProviderID) {
		t.Fatal("env key counts")
	}
}

func TestLoadSaveSettings_RoundTripKeepsFields(t *testing.T) {
	var cfg config.Config
	cfg.Effort = "low"
	cfg.SetProviderConfig("p", config.ProviderConfig{}.WithField("apiKey", "k"))

	got := LoadSettings(cfg, "p")
	if got.Model != "" || got.Effort != "low" || len(got.SlashCommands) != 0 {
		t.Fatalf("fresh settings: %+v", got)
	}
	SaveSettings(&cfg, "p", ProviderSettings{
		Model:         "m",
		Effort:        "high",
		SlashCommands: []config.ProviderSlashEntry{{Name: "x", Description: "y"}},
	})
	if cfg.Effort != "high" {
		t.Errorf("effort is global: %q", cfg.Effort)
	}
	pc := cfg.ProviderConfig("p")
	if pc.Model != "m" || len(pc.SlashCommands) != 1 || pc.Field("apiKey") != "k" {
		t.Errorf("SaveSettings must keep the declared fields: %+v", pc)
	}
	back := LoadSettings(cfg, "p")
	if back.Model != "m" || back.Effort != "high" || back.SlashCommands[0].Name != "x" {
		t.Errorf("round trip: %+v", back)
	}
}

func TestResolveModelID(t *testing.T) {
	p := stubProvider{id: "s"}
	var cfg config.Config
	if got := ResolveModelID(p, " explicit ", cfg); got != "canon:explicit" {
		t.Errorf("explicit: %q", got)
	}
	if got := ResolveModelID(p, "", cfg); got != "stub-default" {
		t.Errorf("no config, no explicit → default: %q", got)
	}
	cfg.SetProviderConfig("s", config.ProviderConfig{Model: "saved"})
	if got := ResolveModelID(p, "", cfg); got != "canon:saved" {
		t.Errorf("configured model: %q", got)
	}
	if got := ResolveModelID(p, "explicit", cfg); got != "canon:explicit" {
		t.Errorf("explicit beats configured: %q", got)
	}
	// The real providers: an empty id resolves to the default, and a
	// configured model is honoured.
	if got := ResolveModelID(Vertex{}, "", config.Config{}); got != VertexDefaultModel {
		t.Errorf("vertex default: %q", got)
	}
	var vc config.Config
	vc.SetProviderConfig(VertexProviderID, config.ProviderConfig{Model: "gemini-2.5-pro"})
	if got := ResolveModelID(Vertex{}, "", vc); got != "gemini-2.5-pro" {
		t.Errorf("vertex configured: %q", got)
	}
	var oc config.Config
	oc.SetProviderConfig(OpenRouterProviderID, config.ProviderConfig{Model: "openai/o3-mini"})
	if got := ResolveModelID(OpenRouter{}, "", oc); got != "openai/o3-mini" {
		t.Errorf("openrouter configured: %q", got)
	}
}

func TestExpandTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := expandTilde("~/k.json"); got != home+"/k.json" {
		t.Errorf("~/ = %q", got)
	}
	if got := expandTilde("~"); got != home {
		t.Errorf("~ = %q", got)
	}
	if got := expandTilde("/abs/k.json"); got != "/abs/k.json" {
		t.Errorf("absolute untouched: %q", got)
	}
	if got := expandTilde("~user/x"); got != "~user/x" {
		t.Errorf("~user untouched: %q", got)
	}
}
