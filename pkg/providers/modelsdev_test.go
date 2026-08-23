package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const modelsDevTestJSON = `{
  "google-vertex": {"id": "google-vertex", "name": "Vertex", "models": {
    "gemini-3.7-flash": {"id": "gemini-3.7-flash", "name": "Gemini 3.7 Flash",
      "description": "High-efficiency Gemini model", "reasoning": true,
      "knowledge": "2026-03", "release_date": "2026-08-13", "status": "beta",
      "modalities": {"input": ["text", "image", "video"], "output": ["text"]},
      "limit": {"context": 1048576, "output": 65536},
      "cost": {"input": 0.75, "output": 3.75, "cache_read": 0.075}},
    "gemini-free": {"id": "gemini-free", "name": "Gemini Free",
      "limit": {"context": 32768, "output": 8192}}
  }},
  "openrouter": {"id": "openrouter", "name": "OpenRouter", "models": {
    "anthropic/claude-sonnet-4.5": {"id": "anthropic/claude-sonnet-4.5", "name": "Claude Sonnet 4.5",
      "description": "Balanced Claude model", "reasoning": true, "knowledge": "2025-07-31",
      "limit": {"context": 1000000, "output": 64000},
      "cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}},
    "vendor/nodesc": {"id": "vendor/nodesc", "name": "No Desc", "limit": {"context": 1000, "output": 100}}
  }},
  "anthropic": {"id": "anthropic", "name": "Anthropic", "models": {
    "claude-opus-5": {"id": "claude-opus-5", "name": "Claude Opus 5", "limit": {"context": 1, "output": 1}}
  }}
}`

const modelsDevTestJSONv2 = `{
  "google-vertex": {"id": "google-vertex", "models": {
    "gemini-3.7-flash": {"id": "gemini-3.7-flash", "name": "Gemini 3.7 Flash v2",
      "limit": {"context": 2097152, "output": 65536}, "cost": {"input": 0.5, "output": 3}}
  }}
}`

