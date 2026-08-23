package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Marketplace source kinds — the `source` field of known_marketplaces.json.
const (
	MarketplaceSourceGitHub    = "github"
	MarketplaceSourceGit       = "git"
	MarketplaceSourceDirectory = "directory"
	MarketplaceSourceURL       = "url"
)

// MarketplaceSource says where a marketplace comes from.
type MarketplaceSource struct {
	Kind string `json:"source"`
	Repo string `json:"repo,omitempty"`
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
}

// ParseMarketplaceSource classifies what the user typed after
// `/skills add marketplace`: a GitHub `owner/repo`, a git URL, a local
// directory, or a direct URL to a marketplace.json. Relative directories
// resolve against cwd.
func ParseMarketplaceSource(cwd, raw string) (MarketplaceSource, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return MarketplaceSource{}, fmt.Errorf("marketplace source is required: owner/repo, a git URL, a directory, or a marketplace.json URL")
	}
	isHTTP := strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
	switch {
	case isHTTP && strings.HasSuffix(strings.ToLower(raw), ".json"):
		return MarketplaceSource{Kind: MarketplaceSourceURL, URL: raw}, nil
	case strings.Contains(raw, "://"), strings.HasPrefix(raw, "git@"):
		return MarketplaceSource{Kind: MarketplaceSourceGit, URL: raw}, nil
	}
	abs := raw
	if strings.HasPrefix(abs, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			abs = filepath.Join(home, abs[2:])
		}
	}
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return MarketplaceSource{Kind: MarketplaceSourceDirectory, Path: filepath.Clean(abs)}, nil
	}
	if githubShorthandRe.MatchString(raw) && !strings.HasPrefix(raw, ".") {
		return MarketplaceSource{Kind: MarketplaceSourceGitHub, Repo: strings.TrimSuffix(raw, ".git")}, nil
	}
	return MarketplaceSource{}, fmt.Errorf("%q is not a directory, a git URL, or an owner/repo", raw)
}

// GitURL is the clone URL for git-backed sources, "" otherwise.
func (s MarketplaceSource) GitURL() string {
	switch s.Kind {
	case MarketplaceSourceGitHub:
		return "https://github.com/" + strings.TrimSuffix(s.Repo, ".git") + ".git"
	case MarketplaceSourceGit:
		return s.URL
	}
	return ""
}

// Display is the one-line form shown in the browser.
func (s MarketplaceSource) Display() string {
	switch s.Kind {
	case MarketplaceSourceGitHub:
		return "github.com/" + s.Repo
	case MarketplaceSourceGit, MarketplaceSourceURL:
		return s.URL
	case MarketplaceSourceDirectory:
		return s.Path
	}
	return ""
}

// Raw is the string form AddMarketplace accepts back — what a user would
// have typed to register this source.
func (s MarketplaceSource) Raw() string {
	switch s.Kind {
	case MarketplaceSourceGitHub:
		return s.Repo
	case MarketplaceSourceGit, MarketplaceSourceURL:
		return s.URL
	case MarketplaceSourceDirectory:
		return s.Path
	}
	return ""
}

func (s MarketplaceSource) Equal(o MarketplaceSource) bool {
	return s.Kind == o.Kind && s.Repo == o.Repo && s.URL == o.URL && filepath.Clean(s.Path) == filepath.Clean(o.Path)
}
