package plugin

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestInstallLocal(t *testing.T) {
	root := t.TempDir()
	dir := writePlugin(t, "local-one", "installed in place", map[string]string{
		"skills/hello/SKILL.md": "---\nname: hello\ndescription: hello skill\n---\nbody",
	}, nil)

	entry, err := InstallLocal(root, dir)
	if err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if entry.ID != "local-one@local" || !entry.Enabled || entry.Source.Type != "local" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if entry.InstallPath != dir {
		t.Fatalf("local install must reference the original path, got %s", entry.InstallPath)
	}
	entries, err := ListInstalled(root)
	if err != nil || len(entries) != 1 || entries[0].Name != "local-one" {
		t.Fatalf("registry mismatch: %v %+v", err, entries)
	}
}

func TestInstallLocalMissingManifest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallLocal(root, dir); err == nil {
		t.Fatal("installing a directory without a manifest must fail")
	}
}

func TestInstallZipWithChecksum(t *testing.T) {
	data := zipBytes(t, map[string]string{
		".claude-plugin/plugin.json": `{"name": "zipper", "description": "from zip", "version": "2.0.0"}`,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	root := t.TempDir()
	entry, err := InstallZip(root, "zipper", srv.URL, sha256Hex(t, data), false)
	if err != nil {
		t.Fatalf("InstallZip: %v", err)
	}
	if entry.Version != "2.0.0" || entry.ID != "zipper@local" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if _, err := os.Stat(filepath.Join(entry.InstallPath, ManifestDir, "plugin.json")); err != nil {
		t.Fatalf("cache not materialized: %v", err)
	}

	if _, err := InstallZip(root, "zipper", srv.URL, strings.Repeat("0", 64), false); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("wrong digest must be rejected, got %v", err)
	}
}

func TestInstallZipStripRoot(t *testing.T) {
	data := zipBytes(t, map[string]string{
		"wrapped/.claude-plugin/plugin.json": `{"name": "wrapped", "description": "single root zip"}`,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	root := t.TempDir()
	entry, err := InstallZip(root, "wrapped", srv.URL, sha256Hex(t, data), true)
	if err != nil {
		t.Fatalf("InstallZip stripRoot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(entry.InstallPath, ManifestDir, "plugin.json")); err != nil {
		t.Fatalf("single top-level dir was not stripped: %v", err)
	}
}

func TestZipSlipRejected(t *testing.T) {
	data := zipBytes(t, map[string]string{
		".claude-plugin/plugin.json": `{"name": "evil", "description": "escape"}`,
		"../evil.txt":                "pwn",
	})
	root := t.TempDir()
	if _, _, err := extractPluginZip(root, "", "evil", data, false); err == nil || !strings.Contains(err.Error(), "unsafe zip entry") {
		t.Fatalf("zip-slip entry must be rejected, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "evil.txt")); err == nil {
		t.Fatal("escaped file must not be written")
	}
}

func TestRegistryCorruptedTolerated(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, registryFileName), []byte("{{{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := loadRegistry(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("corrupted registry must read as empty, got %v %v", entries, err)
	}
	plugins, errs := Discover(root)
	if len(plugins) != 0 || len(errs) != 0 {
		t.Fatalf("discover over corrupted registry must be a no-op, got %+v %v", plugins, errs)
	}
	if err := Enable(root, "ghost"); err == nil || !strings.Contains(err.Error(), ErrNotFound.Error()) {
		t.Fatalf("enable on missing plugin must return ErrNotFound, got %v", err)
	}
}

func TestUninstallKeepsCache(t *testing.T) {
	root := t.TempDir()
	dir := writePlugin(t, "cached", "d", nil, nil)
	entry, err := InstallLocal(root, dir)
	if err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := Uninstall(root, "cached"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if entries, _ := ListInstalled(root); len(entries) != 0 {
		t.Fatalf("uninstall must drop the registry entry, got %+v", entries)
	}
	// comate 语义：卸载只摘记录，缓存目录保留以便快速重装。
	if _, err := os.Stat(entry.InstallPath); err != nil {
		t.Fatalf("uninstall must keep the cached files: %v", err)
	}
}

// initGitRepo turns dir into a git repository with one commit, so it can be
// cloned via file:// by the marketplace tests.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("add", ".")
	run("commit", "-m", "init")
}

func writeMarketplaceRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "mp-repo")
	files := map[string]string{
		".claude-plugin/marketplace.json":            `{"name": "test-mp", "plugins": [{"name": "greeter", "description": "greets", "source": "plugins/greeter"}]}`,
		"plugins/greeter/.claude-plugin/plugin.json": `{"name": "greeter", "description": "greets people", "version": "0.1.0"}`,
		"plugins/greeter/skills/hello/SKILL.md":      "---\nname: hello\ndescription: hello skill\n---\nbody",
	}
	for rel, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	initGitRepo(t, repo)
	return repo
}

func TestMarketplaceLifecycle(t *testing.T) {
	repo := writeMarketplaceRepo(t)
	root := t.TempDir()
	url := "file:///" + filepath.ToSlash(repo)

	manifest, err := AddMarketplace(root, url)
	if err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if manifest.Name != "test-mp" || len(manifest.Plugins) != 1 {
		t.Fatalf("unexpected marketplace manifest: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(root, "marketplaces", "test-mp", ".claude-plugin", "marketplace.json")); err != nil {
		t.Fatalf("marketplace clone missing: %v", err)
	}

	plugins, err := ListMarketplacePlugins(root, "test-mp")
	if err != nil || len(plugins) != 1 || plugins[0].Name != "greeter" {
		t.Fatalf("ListMarketplacePlugins mismatch: %v %+v", err, plugins)
	}

	entry, err := InstallFromMarketplace(root, "test-mp", "greeter")
	if err != nil {
		t.Fatalf("InstallFromMarketplace: %v", err)
	}
	if entry.ID != "greeter@test-mp" || entry.Version != "0.1.0" || !entry.Enabled {
		t.Fatalf("unexpected installed entry: %+v", entry)
	}

	installed, err := ListInstalled(root)
	if err != nil || len(installed) != 1 || !strings.HasPrefix(installed[0].InstallPath, filepath.Join(root, "cache")) {
		t.Fatalf("registry/cache mismatch: %v %+v", err, installed)
	}

	if err := Disable(root, "greeter"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if got, _ := Discover(root); len(got) != 0 {
		t.Fatalf("disabled plugin must not be discovered, got %+v", got)
	}
	if err := Enable(root, "greeter"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	got, errs := Discover(root)
	if len(errs) != 0 || len(got) != 1 || len(got[0].Skills) != 1 {
		t.Fatalf("enabled plugin must be discovered with its skill, got %+v %v", got, errs)
	}

	if err := Uninstall(root, "greeter"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if entries, _ := ListInstalled(root); len(entries) != 0 {
		t.Fatalf("uninstall must drop the entry, got %+v", entries)
	}

	if err := RemoveMarketplace(root, "test-mp"); err != nil {
		t.Fatalf("RemoveMarketplace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "marketplaces", "test-mp")); err == nil {
		t.Fatal("marketplace clone must be removed")
	}
	if _, err := AddMarketplace(root, url); err == nil {
		_ = err // re-add after removal must succeed — checked below
	}
	if _, err := os.Stat(filepath.Join(root, "marketplaces", "test-mp")); err != nil {
		t.Fatalf("marketplace re-add failed: %v", err)
	}
}

func TestAddMarketplaceDuplicateRejected(t *testing.T) {
	repo := writeMarketplaceRepo(t)
	root := t.TempDir()
	url := "file:///" + filepath.ToSlash(repo)
	if _, err := AddMarketplace(root, url); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if _, err := AddMarketplace(root, url); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate marketplace must be rejected, got %v", err)
	}
}
