package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// models.dev (https://models.dev, MIT) is the community model database ask
// uses for the facts provider APIs do not publish — descriptions, token
// limits, prices, knowledge cutoffs. Vertex's publisher-model API returns
// nothing beyond an id, so for Vertex this is the only source of all of it.

const (
	ModelsDevDefaultURL = "https://models.dev/api.json"
	ModelsDevCacheTTL   = 24 * time.Hour
)

var (
	ModelsDevURL        = ModelsDevDefaultURL
	ModelsDevHTTPClient = http.DefaultClient
	ModelsDevCachePath  = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "ask", "cache", "models-dev.json"), nil
	}
)

// modelsDevProviderIDs maps ask provider ids onto models.dev provider keys.
// Only mapped providers are parsed out of the (multi-MB) payload.
var modelsDevProviderIDs = map[string]string{
	VertexProviderID:     "google-vertex",
	OpenRouterProviderID: "openrouter",
	ClaudeCodeProviderID: "anthropic",
}

type modelsDevModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Reasoning   bool   `json:"reasoning"`
	Knowledge   string `json:"knowledge"`
	ReleaseDate string `json:"release_date"`
	Status      string `json:"status"`
	Modalities  struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Limit struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	Cost *struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
}

func (m modelsDevModel) modelMeta() ModelMeta {
	meta := ModelMeta{
		ID:              m.ID,
		Name:            m.Name,
		Description:     m.Description,
		ContextWindow:   m.Limit.Context,
		MaxOutputTokens: m.Limit.Output,
		InputModalities: m.Modalities.Input,
		Reasoning:       m.Reasoning,
		KnowledgeCutoff: m.Knowledge,
		ReleaseDate:     m.ReleaseDate,
		Status:          m.Status,
	}
	if m.Cost != nil {
		meta.Pricing = &ModelPricing{
			InputPer1M:       m.Cost.Input,
			OutputPer1M:      m.Cost.Output,
			CachedInputPer1M: m.Cost.CacheRead,
			CacheWritePer1M:  m.Cost.CacheWrite,
		}
	}
	return meta
}

var modelsDev = struct {
	loadMu     sync.Mutex
	mu         sync.RWMutex
	byProvider map[string]map[string]ModelMeta
	loaded     bool
}{}

// LoadModelsDev makes models.dev data available to ModelsDevMeta: memory
// first, then a disk cache younger than ModelsDevCacheTTL, then the network
// (refreshing the disk cache). A failed fetch falls back to a stale disk
// cache rather than returning nothing.
func LoadModelsDev(ctx context.Context) error {
	modelsDev.loadMu.Lock()
	defer modelsDev.loadMu.Unlock()
	if ModelsDevLoaded() {
		return nil
	}

	path, pathErr := ModelsDevCachePath()
	var stale []byte
	if pathErr == nil {
		if data, modTime, ok := readModelsDevCache(path); ok {
			if time.Since(modTime) < ModelsDevCacheTTL {
				if err := installModelsDev(data); err == nil {
					return nil
				}
			}
			stale = data
		}
	}

	data, fetchErr := fetchModelsDev(ctx)
	if fetchErr != nil {
		if stale != nil {
			if err := installModelsDev(stale); err == nil {
				return nil
			}
		}
		return fetchErr
	}
	if err := installModelsDev(data); err != nil {
		return err
	}
	if pathErr == nil {
		writeModelsDevCache(path, data)
	}
	return nil
}

func ModelsDevLoaded() bool {
	modelsDev.mu.RLock()
	defer modelsDev.mu.RUnlock()
	return modelsDev.loaded
}

// ModelsDevMeta is an in-memory lookup; it reports ok=false until
// LoadModelsDev has succeeded, for unmapped providers, and for unknown ids.
func ModelsDevMeta(providerID, modelID string) (ModelMeta, bool) {
	modelsDev.mu.RLock()
	defer modelsDev.mu.RUnlock()
	if !modelsDev.loaded {
		return ModelMeta{}, false
	}
	models, ok := modelsDev.byProvider[providerID]
	if !ok {
		return ModelMeta{}, false
	}
	m, ok := models[NormalizeModelID(modelID)]
	return m, ok
}

func fetchModelsDev(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ask (https://github.com/Cidan/ask)")
	resp, err := ModelsDevHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func installModelsDev(data []byte) error {
	var providersRaw map[string]json.RawMessage
	if err := json.Unmarshal(data, &providersRaw); err != nil {
		return fmt.Errorf("models.dev: %w", err)
	}
	byProvider := make(map[string]map[string]ModelMeta, len(modelsDevProviderIDs))
	for askID, devID := range modelsDevProviderIDs {
		raw, ok := providersRaw[devID]
		if !ok {
			continue
		}
		var prov struct {
			Models map[string]modelsDevModel `json:"models"`
		}
		if err := json.Unmarshal(raw, &prov); err != nil {
			return fmt.Errorf("models.dev: provider %s: %w", devID, err)
		}
		models := make(map[string]ModelMeta, len(prov.Models))
		for id, m := range prov.Models {
			if m.ID == "" {
				m.ID = id
			}
			models[id] = m.modelMeta()
		}
		byProvider[askID] = models
	}
	if len(byProvider) == 0 {
		return errors.New("models.dev: payload has none of the mapped providers")
	}
	modelsDev.mu.Lock()
	modelsDev.byProvider = byProvider
	modelsDev.loaded = true
	modelsDev.mu.Unlock()
	return nil
}

func readModelsDevCache(path string) ([]byte, time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, time.Time{}, false
	}
	return data, info.ModTime(), true
}

func writeModelsDevCache(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
