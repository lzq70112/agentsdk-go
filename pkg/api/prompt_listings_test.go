package api

import (
	"context"
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/runtime/skills"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/subagents"
)

func TestBuildSkillsListingEmpty(t *testing.T) {
	if out := buildSkillsListing(nil); out != "" {
		t.Fatalf("nil registry should yield empty listing: %q", out)
	}
	if out := buildSkillsListing(skills.NewRegistry()); out != "" {
		t.Fatalf("empty registry should yield empty listing: %q", out)
	}
}

func TestBuildSkillsListingSortedFormat(t *testing.T) {
	reg := skills.NewRegistry()
	noop := skills.HandlerFunc(func(context.Context, skills.ActivationContext) (skills.Result, error) {
		return skills.Result{}, nil
	})
	for _, def := range []skills.Definition{
		{Name: "zeta", Description: "last"},
		{Name: "alpha", Description: "first"},
		{Name: "mid", Description: "middle"},
	} {
		if err := reg.Register(def, noop); err != nil {
			t.Fatalf("register %s: %v", def.Name, err)
		}
	}
	out := buildSkillsListing(reg)
	if !strings.HasPrefix(out, "# Available skills") {
		t.Fatalf("missing heading: %q", out)
	}
	i, j := strings.Index(out, "- alpha:"), strings.Index(out, "- zeta:")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("entries not sorted by name: %q", out)
	}
	if !strings.Contains(out, "- alpha: first\n") {
		t.Fatalf("missing formatted entry: %q", out)
	}
}

func TestBuildSubagentsListingEmptyAndSorted(t *testing.T) {
	if out := buildSubagentsListing(nil); out != "" {
		t.Fatalf("nil manager should yield empty listing: %q", out)
	}
	if out := buildSubagentsListing(subagents.NewManager()); out != "" {
		t.Fatalf("empty manager should yield empty listing: %q", out)
	}

	mgr := subagents.NewManager()
	noop := subagents.HandlerFunc(func(context.Context, subagents.Context, subagents.Request) (subagents.Result, error) {
		return subagents.Result{}, nil
	})
	for _, def := range []subagents.Definition{
		{Name: "worker", Description: "does work"},
		{Name: "analyst", Description: "analyzes"},
	} {
		if err := mgr.Register(def, noop); err != nil {
			t.Fatalf("register %s: %v", def.Name, err)
		}
	}
	out := buildSubagentsListing(mgr)
	if !strings.HasPrefix(out, "# Available agent types") {
		t.Fatalf("missing heading: %q", out)
	}
	i, j := strings.Index(out, "- analyst:"), strings.Index(out, "- worker:")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("entries not sorted by name: %q", out)
	}
}

func TestBuildMCPInstructionsListing(t *testing.T) {
	if out := buildMCPInstructionsListing(nil); out != "" {
		t.Fatalf("nil servers should yield empty listing: %q", out)
	}
	out := buildMCPInstructionsListing([]mcpServer{
		{Name: "zeta-mcp", Spec: "x"},
		{Name: "", Spec: "skipped"},
		{Name: "alpha-mcp", Spec: "y"},
	})
	if !strings.HasPrefix(out, "# MCP servers") {
		t.Fatalf("missing heading: %q", out)
	}
	if !strings.Contains(out, "mcp__<server>__<tool>") {
		t.Fatalf("missing tool naming convention: %q", out)
	}
	i, j := strings.Index(out, "- alpha-mcp"), strings.Index(out, "- zeta-mcp")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("servers not sorted by name: %q", out)
	}
	if strings.Count(out, "- ") != 2 {
		t.Fatalf("empty-name server should be skipped: %q", out)
	}
}
