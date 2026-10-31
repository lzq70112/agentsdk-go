package plugin

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxZipBytes caps a downloaded plugin archive to keep a bad URL from
// exhausting memory.
const maxZipBytes = 256 << 20 // 256 MiB

// ErrNotFound is returned when a referenced plugin or marketplace does not exist.
var ErrNotFound = fmt.Errorf("plugin: not found")

// AddMarketplace clones a marketplace repository (shallow), reads its manifest
// and registers it under its declared name.
func AddMarketplace(root, url string) (MarketplaceManifest, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return MarketplaceManifest{}, fmt.Errorf("plugin: marketplace url is required")
	}
	if err := os.MkdirAll(filepath.Join(root, "marketplaces"), 0o755); err != nil {
		return MarketplaceManifest{}, fmt.Errorf("plugin: mkdir marketplaces: %w", err)
	}
	provisional := filepath.Join(root, "marketplaces", "tmp-"+hashShort(url))
	defer os.RemoveAll(provisional)
	if out, err := exec.Command("git", "clone", "--depth", "1", url, provisional).CombinedOutput(); err != nil {
		_ = os.RemoveAll(provisional)
		return MarketplaceManifest{}, fmt.Errorf("plugin: git clone %s: %w: %s", url, err, strings.TrimSpace(string(out)))
	}
	manifest, err := readMarketplaceManifest(provisional)
	if err != nil {
		return MarketplaceManifest{}, err
	}
	if manifest.Name == "" {
		manifest.Name = provisionedName(url)
	}
	final := filepath.Join(root, "marketplaces", manifest.Name)
	if _, err := os.Stat(final); err == nil {
		return MarketplaceManifest{}, fmt.Errorf("plugin: marketplace %q already exists", manifest.Name)
	}
	if err := os.Rename(provisional, final); err != nil {
		return MarketplaceManifest{}, fmt.Errorf("plugin: rename marketplace: %w", err)
	}
	mps, _ := loadKnownMarketplaces(root)
	for _, mp := range mps {
		if mp.Name == manifest.Name {
			mp.URL = url
			return manifest, saveKnownMarketplaces(root, mps)
		}
	}
	mps = append(mps, KnownMarketplace{Name: manifest.Name, URL: url})
	return manifest, saveKnownMarketplaces(root, mps)
}

// RemoveMarketplace deregisters a marketplace and deletes its clone. Installed
// plugins from it stay installed (their cache copies are independent).
func RemoveMarketplace(root, name string) error {
	mps, _ := loadKnownMarketplaces(root)
	out := mps[:0]
	found := false
	for _, mp := range mps {
		if mp.Name == name {
			found = true
			continue
		}
		out = append(out, mp)
	}
	if !found {
		return fmt.Errorf("plugin: marketplace %q: %w", name, ErrNotFound)
	}
	if err := saveKnownMarketplaces(root, out); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(root, "marketplaces", name))
	return nil
}

// ListMarketplacePlugins returns the plugin entries a marketplace offers.
func ListMarketplacePlugins(root, name string) ([]MarketplacePlugin, error) {
	clone := filepath.Join(root, "marketplaces", name)
	if _, err := os.Stat(clone); err != nil {
		return nil, fmt.Errorf("plugin: marketplace %q: %w", name, ErrNotFound)
	}
	manifest, err := readMarketplaceManifest(clone)
	if err != nil {
		return nil, err
	}
	return manifest.Plugins, nil
}

// InstallFromMarketplace materializes a marketplace plugin into the cache and
// registers it. Entries with a relative source path are copied from the clone;
// entries with a URL source are downloaded as zip archives.
func InstallFromMarketplace(root, mpName, pluginName string) (InstalledEntry, error) {
	clone := filepath.Join(root, "marketplaces", mpName)
	manifest, err := readMarketplaceManifest(clone)
	if err != nil {
		return InstalledEntry{}, err
	}
	for _, entry := range manifest.Plugins {
		if entry.Name != pluginName {
			continue
		}
		if isRemoteSource(entry.Source) {
			return installZip(root, mpName, entry.Name, entry.Source, entry.SHA256, entry.StripRoot)
		}
		src := filepath.Join(clone, filepath.FromSlash(entry.Source))
		return installFromDir(root, mpName, entry.Name, src, PluginSource{
			Type: "marketplace", URL: marketplaceURL(root, mpName), Path: entry.Source, SHA256: entry.SHA256,
		})
	}
	return InstalledEntry{}, fmt.Errorf("plugin: marketplace %q has no plugin %q", mpName, pluginName)
}

// InstallZip downloads a plugin archive, verifies its digest and registers it.
func InstallZip(root, name, url, sha256Hex string, stripRoot bool) (InstalledEntry, error) {
	return installZip(root, "", name, url, sha256Hex, stripRoot)
}

// InstallLocal registers a local plugin directory without copying it, so
// plugin authors can iterate in place. The path must contain a valid manifest.
func InstallLocal(root, dir string) (InstalledEntry, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return InstalledEntry{}, fmt.Errorf("plugin: resolve %s: %w", dir, err)
	}
	return registerInstalled(root, "", abs, PluginSource{Type: "local", Path: abs})
}

// Enable flips a plugin's enabled flag; changes take effect on next process
// start (the host restarts to pick them up).
func Enable(root, name string) error { return setEnabled(root, name, true) }

