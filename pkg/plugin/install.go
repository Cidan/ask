package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cidan/ask/pkg/config"
)

// Ref names a plugin as "name@marketplace".
type Ref struct {
	Plugin      string
	Marketplace string
}

// ParseRef splits "name@marketplace".
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, "@")
	if i <= 0 || i == len(s)-1 {
		return Ref{}, fmt.Errorf("plugin reference %q must be name@marketplace", s)
	}
	return Ref{Plugin: s[:i], Marketplace: s[i+1:]}, nil
}

func (r Ref) String() string { return r.Plugin + "@" + r.Marketplace }

// Installed is a plugin enabled for the current project/user.
type Installed struct {
	Ref    Ref
	Scope  Scope
	Scopes []Scope
	// Dir is the installed copy; "" (and Missing) when the project file
	// enables a plugin this machine has not fetched yet.
	Dir      string
	Version  string
	Missing  bool
	Enabled  bool
	Entry    *Entry
	Manifest *PluginManifest
}

// Contents lists what the installed copy contributes.
func (in Installed) Contents() Contents {
	if in.Dir == "" {
		return Contents{}
	}
	return ResolveContents(in.Dir, in.Entry, in.Manifest)
}

// Description prefers the plugin's own manifest over the catalog entry.
func (in Installed) Description() string {
	if in.Manifest != nil && in.Manifest.Description != "" {
		return in.Manifest.Description
	}
	if in.Entry != nil {
		return in.Entry.Description
	}
	return ""
}

func projectRootOf(cwd string) string {
	if cwd == "" {
		return ""
	}
	if root := config.ProjectRoot(cwd); root != "" {
		return root
	}
	return cwd
}

