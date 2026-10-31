package plugin

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
) // extractPluginZip unpacks a plugin archive into the cache layout and returns
// the probed version plus the materialized directory. Every entry path is
// validated against zip-slip attacks: cleaned paths must stay inside the
// destination, and absolute or parent-escaping names are rejected outright.
func extractPluginZip(root, mpName, name string, data []byte, stripRoot bool) (version, dir string, err error) {
	tmp, err := os.MkdirTemp("", "plugin-zip-")
	if err != nil {
		return "", "", fmt.Errorf("plugin: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	reader, err := zip.NewReader(strings.NewReader(string(data)), int64(len(data)))
	if err != nil {
		return "", "", fmt.Errorf("plugin: %s: open zip: %w", name, err)
	}
	prefix := ""
	if stripRoot {
		prefix = commonTopLevelDir(reader)
	}
	for _, f := range reader.File {
		clean := filepath.Clean(f.Name)
		if prefix != "" {
			trimmed, ok := strings.CutPrefix(filepath.ToSlash(clean), prefix)
			if !ok || trimmed == "" {
				continue // outside the common root: nothing to extract
			}
			clean = filepath.Clean(trimmed)
		}
		if filepath.IsAbs(clean) || hasParentRef(clean) {
			return "", "", fmt.Errorf("plugin: %s: unsafe zip entry %q", name, f.Name)
		}
		target := filepath.Join(tmp, clean)
		if target != tmp && !strings.HasPrefix(target, tmp+string(os.PathSeparator)) {
			return "", "", fmt.Errorf("plugin: %s: unsafe zip entry %q", name, f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", "", fmt.Errorf("plugin: mkdir %s: %w", target, err)
			}
			continue
		}
		if err := extractZipFile(f, target); err != nil {
			return "", "", err
		}
	}

	dest := pluginCacheDir(root, mpName, name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", "", fmt.Errorf("plugin: mkdir %s: %w", filepath.Dir(dest), err)
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", "", fmt.Errorf("plugin: clear %s: %w", dest, err)
	}
	version, err = probeVersion(tmp)
	if err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", "", fmt.Errorf("plugin: materialize %s: %w", dest, err)
	}
	return version, dest, nil
}

func extractZipFile(f *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("plugin: mkdir %s: %w", filepath.Dir(target), err)
	}
	src, err := f.Open()
	if err != nil {
		return fmt.Errorf("plugin: open zip entry %s: %w", f.Name, err)
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode().Perm())
	if err != nil {
		return fmt.Errorf("plugin: create %s: %w", target, err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil { //nolint:gosec // archive size capped at download time
		return fmt.Errorf("plugin: extract %s: %w", f.Name, err)
	}
	return nil
}

// commonTopLevelDir returns "top/" when every entry shares the same first path
// segment (the single-root case), otherwise "".
func commonTopLevelDir(r *zip.Reader) string {
	var top string
	for i, f := range r.File {
		first, _, found := strings.Cut(filepath.ToSlash(f.Name), "/")
		if !found {
			return "" // a top-level loose file: no single root
		}
		if i == 0 {
			top = first
		} else if first != top {
			return ""
		}
	}
	if top == "" {
		return ""
	}
	return top + "/"
}

// copyTree recursively copies src into dest (dest is created) and returns the
// probed plugin version plus dest.
func copyTree(src, dest string) (version string, dir string, err error) {
	if err := os.RemoveAll(dest); err != nil {
		return "", "", fmt.Errorf("plugin: clear %s: %w", dest, err)
	}
	err = filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks and specials when copying
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("plugin: read %s: %w", path, err)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("plugin: stat %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("plugin: mkdir %s: %w", filepath.Dir(target), err)
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		return "", "", fmt.Errorf("plugin: copy %s -> %s: %w", src, dest, err)
	}
	version, err = probeVersion(dest)
	if err != nil {
		return "", "", err
	}
	return version, dest, nil
}

// probeVersion reads the manifest of a materialized plugin to learn its version.
func probeVersion(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, ManifestDir, "plugin.json")); err != nil {
		return "", fmt.Errorf("plugin: materialized content has no %s/plugin.json", ManifestDir)
	}
	manifest, err := loadManifest(dir)
	if err != nil {
		return "", err
	}
	return manifest.Version, nil
}

func hasParentRef(clean string) bool {
	if clean == ".." {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}
