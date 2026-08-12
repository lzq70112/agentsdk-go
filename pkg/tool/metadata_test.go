package tool_test

import (
	"context"
	"testing"

	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
	toolbuiltin "github.com/stellarlinkco/agentsdk-go/pkg/tool/builtin"
)

type stubMetadataTool struct{}

func (stubMetadataTool) Name() string { return "plain" }

func (stubMetadataTool) Description() string { return "plain" }

func (stubMetadataTool) Schema() *tool.JSONSchema { return nil }

func (stubMetadataTool) Execute(context.Context, map[string]any) (*tool.ToolResult, error) {
	return &tool.ToolResult{Success: true}, nil
}

func TestMetadataOfDefaultsAndBuiltins(t *testing.T) {
	t.Parallel()

	if got := tool.MetadataOf(stubMetadataTool{}); got != (tool.Metadata{}) {
		t.Fatalf("default metadata = %+v, want zero value", got)
	}

	tests := []struct {
		name string
		tool tool.Tool
		want tool.Metadata
	}{
		{
			name: "read",
			tool: toolbuiltin.NewReadTool(),
			want: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true},
		},
		{
			name: "glob",
			tool: toolbuiltin.NewGlobTool(),
			want: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true},
		},
		{
			name: "grep",
			tool: toolbuiltin.NewGrepTool(),
			want: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true},
		},
		{
			name: "bash",
			tool: toolbuiltin.NewBashTool(),
			want: tool.Metadata{IsDestructive: true},
		},
		{
			name: "write",
			tool: toolbuiltin.NewWriteTool(),
			want: tool.Metadata{},
		},
		{
			name: "edit",
			tool: toolbuiltin.NewEditTool(),
			want: tool.Metadata{},
		},
		{
			name: "tool_search",
			tool: toolbuiltin.NewToolSearchTool(nil),
			want: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tool.MetadataOf(tt.tool); got != tt.want {
				t.Fatalf("MetadataOf(%s) = %+v, want %+v", tt.name, got, tt.want)
			}
		})
	}
}
