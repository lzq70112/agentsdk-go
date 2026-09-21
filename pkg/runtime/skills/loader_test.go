package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileOverrideOrOS(t *testing.T) {
	fileOpOverridesMu.Lock()
	fileOpOverrides.read = func(string) ([]byte, error) { return []byte("ok"), nil }
	fileOpOverridesMu.Unlock()
	defer func() {
		fileOpOverridesMu.Lock()
		fileOpOverrides.read = nil
		fileOpOverridesMu.Unlock()
	}()

	data, err := readFileOverrideOrOS("any")
	if err != nil || string(data) != "ok" {
		t.Fatalf("unexpected read %q err=%v", data, err)
	}

	fileOpOverridesMu.Lock()
	fileOpOverrides.read = func(string) ([]byte, error) { return nil, errors.New("boom") }
	fileOpOverridesMu.Unlock()
	if _, err := readFileOverrideOrOS("any"); err == nil {
		t.Fatalf("expected override error")
	}

	// fallback to os.ReadFile when override is nil
	fileOpOverridesMu.Lock()
	fileOpOverrides.read = nil
	fileOpOverridesMu.Unlock()
	path := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	data, err = readFileOverrideOrOS(path)
	if err != nil || string(data) != "data" {
		t.Fatalf("expected fallback read, got %q err=%v", data, err)
	}
}

func TestLoadFromFSRespectsDisableAutoActivation(t *testing.T) {
	root := t.TempDir()
	skillPath := filepath.Join(root, ".agents", "skills", "manual", "SKILL.md")
	content := strings.Join([]string{
		"---",
		"name: manual",
		"description: only via tool",
		"disable-auto-activation: true",
		"---",
		"body",
	}, "\n")
	mustWrite(t, skillPath, content)

	regs, errs := LoadFromFS(LoaderOptions{ProjectRoot: root})
	if len(errs) != 0 || len(regs) != 1 {
		t.Fatalf("unexpected load result regs=%v errs=%v", regs, errs)
	}
	if !regs[0].Definition.DisableAutoActivation {
		t.Fatalf("expected DisableAutoActivation=true, got %+v", regs[0].Definition)
	}
}
