// Package plugin implements discovery, loading and installation of
// .claude-plugin standard plugins (skills/agents/commands/MCP resources).
package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ManifestDir is the fixed manifest directory inside a plugin root.
const ManifestDir = ".claude-plugin"

// Manifest mirrors .claude-plugin/plugin.json. Name and Description are
// required; Skills, when present (even as an empty array), replaces the
// default skills/ directory scan (Claude Code / comate semantics).
type Manifest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Version     string   `json:"version,omitempty"`
	Author      string   `json:"author,omitempty"`
	Skills      []string `json:"skills,omitempty"`

	// skillsDeclared reports whether the "skills" key existed in the JSON,
	// which switches skill discovery from the default directory scan to the
	// declared relative paths (an empty declaration disables skills entirely).
	skillsDeclared bool
}

// SkillsDeclared reports whether plugin.json contained a "skills" key.
func (m Manifest) SkillsDeclared() bool { return m.skillsDeclared }

// PluginSource describes where a plugin came from, mirroring the registry.
type PluginSource struct {
	Type      string `json:"type"`                // marketplace | zip | local
	URL       string `json:"url,omitempty"`       // marketplace repo / zip url
	SHA256    string `json:"sha256,omitempty"`    // zip integrity
	Path      string `json:"path,omitempty"`      // marketplace-relative path / local dir
	StripRoot bool   `json:"stripRoot,omitempty"` // zip: strip the single top-level dir
}

// InstalledEntry is one record of installed_plugins.json.
type InstalledEntry struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Marketplace string       `json:"marketplace,omitempty"`
	Version     string       `json:"version,omitempty"`
	InstallPath string       `json:"installPath"`
	InstalledAt string       `json:"installedAt"`
	UpdatedAt   string       `json:"updatedAt"`
	Scope       string       `json:"scope"`
	Source      PluginSource `json:"source"`
	Enabled     bool         `json:"enabled"`
}

// installedRegistry is the on-disk shape of installed_plugins.json.
type installedRegistry struct {
	Version int              `json:"version"`
	Plugins []InstalledEntry `json:"plugins"`
}

// MarketplaceSource is the "source" field of a marketplace plugin entry. The
// .claude-plugin standard allows two shapes: a plain string (a path relative
// to the marketplace clone, or an http(s) zip archive URL) or an object
// pointing at another git repository (git-subdir / github / git).
type MarketplaceSource struct {
	Raw  string // original string value when the field was a plain string
	Kind string // path | zip | git
	URL  string // git shape: repository URL
	Path string // git shape: subdirectory inside the repo ("" = repo root)
	Ref  string // git shape: optional branch/tag
	SHA  string // git shape: optional commit pin
}

// UnmarshalJSON accepts both the string and the object form. Object forms
// other than git-subdir/github/git fail with an explicit error so a bad
// manifest surfaces its real problem instead of silently degrading.
func (s *MarketplaceSource) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		kind := "path"
		if isRemoteSource(str) {
			kind = "zip"
		}
		*s = MarketplaceSource{Raw: str, Kind: kind}
		return nil
	}
	var obj struct {
		Source string `json:"source"`
		URL    string `json:"url"`
		Repo   string `json:"repo"`
		Path   string `json:"path"`
		Ref    string `json:"ref"`
		SHA    string `json:"sha"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("plugin: marketplace source: %w", err)
	}
	switch obj.Source {
	case "git-subdir", "git":
		*s = MarketplaceSource{Kind: "git", URL: obj.URL, Path: obj.Path, Ref: obj.Ref, SHA: obj.SHA}
	case "github":
		if obj.Repo == "" {
			return fmt.Errorf("plugin: marketplace source github: repo is required")
		}
		url := obj.URL
		if url == "" {
			url = "https://github.com/" + strings.TrimSuffix(obj.Repo, ".git") + ".git"
		}
		*s = MarketplaceSource{Kind: "git", URL: url, Path: obj.Path, Ref: obj.Ref, SHA: obj.SHA}
	default:
		return fmt.Errorf("plugin: unsupported marketplace source type %q (supported: string path, string zip url, git-subdir, github, git)", obj.Source)
	}
	return nil
}

// MarketplacePlugin is one plugin entry inside a marketplace manifest.
type MarketplacePlugin struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Version     string            `json:"version,omitempty"`
	Source      MarketplaceSource `json:"source,omitempty"` // relative path, zip URL or git object
	SHA256      string            `json:"sha256,omitempty"`
	StripRoot   bool              `json:"stripRoot,omitempty"`
}

// MarketplaceManifest mirrors .claude-plugin/marketplace.json.
type MarketplaceManifest struct {
	Name    string              `json:"name"`
	Plugins []MarketplacePlugin `json:"plugins,omitempty"`
}

// KnownMarketplace is one record of known_marketplaces.json.
type KnownMarketplace struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// loadManifest reads and validates .claude-plugin/plugin.json under pluginDir.
func loadManifest(pluginDir string) (Manifest, error) {
	path := filepath.Join(pluginDir, ManifestDir, "plugin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin: read manifest %s: %w", path, err)
	}
	var raw struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Version     string          `json:"version"`
		Author      json.RawMessage `json:"author"`
		Skills      *[]string       `json:"skills"`
	}
	// 宽松解析：生态内 plugin.json 常见 homepage/license/keywords/mcpServers
	// 等附加字段（存档或展示用途），一律忽略，不做 schema 拒绝。
	if err := json.Unmarshal(data, &raw); err != nil {
		return Manifest{}, fmt.Errorf("plugin: decode %s: %w", path, err)
	}
	m := Manifest{
		Name:        strings.TrimSpace(raw.Name),
		Description: strings.TrimSpace(raw.Description),
		Version:     strings.TrimSpace(raw.Version),
	}
	if m.Name == "" {
		return Manifest{}, fmt.Errorf("plugin: %s: name is required", path)
	}
	if !isValidPluginName(m.Name) {
		return Manifest{}, fmt.Errorf("plugin: %s: invalid name %q", path, m.Name)
	}
	if m.Description == "" {
		return Manifest{}, fmt.Errorf("plugin: %s: description is required", path)
	}
	if raw.Author != nil {
		m.Author = normalizeAuthor(raw.Author)
	}
	if raw.Skills != nil {
		m.skillsDeclared = true
		m.Skills = *raw.Skills
	}
	return m, nil
}

// isValidPluginName restricts names to lowercase alphanumerics, dash and dot —
// they become registry keys and namespace identifiers.
func isValidPluginName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// normalizeAuthor accepts either a plain string or an object {name, url}.
func normalizeAuthor(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.URL != "" {
			return strings.TrimSpace(obj.Name + " (" + obj.URL + ")")
		}
		return strings.TrimSpace(obj.Name)
	}
	return ""
}