// InstallPlugin fetches ref's plugin into the cache and enables it in scope.
func InstallPlugin(ctx context.Context, cwd string, ref Ref, scope Scope) (Installed, error) {
	m, ok := FindMarketplace(cwd, ref.Marketplace)
	if !ok {
		return Installed{}, fmt.Errorf("marketplace %q is not registered", ref.Marketplace)
	}
	if !m.Fetched() {
		if err := RefreshMarketplace(ctx, cwd, m); err != nil {
			return Installed{}, fmt.Errorf("marketplace %q: %w", ref.Marketplace, err)
		}
		m.load()
		if !m.Fetched() {
			return Installed{}, fmt.Errorf("marketplace %q: %s", ref.Marketplace, m.Err)
		}
	}
	entry, ok := m.Entry(ref.Plugin)
	if !ok {
		return Installed{}, fmt.Errorf("plugin %q is not in marketplace %q", ref.Plugin, ref.Marketplace)
	}
	srcDir, sha, cleanup, err := materializeEntry(ctx, m, entry)
	if err != nil {
		return Installed{}, err
	}
	defer cleanup()

	version := entry.Version
	if version == "" {
		switch {
		case sha != "":
			version = shortSHA(sha)
		default:
			version = "latest"
		}
	}
	cache := CacheDir()
	if cache == "" {
		return Installed{}, fmt.Errorf("no home directory: cannot install plugins")
	}
	dest := filepath.Join(cache, ref.Marketplace, ref.Plugin, version)
	if err := os.RemoveAll(dest); err != nil {
		return Installed{}, err
	}
	if err := copyDir(srcDir, dest); err != nil {
		_ = os.RemoveAll(dest)
		return Installed{}, fmt.Errorf("install %s: %w", ref, err)
	}
	entryCopy := entry
	rec := InstalledRecord{
		Scope:        scope,
		InstallPath:  dest,
		Version:      version,
		InstalledAt:  timestamp(),
		LastUpdated:  timestamp(),
		GitCommitSha: sha,
		Entry:        &entryCopy,
	}
	root := projectRootOf(cwd)
	if scope == ScopeProject {
		rec.ProjectRoot = root
	}
	err = withLock(func() error {
		f := readInstalled()
		key := ref.String()
		var kept []InstalledRecord
		for _, r := range f.Plugins[key] {
			if r.Scope == scope && (scope == ScopeUser || r.ProjectRoot == root) {
				if r.InstalledAt != "" {
					rec.InstalledAt = r.InstalledAt
				}
				continue
			}
			kept = append(kept, r)
		}
		f.Plugins[key] = append(kept, rec)
		if err := writeInstalled(f); err != nil {
			return err
		}
		if scope != ScopeProject {
			return nil
		}
		pf := ReadProjectFile(cwd)
		if pf.Enabled == nil {
			pf.Enabled = map[string]bool{}
		}
		pf.Enabled[key] = true
		if pf.Marketplaces == nil {
			pf.Marketplaces = map[string]KnownMarketplace{}
		}
		if _, ok := pf.Marketplaces[m.Name]; !ok {
			pf.Marketplaces[m.Name] = KnownMarketplace{Source: m.Source}
		}
		return writeProjectFile(cwd, pf)
	})
	if err != nil {
		return Installed{}, err
	}
	man, _ := ReadPluginManifest(dest)
	return Installed{
		Ref:      ref,
		Scope:    scope,
		Scopes:   []Scope{scope},
		Dir:      dest,
		Version:  version,
		Enabled:  true,
		Entry:    &entryCopy,
		Manifest: man,
	}, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// EntryLocalDir returns the plugin directory for a path-source entry
// inside the marketplace's local clone, so its contents can be listed
// before installing. Remote entries have no local directory.
func EntryLocalDir(m Marketplace, entry Entry) (string, bool) {
	if entry.Source.Remote() || m.Dir == "" {
		return "", false
	}
	rel := entry.Source.Path
	if m.Manifest != nil && m.Manifest.Metadata != nil && m.Manifest.Metadata.PluginRoot != "" &&
		!strings.HasPrefix(rel, "./") && !strings.HasPrefix(rel, "../") && !strings.HasPrefix(rel, "/") {
		rel = filepath.Join(m.Manifest.Metadata.PluginRoot, rel)
	}
	abs, ok := resolveInside(m.Dir, rel)
	if !ok || !dirExists(abs) {
		return "", false
	}
	return abs, true
}

// materializeEntry resolves the entry's plugin directory on disk: inside
// the marketplace clone for path sources, a fresh clone for git-backed
// ones. cleanup removes any temporary clone.
func materializeEntry(ctx context.Context, m Marketplace, entry Entry) (dir, sha string, cleanup func(), err error) {
	cleanup = func() {}
	src := entry.Source
	if !src.Remote() {
		if m.Dir == "" {
			return "", "", cleanup, fmt.Errorf("marketplace %q has no local directory", m.Name)
		}
		rel := src.Path
		if m.Manifest != nil && m.Manifest.Metadata != nil && m.Manifest.Metadata.PluginRoot != "" &&
			!strings.HasPrefix(rel, "./") && !strings.HasPrefix(rel, "../") && !strings.HasPrefix(rel, "/") {
			rel = filepath.Join(m.Manifest.Metadata.PluginRoot, rel)
		}
		abs, ok := resolveInside(m.Dir, rel)
		if !ok || !dirExists(abs) {
			return "", "", cleanup, fmt.Errorf("plugin %q: source %q is not a directory inside marketplace %q", entry.Name, src.Path, m.Name)
		}
		if gitIsRepo(m.Dir) {
			sha = gitHeadSHA(ctx, m.Dir)
		}
		return abs, sha, cleanup, nil
	}
	url := src.GitURL()
	if url == "" {
		return "", "", cleanup, fmt.Errorf("plugin %q: unsupported source kind %q", entry.Name, src.Kind)
	}
	cache := CacheDir()
	if cache == "" {
		return "", "", cleanup, fmt.Errorf("no home directory: cannot install plugins")
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", "", cleanup, err
	}
	tmp, err := os.MkdirTemp(cache, ".tmp-*")
	if err != nil {
		return "", "", cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	if err := gitClone(ctx, url, tmp); err != nil {
		return "", "", cleanup, err
	}
	switch {
	case src.SHA != "":
		if err := gitCheckout(ctx, tmp, src.SHA); err != nil {
			return "", "", cleanup, err
		}
	case src.Ref != "":
		if err := gitCheckout(ctx, tmp, src.Ref); err != nil {
			return "", "", cleanup, err
		}
	}
	sha = gitHeadSHA(ctx, tmp)
	dir = tmp
	if src.Path != "" {
		abs, ok := resolveInside(tmp, src.Path)
		if !ok || !dirExists(abs) {
			return "", "", cleanup, fmt.Errorf("plugin %q: path %q not found in %s", entry.Name, src.Path, url)
		}
		dir = abs
	}
	return dir, sha, cleanup, nil
}

// UninstallPlugin disables ref in scope; the cached copy is removed once
// no scope references it.
func UninstallPlugin(cwd string, ref Ref, scope Scope) error {
	root := projectRootOf(cwd)
	return withLock(func() error {
		f := readInstalled()
		key := ref.String()
		var kept []InstalledRecord
		removed := false
		for _, r := range f.Plugins[key] {
			if r.Scope == scope && (scope == ScopeUser || r.ProjectRoot == root) {
				removed = true
				continue
			}
			kept = append(kept, r)
		}
		f.Plugins[key] = kept
		if scope == ScopeProject {
			pf := ReadProjectFile(cwd)
			if pf.Enabled[key] {
				removed = true
				delete(pf.Enabled, key)
				if err := writeProjectFile(cwd, pf); err != nil {
					return err
				}
			}
		}
		if !removed {
			return fmt.Errorf("plugin %s is not installed in %s scope", key, scope)
		}
		if err := writeInstalled(f); err != nil {
			return err
		}
		if len(kept) == 0 {
			if cache := CacheDir(); cache != "" {
				dir := filepath.Join(cache, ref.Marketplace, ref.Plugin)
				if insideDir(cache, dir) {
					_ = os.RemoveAll(dir)
					removeDirIfEmpty(filepath.Join(cache, ref.Marketplace))
				}
			}
		}
		return nil
	})
}

// SetEnabled flips a user-scope install on or off without removing it.
func SetEnabled(ref Ref, enabled bool) error {
	return withLock(func() error {
		f := readInstalled()
		key := ref.String()
		found := false
		for i, r := range f.Plugins[key] {
			if r.Scope == ScopeUser {
				v := enabled
				f.Plugins[key][i].Enabled = &v
				found = true
			}
		}
		if !found {
			return fmt.Errorf("plugin %s is not installed in user scope", key)
		}
		return writeInstalled(f)
	})
}

// EnabledPlugins resolves every plugin enabled for cwd — user installs
// plus the project file — in ref order. Entries the project enables but
// this machine has not fetched come back Missing.
func EnabledPlugins(cwd string) []Installed {
	f := readInstalled()
	pf := ReadProjectFile(cwd)
	root := projectRootOf(cwd)
	byRef := map[string]*Installed{}
	var keys []string
	add := func(key string, rec *InstalledRecord, scope Scope) {
		if in, ok := byRef[key]; ok {
			in.Scopes = append(in.Scopes, scope)
			return
		}
		ref, err := ParseRef(key)
		if err != nil {
			return
		}
		in := &Installed{Ref: ref, Scope: scope, Scopes: []Scope{scope}, Enabled: true}
		if rec != nil {
			in.Version = rec.Version
			in.Entry = rec.Entry
			if dirExists(rec.InstallPath) {
				in.Dir = rec.InstallPath
			}
		}
		if in.Dir == "" {
			in.Dir = probeCache(ref)
		}
		if in.Dir == "" {
			in.Missing = true
			if in.Entry == nil {
				if m, ok := FindMarketplace(cwd, ref.Marketplace); ok {
					if e, ok := m.Entry(ref.Plugin); ok {
						in.Entry = &e
					}
				}
			}
		} else {
			in.Manifest, _ = ReadPluginManifest(in.Dir)
		}
		byRef[key] = in
		keys = append(keys, key)
	}
	for _, key := range sortedKeys(f.Plugins) {
		for i := range f.Plugins[key] {
			r := &f.Plugins[key][i]
			if r.Scope == ScopeUser && r.enabled() {
				add(key, r, ScopeUser)
			}
		}
	}
	for _, key := range sortedKeys(pf.Enabled) {
		if !pf.Enabled[key] {
			continue
		}
		add(key, bestRecord(f, key, root), ScopeProject)
	}
	sort.Strings(keys)
	out := make([]Installed, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byRef[k])
	}
	return out
}

// InstalledPlugins lists every install record on this machine (enabled or
// not) plus the project's enabled set, for the browser's installed lens.
func InstalledPlugins(cwd string) []Installed {
	enabled := EnabledPlugins(cwd)
	seen := map[string]bool{}
	for _, in := range enabled {
		seen[in.Ref.String()] = true
	}
	f := readInstalled()
	for _, key := range sortedKeys(f.Plugins) {
		if seen[key] {
			continue
		}
		for i := range f.Plugins[key] {
			r := f.Plugins[key][i]
			if r.Scope != ScopeUser {
				continue
			}
			ref, err := ParseRef(key)
			if err != nil {
				continue
			}
			in := Installed{Ref: ref, Scope: ScopeUser, Scopes: []Scope{ScopeUser}, Version: r.Version, Entry: r.Entry, Enabled: false}
			if dirExists(r.InstallPath) {
				in.Dir = r.InstallPath
				in.Manifest, _ = ReadPluginManifest(in.Dir)
			} else {
				in.Missing = true
			}
			enabled = append(enabled, in)
			seen[key] = true
			break
		}
	}
	sort.Slice(enabled, func(i, j int) bool { return enabled[i].Ref.String() < enabled[j].Ref.String() })
	return enabled
}

// FindInstalled returns the enabled install for ref, if any.
func FindInstalled(cwd string, ref Ref) (Installed, bool) {
	for _, in := range EnabledPlugins(cwd) {
		if in.Ref == ref {
			return in, true
		}
	}
	return Installed{}, false
}

func bestRecord(f installedFile, key, root string) *InstalledRecord {
	recs := f.Plugins[key]
	for i := range recs {
		if recs[i].Scope == ScopeProject && recs[i].ProjectRoot == root {
			return &recs[i]
		}
	}
	for i := range recs {
		if dirExists(recs[i].InstallPath) {
			return &recs[i]
		}
	}
	if len(recs) > 0 {
		return &recs[0]
	}
	return nil
}

func probeCache(ref Ref) string {
	cache := CacheDir()
	if cache == "" {
		return ""
	}
	base := filepath.Join(cache, ref.Marketplace, ref.Plugin)
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return ""
	}
	sort.Strings(versions)
	return filepath.Join(base, versions[len(versions)-1])
}

// SyncReport is what SyncProject did.
type SyncReport struct {
	Installed []Ref
	Errors    []error
}

// SyncProject fetches every plugin the project file enables but this
// machine lacks, registering the project's marketplaces as needed.
func SyncProject(ctx context.Context, cwd string) SyncReport {
	var rep SyncReport
	for _, in := range EnabledPlugins(cwd) {
		if !in.Missing {
			continue
		}
		if _, err := InstallPlugin(ctx, cwd, in.Ref, ScopeProject); err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("%s: %w", in.Ref, err))
			continue
		}
		rep.Installed = append(rep.Installed, in.Ref)
	}
	return rep
}
