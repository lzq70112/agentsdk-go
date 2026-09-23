package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/config"
)

func TestLoadSettingsMergesOverridesAndInitialisesEnv(t *testing.T) {
	root := t.TempDir()
	agentsDir := filepath.Join(root, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}

	overlayPath := filepath.Join(root, "custom_settings.json")
	if err := os.WriteFile(overlayPath, []byte(`{"env":{"A":"B"},"permissions":{"additionalDirectories":["/tmp/data"]}}`), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	overrideEnv := map[string]string{"C": "D"}
	override := &config.Settings{Model: "override-model", Env: overrideEnv}
	opts := Options{ProjectRoot: root, SettingsPath: overlayPath, SettingsOverrides: override}

	settings, err := loadSettings(opts)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if settings.Env["A"] != "B" || settings.Env["C"] != "D" {
		t.Fatalf("env merge failed: %+v", settings.Env)
	}
	if settings.Model != "override-model" {
		t.Fatalf("override model lost, got %s", settings.Model)
	}
	if settings.Permissions == nil || len(settings.Permissions.AdditionalDirectories) != 1 {
		t.Fatalf("permissions mapping missing: %+v", settings.Permissions)
	}
}

func TestLoadSettingsUsesDefaultsWhenProjectConfigMissing(t *testing.T) {
	root := t.TempDir()

	settings, err := loadSettings(Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if settings == nil {
		t.Fatal("expected defaults, got nil settings")
	}
	if settings.CleanupPeriodDays == nil || *settings.CleanupPeriodDays != 30 {
		got := 0
		if settings.CleanupPeriodDays != nil {
			got = *settings.CleanupPeriodDays
		}
		t.Fatalf("expected default cleanup period 30, got %d", got)
	}
	if settings.Permissions == nil || settings.Permissions.DefaultMode != "askBeforeRunningTools" {
		t.Fatalf("default permissions not applied: %+v", settings.Permissions)
	}
	if settings.Sandbox == nil || settings.Sandbox.Enabled == nil || *settings.Sandbox.Enabled {
		t.Fatalf("expected sandbox disabled by default, got %+v", settings.Sandbox)
	}
	if settings.Env == nil || len(settings.Env) != 0 {
		t.Fatalf("expected empty env map, got %+v", settings.Env)
	}
}

func TestLoadSettingsClonesCustomLoader(t *testing.T) {
	root := t.TempDir()
	originalOverrides := &config.Settings{Model: "custom"}
	custom := &config.SettingsLoader{ProjectRoot: root, RuntimeOverrides: originalOverrides}

	settings, err := loadSettings(Options{
		ProjectRoot:       root,
		SettingsLoader:    custom,
		SettingsOverrides: &config.Settings{Model: "override"},
	})
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if settings.Model != "override" {
		t.Fatalf("expected override model, got %s", settings.Model)
	}
	if custom.RuntimeOverrides != originalOverrides {
		t.Fatalf("expected caller-provided loader to remain unchanged")
	}
}

func TestProjectConfigFromSettingsNilInput(t *testing.T) {
	cfg := projectConfigFromSettings(nil)
	if cfg == nil {
		t.Fatal("expected defensive config")
	}
	if cfg.Env == nil || cfg.Permissions == nil {
		t.Fatalf("expected defaulted fields, got env=%+v perms=%+v", cfg.Env, cfg.Permissions)
	}
}

func TestLoadSettingsFileIgnoresEmptyPath(t *testing.T) {
	settings, err := loadSettingsFile("   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if settings != nil {
		t.Fatalf("expected nil settings for empty path, got %+v", settings)
	}
}

func TestLoadSettingsFileMissingPathErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if _, err := loadSettingsFile(path); err == nil {
		t.Fatal("expected error for missing explicit path")
	}
}

func TestLoadSettingsErrorsOnMissingExplicitOverlay(t *testing.T) {
	root := t.TempDir()
	opts := Options{ProjectRoot: root, SettingsPath: filepath.Join(root, "absent.json")}
	if _, err := loadSettings(opts); err == nil {
		t.Fatal("expected loadSettings to fail for missing overlay")
	}
}

func streamStallBoolPtr(v bool) *bool { return &v }

func TestApplyStreamStallFromSettings(t *testing.T) {
	opts := Options{}.withDefaults()
	if opts.StreamStall.Timeout != 60*time.Second {
		t.Fatalf("unexpected default timeout %v", opts.StreamStall.Timeout)
	}
	if !opts.StreamStall.FallbackEnabled {
		t.Fatalf("expected default fallback enabled")
	}

	settings := &config.Settings{
		StreamStall: &config.StreamStallConfig{
			Timeout:         "300s",
			FallbackEnabled: streamStallBoolPtr(false),
		},
	}
	merged := applyStreamStallFromSettings(opts, settings)
	if merged.StreamStall.Timeout != 300*time.Second {
		t.Fatalf("expected timeout 300s, got %v", merged.StreamStall.Timeout)
	}
	if merged.StreamStall.FallbackEnabled {
		t.Fatalf("expected fallback disabled")
	}

	// Empty settings should leave defaults intact.
	merged = applyStreamStallFromSettings(opts, &config.Settings{})
	if merged.StreamStall.Timeout != 60*time.Second {
		t.Fatalf("default timeout changed unexpectedly: %v", merged.StreamStall.Timeout)
	}

	// Invalid duration is ignored and logged; defaults are preserved.
	settings.StreamStall.Timeout = "not-a-duration"
	merged = applyStreamStallFromSettings(opts, settings)
	if merged.StreamStall.Timeout != 60*time.Second {
		t.Fatalf("invalid duration should not override default, got %v", merged.StreamStall.Timeout)
	}
}
