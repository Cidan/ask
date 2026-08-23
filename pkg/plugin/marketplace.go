package plugin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
)

// Marketplace is one registered marketplace plus its fetched catalog.
type Marketplace struct {
	Name   string
	Scope  Scope
	Source MarketplaceSource
	// Dir is the local clone (or the directory itself for directory
	// sources). It may not exist yet on this machine for a marketplace a
	// teammate registered in .ask/plugins.json.
	Dir      string
	Manifest *MarketplaceManifest
	// Err explains why Manifest is nil (not fetched, malformed).
	Err string
}

// Writable reports whether Publish can land files in this marketplace: a
// directory or git clone we hold locally. A marketplace fetched as a bare
// marketplace.json URL has nowhere to put a plugin.
func (m Marketplace) Writable() bool {
	return m.Dir != "" && m.Source.Kind != MarketplaceSourceURL && dirExists(m.Dir)
}

// Fetched reports whether the catalog is available locally.
func (m Marketplace) Fetched() bool { return m.Manifest != nil }

// Entry returns the catalog entry for plugin name.
func (m Marketplace) Entry(name string) (Entry, bool) { return m.Manifest.Entry(name) }

func marketplaceDir(name string, src MarketplaceSource, installLocation string) string {
	if src.Kind == MarketplaceSourceDirectory {
		return src.Path
	}
	if installLocation != "" && dirExists(installLocation) {
		return installLocation
	}
	if d := MarketplacesDir(); d != "" {
		return filepath.Join(d, name)
	}
	return ""
}

func (m *Marketplace) load() {
	m.Manifest = nil
	m.Err = ""
	if m.Dir == "" || !dirExists(m.Dir) {
		m.Err = "not fetched on this machine"
		return
	}
	man, err := ReadMarketplaceManifest(m.Dir)
	if err != nil {
		m.Err = err.Error()
		return
	}
	m.Manifest = man
}

// ListMarketplaces merges the user registrations with the project file.
// A name registered in both scopes is listed once, as the user's.
func ListMarketplaces(cwd string) []Marketplace {
	var out []Marketplace
	seen := map[string]bool{}
	add := func(name string, km KnownMarketplace, scope Scope) {
		if seen[name] {
			return
		}
		seen[name] = true
		m := Marketplace{Name: name, Scope: scope, Source: km.Source, Dir: marketplaceDir(name, km.Source, km.InstallLocation)}
		m.load()
		out = append(out, m)
	}
	known := readKnownMarketplaces()
	for _, name := range sortedKeys(known) {
		add(name, known[name], ScopeUser)
	}
	pf := ReadProjectFile(cwd)
	for _, name := range sortedKeys(pf.Marketplaces) {
		add(name, pf.Marketplaces[name], ScopeProject)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FindMarketplace looks a registered marketplace up by name.
func FindMarketplace(cwd, name string) (Marketplace, bool) {
	for _, m := range ListMarketplaces(cwd) {
		if m.Name == name {
			return m, true
		}
	}
	return Marketplace{}, false
}

// AddMarketplace registers the marketplace at raw (owner/repo, git URL,
// directory, or marketplace.json URL) in scope, fetching its catalog.
// Re-adding a source that is already registered is idempotent.
func AddMarketplace(ctx context.Context, cwd, raw string, scope Scope) (Marketplace, error) {
	src, err := ParseMarketplaceSource(cwd, raw)
	if err != nil {
		return Marketplace{}, err
	}
	for _, m := range ListMarketplaces(cwd) {
		if m.Source.Equal(src) {
			if !m.Fetched() {
				if err := RefreshMarketplace(ctx, cwd, m); err != nil {
					return Marketplace{}, err
				}
				m.load()
			}
			return registerMarketplace(cwd, m, scope)
		}
	}
	dir, man, err := fetchMarketplace(ctx, src)
	if err != nil {
		return Marketplace{}, err
	}
	temporary := src.Kind != MarketplaceSourceDirectory
	if existing, ok := FindMarketplace(cwd, man.Name); ok && !existing.Source.Equal(src) {
		if temporary {
			_ = os.RemoveAll(dir)
		}
		return Marketplace{}, fmt.Errorf("marketplace %q is already registered from %s", man.Name, existing.Source.Display())
	}
	final := dir
	if temporary {
		final = marketplaceDir(man.Name, src, "")
		if final == "" {
			_ = os.RemoveAll(dir)
			return Marketplace{}, fmt.Errorf("no home directory: cannot store marketplace %q", man.Name)
		}
		_ = os.RemoveAll(final)
		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return Marketplace{}, err
		}
		if err := os.Rename(dir, final); err != nil {
			_ = os.RemoveAll(dir)
			return Marketplace{}, err
		}
	}
	m := Marketplace{Name: man.Name, Source: src, Dir: final, Manifest: man}
	return registerMarketplace(cwd, m, scope)
}

// HTTPClient fetches marketplace.json URLs; swappable for tests.
var HTTPClient = http.DefaultClient

const maxMarketplaceJSON = 4 << 20

func fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxMarketplaceJSON))
}