func resetModelsDev(t *testing.T) {
	t.Helper()
	reset := func() {
		modelsDev.mu.Lock()
		modelsDev.byProvider = nil
		modelsDev.loaded = false
		modelsDev.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func resetOpenRouterMeta(t *testing.T) {
	t.Helper()
	reset := func() {
		openRouterMeta.mu.Lock()
		openRouterMeta.byID = map[string]openRouterModelMeta{}
		openRouterMeta.fetched = false
		openRouterMeta.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// modelsDevTestServer serves `body` with `status` and counts requests; the
// package-level URL is pointed at it for the test's lifetime.
func modelsDevTestServer(t *testing.T, status int, body string) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	prev := ModelsDevURL
	ModelsDevURL = srv.URL
	t.Cleanup(func() { ModelsDevURL = prev })
	return &hits
}

func modelsDevTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeModelsDevTestCache(t *testing.T, body string, age time.Duration) string {
	t.Helper()
	path, err := ModelsDevCachePath()
	if err != nil {
		t.Fatal(err)
	}
	writeModelsDevCache(path, []byte(body))
	if age > 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestLoadModelsDev_FetchesParsesAndWritesCache(t *testing.T) {
	home := modelsDevTestHome(t)
	resetModelsDev(t)
	hits := modelsDevTestServer(t, http.StatusOK, modelsDevTestJSON)

	if err := LoadModelsDev(context.Background()); err != nil {
		t.Fatalf("LoadModelsDev: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected one fetch, got %d", hits.Load())
	}
	if !ModelsDevLoaded() {
		t.Fatal("ModelsDevLoaded must report true after a successful load")
	}

	m, ok := ModelsDevMeta("vertex", "gemini-3.7-flash")
	if !ok {
		t.Fatal("vertex gemini-3.7-flash must resolve")
	}
	if m.ID != "gemini-3.7-flash" || m.Name != "Gemini 3.7 Flash" || m.Description != "High-efficiency Gemini model" {
		t.Errorf("identity fields wrong: %+v", m)
	}
	if m.ContextWindow != 1_048_576 || m.MaxOutputTokens != 65_536 {
		t.Errorf("limits wrong: %+v", m)
	}
	if m.Pricing == nil || m.Pricing.InputPer1M != 0.75 || m.Pricing.OutputPer1M != 3.75 || m.Pricing.CachedInputPer1M != 0.075 || m.Pricing.CacheWritePer1M != 0 {
		t.Errorf("pricing wrong: %+v", m.Pricing)
	}
	if !m.Reasoning || m.KnowledgeCutoff != "2026-03" || m.ReleaseDate != "2026-08-13" || m.Status != "beta" {
		t.Errorf("capability/date fields wrong: %+v", m)
	}
	if len(m.InputModalities) != 3 || m.InputModalities[1] != "image" {
		t.Errorf("modalities wrong: %v", m.InputModalities)
	}

	if free, ok := ModelsDevMeta("vertex", "gemini-free"); !ok || free.Pricing != nil {
		t.Errorf("model without cost must resolve with nil Pricing: ok=%v %+v", ok, free)
	}
	if pre, ok := ModelsDevMeta("vertex", "publishers/google/models/gemini-3.7-flash"); !ok || pre.ID != "gemini-3.7-flash" {
		t.Errorf("prefixed vertex id must normalize: ok=%v %+v", ok, pre)
	}
	if or, ok := ModelsDevMeta("openrouter", "anthropic/claude-sonnet-4.5"); !ok || or.Pricing == nil || or.Pricing.CacheWritePer1M != 3.75 || or.ContextWindow != 1_000_000 {
		t.Errorf("openrouter model wrong: ok=%v %+v", ok, or)
	}
	if _, ok := ModelsDevMeta("vertex", "no-such-model"); ok {
		t.Error("unknown model must miss")
	}
	if _, ok := ModelsDevMeta("anthropic", "claude-opus-5"); ok {
		t.Error("unmapped providers must not be parsed")
	}

	cachePath := filepath.Join(home, ".config", "ask", "cache", "models-dev.json")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("disk cache not written: %v", err)
	}
	if string(data) != modelsDevTestJSON {
		t.Error("disk cache must hold the payload verbatim")
	}

	if err := LoadModelsDev(context.Background()); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("a loaded catalog must not refetch, got %d hits", hits.Load())
	}
}

func TestLoadModelsDev_FreshDiskCacheSkipsNetwork(t *testing.T) {
	modelsDevTestHome(t)
	resetModelsDev(t)
	hits := modelsDevTestServer(t, http.StatusInternalServerError, "boom")
	writeModelsDevTestCache(t, modelsDevTestJSON, 0)

	if err := LoadModelsDev(context.Background()); err != nil {
		t.Fatalf("LoadModelsDev: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("fresh cache must not touch the network, got %d hits", hits.Load())
	}
	if m, ok := ModelsDevMeta("vertex", "gemini-3.7-flash"); !ok || m.Name != "Gemini 3.7 Flash" {
		t.Errorf("cache contents must be served: ok=%v %+v", ok, m)
	}
}

func TestLoadModelsDev_StaleDiskCacheRefetches(t *testing.T) {
	modelsDevTestHome(t)
	resetModelsDev(t)
	hits := modelsDevTestServer(t, http.StatusOK, modelsDevTestJSONv2)
	path := writeModelsDevTestCache(t, modelsDevTestJSON, 2*ModelsDevCacheTTL)

	if err := LoadModelsDev(context.Background()); err != nil {
		t.Fatalf("LoadModelsDev: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("stale cache must refetch once, got %d hits", hits.Load())
	}
	m, ok := ModelsDevMeta("vertex", "gemini-3.7-flash")
	if !ok || m.Name != "Gemini 3.7 Flash v2" || m.ContextWindow != 2_097_152 {
		t.Errorf("fresh payload must win over the stale cache: ok=%v %+v", ok, m)
	}
	data, _ := os.ReadFile(path)
	if string(data) != modelsDevTestJSONv2 {
		t.Error("disk cache must be rewritten with the fresh payload")
	}
	if info, err := os.Stat(path); err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Error("rewritten cache must carry a fresh mtime")
	}
}

func TestLoadModelsDev_StaleDiskCacheFallbackOnNetworkFailure(t *testing.T) {
	modelsDevTestHome(t)
	resetModelsDev(t)
	hits := modelsDevTestServer(t, http.StatusBadGateway, "down")
	writeModelsDevTestCache(t, modelsDevTestJSON, 2*ModelsDevCacheTTL)

	if err := LoadModelsDev(context.Background()); err != nil {
		t.Fatalf("stale cache must absorb a failed fetch, got %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("expected one fetch attempt, got %d", hits.Load())
	}
	if m, ok := ModelsDevMeta("vertex", "gemini-3.7-flash"); !ok || m.Name != "Gemini 3.7 Flash" {
		t.Errorf("stale contents must be served: ok=%v %+v", ok, m)
	}
}

func TestLoadModelsDev_NoCacheNetworkFailure(t *testing.T) {
	modelsDevTestHome(t)
	resetModelsDev(t)
	modelsDevTestServer(t, http.StatusBadGateway, "down")

	if err := LoadModelsDev(context.Background()); err == nil {
		t.Fatal("no cache + failed fetch must error")
	}
	if ModelsDevLoaded() {
		t.Error("a failed load must not mark the catalog loaded")
	}
	if _, ok := ModelsDevMeta("vertex", "gemini-3.7-flash"); ok {
		t.Error("nothing may resolve after a failed load")
	}
}

func TestLoadModelsDev_RejectsPayloadWithoutMappedProviders(t *testing.T) {
	modelsDevTestHome(t)
	resetModelsDev(t)
	modelsDevTestServer(t, http.StatusOK, `{"cohere": {"models": {}}}`)

	if err := LoadModelsDev(context.Background()); err == nil {
		t.Fatal("a payload with none of ask's providers must error")
	}
	if ModelsDevLoaded() {
		t.Error("rejected payload must not mark the catalog loaded")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "ask", "cache", "models-dev.json")); err == nil {
		t.Error("rejected payload must not be cached")
	}
}

func TestModelsDevMeta_NotLoadedMisses(t *testing.T) {
	resetModelsDev(t)
	if _, ok := ModelsDevMeta("vertex", "gemini-3.7-flash"); ok {
		t.Error("lookup before any load must miss")
	}
	if ModelsDevLoaded() {
		t.Error("ModelsDevLoaded must be false before any load")
	}
}
