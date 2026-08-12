package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/config"
)

func TestRegistryUnregister(t *testing.T) {
	reg := NewRegistry()

	def := Definition{Name: "my-skill", Description: "test"}
	handler := HandlerFunc(func(context.Context, ActivationContext) (Result, error) {
		return Result{Output: "ok"}, nil
	})
	if err := reg.Register(def, handler); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Verify it exists
	if _, ok := reg.Get("my-skill"); !ok {
		t.Fatal("expected skill to exist before unregister")
	}

	// Unregister
	if !reg.Unregister("my-skill") {
		t.Fatal("expected Unregister to return true for existing skill")
	}

	// Verify it's gone
	if _, ok := reg.Get("my-skill"); ok {
		t.Fatal("expected skill to be removed after unregister")
	}

	// Unregister again should return false
	if reg.Unregister("my-skill") {
		t.Fatal("expected Unregister to return false for non-existent skill")
	}
}

func TestRegistryUnregisterCaseInsensitive(t *testing.T) {
	reg := NewRegistry()

	// Register with a valid lowercase name (Validate() rejects uppercase).
	def := Definition{Name: "my-skill", Description: "test"}
	handler := HandlerFunc(func(context.Context, ActivationContext) (Result, error) {
		return Result{Output: "ok"}, nil
	})
	if err := reg.Register(def, handler); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Unregister with different case — lookup is case-insensitive.
	if !reg.Unregister("MY-SKILL") {
		t.Fatal("expected case-insensitive unregister to succeed")
	}
	// Verify the skill is actually gone, not just that Unregister returned true.
	if _, ok := reg.Get("my-skill"); ok {
		t.Fatal("expected skill to be removed after case-insensitive unregister")
	}
}

// TestLoadSingleSkill verifies the single-skill loader so that Runtime.ReloadSkill
// can re-read a modified SKILL.md at runtime. It must produce the same Definition
// shape as the bulk LoadFromFS path (name from metadata, description, handler).
func TestLoadSingleSkill(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".agents", "skills", "test-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\nname: test-skill\ndescription: \"A test skill\"\n---\n# Test Skill\n\nHello world.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	fsLayer := config.NewFS(root, nil)
	reg, err := LoadSingleSkill(skillDir, fsLayer)
	if err != nil {
		t.Fatalf("LoadSingleSkill: %v", err)
	}
	if reg.Definition.Name != "test-skill" {
		t.Fatalf("expected name 'test-skill', got %q", reg.Definition.Name)
	}
	if reg.Handler == nil {
		t.Fatal("expected non-nil handler")
	}
}

// TestLoadSingleSkillMissingFile ensures the loader surfaces a clear error
// when the SKILL.md is absent, rather than silently registering an empty skill.
func TestLoadSingleSkillMissingFile(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".agents", "skills", "ghost")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fsLayer := config.NewFS(root, nil)
	if _, err := LoadSingleSkill(skillDir, fsLayer); err == nil {
		t.Fatal("expected error when SKILL.md is missing")
	}
}