func fetchMarketplace(ctx context.Context, src MarketplaceSource) (string, *MarketplaceManifest, error) {
	switch src.Kind {
	case MarketplaceSourceDirectory:
		man, err := ReadMarketplaceManifest(src.Path)
		if err != nil {
			return "", nil, fmt.Errorf("%s is not a marketplace: %w", src.Path, err)
		}
		return src.Path, man, nil
	case MarketplaceSourceGitHub, MarketplaceSourceGit:
		tmp, err := tempMarketplaceDir()
		if err != nil {
			return "", nil, err
		}
		if err := gitClone(ctx, src.GitURL(), tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return "", nil, err
		}
		man, err := ReadMarketplaceManifest(tmp)
		if err != nil {
			_ = os.RemoveAll(tmp)
			return "", nil, fmt.Errorf("%s is not a marketplace: %w", src.Display(), err)
		}
		return tmp, man, nil
	case MarketplaceSourceURL:
		data, err := fetchURL(ctx, src.URL)
		if err != nil {
			return "", nil, err
		}
		tmp, err := tempMarketplaceDir()
		if err != nil {
			return "", nil, err
		}
		if err := writeFileAtomic(filepath.Join(tmp, filepath.FromSlash(MarketplaceManifestRel)), data, 0o644); err != nil {
			_ = os.RemoveAll(tmp)
			return "", nil, err
		}
		man, err := ReadMarketplaceManifest(tmp)
		if err != nil {
			_ = os.RemoveAll(tmp)
			return "", nil, fmt.Errorf("%s is not a marketplace: %w", src.URL, err)
		}
		return tmp, man, nil
	}
	return "", nil, fmt.Errorf("unknown marketplace source kind %q", src.Kind)
}

