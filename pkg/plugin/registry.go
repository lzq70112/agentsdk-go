package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	registryFileName        = "installed_plugins.json"
	knownMarketplacesFileNm = "known_marketplaces.json"
)

// registryPaths returns the installed_plugins.json and known_marketplaces.json
// paths under pluginsRoot.
func registryPaths(root string) (string, string) {
	return filepath.Join(root, registryFileName), filepath.Join(root, knownMarketplacesFileNm)
}

// loadRegistry reads installed_plugins.json. A missing or corrupted file is
// treated as an empty registry (re-install restores it), matching the
// tolerant-recovery pattern used elsewhere in the SDK.
func loadRegistry(root string) ([]InstalledEntry, error) {
	path, _ := registryPaths(root)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugin: read %s: %w", path, err)
	}
	var reg installedRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, nil // corrupted: treated as empty
	}
	return reg.Plugins, nil
}

// saveRegistry atomically writes installed_plugins.json (tmp + rename).
func saveRegistry(root string, entries []InstalledEntry) error {
	path, _ := registryPaths(root)
	reg := installedRegistry{Version: 1, Plugins: entries}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("plugin: marshal %s: %w", path, err)
	}
	return atomicWrite(path, data)
}

// loadKnownMarketplaces reads known_marketplaces.json; missing or corrupted
// yields an empty list.
func loadKnownMarketplaces(root string) ([]KnownMarketplace, error) {
	_, path := registryPaths(root)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugin: read %s: %w", path, err)
	}
	var mps []KnownMarketplace
	if err := json.Unmarshal(data, &mps); err != nil {
		return nil, nil // corrupted: treated as empty
	}
	return mps, nil
}

// saveKnownMarketplaces atomically writes known_marketplaces.json.
func saveKnownMarketplaces(root string, mps []KnownMarketplace) error {
	_, path := registryPaths(root)
	data, err := json.MarshalIndent(mps, "", "  ")
	if err != nil {
		return fmt.Errorf("plugin: marshal %s: %w", path, err)
	}
	return atomicWrite(path, data)
}

// atomicWrite writes data to path via tmp file + rename in the same directory.
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("plugin: mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("plugin: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("plugin: rename %s: %w", path, err)
	}
	return nil
}

// upsertEntry replaces the entry with the same ID or appends it.
func upsertEntry(entries []InstalledEntry, entry InstalledEntry) []InstalledEntry {
	for i := range entries {
		if entries[i].ID == entry.ID {
			entries[i] = entry
			return entries
		}
	}
	return append(entries, entry)
}

// findEntry returns the index of the entry with the given plugin name, or -1.
func findEntry(entries []InstalledEntry, name string) int {
	for i := range entries {
		if entries[i].Name == name {
			return i
		}
	}
	return -1
}
