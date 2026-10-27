package watcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Watchers are machine-wide — whichever window runs them must see every
// project's — so they live under ~/.config/ask, never in a repo, one
// directory per project (ProjectDir).
const (
	dirName        = "watchers"
	scriptSuffix   = ".script"
	stateDirSuffix = ".state"
	defsLockName   = "watchers.defs.lock"
)

// ErrNotFound is returned for a watcher with no definition.
var ErrNotFound = errors.New("watcher not found")

func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "ask")
}

// Dir is ~/.config/ask/watchers: a directory per project holding each
// watcher's <name>.json, its <name>.script when it has one, and its
// <name>.state/ scratch directory.
func Dir() string {
	base := configDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, dirName)
}

// projectPath is a project's directory in the store.
func projectPath(project string) string { return filepath.Join(Dir(), ProjectDir(project)) }

// DefPath is where a watcher's definition is stored.
func DefPath(r Ref) string { return filepath.Join(projectPath(r.Project), r.Name+".json") }

// ScriptPath is where a watcher's stored script lives.
func ScriptPath(r Ref) string { return filepath.Join(projectPath(r.Project), r.Name+scriptSuffix) }

// StateDir is the directory a watcher's script may keep its own state in
// (what it reported last time, a cursor, a cache). The runner creates it
// and passes it as ASK_WATCHER_STATE_DIR; deleting the watcher removes it.
func StateDir(r Ref) string { return filepath.Join(projectPath(r.Project), r.Name+stateDirSuffix) }

