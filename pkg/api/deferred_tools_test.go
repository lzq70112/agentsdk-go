package api

import (
	"context"
	"strings"
	"testing"

	"github.com/stellarlinkco/agentsdk-go/pkg/model"
	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
	toolbuiltin "github.com/stellarlinkco/agentsdk-go/pkg/tool/builtin"
)

type deferredFooTool struct {
	calls int
}

func (t *deferredFooTool) Name() string             { return "foo" }
func (t *deferredFooTool) Description() string      { return "foo capability" }
func (t *deferredFooTool) Schema() *tool.JSONSchema { return &tool.JSONSchema{Type: "object"} }
func (t *deferredFooTool) ShouldDefer() bool        { return true }
func (t *deferredFooTool) Execute(context.Context, map[string]any) (*tool.ToolResult, error) {
	t.calls++
	return &tool.ToolResult{Success: true, Output: "foo"}, nil
}

func TestAvailableToolsForSessionExcludesDeferredUntilActivated(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	foo := &deferredFooTool{}
	if err := reg.Register(foo); err != nil {
		t.Fatalf("register foo: %v", err)
	}
	if err := reg.Register(toolbuiltin.NewToolSearchTool([]tool.Tool{foo})); err != nil {
		t.Fatalf("register search: %v", err)
	}

	state := newDeferredToolState(reg)
	initial := availableToolsForSession(reg, nil, state, "")
	if len(initial) != 1 || canonicalToolName(initial[0].Name) != canonicalToolName(toolbuiltin.ToolSearchName) {
		t.Fatalf("unexpected initial tools: %+v", initial)
	}

	state.activate("sess", []string{"foo"})
	after := availableToolsForSession(reg, nil, state, "sess")
	if len(after) != 2 {
		t.Fatalf("unexpected activated tools: %+v", after)
	}
}

func TestRuntimeToolSearchActivatesDeferredToolsForLaterTurns(t *testing.T) {
	t.Parallel()

	root := newClaudeProject(t)
	foo := &deferredFooTool{}
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID:        "tool_1",
			Name:      toolbuiltin.ToolSearchName,
			Arguments: map[string]any{"query": "foo"},
		}}}},
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID:        "tool_2",
			Name:      "foo",
			Arguments: map[string]any{},
		}}}},
		{Message: model.Message{Role: "assistant", Content: "done"}},
	}}

	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       mdl,
		Tools:       []tool.Tool{foo},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	resp, err := rt.Run(context.Background(), Request{Prompt: "find foo"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp == nil || resp.Result == nil || resp.Result.Output != "done" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if foo.calls != 1 {
		t.Fatalf("expected deferred tool execution, got %d", foo.calls)
	}
	if len(mdl.requests) != 3 {
		t.Fatalf("expected 3 model requests, got %d", len(mdl.requests))
	}
	if hasToolDef(mdl.requests[0], toolbuiltin.ToolSearchName) == false {
		t.Fatalf("ToolSearch missing from first request: %+v", mdl.requests[0].Tools)
	}
	if hasToolDef(mdl.requests[0], "foo") {
		t.Fatalf("deferred tool leaked into first request: %+v", mdl.requests[0].Tools)
	}
	if !hasToolDef(mdl.requests[1], "foo") {
		t.Fatalf("deferred tool not activated in second request: %+v", mdl.requests[1].Tools)
	}
	if want := "<available-deferred-tools>\nfoo\n</available-deferred-tools>"; mdl.requests[0].System == "" || !contains(mdl.requests[0].System, want) {
		t.Fatalf("missing deferred tool section: %q", mdl.requests[0].System)
	}
}

func hasToolDef(req model.Request, name string) bool {
	for _, def := range req.Tools {
		if canonicalToolName(def.Name) == canonicalToolName(name) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