func tempMarketplaceDir() (string, error) {
	base := MarketplacesDir()
	if base == "" {
		return "", fmt.Errorf("no home directory: cannot store marketplaces")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(base, ".tmp-*")
}

func registerMarketplace(cwd string, m Marketplace, scope Scope) (Marketplace, error) {
	err := withLock(func() error {
		switch scope {
		case ScopeUser:
			known := readKnownMarketplaces()
			if ex, ok := known[m.Name]; ok && !ex.Source.Equal(m.Source) {
				return fmt.Errorf("marketplace %q is already registered from %s", m.Name, ex.Source.Display())
			}
			known[m.Name] = KnownMarketplace{Source: m.Source, InstallLocation: m.Dir, LastUpdated: timestamp()}
			return writeKnownMarketplaces(known)
		case ScopeProject:
			pf := ReadProjectFile(cwd)
			if ex, ok := pf.Marketplaces[m.Name]; ok && !ex.Source.Equal(m.Source) {
				return fmt.Errorf("marketplace %q is already registered in this project from %s", m.Name, ex.Source.Display())
			}
			if pf.Marketplaces == nil {
				pf.Marketplaces = map[string]KnownMarketplace{}
			}
			pf.Marketplaces[m.Name] = KnownMarketplace{Source: m.Source}
			return writeProjectFile(cwd, pf)
		}
		return fmt.Errorf("unknown scope %q", scope)
	})
	if err != nil {
		return Marketplace{}, err
	}
	m.Scope = scope
	return m, nil
}

// RemoveMarketplace drops the registration in scope. The clone is deleted
// once no scope references it; directory sources are never touched.
func RemoveMarketplace(cwd, name string, scope Scope) error {
	return withLock(func() error {
		known := readKnownMarketplaces()
		pf := ReadProjectFile(cwd)
		var km KnownMarketplace
		switch scope {
		case ScopeUser:
			ex, ok := known[name]
			if !ok {
				return fmt.Errorf("marketplace %q is not registered in user scope", name)
			}
			km = ex
			delete(known, name)
			if err := writeKnownMarketplaces(known); err != nil {
				return err
			}
		case ScopeProject:
			ex, ok := pf.Marketplaces[name]
			if !ok {
				return fmt.Errorf("marketplace %q is not registered in this project", name)
			}
			km = ex
			delete(pf.Marketplaces, name)
			if err := writeProjectFile(cwd, pf); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown scope %q", scope)
		}
		_, userHas := known[name]
		_, projectHas := pf.Marketplaces[name]
		if userHas || projectHas || km.Source.Kind == MarketplaceSourceDirectory {
			return nil
		}
		dir := marketplaceDir(name, km.Source, km.InstallLocation)
		if base := MarketplacesDir(); base != "" && dir != "" && insideDir(base, dir) && dir != base {
			_ = os.RemoveAll(dir)
		}
		return nil
	})
}

// RefreshMarketplace pulls the latest catalog for m.
func RefreshMarketplace(ctx context.Context, cwd string, m Marketplace) error {
	switch m.Source.Kind {
	case MarketplaceSourceDirectory:
	case MarketplaceSourceGitHub, MarketplaceSourceGit:
		if m.Dir == "" {
			return fmt.Errorf("marketplace %q has no local directory", m.Name)
		}
		if gitIsRepo(m.Dir) {
			if err := gitPull(ctx, m.Dir); err != nil {
				return err
			}
		} else {
			_ = os.RemoveAll(m.Dir)
			if err := gitClone(ctx, m.Source.GitURL(), m.Dir); err != nil {
				return err
			}
		}
	case MarketplaceSourceURL:
		data, err := fetchURL(ctx, m.Source.URL)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(m.Dir, filepath.FromSlash(MarketplaceManifestRel)), data, 0o644); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown marketplace source kind %q", m.Source.Kind)
	}
	_ = withLock(func() error {
		known := readKnownMarketplaces()
		if km, ok := known[m.Name]; ok {
			km.LastUpdated = timestamp()
			if km.InstallLocation == "" {
				km.InstallLocation = m.Dir
			}
			known[m.Name] = km
			return writeKnownMarketplaces(known)
		}
		return nil
	})
	return nil
}

// RefreshAll refreshes every registered marketplace, collecting failures.
func RefreshAll(ctx context.Context, cwd string) []error {
	var errs []error
	for _, m := range ListMarketplaces(cwd) {
		if err := RefreshMarketplace(ctx, cwd, m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, err))
		}
	}
	return errs
}

// InitMarketplace turns dir into an empty marketplace (git-initialised)
// so the user can publish into it and push it wherever they like.
func InitMarketplace(ctx context.Context, dir, name, owner string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, filepath.FromSlash(MarketplaceManifestRel))
	if fileExists(path) {
		return fmt.Errorf("%s is already a marketplace", dir)
	}
	man := MarketplaceManifest{Name: name, Owner: Owner{Name: owner}, Plugins: []Entry{}}
	if err := writeJSONAtomic(path, man); err != nil {
		return err
	}
	if !gitIsRepo(dir) {
		if _, err := RunGit(ctx, dir, "init", "--quiet"); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
