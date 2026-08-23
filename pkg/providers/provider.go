package providers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Provider is the contract every in-process LLM provider satisfies. The
// TUI, the headless engine, the /config screen, and the model picker are
// written against this interface and the registry below, so adding a
// provider is implementing it and adding it to builtin. Every method is
// required; optional capabilities are separate interfaces (ModelLister)
// that call sites discover by type assertion.
type Provider interface {
	// ID is the short stable identifier stored in config ("vertex").
	ID() string
	// DisplayName is the human-readable name used in UI copy and errors.
	DisplayName() string
	// DefaultModel is the model used when the user has not picked one.
	DefaultModel() string
	// ModelOptions are the static catalog ids the picker shows before a
	// live listing lands.
	ModelOptions() []string
	// EffortOptions are the /effort choices; empty hides /effort.
	EffortOptions() []string
	// Settings declares the provider's configuration fields. The /config
	// screen renders them, the model picker's key prompt asks for the
	// Secret one, and Configured is judged against them.
	Settings() []SettingField
	// Configured reports whether pc (with the fields' env fallbacks)
	// carries the credentials the provider needs. Never touches the
	// network.
	Configured(pc config.ProviderConfig) bool
	// BuildModel constructs the ADK LLM for modelID from pc.
	BuildModel(ctx context.Context, pc config.ProviderConfig, modelID string) (model.LLM, error)
	// CanonicalModelID normalizes a user- or config-supplied model id. An
	// empty or foreign id resolves to fallback; an empty fallback means
	// DefaultModel.
	CanonicalModelID(modelID, fallback string) string
	// CallOptions maps ask's effort onto the wire request for modelID.
	CallOptions(modelID, effort string) (*genai.GenerateContentConfig, *float64)
	// SupportsImages reports whether modelID accepts image attachments.
	SupportsImages(modelID string) bool
	// ContextWindow is modelID's input window in tokens.
	ContextWindow(modelID string) int64
	// MaxOutputTokens is modelID's output budget in tokens.
	MaxOutputTokens(modelID string) int64
}

// ModelLister is the optional capability of enumerating the models a
// provider currently serves. A provider without it is listed from the
// static catalog only.
type ModelLister interface {
	ListModels(ctx context.Context, pc config.ProviderConfig) ([]string, error)
}

// SettingField is one configuration field a provider declares. The
// /config → <provider> screen is rendered from these, one row per field.
type SettingField struct {
	// Key is the field's key in config.ProviderConfig.Fields.
	Key string
	// Title is the row label.
	Title string
	// Hint is shown while editing the field.
	Hint string
	// Secret masks the value in the UI and marks the field as the
	// credential the model picker prompts for.
	Secret bool
	// EnvKey is the environment variable consulted when the field is
	// unset.
	EnvKey string
	// Default is the value used when neither config nor env sets one.
	Default string
	// Validate rejects a draft before it is saved; nil accepts anything.
	Validate func(string) error
}

// SettingValue resolves f against pc: the stored value, else the env
// fallback, else the default.
func SettingValue(pc config.ProviderConfig, f SettingField) string {
	if v := strings.TrimSpace(pc.Field(f.Key)); v != "" {
		return v
	}
	if f.EnvKey != "" {
		if v := strings.TrimSpace(os.Getenv(f.EnvKey)); v != "" {
			return v
		}
	}
	return f.Default
}

// SecretField returns p's credential field, if it declares one.
func SecretField(p Provider) (SettingField, bool) {
	for _, f := range p.Settings() {
		if f.Secret {
			return f, true
		}
	}
	return SettingField{}, false
}

// FieldByKey returns p's setting with the given key.
func FieldByKey(p Provider, key string) (SettingField, bool) {
	for _, f := range p.Settings() {
		if f.Key == key {
			return f, true
		}
	}
	return SettingField{}, false
}

var (
	registryMu sync.RWMutex
	registry   []Provider
)

// builtin is the registration order. The first entry is the default
// provider for an empty config.
var builtin = []Provider{Vertex{}, OpenRouter{}, ClaudeCode{}}

func init() {
	for _, p := range builtin {
		Register(p)
	}
}