// List returns every stored watcher in every project, by project then
// name. Malformed files, and files that disagree with where they are
// stored, are skipped.
func List() []Def {
	dir := Dir()
	if dir == "" {
		return nil
	}
	projects, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Def
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, p.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			fname := f.Name()
			if f.IsDir() || !strings.HasSuffix(fname, ".json") {
				continue
			}
			d, err := readDef(filepath.Join(dir, p.Name(), fname))
			if err != nil || ProjectDir(d.Project) != p.Name() || d.Name+".json" != fname {
				continue
			}
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Get returns the watcher r names.
func Get(r Ref) (Def, error) {
	if err := ValidName(r.Name); err != nil {
		return Def{}, err
	}
	d, err := readDef(DefPath(r))
	if errors.Is(err, os.ErrNotExist) {
		return Def{}, fmt.Errorf("%w: %s in %s", ErrNotFound, r.Name, r.Project)
	}
	if err == nil && (d.Name != r.Name || d.Project != r.Project) {
		return Def{}, fmt.Errorf("watcher file %s names %s in %s", DefPath(r), d.Name, d.Project)
	}
	return d, err
}

// Resolve finds a watcher by name: in project when it has one by that
// name, else the one watcher of that name anywhere. More than one
// elsewhere is ambiguous.
func Resolve(project, name string) (Def, error) {
	if err := ValidName(name); err != nil {
		return Def{}, err
	}
	if d, err := Get(Ref{Project: project, Name: name}); err == nil {
		return d, nil
	}
	var found []Def
	for _, d := range List() {
		if d.Name == name {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		return Def{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	case 1:
		return found[0], nil
	}
	projects := make([]string, len(found))
	for i, d := range found {
		projects[i] = d.Project
	}
	return Def{}, fmt.Errorf("watcher %q exists in several projects (%s): name the project", name, strings.Join(projects, ", "))
}

func readDef(path string) (Def, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Def{}, err
	}
	var d Def
	if err := json.Unmarshal(data, &d); err != nil {
		return Def{}, fmt.Errorf("watcher %s: %w", path, err)
	}
	if _, err := os.Stat(ScriptPath(d.Ref())); err == nil {
		d.HasScript = true
	}
	return d, nil
}

// ReadScript returns a watcher's stored script, or "" when it runs a
// command.
func ReadScript(r Ref) (string, error) {
	data, err := os.ReadFile(ScriptPath(r))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(data), err
}

// Save stores d in the project its working directory belongs to (which
// Save sets as d.Project). old is the watcher being edited, the zero Ref to
// create one. script replaces the stored script when non-nil (an empty
// string removes it); nil keeps whatever is stored. A rename, or a move to
// another project, carries the script, the scratch directory, and the
// shared run state along. HasScript is derived from the result, and the
// definition is validated against it.
func Save(old Ref, d Def, script *string) (Def, error) {
	if err := ValidName(d.Name); err != nil {
		return Def{}, err
	}
	if script != nil && *script != "" && !strings.HasPrefix(*script, "#!") {
		return Def{}, errors.New(`a watcher script must start with a "#!" line naming its interpreter (e.g. "#!/usr/bin/env python3")`)
	}
	d.Project = ProjectOf(d.Cwd)
	creating := old == (Ref{})
	moved := !creating && old != d.Ref()
	var saved Def
	err := withFileLock(filepath.Join(configDir(), defsLockName), func() error {
		if Dir() == "" {
			return errors.New("no home directory for the watcher store")
		}
		if !creating {
			if _, err := os.Stat(DefPath(old)); err != nil {
				return fmt.Errorf("%w: %s in %s", ErrNotFound, old.Name, old.Project)
			}
		}
		if creating || moved {
			if _, err := os.Stat(DefPath(d.Ref())); err == nil {
				return fmt.Errorf("watcher %q already exists in %s", d.Name, d.Project)
			}
		}

		hasScript := false
		switch {
		case script != nil:
			hasScript = *script != ""
		case !creating:
			_, err := os.Stat(ScriptPath(old))
			hasScript = err == nil
		}
		d.HasScript = hasScript
		if err := d.Validate(); err != nil {
			return err
		}

		if err := os.MkdirAll(projectPath(d.Project), 0o755); err != nil {
			return err
		}
		data, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFileAtomic(DefPath(d.Ref()), append(data, '\n'), 0o644); err != nil {
			return err
		}
		switch {
		case script != nil && *script != "":
			if err := writeFileAtomic(ScriptPath(d.Ref()), []byte(*script), 0o700); err != nil {
				return err
			}
			if moved {
				_ = os.Remove(ScriptPath(old))
			}
		case script != nil:
			_ = os.Remove(ScriptPath(d.Ref()))
			if moved {
				_ = os.Remove(ScriptPath(old))
			}
		case moved && hasScript:
			if err := os.Rename(ScriptPath(old), ScriptPath(d.Ref())); err != nil {
				return err
			}
		}
		if moved {
			if _, err := os.Stat(StateDir(old)); err == nil {
				if err := os.Rename(StateDir(old), StateDir(d.Ref())); err != nil {
					return err
				}
			}
			if err := os.Remove(DefPath(old)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removeIfEmpty(projectPath(old.Project))
		}
		saved = d
		return nil
	})
	if err != nil {
		return Def{}, err
	}
	if moved {
		_ = MutateState(func(s *State) error {
			if st, ok := s.Watchers[old.ID()]; ok {
				delete(s.Watchers, old.ID())
				s.Watchers[d.ID()] = st
			}
			return nil
		})
	}
	return saved, nil
}

// SetPaused pauses or resumes a watcher.
func SetPaused(r Ref, paused bool) (Def, error) {
	d, err := Get(r)
	if err != nil {
		return Def{}, err
	}
	d.Paused = paused
	return Save(r, d, nil)
}

// Delete removes a watcher: its definition, script, scratch directory, and
// shared run state.
func Delete(r Ref) error {
	if err := ValidName(r.Name); err != nil {
		return err
	}
	err := withFileLock(filepath.Join(configDir(), defsLockName), func() error {
		if err := os.Remove(DefPath(r)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: %s in %s", ErrNotFound, r.Name, r.Project)
			}
			return err
		}
		_ = os.Remove(ScriptPath(r))
		if err := os.RemoveAll(StateDir(r)); err != nil {
			return err
		}
		removeIfEmpty(projectPath(r.Project))
		return nil
	})
	if err != nil {
		return err
	}
	return MutateState(func(s *State) error {
		delete(s.Watchers, r.ID())
		return nil
	})
}

// removeIfEmpty drops a project's store directory once its last watcher
// is gone.
func removeIfEmpty(dir string) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}

// withFileLock holds an exclusive advisory lock on path around fn, so
// read-modify-write cycles from several ask processes never interleave.
func withFileLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockExclusive(f); err != nil {
		return err
	}
	defer unlockFile(f)
	return fn()
}

// writeFileAtomic writes through a temp file and a rename, so a reader in
// another process never sees half a file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
