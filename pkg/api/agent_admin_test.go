package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellarlinkco/agentsdk-go/pkg/model"
)

func TestRuntimeUpdateSystemPrompt(t *testing.T) {
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// Initially, no identity section for our new prompt.
	before := rt.GetSystemPrompt()
	if strings.Contains(before, "my-new-prompt") {
		t.Fatal("expected prompt to not contain new text before update")
	}

	// Update the identity section.
	rt.UpdateSystemPrompt("You are a helpful ops bot.")

	// In-memory snapshot reflects new prompt immediately.
	after := rt.GetSystemPrompt()
	if !strings.Contains(after, "You are a helpful ops bot.") {
		t.Fatalf("expected prompt to contain new text, got: %q", after)
	}

	// Hot-reload semantics: the next Run must pick up the new system prompt.
	// This is WHY GetSystemPrompt/UpdateSystemPrompt exist — admins edit the
	// prompt at runtime and expect it to apply to subsequent requests, not a
	// future restart.
	resp, err := rt.Run(context.Background(), Request{Prompt: "test"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Result == nil {
		t.Fatal("expected response result")
	}
	if len(mdl.requests) == 0 {
		t.Fatal("expected at least one model request")
	}
	systemPrompt := mdl.requests[0].System
	if !strings.Contains(systemPrompt, "You are a helpful ops bot.") {
		t.Fatalf("system prompt not applied to model request: %q", systemPrompt)
	}
}

func TestRuntimeUpdateSystemPromptEmpty(t *testing.T) {
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// Set a prompt, then clear it: an empty update removes the identity section.
	rt.UpdateSystemPrompt("initial prompt")
	if !strings.Contains(rt.GetSystemPrompt(), "initial prompt") {
		t.Fatal("expected prompt to contain initial text")
	}

	rt.UpdateSystemPrompt("")
	// Other sections (rules, memory) may remain, but the cleared text must be gone.
	if strings.Contains(rt.GetSystemPrompt(), "initial prompt") {
		t.Fatal("expected prompt to not contain cleared text")
	}
}

func TestRuntimeReloadAgentsMD(t *testing.T) {
	root := newClaudeProject(t)

	// Seed an AGENTS.md so New() loads it into the memory section.
	initial := "# Project Memory\n\nThis is the project context."
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(initial), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// At init, the memory section reflects the seeded AGENTS.md.
	if prompt := rt.GetSystemPrompt(); !strings.Contains(prompt, "Project Memory") {
		t.Fatalf("expected initial prompt to contain AGENTS.md content, got: %q", prompt)
	}

	// Hot-reload scenario: the file changes at runtime.
	updated := "# Updated Memory\n\nThe context has changed."
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(updated), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	// Before reload, the stale content is still cached — this is WHY ReloadAgentsMD
	// exists: admins edit AGENTS.md and must re-read it without restarting.
	if err := rt.ReloadAgentsMD(); err != nil {
		t.Fatalf("ReloadAgentsMD: %v", err)
	}

	prompt := rt.GetSystemPrompt()
	if !strings.Contains(prompt, "Updated Memory") {
		t.Fatalf("expected prompt to contain updated AGENTS.md content, got: %q", prompt)
	}
	if strings.Contains(prompt, "Project Memory") {
		t.Fatal("expected old AGENTS.md content to be replaced, not appended")
	}
}

func TestRuntimeUpdateSettings(t *testing.T) {
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// Verify initial DisallowedTools is empty
	s1 := rt.Settings()
	if len(s1.DisallowedTools) != 0 {
		t.Fatalf("expected empty DisallowedTools initially, got: %v", s1.DisallowedTools)
	}

	// Update settings
	newSettings := *s1
	newSettings.DisallowedTools = []string{"bash", "write"}
	rt.UpdateSettings(&newSettings)

	// Verify
	s2 := rt.Settings()
	if len(s2.DisallowedTools) != 2 {
		t.Fatalf("expected 2 DisallowedTools, got: %v", s2.DisallowedTools)
	}
}

func TestRuntimeListMCPServersEmpty(t *testing.T) {
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	servers := rt.ListMCPServers()
	if len(servers) != 0 {
		t.Fatalf("expected 0 MCP servers, got %d", len(servers))
	}
}

func TestRuntimeRemoveMCPServerNotFound(t *testing.T) {
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if err := rt.RemoveMCPServer("nonexistent"); err == nil {
		t.Fatal("expected error for non-existent MCP server")
	}
}

func TestRuntimeReloadAndUnregisterSkill(t *testing.T) {
	root := newClaudeProject(t)

	// Create a skill directory
	skillDir := filepath.Join(root, ".agents", "skills", "my-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\nname: my-skill\ndescription: \"test\"\n---\n# Body\n\nHello.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// Skill should be loaded at init
	skills1 := rt.ListSkills()
	found := false
	for _, s := range skills1 {
		if s.Name == "my-skill" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected my-skill to be loaded at init")
	}

	// Modify the skill
	newContent := "---\nname: my-skill\ndescription: \"updated\"\n---\n# Updated Body\n\nNew content.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(newContent), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Reload the skill
	if err := rt.ReloadSkill("my-skill"); err != nil {
		t.Fatalf("ReloadSkill: %v", err)
	}

	// Verify description updated
	skills2 := rt.ListSkills()
	for _, s := range skills2 {
		if s.Name == "my-skill" {
			if s.Description != "updated" {
				t.Fatalf("expected updated description, got %q", s.Description)
			}
		}
	}

	// Unregister the skill
	if !rt.UnregisterSkill("my-skill") {
		t.Fatal("expected UnregisterSkill to return true")
	}

	// Verify it's gone
	skills3 := rt.ListSkills()
	for _, s := range skills3 {
		if s.Name == "my-skill" {
			t.Fatal("expected my-skill to be unregistered")
		}
	}
}