// Disable flips a plugin's enabled flag; changes take effect on next process start.
func Disable(root, name string) error { return setEnabled(root, name, false) }

// Uninstall removes the registry record but keeps the cached files so a
// re-install does not need to re-download (comate semantics).
func Uninstall(root, name string) error {
	entries, err := loadRegistry(root)
	if err != nil {
		return err
	}
	idx := findEntry(entries, name)
	if idx < 0 {
		return fmt.Errorf("plugin: %q: %w", name, ErrNotFound)
	}
	entries = append(entries[:idx], entries[idx+1:]...)
	return saveRegistry(root, entries)
}

// ListInstalled returns the current registry entries.
func ListInstalled(root string) ([]InstalledEntry, error) { return loadRegistry(root) }

// --- internal install paths ---

func installZip(root, mpName, name, url, sha256Hex string, stripRoot bool) (InstalledEntry, error) {
	data, err := download(url)
	if err != nil {
		return InstalledEntry{}, err
	}
	if sha256Hex != "" {
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(sha256Hex))) != 1 {
			return InstalledEntry{}, fmt.Errorf("plugin: %s: sha256 mismatch (got %s)", name, got)
		}
	}
	version, dir, err := extractPluginZip(root, mpName, name, data, stripRoot)
	if err != nil {
		return InstalledEntry{}, err
	}
	src := PluginSource{Type: "zip", URL: url, SHA256: sha256Hex, StripRoot: stripRoot}
	return registerInstalled(root, mpName, dir, src, withVersion(version))
}

func installFromDir(root, mpName, name, src string, source PluginSource) (InstalledEntry, error) {
	if _, err := os.Stat(filepath.Join(src, ManifestDir, "plugin.json")); err != nil {
		return InstalledEntry{}, fmt.Errorf("plugin: %s: missing %s/plugin.json: %w", name, ManifestDir, err)
	}
	version, dir, err := copyTree(src, pluginCacheDir(root, mpName, name))
	if err != nil {
		return InstalledEntry{}, err
	}
	return registerInstalled(root, mpName, dir, source, withVersion(version))
}

// registerInstalled validates the materialized plugin, probes its version and
// records it in the registry (enabled by default).
func registerInstalled(root, mpName, dir string, source PluginSource, opts ...func(*InstalledEntry)) (InstalledEntry, error) {
	p, err := Load(dir)
	if err != nil {
		return InstalledEntry{}, err
	}
	if mpName == "" {
		mpName = "local"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	entry := InstalledEntry{
		ID:          p.Manifest.Name + "@" + mpName,
		Name:        p.Manifest.Name,
		Marketplace: mpName,
		Version:     p.Manifest.Version,
		InstallPath: dir,
		InstalledAt: now,
		UpdatedAt:   now,
		Scope:       "user",
		Source:      source,
		Enabled:     true,
	}
	for _, opt := range opts {
		opt(&entry)
	}
	entries, err := loadRegistry(root)
	if err != nil {
		return InstalledEntry{}, err
	}
	if err := saveRegistry(root, upsertEntry(entries, entry)); err != nil {
		return InstalledEntry{}, err
	}
	return entry, nil
}

func withVersion(v string) func(*InstalledEntry) {
	return func(e *InstalledEntry) {
		if v != "" {
			e.Version = v
		}
	}
}

func setEnabled(root, name string, enabled bool) error {
	entries, err := loadRegistry(root)
	if err != nil {
		return err
	}
	idx := findEntry(entries, name)
	if idx < 0 {
		return fmt.Errorf("plugin: %q: %w", name, ErrNotFound)
	}
	entries[idx].Enabled = enabled
	entries[idx].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return saveRegistry(root, entries)
}

// --- helpers ---

func pluginCacheDir(root, mpName, name string) string {
	if mpName == "" {
		mpName = "local"
	}
	return filepath.Join(root, "cache", mpName, name)
}

func readMarketplaceManifest(clone string) (MarketplaceManifest, error) {
	path := filepath.Join(clone, ManifestDir, "marketplace.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return MarketplaceManifest{}, fmt.Errorf("plugin: read %s: %w (not a marketplace repo?)", path, err)
	}
	var manifest MarketplaceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return MarketplaceManifest{}, fmt.Errorf("plugin: decode %s: %w", path, err)
	}
	return manifest, nil
}

func marketplaceURL(root, mpName string) string {
	mps, _ := loadKnownMarketplaces(root)
	for _, mp := range mps {
		if mp.Name == mpName {
			return mp.URL
		}
	}
	return ""
}

func isRemoteSource(source string) bool {
	s := strings.TrimSpace(source)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func provisionedName(url string) string {
	s := strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	if idx := strings.LastIndexAny(s, "/\\"); idx >= 0 {
		s = s[idx+1:]
	}
	if s == "" {
		return "marketplace-" + hashShort(url)
	}
	return s
}

func hashShort(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func download(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:gosec // url comes from admin-authenticated configuration
	if err != nil {
		return nil, fmt.Errorf("plugin: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plugin: download %s: status %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxZipBytes+1))
	if err != nil {
		return nil, fmt.Errorf("plugin: download %s: %w", url, err)
	}
	if len(data) > maxZipBytes {
		return nil, fmt.Errorf("plugin: download %s: exceeds %d bytes limit", url, maxZipBytes)
	}
	return data, nil
}
