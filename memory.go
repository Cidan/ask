package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/Cidan/memmy"
)

// memoryGeminiEmbedderDim is the on-disk dim for the production
// (Gemini) embedder. Keep stable; changing it requires migrating the
// bbolt file. 768 is a documented Gemini output_dimensionality that
// trades cost vs the 3072 default while still giving meaningful
// semantic separation.
const memoryGeminiEmbedderDim = 768

// memoryFakeEmbedderDim is the on-disk dim for the fake embedder.
// Tests use this; production never touches it.
const memoryFakeEmbedderDim = 64

// memoryGeminiEmbedderModel pins the production model so memory built
// with one model can't be silently joined to a corpus built with
// another. Bumping this is a corpus-incompatible change and requires
// blowing away memory.db.
const memoryGeminiEmbedderModel = "gemini-embedding-001"

// memoryTenantScope is the fixed value of the `scope` tenant key for
// every Write/Recall ask issues. Pinning it (via Enum on the schema)
// keeps the door open for other harnesses to coexist on the same DB
// later without colliding with ask-owned data.
const memoryTenantScope = "ask"

// memoryService is the process-wide lazy singleton (DESIGN.md §0:
// "transport adapters wrap a single MemoryService"). The /config →
// Memory toggle is per-machine, so one Service per ask process is the
// right granularity. Tabs share it; per-tab tenancy is enforced via
// the `project: <cwd>` tenant tuple at call time.
//
// Toggling the config flag from the UI calls openMemoryService /
// closeMemoryService. Both are idempotent — repeated open while open
// is a no-op, repeated close while closed is a no-op — so the picker
// handler doesn't have to reason about prior state.
var (
	memoryMu     sync.Mutex
	memorySvc    memmy.Service
	memoryCloser io.Closer
	memorySchema *memmy.TenantSchema
)

// errMemoryNoKey signals that memory was toggled on but no Gemini key
// is configured. Surfaced as a toast in the picker; the persisted
// Enabled flag is left alone so a key paste retries the open.
var errMemoryNoKey = errors.New("memory: GeminiKey is required (paste one in /config → Memory)")

// memoryDBPath returns the absolute path to the bbolt database file
// for the active embedder. The path is suffixed with the embedder
// type so switching embedders cannot collide with a prior corpus's
// dim — bbolt validates dim at open and would otherwise refuse to
// reopen a file written by a different embedder.
//
// We resolve through $HOME (not XDG_DATA_HOME) because isolateHome in
// tests pins $HOME at a tmp dir, and we want test runs to land their
// db there without each test having to also set XDG_DATA_HOME.
func memoryDBPath(useFake bool) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := "memory.db"
	if useFake {
		name = "memory-fake.db"
	}
	return filepath.Join(home, ".local", "share", "ask", name), nil
}

// memoryTenantSchema constructs the tuple validator that gates every
// Write / Recall. `project` carries the absolute cwd so per-project
// memory partitions cleanly even with one machine-wide toggle, and
// `scope` is pinned to "ask" so future harnesses on the same DB can
// be discriminated. The schema is intentionally minimal — extra keys
// are not allowed.
func memoryTenantSchema() (*memmy.TenantSchema, error) {
	return memmy.NewTenantSchema(memmy.TenantSchemaConfig{
		Description: "ask harness — per-project memory",
		Keys: map[string]memmy.TenantKeyConfig{
			"project": {Required: true},
			"scope":   {Required: true, Enum: []string{memoryTenantScope}},
		},
	})
}

// openMemoryService brings the singleton up using the production
// (Gemini) embedder, reading the API key from the supplied config.
// Idempotent. Returns errMemoryNoKey when the key is missing — the
// caller should report that distinctly from a real open failure so
// the user can paste a key without seeing a generic crash message.
func openMemoryService(cfg askConfig) error {
	if cfg.Memory.GeminiKey == "" {
		return errMemoryNoKey
	}
	emb, err := memmy.NewGeminiEmbedder(context.Background(), memmy.GeminiEmbedderOptions{
		APIKey: cfg.Memory.GeminiKey,
		Model:  memoryGeminiEmbedderModel,
		Dim:    memoryGeminiEmbedderDim,
	})
	if err != nil {
		return fmt.Errorf("memory gemini init: %w", err)
	}
	return openMemoryServiceWith(emb, false)
}

