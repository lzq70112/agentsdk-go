package toolbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
)

const ToolSearchName = "tool_search"

var toolSearchSchema = &tool.JSONSchema{
	Type: "object",
	Properties: map[string]any{
		"query": map[string]any{
			"type":        "string",
			"description": "Tool name or capability to search for among deferred tools.",
		},
	},
	Required: []string{"query"},
}

type ToolSearchTool struct {
	deferred []tool.Tool
}

func NewToolSearchTool(tools []tool.Tool) *ToolSearchTool {
	deferred := make([]tool.Tool, 0, len(tools))
	for _, impl := range tools {
		if impl == nil || !tool.ShouldDefer(impl) {
			continue
		}
		deferred = append(deferred, impl)
	}
	return &ToolSearchTool{deferred: deferred}
}

func (t *ToolSearchTool) Name() string { return ToolSearchName }

func (t *ToolSearchTool) Description() string {
	return "Search deferred tools by name or description and activate matches for later turns."
}

func (t *ToolSearchTool) Schema() *tool.JSONSchema { return toolSearchSchema }

func (t *ToolSearchTool) Metadata() tool.Metadata {
	return tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true}
}

func (t *ToolSearchTool) Execute(_ context.Context, params map[string]any) (*tool.ToolResult, error) {
	query, err := toolSearchQuery(params)
	if err != nil {
		return nil, err
	}
	matches := t.matches(query)
	payload := make([]map[string]any, 0, len(matches))
	activated := make([]string, 0, len(matches))
	for _, impl := range matches {
		payload = append(payload, map[string]any{
			"name":        impl.Name(),
			"description": strings.TrimSpace(impl.Description()),
			"parameters":  toolSearchSchemaMap(impl.Schema()),
		})
		activated = append(activated, impl.Name())
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &tool.ToolResult{
		Success: true,
		Output:  string(raw),
		Data: map[string]any{
			"matches":   payload,
			"activated": activated,
		},
	}, nil
}

func toolSearchQuery(params map[string]any) (string, error) {
	if params == nil {
		return "", errors.New("query is required")
	}
	raw, ok := params["query"]
	if !ok {
		return "", errors.New("query is required")
	}
	query, ok := raw.(string)
	if !ok {
		return "", errors.New("query must be string")
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return "", errors.New("query cannot be empty")
	}
	return query, nil
}

func (t *ToolSearchTool) matches(query string) []tool.Tool {
	if t == nil || len(t.deferred) == 0 {
		return nil
	}
	var matches []tool.Tool
	for _, impl := range t.deferred {
		name := strings.ToLower(strings.TrimSpace(impl.Name()))
		desc := strings.ToLower(strings.TrimSpace(impl.Description()))
		if strings.Contains(name, query) || strings.Contains(desc, query) {
			matches = append(matches, impl)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		return strings.TrimSpace(matches[i].Name()) < strings.TrimSpace(matches[j].Name())
	})
	return matches
}

func toolSearchSchemaMap(schema *tool.JSONSchema) map[string]any {
	if schema == nil {
		return nil
	}
	out := map[string]any{}
	if schema.Type != "" {
		out["type"] = schema.Type
	}
	if len(schema.Properties) > 0 {
		out["properties"] = schema.Properties
	}
	if len(schema.Required) > 0 {
		out["required"] = append([]string(nil), schema.Required...)
	}
	return out
}
