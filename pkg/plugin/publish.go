package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PublishRequest describes what to land in a marketplace as one plugin.
type PublishRequest struct {
	PluginName  string
	Description string
	Version     string
	// SkillDirs are skill packages (directories holding SKILL.md).
	SkillDirs []string
	// AgentFiles are agent definitions (*.md).
	AgentFiles []string
	// WorkflowFiles are ask workflow definitions (*.json).
	WorkflowFiles []string
	Message       string
	// NoPush keeps the commit local even when the marketplace has a
	// remote. By default a git-backed marketplace is pulled (ff-only)
	// before the commit and pushed after it.
	NoPush bool
}

// PublishResult reports where the plugin landed and how far git got.
type PublishResult struct {
	PluginDir string
	Version   string
	Committed bool
	Pushed    bool
	Commit    string
	Note      string
}

// Publish copies the request's items into <marketplace>/plugins/<name>/,
// writes the plugin manifest, upserts the catalog entry, and commits (and
// pushes, when asked and a remote exists).
func Publish(ctx context.Context, m Marketplace, req PublishRequest) (PublishResult, error) {
	if !m.Writable() {
		return PublishResult{}, fmt.Errorf("marketplace %q is read-only here (%s)", m.Name, m.Source.Display())
	}
	if !m.Fetched() {
		return PublishResult{}, fmt.Errorf("marketplace %q: %s", m.Name, m.Err)
	}
	if err := ValidateName(req.PluginName); err != nil {
		return PublishResult{}, fmt.Errorf("plugin %w", err)
	}
	if len(req.SkillDirs)+len(req.AgentFiles)+len(req.WorkflowFiles) == 0 {
		return PublishResult{}, fmt.Errorf("nothing to publish")
	}
	pluginRel := filepath.Join("plugins", req.PluginName)
	pluginDir := filepath.Join(m.Dir, pluginRel)
	remote := gitIsRepo(m.Dir) && !req.NoPush && gitHasRemote(ctx, m.Dir)
	if remote {
		if err := gitPull(ctx, m.Dir); err != nil {
			return PublishResult{}, fmt.Errorf("marketplace %q: pull before publish: %w", m.Name, err)
		}
	}
	existing, err := ReadPluginManifest(pluginDir)
	if err != nil {
		return PublishResult{}, err
	}
	for _, dir := range req.SkillDirs {
		if !fileExists(filepath.Join(dir, "SKILL.md")) {
			return PublishResult{}, fmt.Errorf("%s is not a skill package (no SKILL.md)", dir)
		}
		dst := filepath.Join(pluginDir, "skills", filepath.Base(dir))
		if err := os.RemoveAll(dst); err != nil {
			return PublishResult{}, err
		}
		if err := copyDir(dir, dst); err != nil {
			return PublishResult{}, err
		}
	}
	for _, f := range req.AgentFiles {
		if err := copyFile(f, filepath.Join(pluginDir, "agents", filepath.Base(f)), 0o644); err != nil {
			return PublishResult{}, err
		}
	}
	for _, f := range req.WorkflowFiles {
		if err := copyFile(f, filepath.Join(pluginDir, "workflows", filepath.Base(f)), 0o644); err != nil {
			return PublishResult{}, err
		}
	}

	man := existing
	if man == nil {
		man = &PluginManifest{}
	}
	man.Name = req.PluginName
	if req.Description != "" {
		man.Description = req.Description
	}
	switch {
	case req.Version != "":
		man.Version = req.Version
	case man.Version == "":
		man.Version = "1.0.0"
	case existing != nil:
		// Republishing an existing plugin is an update: bump the patch
		// number so installs can tell the copies apart.
		man.Version = BumpPatch(man.Version)
	}
	if err := writeJSONAtomic(filepath.Join(pluginDir, filepath.FromSlash(PluginManifestRel)), man); err != nil {
		return PublishResult{}, err
	}

	entry := map[string]any{
		"name":    req.PluginName,
		"source":  "./" + filepath.ToSlash(pluginRel),
		"version": man.Version,
	}
	if man.Description != "" {
		entry["description"] = man.Description
	}
	manifestPath := filepath.Join(m.Dir, filepath.FromSlash(MarketplaceManifestRel))
	if err := upsertMarketplaceEntry(manifestPath, entry); err != nil {
		return PublishResult{}, err
	}

	res := PublishResult{PluginDir: pluginDir, Version: man.Version}
	if !gitIsRepo(m.Dir) {
		res.Note = "not a git repository — files written, nothing committed"
		return res, nil
	}
	if _, err := RunGit(ctx, m.Dir, "add", "-A", "--", filepath.ToSlash(pluginRel), MarketplaceManifestRel); err != nil {
		return res, err
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = "publish " + req.PluginName
	}
	if out, err := RunGit(ctx, m.Dir, "commit", "--quiet", "-m", msg); err != nil {
		if strings.Contains(out, "nothing to commit") || strings.Contains(err.Error(), "nothing to commit") {
			res.Note = "nothing changed"
			return res, nil
		}
		return res, err
	}
	res.Committed = true
	res.Commit = gitHeadSHA(ctx, m.Dir)
	if !remote {
		if req.NoPush {
			res.Note = "committed, not pushed"
		} else {
			res.Note = "committed; no git remote to push to"
		}
		return res, nil
	}
	if _, err := RunGit(ctx, m.Dir, "push", "--quiet"); err != nil {
		res.Note = "committed; push failed"
		return res, err
	}
	res.Pushed = true
	return res, nil
}

// upsertMarketplaceEntry edits marketplace.json as a generic document so
// fields ask does not model ($schema, renames, …) survive the rewrite.
func upsertMarketplaceEntry(path string, entry map[string]any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	plugins, _ := doc["plugins"].([]any)
	replaced := false
	for i, p := range plugins {
		pm, ok := p.(map[string]any)
		if !ok || pm["name"] != entry["name"] {
			continue
		}
		for k, v := range entry {
			pm[k] = v
		}
		plugins[i] = pm
		replaced = true
	}
	if !replaced {
		plugins = append(plugins, entry)
	}
	doc["plugins"] = plugins
	return writeJSONAtomic(path, doc)
}
