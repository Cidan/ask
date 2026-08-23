package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A Publication links a local skill, agent, or workflow to the plugin it
// was published as. The local copy stays the source of truth; the link
// records what was last published (Hash) so the two can be compared.
type Publication struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Marketplace string `json:"marketplace"`
	Plugin      string `json:"plugin"`
	// File is the item's name inside the plugin: the skill directory,
	// the agent file, or the workflow file.
	File        string `json:"file"`
	Version     string `json:"version,omitempty"`
	Hash        string `json:"hash"`
	PublishedAt string `json:"publishedAt,omitempty"`
	Commit      string `json:"commit,omitempty"`
}

// Ref is the plugin reference the publication points at.
func (p Publication) Ref() Ref { return Ref{Plugin: p.Plugin, Marketplace: p.Marketplace} }

func publicationKey(kind, name string) string { return kind + ":" + name }

func publishedPath() string {
	if r := RootDir(); r != "" {
		return filepath.Join(r, "published.json")
	}
	return ""
}

func readUserPublished() map[string]Publication {
	out := map[string]Publication{}
	if p := publishedPath(); p != "" {
		_ = readJSON(p, &out)
	}
	if out == nil {
		out = map[string]Publication{}
	}
	return out
}

func writeUserPublished(m map[string]Publication) error {
	p := publishedPath()
	if p == "" {
		return fmt.Errorf("no home directory: cannot record publications")
	}
	if len(m) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeJSONAtomic(p, m)
}

// RecordPublication stores pub — in the project file for project-scope
// items (committed, so the team sees where the item lives), in the
// user file otherwise.
func RecordPublication(cwd string, pub Publication) error {
	return withLock(func() error {
		key := publicationKey(pub.Kind, pub.Name)
		if pub.Scope == string(ScopeProject) {
			pf := ReadProjectFile(cwd)
			if pf.Published == nil {
				pf.Published = map[string]Publication{}
			}
			pf.Published[key] = pub
			return writeProjectFile(cwd, pf)
		}
		all := readUserPublished()
		all[key] = pub
		return writeUserPublished(all)
	})
}

// ForgetPublication drops the link (the marketplace copy is untouched).
func ForgetPublication(cwd, kind, name, scope string) error {
	return withLock(func() error {
		key := publicationKey(kind, name)
		if scope == string(ScopeProject) {
			pf := ReadProjectFile(cwd)
			delete(pf.Published, key)
			return writeProjectFile(cwd, pf)
		}
		all := readUserPublished()
		delete(all, key)
		return writeUserPublished(all)
	})
}

// Publications lists every link visible from cwd.
func Publications(cwd string) []Publication {
	var out []Publication
	for _, p := range readUserPublished() {
		out = append(out, p)
	}
	for _, p := range ReadProjectFile(cwd).Published {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// FindPublication returns the link for a local item.
func FindPublication(cwd, kind, name, scope string) (Publication, bool) {
	for _, p := range Publications(cwd) {
		if p.Kind == kind && p.Name == name && (scope == "" || p.Scope == scope) {
			return p, true
		}
	}
	return Publication{}, false
}

// PublicationForRef says whether a marketplace plugin is one the user
// published from a local copy here.
func PublicationForRef(cwd string, ref Ref) (Publication, bool) {
	for _, p := range Publications(cwd) {
		if p.Ref() == ref {
			return p, true
		}
	}
	return Publication{}, false
}

// SyncState compares the local copy and the marketplace copy against
// what was last published.
type SyncState int

const (
	SyncInSync SyncState = iota
	SyncLocalChanged
	SyncMarketplaceChanged
	SyncDiverged
	SyncMissing
)

func (s SyncState) String() string {
	switch s {
	case SyncInSync:
		return "in sync"
	case SyncLocalChanged:
		return "local changes"
	case SyncMarketplaceChanged:
		return "marketplace newer"
	case SyncDiverged:
		return "diverged"
	}
	return "missing"
}

// PublishedCopyPath is where the marketplace holds the published item.
func PublishedCopyPath(m Marketplace, pub Publication) string {
	base := filepath.Join(m.Dir, "plugins", pub.Plugin)
	switch pub.Kind {
	case "agent":
		return filepath.Join(base, "agents", pub.File)
	case "workflow":
		return filepath.Join(base, "workflows", pub.File)
	}
	return filepath.Join(base, "skills", pub.File)
}

// Status classifies a publication given the current hash of the local
// copy (HashPath of the skill dir / agent file, HashBytes of a workflow
// export).
func Status(cwd string, pub Publication, localHash string) SyncState {
	m, ok := FindMarketplace(cwd, pub.Marketplace)
	if !ok || m.Dir == "" {
		return SyncMissing
	}
	remote := PublishedCopyPath(m, pub)
	if _, err := os.Stat(remote); err != nil {
		return SyncMissing
	}
	local := localHash
	published := HashPath(remote)
	switch {
	case local == pub.Hash && published == pub.Hash:
		return SyncInSync
	case local != pub.Hash && published == pub.Hash:
		return SyncLocalChanged
	case local == pub.Hash && published != pub.Hash:
		return SyncMarketplaceChanged
	}
	return SyncDiverged
}

// Pull replaces the local copy with the marketplace copy and re-bases
// the publication on it.
func Pull(cwd string, pub Publication, localPath string) (Publication, error) {
	m, ok := FindMarketplace(cwd, pub.Marketplace)
	if !ok || m.Dir == "" {
		return pub, fmt.Errorf("marketplace %q is not fetched", pub.Marketplace)
	}
	remote := PublishedCopyPath(m, pub)
	info, err := os.Stat(remote)
	if err != nil {
		return pub, fmt.Errorf("published copy missing: %s", remote)
	}
	if info.IsDir() {
		if err := os.RemoveAll(localPath); err != nil {
			return pub, err
		}
		if err := copyDir(remote, localPath); err != nil {
			return pub, err
		}
	} else if err := copyFile(remote, localPath, 0o644); err != nil {
		return pub, err
	}
	pub.Hash = HashPath(localPath)
	pub.PublishedAt = timestamp()
	return pub, RecordPublication(cwd, pub)
}

// HashBytes hashes file content the way HashPath hashes a file.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashPath hashes a file, or a directory's relative paths and contents
// (sorted, .git skipped), so two copies compare by content alone.
func HashPath(p string) string {
	h := sha256.New()
	info, err := os.Stat(p)
	if err != nil {
		return ""
	}
	if !info.IsDir() {
		f, err := os.Open(p)
		if err != nil {
			return ""
		}
		defer f.Close()
		_, _ = io.Copy(h, f)
		return hex.EncodeToString(h.Sum(nil))
	}
	var files []string
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" && path != p {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	for _, f := range files {
		rel, _ := filepath.Rel(p, f)
		_, _ = io.WriteString(h, filepath.ToSlash(rel))
		h.Write([]byte{0})
		if data, err := os.ReadFile(f); err == nil {
			h.Write(data)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// BumpPatch increments the patch number of a semver string; anything
// that is not major.minor.patch is returned unchanged.
func BumpPatch(v string) string {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) != 3 {
		return v
	}
	var n int
	if _, err := fmt.Sscanf(parts[2], "%d", &n); err != nil {
		return v
	}
	return fmt.Sprintf("%s.%s.%d", parts[0], parts[1], n+1)
}
