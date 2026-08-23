package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderConfig_FlatJSONRoundTrip(t *testing.T) {
	pc := ProviderConfig{
		Model:         "gemini-3.7-flash",
		SlashCommands: []ProviderSlashEntry{{Name: "x", Description: "y"}},
		Fields:        map[string]string{"project": "proj", "location": "global", "empty": ""},
	}
	data, err := json.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	// Flat on disk: the declared fields sit next to model, and empty
	// values are not written.
	want := `{"location":"global","model":"gemini-3.7-flash","project":"proj","slashCommands":[{"name":"x","description":"y"}]}`
	if string(data) != want {
		t.Fatalf("marshal = %s\nwant %s", data, want)
	}
	var back ProviderConfig
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Model != pc.Model || len(back.SlashCommands) != 1 || back.Field("project") != "proj" || back.Field("location") != "global" {
		t.Errorf("round trip: %+v", back)
	}
	if _, ok := back.Fields["empty"]; ok {
		t.Error("empty values must not survive")
	}

	// Non-string extras are dropped; malformed typed keys are errors.
	var tolerant ProviderConfig
	if err := json.Unmarshal([]byte(`{"apiKey":"k","timeout":5,"nested":{"a":1}}`), &tolerant); err != nil {
		t.Fatal(err)
	}
	if tolerant.Field("apiKey") != "k" || len(tolerant.Fields) != 1 {
		t.Errorf("non-string extras must be dropped: %+v", tolerant.Fields)
	}
	var bad ProviderConfig
	if err := json.Unmarshal([]byte(`{"model":5}`), &bad); err == nil {
		t.Error("a non-string model must be rejected")
	}
}

func TestProviderConfig_FieldHelpers(t *testing.T) {
	var pc ProviderConfig
	if !pc.IsEmpty() || pc.Field("x") != "" {
		t.Fatal("zero value is empty")
	}
	with := pc.WithField("x", "1")
	if with.Field("x") != "1" || pc.Fields != nil {
		t.Error("WithField must copy, not share")
	}
	if with.IsEmpty() {
		t.Error("a field makes the block non-empty")
	}
	cleared := with.WithField("x", "")
	if !cleared.IsEmpty() || cleared.Fields != nil {
		t.Errorf("clearing the last field must leave a nil map: %+v", cleared)
	}
	if with.Field("x") != "1" {
		t.Error("the original must be untouched")
	}
}

func TestConfig_SetProviderConfig(t *testing.T) {
	var cfg Config
	cfg.SetProviderConfig("p", ProviderConfig{})
	if cfg.Providers != nil {
		t.Error("storing an empty block must not create the map")
	}
	cfg.SetProviderConfig("p", ProviderConfig{Model: "m"})
	if cfg.ProviderConfig("p").Model != "m" {
		t.Error("stored block must read back")
	}
	cfg.SetProviderConfig("p", ProviderConfig{})
	if cfg.Providers != nil {
		t.Error("emptying the last block must drop the map so ask.json has no {} rows")
	}
}

func TestConfig_ProvidersPersistFlat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var cfg Config
	cfg.SetProviderConfig("vertex", ProviderConfig{Model: "gemini-2.5-pro", Fields: map[string]string{"project": "proj"}})
	cfg.SetProviderConfig("openrouter", ProviderConfig{Fields: map[string]string{"apiKey": "k"}})
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "ask", "ask.json"))
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	if shape.Providers["vertex"]["project"] != "proj" || shape.Providers["vertex"]["model"] != "gemini-2.5-pro" || shape.Providers["openrouter"]["apiKey"] != "k" {
		t.Errorf("on-disk shape: %s", raw)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProviderConfig("vertex").Field("project") != "proj" || loaded.ProviderConfig("openrouter").Field("apiKey") != "k" {
		t.Errorf("load: %+v", loaded.Providers)
	}
}

func TestMigrateLegacyProviderBlocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "ask")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{
  "provider": "vertex",
  "vertex": {"project": "old-proj", "location": "us-central1", "serviceAccount": "/keys/sa.json", "baseURL": "x", "effort": "high", "model": "gemini-2.5-pro",
             "slashCommands": [{"name": "s", "description": "d"}]},
  "openrouter": {"apiKey": "or-key", "baseURL": "https://example/v1"},
  "deepseek": {"apiKey": "ignored"}
}`
	if err := os.WriteFile(filepath.Join(dir, "ask.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.ProviderConfig("vertex")
	if v.Model != "gemini-2.5-pro" || v.Field("project") != "old-proj" || v.Field("location") != "us-central1" || len(v.SlashCommands) != 1 {
		t.Errorf("vertex block: %+v", v)
	}
	if v.Field("serviceAccountKey") != "/keys/sa.json" {
		t.Errorf("serviceAccount alias must be lifted into serviceAccountKey: %+v", v.Fields)
	}
	for _, dropped := range []string{"serviceAccount", "baseURL", "effort"} {
		if _, ok := v.Fields[dropped]; ok {
			t.Errorf("legacy key %q must be dropped: %+v", dropped, v.Fields)
		}
	}
	o := cfg.ProviderConfig("openrouter")
	if o.Field("apiKey") != "or-key" || o.Field("baseURL") != "https://example/v1" || o.Field("effort") != "" {
		t.Errorf("openrouter block: %+v", o)
	}
	if _, ok := cfg.Providers["deepseek"]; ok {
		t.Error("unregistered legacy blocks are not lifted")
	}
	if cfg.Effort != "high" {
		t.Errorf("effort migration still applies: %q", cfg.Effort)
	}

	// Saving writes the new shape only; a reload is stable.
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "ask.json"))
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["vertex"]; ok {
		t.Error("legacy top-level block must not be re-written")
	}
	again, _ := Load()
	if again.ProviderConfig("vertex").Field("project") != "old-proj" {
		t.Errorf("reload after migration: %+v", again.Providers)
	}

	// An explicit new block wins over a legacy one.
	both := `{"providers": {"vertex": {"project": "new-proj"}}, "vertex": {"project": "old-proj"}}`
	if err := os.WriteFile(filepath.Join(dir, "ask.json"), []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load()
	if cfg.ProviderConfig("vertex").Field("project") != "new-proj" {
		t.Errorf("new block must win: %+v", cfg.Providers)
	}

	// A legacy serviceAccountKey is kept over the alias.
	alias := `{"vertex": {"serviceAccountKey": "/a.json", "serviceAccount": "/b.json"}}`
	if err := os.WriteFile(filepath.Join(dir, "ask.json"), []byte(alias), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load()
	if cfg.ProviderConfig("vertex").Field("serviceAccountKey") != "/a.json" {
		t.Errorf("explicit key beats alias: %+v", cfg.Providers)
	}
}
