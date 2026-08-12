package api

import (
	"context"
	"strings"
	"testing"

	"github.com/stellarlinkco/agentsdk-go/pkg/model"
)

func TestSystemPromptBuilderBuildOrdersByPriority(t *testing.T) {
	t.Parallel()

	builder := NewSystemPromptBuilder()
	builder.AddSection("tools", "tools", 30)
	builder.AddSection("rules", "rules", 10)
	builder.AddSection("skills", "skills", 20)

	got := builder.Build()
	if got != "rules\n\nskills\n\ntools" {
		t.Fatalf("unexpected build order: %q", got)
	}
}

func TestRuntimeUsesInjectedSystemPromptBuilder(t *testing.T) {
	t.Parallel()

	root := newClaudeProject(t)
	builder := NewSystemPromptBuilder()
	builder.AddSection("custom", "custom-section", 20)
	builder.AddSection(SystemPromptSectionIdentity, "identity-section", SystemPromptPriorityIdentity)

	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{
		ProjectRoot:         root,
		Model:               mdl,
		SystemPromptBuilder: builder,
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if _, err := rt.Run(context.Background(), Request{Prompt: "hello"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(mdl.requests) == 0 {
		t.Fatal("expected model request")
	}
	if !strings.Contains(mdl.requests[0].System, "identity-section") || !strings.Contains(mdl.requests[0].System, "custom-section") {
		t.Fatalf("builder content missing from system prompt: %q", mdl.requests[0].System)
	}
	if strings.Index(mdl.requests[0].System, "identity-section") > strings.Index(mdl.requests[0].System, "custom-section") {
		t.Fatalf("unexpected section order: %q", mdl.requests[0].System)
	}
}
