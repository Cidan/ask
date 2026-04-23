package main

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:plugins/ask-usage
var usagePluginFS embed.FS

// embeddedPluginRoot is the path *inside* usagePluginFS of the ask-usage
// plugin root. Keep in sync with the //go:embed pattern above.
const embeddedPluginRoot = "plugins/ask-usage"

// extractUsagePlugin writes the embedded ask-usage plugin tree to a
// stable on-disk location and returns the *parent* directory (the one
// that should be passed to `claude --plugin-dir`). Returns "" on any
// failure; callers should debugLog and continue without the plugin.
//
// Location: $XDG_CACHE_HOME/ask/plugins/ask-usage/ (default $HOME/.cache/...).
// The parent returned is therefore .../ask/plugins/ — claude then scans
// that dir and finds ask-usage/ as a plugin subdir.
//
// Idempotent: always overwrites the embedded files (version-bump-safe),
// but never touches sibling files or the plugin's own .cache.json. The
// plugin's cache lives alongside the scripts in the same dir, so we
// take care NOT to clobber .cache.json during re-extraction.
func extractUsagePlugin() (string, error) {
	cacheHome := os.Getenv("XDG_CACHE_HOME")
	if cacheHome == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", err
		}
		cacheHome = filepath.Join(home, ".cache")
	}
	parent := filepath.Join(cacheHome, "ask", "plugins")
	pluginDir := filepath.Join(parent, "ask-usage")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		return "", err
	}

	err := fs.WalkDir(usagePluginFS, embeddedPluginRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(embeddedPluginRoot, p)
		if err != nil {
			return err
		}
		target := filepath.Join(pluginDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := usagePluginFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return "", err
	}
	return parent, nil
}

// usagePluginCacheFile is the on-disk path the plugin writes its usage
// snapshot to. Returns "" if the plugin has not been extracted yet (or
// if $HOME / $XDG_CACHE_HOME resolution fails).
func usagePluginCacheFile() string {
	cacheHome := os.Getenv("XDG_CACHE_HOME")
	if cacheHome == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		cacheHome = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheHome, "ask", "plugins", "ask-usage", ".cache.json")
}
