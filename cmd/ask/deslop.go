package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// deslopSidecarVersion tags the on-disk sidecar shape for future migrations.
const deslopSidecarVersion = 1

// deslopSidecar is the per-session sidecar that maps a raw assistant block to
// its display-time desloped rewrite. It lives in <sessionID>.deslop.json next
// to the raw session file, which is never modified: the ADK transcript keeps
// the raw model output, and this file lets /resume show the same cleaned text
// the user saw live without re-running the rewrite model.
type deslopSidecar struct {
	Version int               `json:"version"`
	Entries map[string]string `json:"entries"`
}

// deslopBlockKey is the stable cross-path key for one assistant block: a hash
// of the raw block text. Live emit and /resume replay both reconstruct the same
// trimmed join of an event's non-thought text parts, so hashing that string
// matches without threading any event id through the transcript.
func deslopBlockKey(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// deslopSidecarMu serializes read-modify-write of a session's sidecar file.
var deslopSidecarMu sync.Mutex

func (st *agentSessionStore) deslopPathFor(id, cwd string) (string, error) {
	dir, err := st.svc(cwd).DirFor(cwd)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".deslop.json"), nil
}

// loadDeslop reads the sidecar map for a session, returning an empty (non-nil)
// map when none exists yet.
func (st *agentSessionStore) loadDeslop(id, cwd string) map[string]string {
	path, err := st.deslopPathFor(id, cwd)
	if err != nil {
		return map[string]string{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	var sc deslopSidecar
	if err := json.Unmarshal(data, &sc); err != nil || sc.Entries == nil {
		return map[string]string{}
	}
	return sc.Entries
}

// mergeDeslop merges new raw→desloped entries into the session's sidecar and
// writes it back atomically. New entries win over stored ones.
func (st *agentSessionStore) mergeDeslop(id, cwd string, entries map[string]string) error {
	if len(entries) == 0 {
		return nil
	}
	deslopSidecarMu.Lock()
	defer deslopSidecarMu.Unlock()

	merged := st.loadDeslop(id, cwd)
	for k, v := range entries {
		merged[k] = v
	}
	path, err := st.deslopPathFor(id, cwd)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(deslopSidecar{Version: deslopSidecarVersion, Entries: merged}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