// openMemoryServiceWith is the lower-level open path used by the
// production constructor and by tests (which pass a fake embedder).
// useFake selects the per-embedder bbolt path so the production and
// test corpora stay distinct on disk.
func openMemoryServiceWith(emb memmy.Embedder, useFake bool) error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	if memorySvc != nil {
		return nil
	}
	path, err := memoryDBPath(useFake)
	if err != nil {
		return fmt.Errorf("memory db path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("memory mkdir %s: %w", filepath.Dir(path), err)
	}
	schema, err := memoryTenantSchema()
	if err != nil {
		return fmt.Errorf("memory tenant schema: %w", err)
	}
	svc, closer, err := memmy.Open(memmy.Options{
		DBPath:       path,
		Embedder:     emb,
		TenantSchema: schema,
	})
	if err != nil {
		return fmt.Errorf("memory open: %w", err)
	}
	memorySvc = svc
	memoryCloser = closer
	memorySchema = schema
	return nil
}

// closeMemoryService tears the singleton down. Idempotent. Safe to call
// during process shutdown even if the service was never opened.
func closeMemoryService() error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	if memorySvc == nil {
		return nil
	}
	err := memoryCloser.Close()
	memorySvc = nil
	memoryCloser = nil
	memorySchema = nil
	return err
}

// memoryServiceOpen reports whether the singleton is currently open.
// Used by the /config picker to render its `on/off` summary and by
// tests as a black-box assertion.
func memoryServiceOpen() bool {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	return memorySvc != nil
}

// memoryConfigEnabled is the canonical on-disk truth. The picker reads
// it before deciding whether to flip; startup reads it to decide
// whether to attempt openMemoryService.
func memoryConfigEnabled(cfg askConfig) bool {
	return cfg.Memory.Enabled != nil && *cfg.Memory.Enabled
}

// memoryStatsLine returns a one-line human summary of the live store
// suitable for a toast / status row. Empty string when the service is
// not open or stats fail (we never want a stats hiccup to surface as a
// scary error in the picker).
func memoryStatsLine() string {
	memoryMu.Lock()
	svc := memorySvc
	memoryMu.Unlock()
	if svc == nil {
		return ""
	}
	res, err := svc.Stats(context.Background(), memmy.StatsRequest{})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d node(s), %d edge(s)", res.NodeCount, res.MemoryEdgeCount)
}

// memoryTenant builds the per-tab tenant tuple for memmy ops. The
// scope is always "ask"; project is the tab's cwd. Returns nil when
// either field is missing — the schema rejects empty values, and we
// would rather skip a memory op than feed bad data.
func memoryTenant(cwd string) map[string]string {
	if cwd == "" {
		return nil
	}
	return map[string]string{
		"project": cwd,
		"scope":   memoryTenantScope,
	}
}

// memoryRecall queries the live service and returns hits. Returns
// (nil, nil) when the service is not open — callers that want
// fail-soft semantics can ignore both results. ctx is propagated
// straight to memmy.
func memoryRecall(ctx context.Context, cwd, query string, k int) ([]memmy.RecallHit, error) {
	memoryMu.Lock()
	svc := memorySvc
	memoryMu.Unlock()
	if svc == nil {
		return nil, nil
	}
	tenant := memoryTenant(cwd)
	if tenant == nil {
		return nil, nil
	}
	res, err := svc.Recall(ctx, memmy.RecallRequest{
		Tenant: tenant,
		Query:  query,
		K:      k,
	})
	if err != nil {
		return nil, err
	}
	return res.Results, nil
}

// memoryWrite records an observation. Returns nil and skips silently
// when the service is closed; this lets the hook handlers call
// memoryWrite unconditionally without first checking whether memory
// is enabled.
func memoryWrite(ctx context.Context, cwd, message string) error {
	memoryMu.Lock()
	svc := memorySvc
	memoryMu.Unlock()
	if svc == nil {
		return nil
	}
	tenant := memoryTenant(cwd)
	if tenant == nil {
		return nil
	}
	_, err := svc.Write(ctx, memmy.WriteRequest{
		Tenant:  tenant,
		Message: message,
	})
	return err
}