// Register adds p to the registry, replacing an earlier registration with
// the same id in place. A malformed provider — empty id, or a setting
// whose key is empty, duplicated, or reserved — panics: that is a
// programming error, not a runtime condition.
func Register(p Provider) {
	if err := validateProvider(p); err != nil {
		panic(err)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	for i, q := range registry {
		if q.ID() == p.ID() {
			registry[i] = p
			return
		}
	}
	registry = append(registry, p)
}

func validateProvider(p Provider) error {
	if p == nil {
		return fmt.Errorf("providers: Register(nil)")
	}
	if strings.TrimSpace(p.ID()) == "" {
		return fmt.Errorf("providers: %T has an empty id", p)
	}
	seen := make(map[string]bool)
	for _, f := range p.Settings() {
		if f.Key == "" {
			return fmt.Errorf("providers: %s declares a setting with an empty key", p.ID())
		}
		for _, reserved := range config.ProviderConfigReservedKeys {
			if f.Key == reserved {
				return fmt.Errorf("providers: %s declares a setting under the reserved key %q", p.ID(), f.Key)
			}
		}
		if seen[f.Key] {
			return fmt.Errorf("providers: %s declares the setting %q twice", p.ID(), f.Key)
		}
		seen[f.Key] = true
	}
	return nil
}

// Get returns the registered provider with the given id.
func Get(id string) (Provider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	for _, p := range registry {
		if p.ID() == id {
			return p, true
		}
	}
	return nil, false
}

// All returns the registered providers in registration order.
func All() []Provider {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return append([]Provider(nil), registry...)
}

// DefaultProviderID is the id of the first registered provider — what an
// empty config resolves to.
func DefaultProviderID() string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if len(registry) == 0 {
		return ""
	}
	return registry[0].ID()
}

// ProviderConfigured reports whether provider id can run with cfg —
// false for an unknown id or one missing its credentials.
func ProviderConfigured(cfg config.Config, id string) bool {
	p, ok := Get(id)
	if !ok {
		return false
	}
	return p.Configured(cfg.ProviderConfig(id))
}

// ProviderSettings is the per-provider slice of configuration the TUI
// reads and writes: the picked model, the reasoning effort (global —
// every provider shares Config.Effort), and the discovered slash
// commands.
type ProviderSettings struct {
	Model         string                      `json:"model"`
	Effort        string                      `json:"effort"`
	SlashCommands []config.ProviderSlashEntry `json:"slashCommands,omitempty"`
}

// LoadSettings reads provider id's settings out of cfg.
func LoadSettings(cfg config.Config, id string) ProviderSettings {
	pc := cfg.ProviderConfig(id)
	return ProviderSettings{
		Model:         pc.Model,
		Effort:        cfg.Effort,
		SlashCommands: pc.SlashCommands,
	}
}

// SaveSettings writes s into cfg under provider id, keeping the
// provider's declared fields untouched.
func SaveSettings(cfg *config.Config, id string, s ProviderSettings) {
	pc := cfg.ProviderConfig(id)
	pc.Model = s.Model
	pc.SlashCommands = s.SlashCommands
	cfg.SetProviderConfig(id, pc)
	cfg.Effort = s.Effort
}

// ResolveModelID picks the model a session on p should run: the explicit
// id when one was given, else the configured one, else the provider
// default — canonicalized either way.
func ResolveModelID(p Provider, explicit string, cfg config.Config) string {
	if m := strings.TrimSpace(explicit); m != "" {
		return p.CanonicalModelID(m, "")
	}
	return p.CanonicalModelID(cfg.ProviderConfig(p.ID()).Model, p.DefaultModel())
}

// MissingAPIKeyError returns a descriptive error when an API key is missing.
func MissingAPIKeyError(envKey string, hint ...string) error {
	picker := "the model picker"
	if len(hint) > 0 && hint[0] != "" {
		picker = hint[0]
	}
	return fmt.Errorf("no API key configured — add one via %s, or export %s", picker, envKey)
}

// expandTilde resolves a leading "~" or "~/" against the home directory.
func expandTilde(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return home + p[1:]
}
