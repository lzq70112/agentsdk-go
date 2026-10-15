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

// 默认关闭自动激活：未写字段的技能不参与 Match；显式写 false 才开启。
func TestLoadFromFSDefaultsAutoActivationOff(t *testing.T) {
	root := t.TempDir()
	write := func(name, frontmatter string) {
		lines := []string{"---", "name: " + name, "description: d"}
		if frontmatter != "" {
			lines = append(lines, frontmatter)
		}
		lines = append(lines, "---", "body")
		mustWrite(t, filepath.Join(root, ".agents", "skills", name, "SKILL.md"), strings.Join(lines, "\n"))
	}
	write("implicit", "")
	write("explicit-off", "disable-auto-activation: false")
	write("explicit-on", "disable-auto-activation: true")

	regs, errs := LoadFromFS(LoaderOptions{ProjectRoot: root})
	if len(errs) != 0 || len(regs) != 3 {
		t.Fatalf("unexpected load result regs=%d errs=%v", len(regs), errs)
	}
	byName := map[string]SkillRegistration{}
	for _, reg := range regs {
		byName[reg.Definition.Name] = reg
	}
	if !byName["implicit"].Definition.DisableAutoActivation {
		t.Fatalf("implicit skill should default to auto-activation off")
	}
	if !byName["explicit-on"].Definition.DisableAutoActivation {
		t.Fatalf("explicit true should keep auto-activation off")
	}
	if byName["explicit-off"].Definition.DisableAutoActivation {
		t.Fatalf("explicit false should enable auto-activation")
	}
}
