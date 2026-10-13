package api

import (
	"context"
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/model"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/skills"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/subagents"
	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

type helperStubTool struct {
	name string
}

func (s *helperStubTool) Name() string        { return s.name }
func (s *helperStubTool) Description() string { return "desc" }
func (s *helperStubTool) Schema() *tool.JSONSchema {
	return &tool.JSONSchema{Type: "object"}
}
func (s *helperStubTool) Execute(context.Context, map[string]interface{}) (*tool.ToolResult, error) {
	return &tool.ToolResult{Success: true}, nil
}

func TestAvailableToolsAndSchemaToMap(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	if err := reg.Register(&helperStubTool{name: "Bash"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	defs := availableTools(reg, map[string]struct{}{"bash": {}})
	if len(defs) != 1 || defs[0].Name != "Bash" {
		t.Fatalf("unexpected tool defs %v", defs)
	}
	if schema := schemaToMap(&tool.JSONSchema{Type: "object", Properties: map[string]any{"a": "b"}}); schema["type"] != "object" {
		t.Fatalf("unexpected schema map %v", schema)
	}
}

func TestRegisterHelpers(t *testing.T) {
	t.Parallel()

	if _, err := registerSkills([]SkillRegistration{{Definition: skills.Definition{Name: "s"}, Handler: nil}}); err == nil {
		t.Fatalf("expected skill handler error")
	}
	if _, err := registerSubagents([]SubagentRegistration{{Definition: subagents.Definition{Name: "x"}, Handler: nil}}); err == nil {
		t.Fatalf("expected subagent handler error")
	}
}

func TestConvertMessages(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{
		{Role: "user", Content: "hi", ToolCalls: []message.ToolCall{{ID: "1", Name: "t", Arguments: map[string]any{"a": "b"}}}},
	}
	modelMsgs := convertMessages(msgs)
	if len(modelMsgs) != 1 || modelMsgs[0].Role != "user" {
		t.Fatalf("unexpected converted messages %v", modelMsgs)
	}
	if modelMsgs[0].ToolCalls[0].Arguments["a"] != "b" {
		t.Fatalf("unexpected tool call arguments")
	}
	if clone := cloneArguments(modelMsgs[0].ToolCalls[0].Arguments); clone["a"] != "b" {
		t.Fatalf("unexpected cloned args")
	}
	if def := availableTools(nil, nil); def != nil {
		t.Fatalf("expected nil defs")
	}
	if schema := schemaToMap(nil); schema != nil {
		t.Fatalf("expected nil schema")
	}
	def := model.ToolDefinition{Name: "x"}
	if def.Name == "" {
		t.Fatalf("unexpected empty def")
	}
}

func TestConvertMessagesPreservesReasoningContent(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{
		{Role: "user", Content: "What is 1+1?"},
		{Role: "assistant", Content: "2", ReasoningContent: "I need to add 1 and 1"},
	}
	modelMsgs := convertMessages(msgs)
	if len(modelMsgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(modelMsgs))
	}
	if modelMsgs[1].ReasoningContent != "I need to add 1 and 1" {
		t.Fatalf("ReasoningContent not preserved: got %q", modelMsgs[1].ReasoningContent)
	}
	// User message should have empty reasoning
	if modelMsgs[0].ReasoningContent != "" {
		t.Fatalf("user message should not have ReasoningContent")
	}
}

func TestBuildSkillsRegistrySkipsDisabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	opts := Options{
		ProjectRoot: dir,
		Skills: []SkillRegistration{
			{Definition: skills.Definition{Name: "ssh-secured"}, Handler: skills.HandlerFunc(func(context.Context, skills.ActivationContext) (skills.Result, error) {
				return skills.Result{}, nil
			})},
			{Definition: skills.Definition{Name: "keep-me"}, Handler: skills.HandlerFunc(func(context.Context, skills.ActivationContext) (skills.Result, error) {
				return skills.Result{}, nil
			})},
		},
		DisabledSkills: []string{"SSH-Secured"},
	}
	skReg, errs := buildSkillsRegistry(opts)
	if len(errs) != 0 {
		t.Fatalf("unexpected loader errors: %v", errs)
	}
	if skReg == nil {
		t.Fatal("expected non-nil skills registry")
	}
	for _, def := range skReg.List() {
		if strings.EqualFold(def.Name, "ssh-secured") {
			t.Fatal("disabled skill should not be registered")
		}
	}
	if _, ok := skReg.Get("keep-me"); !ok {
		t.Fatal("non-disabled skill should remain registered")
	}
}

func TestBuildSubOptionsInheritsDisabledLists(t *testing.T) {
	t.Parallel()

	rt := &Runtime{opts: Options{
		DisabledSkills: []string{"ssh-secured"},
		DisabledMCP:    []string{"context7"},
	}}
	stale := &subagentTool{opts: Options{}}
	stale.bindRuntime(rt)

	subOpts := stale.buildSubOptions(AgentType{Name: "general"}, nil)
	if len(subOpts.DisabledSkills) != 1 || subOpts.DisabledSkills[0] != "ssh-secured" {
		t.Fatalf("expected child to inherit disabled skills, got %+v", subOpts.DisabledSkills)
	}
	if len(subOpts.DisabledMCP) != 1 || subOpts.DisabledMCP[0] != "context7" {
		t.Fatalf("expected child to inherit disabled MCP servers, got %+v", subOpts.DisabledMCP)
	}
	if !subOpts.subagentDisabled {
		t.Fatal("child options must keep subagent disabled")
	}

	// 清单返回克隆：修改子选项不得污染主 runtime 的清单。
	subOpts.DisabledSkills[0] = "tampered"
	if current := rt.DisabledSkills(); len(current) != 1 || current[0] != "ssh-secured" {
		t.Fatalf("runtime disabled skills mutated via child options: %+v", current)
	}
}
