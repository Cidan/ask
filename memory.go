package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/Cidan/memmy"
)

// memoryEmbedderDim is the dimensionality baked into the on-disk vector
// index. The fake embedder is used for the first slice — semantically
// meaningless but deterministic, offline, and zero-config — so this dim
// is just "any consistent number" until we decide on the real embedder
// story. Changing it requires deleting (or migrating) the bbolt file
// because the storage validates dim against the embedder at Open.
const memoryEmbedderDim = 64

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

// memoryDBPath returns the absolute path to the bbolt database file.
// We resolve through $HOME (not XDG_DATA_HOME) because isolateHome in
// tests pins $HOME at a tmp dir, and we want test runs to land their
// db there without each test having to also set XDG_DATA_HOME. The
// fixed-path decision is deliberate (per the integration plan): power
// users who need a different path can edit ask.json once we add the
// schema, but the modal does not expose it.
func memoryDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "ask", "memory.db"), nil
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

// openMemoryService brings the singleton up. Idempotent. Returns the
// open error verbatim so the caller can surface it (toast / log) — the
// caller, not us, decides whether to revert the toggle.
func openMemoryService() error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	if memorySvc != nil {
		return nil
	}
	path, err := memoryDBPath()
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
		Embedder:     memmy.NewFakeEmbedder(memoryEmbedderDim),
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
