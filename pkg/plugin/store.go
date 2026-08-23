package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/config"
)

// On-disk state, all under ~/.config/ask/plugins/ (machine-local) except
// the project file, which lives at <project root>/.ask/plugins.json and
// is meant to be committed:
//
//	known_marketplaces.json   user-scope marketplaces (Claude Code shape)
//	marketplaces/<name>/      git clones (directory sources stay in place)
//	cache/<mkt>/<plugin>/<v>/ installed plugin copies
//	installed_plugins.json    install records (Claude Code v2 shape + scope)
//	<root>/.ask/plugins.json  project-scope marketplaces + enabled plugins

// Scope says where a marketplace registration or plugin enablement lives.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// NormalizeScope maps "" to user and validates the rest.
func NormalizeScope(s string) (Scope, error) {
	switch strings.TrimSpace(s) {
	case "", string(ScopeUser):
		return ScopeUser, nil
	case string(ScopeProject):
		return ScopeProject, nil
	}
	return "", fmt.Errorf("unknown scope %q: valid scopes are %q and %q", s, ScopeUser, ScopeProject)
}

// Now is the clock for timestamps; swappable for tests.
var Now = time.Now

var stateMu sync.Mutex

// withLock serializes read-modify-write cycles over the state files.
func withLock(fn func() error) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	return fn()
}

// RootDir is ~/.config/ask/plugins.
func RootDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "ask", "plugins")
}

func MarketplacesDir() string {
	if r := RootDir(); r != "" {
		return filepath.Join(r, "marketplaces")
	}
	return ""
}

func CacheDir() string {
	if r := RootDir(); r != "" {
		return filepath.Join(r, "cache")
	}
	return ""
}

func knownMarketplacesPath() string {
	if r := RootDir(); r != "" {
		return filepath.Join(r, "known_marketplaces.json")
	}
	return ""
}

func installedPath() string {
	if r := RootDir(); r != "" {
		return filepath.Join(r, "installed_plugins.json")
	}
	return ""
}

// ProjectFilePath is <project root>/.ask/plugins.json.
func ProjectFilePath(cwd string) string {
	if cwd == "" {
		return ""
	}
	root := config.ProjectRoot(cwd)
	if root == "" {
		root = cwd
	}
	return filepath.Join(root, ".ask", "plugins.json")
}

// KnownMarketplace is one registration — the value shape of Claude Code's
// known_marketplaces.json.
type KnownMarketplace struct {
	Source          MarketplaceSource `json:"source"`
	InstallLocation string            `json:"installLocation,omitempty"`
	LastUpdated     string            `json:"lastUpdated,omitempty"`
	AutoUpdate      *bool             `json:"autoUpdate,omitempty"`
}

func readKnownMarketplaces() map[string]KnownMarketplace {
	out := map[string]KnownMarketplace{}
	if p := knownMarketplacesPath(); p != "" {
		_ = readJSON(p, &out)
	}
	if out == nil {
		out = map[string]KnownMarketplace{}
	}
	return out
}

func writeKnownMarketplaces(m map[string]KnownMarketplace) error {
	p := knownMarketplacesPath()
	if p == "" {
		return fmt.Errorf("no home directory: cannot persist marketplaces")
	}
	if len(m) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeJSONAtomic(p, m)
}

// ProjectFile is <root>/.ask/plugins.json.
type ProjectFile struct {
	Marketplaces map[string]KnownMarketplace `json:"marketplaces,omitempty"`
	Enabled      map[string]bool             `json:"enabled,omitempty"`
	Published    map[string]Publication      `json:"published,omitempty"`
}

// ReadProjectFile loads the project's plugin file (zero value when absent).
func ReadProjectFile(cwd string) ProjectFile {
	var pf ProjectFile
	if p := ProjectFilePath(cwd); p != "" {
		_ = readJSON(p, &pf)
	}
	return pf
}

func writeProjectFile(cwd string, pf ProjectFile) error {
	p := ProjectFilePath(cwd)
	if p == "" {
		return fmt.Errorf("no project directory")
	}
	if len(pf.Marketplaces) == 0 && len(pf.Enabled) == 0 && len(pf.Published) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		removeDirIfEmpty(filepath.Dir(p))
		return nil
	}
	return writeJSONAtomic(p, pf)
}

// InstalledRecord is one install of a plugin, keyed by "name@marketplace"
// in installed_plugins.json.
type InstalledRecord struct {
	Scope        Scope  `json:"scope"`
	ProjectRoot  string `json:"projectRoot,omitempty"`
	InstallPath  string `json:"installPath"`
	Version      string `json:"version,omitempty"`
	InstalledAt  string `json:"installedAt,omitempty"`
	LastUpdated  string `json:"lastUpdated,omitempty"`
	GitCommitSha string `json:"gitCommitSha,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
	Entry        *Entry `json:"entry,omitempty"`
}

func (r InstalledRecord) enabled() bool { return r.Enabled == nil || *r.Enabled }

type installedFile struct {
	Version int                          `json:"version"`
	Plugins map[string][]InstalledRecord `json:"plugins"`
}

func readInstalled() installedFile {
	f := installedFile{Version: 2}
	if p := installedPath(); p != "" {
		_ = readJSON(p, &f)
	}
	if f.Plugins == nil {
		f.Plugins = map[string][]InstalledRecord{}
	}
	if f.Version == 0 {
		f.Version = 2
	}
	return f
}

func writeInstalled(f installedFile) error {
	p := installedPath()
	if p == "" {
		return fmt.Errorf("no home directory: cannot persist installed plugins")
	}
	for k, recs := range f.Plugins {
		if len(recs) == 0 {
			delete(f.Plugins, k)
		}
	}
	if len(f.Plugins) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeJSONAtomic(p, f)
}

func timestamp() string { return Now().UTC().Format(time.RFC3339) }
