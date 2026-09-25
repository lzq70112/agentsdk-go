package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	_ "unsafe"

	hooks "github.com/lzq70112/agentsdk-go/pkg/hooks"
	"github.com/lzq70112/agentsdk-go/pkg/mcp"
	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/model"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/skills"
	"github.com/lzq70112/agentsdk-go/pkg/tool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// recordingMockModel 是一个可复用的 model.Model 实现，用于测试。
type recordingMockModel struct {
	responses []model.Response
	calls     []model.Request
	mu        sync.Mutex
	// delay>0 时每次调用前睡眠该时长。用于耗时断言：部分平台（如 Windows）
	// 单调时钟粒度约 0.5ms，瞬时完成的子任务测得 0 时长，注入睡眠可让
	// "耗时被真实测量"这一断言确定成立。
	delay time.Duration
}

func (m *recordingMockModel) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	idx := len(m.calls) - 1
	if idx < len(m.responses) {
		resp := m.responses[idx]
		return &resp, nil
	}
	return &model.Response{
		Message:    model.Message{Role: "assistant", Content: "mock response"},
		StopReason: "end_turn",
	}, nil
}

func (m *recordingMockModel) CompleteStream(ctx context.Context, req model.Request, cb model.StreamHandler) error {
	resp, err := m.Complete(ctx, req)
	if err != nil {
		return err
	}
	if cb != nil {
		if err := cb(model.StreamResult{Final: true, Response: resp}); err != nil {
			return err
		}
	}
	return nil
}

type recordingMockModelFactory struct {
	model model.Model
}

func (f recordingMockModelFactory) Model(ctx context.Context) (model.Model, error) {
	return f.model, nil
}

func TestSubagentTool_NameAndSchema(t *testing.T) {
	st := newSubagentTool(Options{})
	if st.Name() != subagentToolName {
		t.Errorf("Name() = %q, want %q", st.Name(), subagentToolName)
	}
	if st.Description() == "" {
		t.Error("Description() should not be empty")
	}
	if st.Schema() == nil {
		t.Error("Schema() should not be nil")
	}
}

func TestSubagentTool_Execute_RequiresSessionID(t *testing.T) {
	st := newSubagentTool(Options{})
	res, err := st.Execute(context.Background(), map[string]any{
		"name":        "analyzer",
		"instruction": "analyze",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res.Success {
		t.Errorf("expected failure without session id, got success")
	}
}

func TestSubagentTool_Execute_RequiresArguments(t *testing.T) {
	mm := &recordingMockModel{}
	rt, err := New(context.Background(), Options{
		ModelFactory: recordingMockModelFactory{model: mm},
		ProjectRoot:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer rt.Close()

	st := newSubagentTool(rt.opts)
	st.bindRuntime(rt)
	ctx := WithToolSessionID(context.Background(), "test-session")

	res, err := st.Execute(ctx, map[string]any{
		"name":        "",
		"instruction": "",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res.Success {
		t.Errorf("expected failure with empty arguments, got success")
	}
}

func TestSubagentTool_ForksHistoryAndRunsChild(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainHistory := []message.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}

	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         root,
		SystemPrompt:        "you are a helpful assistant",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return mainHistory, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	// 让主 session 先运行一次，确保 history 被加载到内存。
	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	mainBefore, _ := mainRT.SessionHistory(sessionID)
	if len(mainBefore) <= len(mainHistory) {
		t.Fatalf("main history too short: %d", len(mainBefore))
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	ctxWithSession := WithToolSessionID(ctx, sessionID)
	res, err := st.Execute(ctxWithSession, map[string]any{
		"name":        "analyzer",
		"instruction": "summarize the conversation",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	// 主 agent history 不应被子 agent 污染。
	mainAfter, _ := mainRT.SessionHistory(sessionID)
	if len(mainAfter) != len(mainBefore) {
		t.Fatalf("main history mutated: before=%d after=%d", len(mainBefore), len(mainAfter))
	}

	// 验证子 agent 使用了独立的 session id。
	dataMap, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("subagent result data is not map[string]any: %T", res.Data)
	}
	subSessionID, ok := dataMap["sub_session_id"].(string)
	if !ok || subSessionID == "" {
		t.Fatalf("subagent did not return sub_session_id in result data")
	}
	if subSessionID == sessionID {
		t.Fatalf("subagent reused main session id: %s", subSessionID)
	}
	if !strings.Contains(subSessionID, "-sub-") {
		t.Fatalf("subagent session id missing -sub- suffix: %s", subSessionID)
	}

	// 找到子 agent 的 model 调用（消息长度比主 history 长）。
	var subReq *model.Request
	for i := range mm.calls {
		if len(mm.calls[i].Messages) > len(mainBefore) {
			subReq = &mm.calls[i]
			break
		}
	}
	if subReq == nil {
		t.Fatal("subagent runtime was not invoked")
	}
	// 子 agent 的消息应比主 history 多（至少包含 [Subtask] 和子 prompt）。
	if len(subReq.Messages) <= len(mainBefore) {
		t.Fatalf("subagent messages should be longer than main history: sub=%d main=%d", len(subReq.Messages), len(mainBefore))
	}
	last := subReq.Messages[len(subReq.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("expected last subagent message role=user, got %s", last.Role)
	}
}

// TestSubagentTool_ChildRuntimeSubagentToolIsDisabledNoOp 验证子 runtime 完整暴露
// subagent tool（tools 前缀与主请求一致、cache 可命中），但调用它得到明确拒绝、
// 不派生出孙 agent。这条测试守住递归硬约束：若有人为了防递归重新把 subagent tool
// 从子工具集删掉，"子请求暴露 subagent tool"断言先挂；若 no-op 退化消失，拒绝
// 文案缺失与孙 runtime 计数断言挂。mock 脚本：主→派发子，子→试图派发孙（应被
// 拒），子→被拒后自行收尾。
func TestSubagentTool_ChildRuntimeSubagentToolIsDisabledNoOp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mm := &recordingMockModel{responses: []model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID: "call-main", Name: subagentToolName,
			Arguments: map[string]any{"name": "child", "instruction": "干点活"},
		}}}, StopReason: "tool_use"},
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID: "call-child", Name: subagentToolName,
			Arguments: map[string]any{"name": "grandchild", "instruction": "再派生一层"},
		}}}, StopReason: "tool_use"},
		{Message: model.Message{Role: "assistant", Content: "child done"}, StopReason: "end_turn"},
	}}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         root,
		SystemPrompt:        "you are a helpful assistant",
		EnabledBuiltinTools: []string{"read", "subagent"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "child",
		"instruction": "干点活",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	// 子 runtime 的第一轮请求必须暴露 subagent tool：把它删掉防递归会破坏 tools
	// 前缀，那正是被废弃的做法。
	var childReq *model.Request
	for i := range mm.calls {
		if subtaskMessage(&mm.calls[i], "child") != "" {
			childReq = &mm.calls[i]
			break
		}
	}
	if childReq == nil {
		t.Fatal("subagent runtime was not invoked")
	}
	if !hasTool(toolNames(childReq.Tools), subagentToolName) {
		t.Fatalf("child runtime should expose subagent tool, got %v", toolNames(childReq.Tools))
	}

	// 子 agent 试图派生孙 agent 被拒，且拒绝结果回传给了模型。
	fedBack := false
	for i := range mm.calls {
		if strings.Contains(requestContent(&mm.calls[i]), subagentDisabledRefusal) {
			fedBack = true
			break
		}
	}
	if !fedBack {
		t.Fatal("subagent refusal was not fed back to the child model")
	}

	// 没有孙 runtime：任何请求的历史里都不应出现两个不同的 [Subtask] 指令
	// （孙请求 = fork 子历史 + 自己的指令，必然包含两层）。
	for i := range mm.calls {
		joined := requestContent(&mm.calls[i])
		if strings.Count(joined, "[Subtask] ") > 1 {
			t.Fatalf("grandchild runtime was dispatched:\n%s", joined)
		}
	}
}

func TestSubagentTool_Execute_ReturnsErrorOnChildFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mm := &recordingMockModel{}
	// 让 mockModel 第二次调用（子 agent）返回错误。
	failingModel := &failingMockModel{recordingMockModel: mm, failAfter: 1}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: failingModel},
		ProjectRoot:         root,
		SystemPrompt:        "you are a helpful assistant",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	ctxWithSession := WithToolSessionID(ctx, sessionID)
	res, err := st.Execute(ctxWithSession, map[string]any{
		"name":        "analyzer",
		"instruction": "this will fail",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res.Success {
		t.Fatalf("expected failure when child runtime fails, got success: %s", res.Output)
	}
}

// failingMockModel 包装 mockModel，在第 N 次调用后返回错误。
type failingMockModel struct {
	*recordingMockModel
	failAfter int
	count     int
	mu        sync.Mutex
}

func (m *failingMockModel) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	m.mu.Lock()
	m.count++
	count := m.count
	m.mu.Unlock()
	if count > m.failAfter {
		return nil, errors.New("model unavailable")
	}
	return m.recordingMockModel.Complete(ctx, req)
}

func (m *failingMockModel) CompleteStream(ctx context.Context, req model.Request, cb model.StreamHandler) error {
	resp, err := m.Complete(ctx, req)
	if err != nil {
		return err
	}
	if cb != nil {
		if err := cb(model.StreamResult{Final: true, Response: resp}); err != nil {
			return err
		}
	}
	return nil
}

// dummyCustomTool 是一个非内置的自定义 tool，用于验证子 Runtime 继承 CustomTools。
type dummyCustomTool struct{}

func (dummyCustomTool) Name() string        { return "customEcho" }
func (dummyCustomTool) Description() string { return "custom echo tool" }
func (dummyCustomTool) Schema() *tool.JSONSchema {
	return &tool.JSONSchema{Type: "object"}
}
func (dummyCustomTool) Execute(context.Context, map[string]any) (*tool.ToolResult, error) {
	return &tool.ToolResult{Success: true, Output: "echo"}, nil
}

func toolNames(defs []model.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func hasTool(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func TestSubagentTool_ChildRuntimeInheritsSystemPrompt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         root,
		SystemPrompt:        "you are the main assistant",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	ctxWithSession := WithToolSessionID(ctx, sessionID)
	res, err := st.Execute(ctxWithSession, map[string]any{
		"name":        "analyzer",
		"instruction": "summarize",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	if len(mm.calls) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(mm.calls))
	}
	mainSystem := mm.calls[0].System
	childSystem := mm.calls[len(mm.calls)-1].System
	if mainSystem == "" {
		t.Fatal("main system prompt empty")
	}
	if childSystem != mainSystem {
		t.Fatalf("child system prompt differs from main:\nmain=%q\nchild=%q", mainSystem, childSystem)
	}
}

// TestSubagentTool_ChildRequestPrefixByteIdentical 验证子请求与主请求的 tools 与
// system 逐字节一致——这是 KV/prefix cache 命中的硬契约：任何一处漂移（删工具、
// 套白名单、改 system）都会让子请求从第一个 token 起与主请求分叉，缓存全部落空。
// 序列化整个 tools 块与 system 逐字节比对，比"逐个工具名在不在"更接近 provider
// 实际收到的请求。
func TestSubagentTool_ChildRequestPrefixByteIdentical(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are the main assistant",
		EnabledBuiltinTools: []string{"read", "write", "subagent"},
		CustomTools:         []tool.Tool{dummyCustomTool{}},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "summarize",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	if len(mm.calls) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(mm.calls))
	}
	var childReq *model.Request
	for i := range mm.calls {
		if subtaskMessage(&mm.calls[i], "analyzer") != "" {
			childReq = &mm.calls[i]
			break
		}
	}
	if childReq == nil {
		t.Fatal("subagent runtime was not invoked")
	}

	mainTools, err := json.Marshal(mm.calls[0].Tools)
	if err != nil {
		t.Fatalf("marshal main tools: %v", err)
	}
	childTools, err := json.Marshal(childReq.Tools)
	if err != nil {
		t.Fatalf("marshal child tools: %v", err)
	}
	if !bytes.Equal(mainTools, childTools) {
		t.Fatalf("child request tools differ from main:\nmain=%s\nchild=%s", mainTools, childTools)
	}
	if childReq.System != mm.calls[0].System {
		t.Fatalf("child request system differs from main:\nmain=%q\nchild=%q", mm.calls[0].System, childReq.System)
	}

	// messages 也必须是 fork 时刻主 history 的精确前缀，只在末尾追加子任务消息。
	// tools/system 一致只保证请求头部命中缓存，消息序列若在中间分叉，缓存同样落空。
	mainAtFork, _ := mainRT.SessionHistory(sessionID)
	if len(childReq.Messages) <= len(mainAtFork) {
		t.Fatalf("child request should extend main history: child=%d main=%d", len(childReq.Messages), len(mainAtFork))
	}
	mainAsModel := convertMessages(mainAtFork)
	for i := range mainAsModel {
		if !reflect.DeepEqual(childReq.Messages[i], mainAsModel[i]) {
			t.Fatalf("child request message %d differs from main history:\nmain=%+v\nchild=%+v", i, mainAsModel[i], childReq.Messages[i])
		}
	}
	tail := childReq.Messages[len(mainAsModel):]
	hasSubtask := false
	for _, m := range tail {
		if strings.Contains(m.Content, "[Subtask] ") {
			hasSubtask = true
			break
		}
	}
	if !hasSubtask {
		t.Fatalf("child request tail should carry the [Subtask] message, got: %+v", tail)
	}
}

// TestSubagentTool_DisabledInstanceRefusesAndSpawnsNothing 直接对子 runtime 中
// 注册的 subagent tool 实例调用：返回拒绝结果（Success=false、nil error，属正常
// 工具结果而非执行错误），且不创建任何孙 runtime（model 零新增调用）。拒绝发生在
// Execute 最早期，不触达 fork/派发逻辑，也无 goroutine 与会话泄漏。
func TestSubagentTool_DisabledInstanceRefusesAndSpawnsNothing(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	st := newSubagentTool(Options{
		ModelFactory: recordingMockModelFactory{model: mm},
		ProjectRoot:  t.TempDir(),
	})
	subOpts := st.buildSubOptions([]message.Message{{Role: "user", Content: "x"}})

	childRT, err := New(ctx, subOpts)
	if err != nil {
		t.Fatalf("create child runtime: %v", err)
	}
	defer childRT.Close()

	disabledTool := childRT.subagent
	if disabledTool == nil {
		t.Fatal("child runtime did not register a subagent tool")
	}
	if !disabledTool.disabled {
		t.Fatal("child runtime's subagent tool should be disabled")
	}

	callsBefore := len(mm.calls)
	res, err := disabledTool.Execute(WithToolSessionID(ctx, "child-session"), map[string]any{
		"name":        "grandchild",
		"instruction": "再派生一层",
	})
	if err != nil {
		t.Fatalf("disabled subagent tool should return a normal result, got error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("disabled subagent tool should refuse, got: %+v", res)
	}
	if !strings.Contains(res.Output, subagentDisabledRefusal) {
		t.Fatalf("refusal message unclear: %q", res.Output)
	}
	if len(mm.calls) != callsBefore {
		t.Fatalf("disabled subagent tool spawned a grandchild runtime: calls before=%d after=%d", callsBefore, len(mm.calls))
	}
}

func TestSubagentTool_ChildRuntimeInheritsCustomTools(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         root,
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read", "subagent"},
		CustomTools:         []tool.Tool{dummyCustomTool{}},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	ctxWithSession := WithToolSessionID(ctx, sessionID)
	if _, err := st.Execute(ctxWithSession, map[string]any{
		"name":        "analyzer",
		"instruction": "check available tools",
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if len(mm.calls) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(mm.calls))
	}
	mainTools := toolNames(mm.calls[0].Tools)
	childTools := toolNames(mm.calls[len(mm.calls)-1].Tools)

	if !hasTool(mainTools, "customEcho") {
		t.Fatalf("main runtime missing custom tool, got %v", mainTools)
	}
	if !hasTool(mainTools, "read") {
		t.Fatalf("main runtime missing read tool, got %v", mainTools)
	}
	if !hasTool(mainTools, "subagent") {
		t.Fatalf("main runtime missing subagent tool, got %v", mainTools)
	}

	if !hasTool(childTools, "customEcho") {
		t.Fatalf("child runtime missing custom tool, got %v", childTools)
	}
	if !hasTool(childTools, "read") {
		t.Fatalf("child runtime missing read tool, got %v", childTools)
	}
	// 新契约：子 runtime 同样暴露 subagent tool（Name/Description/Schema 与主
	// 一致，保 prefix cache），递归由子 runtime 内的 no-op 阻断而非删工具。
	if !hasTool(childTools, "subagent") {
		t.Fatalf("child runtime should expose subagent tool, got %v", childTools)
	}
}

func TestSubagentTool_ChildRuntimeInheritsSkills(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         root,
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		Skills: []SkillRegistration{{
			Definition: skills.Definition{
				Name: "skill-trigger",
				Matchers: []skills.Matcher{
					skills.KeywordMatcher{All: []string{"skill-trigger"}},
				},
			},
			Handler: skills.HandlerFunc(func(context.Context, skills.ActivationContext) (skills.Result, error) {
				return skills.Result{Output: "[SKILL-PREFIX]"}, nil
			}),
		}},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "skill-trigger main"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	ctxWithSession := WithToolSessionID(ctx, sessionID)
	if _, err := st.Execute(ctxWithSession, map[string]any{
		"name":        "analyzer",
		"instruction": "skill-trigger subtask",
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if len(mm.calls) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(mm.calls))
	}
	mainReq := mm.calls[0]
	childReq := mm.calls[len(mm.calls)-1]

	mainLast := mainReq.Messages[len(mainReq.Messages)-1].Content
	childLast := childReq.Messages[len(childReq.Messages)-1].Content
	if !strings.Contains(mainLast, "[SKILL-PREFIX]") {
		t.Fatalf("main request did not apply skill prefix, last message: %q", mainLast)
	}
	if !strings.Contains(childLast, "[SKILL-PREFIX]") {
		t.Fatalf("child request did not apply skill prefix, last message: %q", childLast)
	}
}

// TestSubagentTool_SubtaskMessageCarriesOutputContract 验证派发给子 agent 的 [Subtask] 消息
// 带有输出契约。这条测试的存在意义：子 agent 若不被告知"返回完整结果而非一句话摘要"，
// 主 agent 拿到过精的输出后只能返工甚至亲自重做，subagent tool 就形同虚设。
func TestSubagentTool_SubtaskMessageCarriesOutputContract(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	instruction := "分析日志格式问题并给出结论"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": instruction,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	// 收集所有 model 调用中的 [Subtask] 消息（取最后一次出现的子 agent 请求）。
	subtask := ""
	for _, call := range mm.calls {
		for _, msg := range call.Messages {
			if strings.Contains(msg.Content, "[Subtask]") {
				subtask = msg.Content
			}
		}
	}
	if subtask == "" {
		t.Fatal("subagent request does not contain [Subtask] message")
	}
	if !strings.Contains(subtask, instruction) {
		t.Fatalf("[Subtask] message lost instruction: %q", subtask)
	}
	for _, want := range []string{"结论先行", "关键证据", "不要只给一句话摘要"} {
		if !strings.Contains(subtask, want) {
			t.Fatalf("[Subtask] message missing output contract %q, got: %q", want, subtask)
		}
	}
	// 身份提醒必须出现在子任务消息末尾：子 agent 不应再派生子 agent。它是软
	// 约束，硬保证是子 runtime 中 subagent tool 的 no-op（见
	// TestSubagentTool_ChildRuntimeSubagentToolIsDisabledNoOp）。
	if !strings.Contains(subtask, subagentIdentityReminder) {
		t.Fatalf("[Subtask] message missing identity reminder, got: %q", subtask)
	}
	if !strings.HasSuffix(subtask, subagentIdentityReminder) {
		t.Fatalf("[Subtask] message should end with identity reminder, got: %q", subtask)
	}
}

// TestBuildForkedHistory_ClonesAndAppendsContract 验证 fork 语义与 runtime 创建解耦：
// 主 history 被深拷贝不被污染，末尾子任务消息依次携带 name、instruction、输出契约、
// 类型追加提示词与身份提醒。这条测试守住的是"子 agent 永远带着完整上下文和契约
// 起步"这一不变量——未来引入异步/恢复时 fork 逻辑若被破坏，这里会先挂。
func TestBuildForkedHistory_ClonesAndAppendsContract(t *testing.T) {
	mainHistory := []message.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}

	forked := buildForkedHistory(mainHistory, "analyzer", "分析日志", "")
	if len(forked) != len(mainHistory)+1 {
		t.Fatalf("forked length = %d, want %d", len(forked), len(mainHistory)+1)
	}
	last := forked[len(forked)-1]
	if last.Role != "user" {
		t.Fatalf("last forked message role = %q, want user", last.Role)
	}
	for _, want := range []string{"[Subtask] analyzer", "分析日志", "结论先行", "不要只给一句话摘要"} {
		if !strings.Contains(last.Content, want) {
			t.Fatalf("forked subtask message missing %q, got: %q", want, last.Content)
		}
	}
	// 身份提醒收尾（追加提示词之后）：提示模型不要派发孙 agent。
	if !strings.HasSuffix(last.Content, subagentIdentityReminder) {
		t.Fatalf("forked subtask message should end with identity reminder, got: %q", last.Content)
	}
	// 主 history 不被污染。
	if len(mainHistory) != 2 || mainHistory[0].Content != "hello" || mainHistory[1].Content != "hi" {
		t.Fatalf("main history mutated: %+v", mainHistory)
	}
}

// TestBuildForkedHistory_AppendPromptBeforeIdentityReminder 验证类型追加提示词与
// 身份提醒的先后顺序：追加提示词在前、身份提醒始终收尾。顺序漂移会让"最后一条
// 指令"的含义变化，模型对末尾内容的权重最高，不能被追加提示词抢位。
func TestBuildForkedHistory_AppendPromptBeforeIdentityReminder(t *testing.T) {
	forked := buildForkedHistory(
		[]message.Message{{Role: "user", Content: "hello"}},
		"reviewer", "审查变更", "先列风险点",
	)
	last := forked[len(forked)-1]
	promptIdx := strings.Index(last.Content, "先列风险点")
	reminderIdx := strings.Index(last.Content, subagentIdentityReminder)
	if promptIdx < 0 {
		t.Fatalf("append prompt missing: %q", last.Content)
	}
	if reminderIdx < 0 {
		t.Fatalf("identity reminder missing: %q", last.Content)
	}
	if promptIdx > reminderIdx {
		t.Fatalf("identity reminder must come after append prompt: %q", last.Content)
	}
	if !strings.HasSuffix(last.Content, subagentIdentityReminder) {
		t.Fatalf("identity reminder must end the subtask message: %q", last.Content)
	}
}

// fakeSubagentTool 冒充工具列表中一个名为 subagent 的自定义实现，用于验证构造子
// Options 时它被原样保留（新的契约不再有任何排除/收窄逻辑）。
type fakeSubagentTool struct{ dummyCustomTool }

func (fakeSubagentTool) Name() string { return subagentToolName }

// TestSubagentTool_BuildSubOptions_KeepsFullToolSetAndInjectsLoader 验证子 Options
// 的构造规则：工具面全量保留（不排除 subagent、不按类型白名单收窄，保 prefix
// cache），loader 永远返回 fork 副本（子 agent 不读文件存储），且子 Options 把
// subagentDisabled 置位，使子 runtime 内的 subagent tool 成为拒绝执行的 no-op。
func TestSubagentTool_BuildSubOptions_KeepsFullToolSetAndInjectsLoader(t *testing.T) {
	forked := []message.Message{{Role: "user", Content: "x"}}
	st := newSubagentTool(Options{
		EnabledBuiltinTools: []string{"read", "subagent"},
		CustomTools:         []tool.Tool{dummyCustomTool{}, fakeSubagentTool{}},
	})

	subOpts := st.buildSubOptions(forked)

	if len(subOpts.EnabledBuiltinTools) != 2 ||
		!hasTool(subOpts.EnabledBuiltinTools, "read") ||
		!hasTool(subOpts.EnabledBuiltinTools, subagentToolName) {
		t.Fatalf("sub options should keep the full builtin set, got %v", subOpts.EnabledBuiltinTools)
	}
	if len(subOpts.CustomTools) != 2 {
		t.Fatalf("sub options should keep all custom tools, got %d", len(subOpts.CustomTools))
	}
	if !subOpts.subagentDisabled {
		t.Fatal("sub options should mark the child subagent tool disabled")
	}
	if subOpts.HistoryLoader == nil {
		t.Fatal("sub options missing history loader")
	}
	loaded, err := subOpts.HistoryLoader("any-session-id")
	if err != nil {
		t.Fatalf("history loader error: %v", err)
	}
	if len(loaded) != len(forked) {
		t.Fatalf("history loader returned %d messages, want %d", len(loaded), len(forked))
	}
}

// TestSubagentTool_Description_ListsRegisteredTypes 验证主 agent 能从 tool 描述看到
// 可用类型清单——否则类型系统对 LLM 不可见，派发时无法按任务性质选择。
func TestSubagentTool_Description_ListsRegisteredTypes(t *testing.T) {
	st := newSubagentTool(Options{AgentTypes: []AgentType{
		{Name: "explorer", Description: "只读搜索型，适合定位代码"},
		{Name: "reviewer", Description: "代码审查型，适合变更审查"},
	}})
	desc := st.Description()
	for _, want := range []string{"explorer", "只读搜索型", "reviewer", "代码审查型", "type"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("Description missing %q, got: %s", want, desc)
		}
	}
	// 未注册类型时文案不出现类型清单，主 agent 已习惯的输入不漂移。
	plain := newSubagentTool(Options{}).Description()
	if strings.Contains(plain, "可用类型") {
		t.Fatalf("description without registered types should not list types, got: %s", plain)
	}
}

// TestSubagentTool_TypeAllowedToolsNoLongerNarrowsTools 验证类型的 AllowedTools
// 白名单不再收窄子 agent 工具面：子请求的工具集与主请求完全一致（read/write/grep/
// customEcho/subagent 全在）。这条测试守住逐字节 cache 契约——白名单收窄曾经把
// write 等工具从子请求删掉，工具块从第一个 token 起就与主请求分叉，KV 缓存全部
// 落空；AllowedTools 字段保留但不再影响子请求工具集。
func TestSubagentTool_TypeAllowedToolsNoLongerNarrowsTools(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read", "write", "grep", "subagent"},
		CustomTools:         []tool.Tool{dummyCustomTool{}},
		AgentTypes: []AgentType{{
			Name:         "explorer",
			Description:  "只读搜索",
			AllowedTools: []string{"read", "grep"},
		}},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()
	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "explorer",
		"instruction": "定位登录逻辑",
		"type":        "explorer",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	var subReq model.Request
	for i := range mm.calls {
		if len(mm.calls[i].Messages) > len(subReq.Messages) {
			subReq = mm.calls[i]
		}
	}
	if len(subReq.Messages) == 0 {
		t.Fatal("subagent runtime was not invoked")
	}
	childTools := toolNames(subReq.Tools)
	for _, want := range []string{"read", "write", "grep", "customEcho", subagentToolName} {
		if !hasTool(childTools, want) {
			t.Fatalf("类型白名单不应收窄工具面：child runtime missing %q, got %v", want, childTools)
		}
	}
	if strings.Join(childTools, ",") != strings.Join(toolNames(mm.calls[0].Tools), ",") {
		t.Fatalf("child tool set drifted from main: main=%v child=%v", toolNames(mm.calls[0].Tools), childTools)
	}
}

// TestSubagentTool_TypeAppendPromptInjected 验证类型追加提示词注入到子任务消息，
// 且子 runtime 的 system prompt 本体与主 runtime 一致——后者是 prefix cache 命中的
// 硬契约，注入只能发生在 history 末尾消息，不能改 system prompt。
func TestSubagentTool_TypeAppendPromptInjected(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are the main assistant",
		EnabledBuiltinTools: []string{"read"},
		AgentTypes: []AgentType{{
			Name:         "reviewer",
			Description:  "代码审查",
			AppendPrompt: "先列出风险点，再给出结论",
		}},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()
	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "reviewer",
		"instruction": "审查本次变更",
		"type":        "reviewer",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	if len(mm.calls) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(mm.calls))
	}
	childReq := mm.calls[len(mm.calls)-1]
	if childReq.System != mm.calls[0].System {
		t.Fatalf("child system prompt must equal main:\nmain=%q\nchild=%q", mm.calls[0].System, childReq.System)
	}
	subtask := ""
	for _, msg := range childReq.Messages {
		if strings.Contains(msg.Content, "[Subtask]") {
			subtask = msg.Content
		}
	}
	if subtask == "" {
		t.Fatal("child request missing [Subtask] message")
	}
	if !strings.Contains(subtask, "先列出风险点，再给出结论") {
		t.Fatalf("[Subtask] message missing type append prompt, got: %q", subtask)
	}
}

// TestSubagentTool_UnknownTypeFallsBackToDefault 验证类型名不存在时回退默认通用型
// （全量继承主工具集），派发不因选错类型而失败。
func TestSubagentTool_UnknownTypeFallsBackToDefault(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read", "write", "subagent"},
		CustomTools:         []tool.Tool{dummyCustomTool{}},
		AgentTypes: []AgentType{{
			Name:         "explorer",
			Description:  "只读搜索",
			AllowedTools: []string{"read"},
			AppendPrompt: "只读",
		}},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()
	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "worker",
		"instruction": "干点活",
		"type":        "nonexistent-type",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	var subReq model.Request
	for i := range mm.calls {
		if len(mm.calls[i].Messages) > len(subReq.Messages) {
			subReq = mm.calls[i]
		}
	}
	childTools := toolNames(subReq.Tools)
	for _, want := range []string{"read", "write", "customEcho"} {
		if !hasTool(childTools, want) {
			t.Fatalf("default fallback should keep %q, got %v", want, childTools)
		}
	}
	if !hasTool(childTools, "subagent") {
		t.Fatalf("child runtime should expose subagent, got %v", childTools)
	}
	subtask := ""
	for _, msg := range subReq.Messages {
		if strings.Contains(msg.Content, "[Subtask]") {
			subtask = msg.Content
		}
	}
	if strings.Contains(subtask, "只读") {
		t.Fatalf("unknown type should not inject explorer append prompt, got: %q", subtask)
	}
}

// TestSubagentTool_ChildKeepsMCPAndFullBuiltinTools 验证带 AllowedTools 的类型
// 派发时，MCP 工具与全部 builtin 工具都进入子请求：类型白名单已不再收窄任何工具面
// （MCP 工具过去就不被收窄，现在 builtin 同样不被收窄），子请求工具集与主请求
// 完全一致——任何收窄都会让 tools 前缀与主请求分叉、KV 缓存落空。
func TestSubagentTool_ChildKeepsMCPAndFullBuiltinTools(t *testing.T) {
	origBase := patchedNewMCPClient
	origOpts := patchedNewMCPClientWithOptions
	patchedNewMCPClient = func(ctx context.Context, spec string, handler func(context.Context, *mcp.ClientSession)) (*mcp.ClientSession, error) {
		return newInMemoryMCPSession(t), nil
	}
	patchedNewMCPClientWithOptions = func(context.Context, string, tool.MCPServerOptions, func(context.Context, *mcp.ClientSession)) (*mcp.ClientSession, error) {
		return newInMemoryMCPSession(t), nil
	}
	t.Cleanup(func() {
		patchedNewMCPClient = origBase
		patchedNewMCPClientWithOptions = origOpts
	})

	ctx := context.Background()
	root := t.TempDir()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         root,
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read", "write", "subagent"},
		MCPServers:          []string{"stdio://fake"},
		AgentTypes: []AgentType{{
			Name:         "explorer",
			Description:  "只读搜索",
			AllowedTools: []string{"read"},
		}},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()
	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "explorer",
		"instruction": "use echo",
		"type":        "explorer",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	var subReq model.Request
	for i := range mm.calls {
		if len(mm.calls[i].Messages) > len(subReq.Messages) {
			subReq = mm.calls[i]
		}
	}
	if len(subReq.Messages) == 0 {
		t.Fatal("subagent runtime was not invoked")
	}
	childTools := toolNames(subReq.Tools)
	for _, want := range []string{"echo", "write", "read", subagentToolName} {
		if !hasTool(childTools, want) {
			t.Fatalf("child runtime missing %q: 类型白名单不应收窄工具面, got %v", want, childTools)
		}
	}
	if strings.Join(childTools, ",") != strings.Join(toolNames(mm.calls[0].Tools), ",") {
		t.Fatalf("child tool set drifted from main: main=%v child=%v", toolNames(mm.calls[0].Tools), childTools)
	}
}

// writeAgentTypeFile 在项目 .agents/subagents/ 下写一个类型定义文件，返回项目根目录。
func writeAgentTypeFile(t *testing.T, filename, content string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".agents", "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	return root
}

// TestSubagentTool_LoadsAgentTypeFiles 验证项目目录的类型定义文件被加载——
// 维护者不写代码即可新增团队类型，这是文件来源存在的意义。
func TestSubagentTool_LoadsAgentTypeFiles(t *testing.T) {
	root := writeAgentTypeFile(t, "explorer.md", `---
name: explorer
description: 只读搜索型，适合定位代码
allowed-tools:
  - read
  - grep
---

先定位再总结，不要修改任何文件。
`)

	st := newSubagentTool(Options{ProjectRoot: root})
	at := st.lookupAgentType("explorer")
	if at.Name != "explorer" {
		t.Fatalf("explorer type not loaded, got %+v", at)
	}
	if at.Description != "只读搜索型，适合定位代码" {
		t.Fatalf("description = %q", at.Description)
	}
	if len(at.AllowedTools) != 2 || at.AllowedTools[0] != "read" || at.AllowedTools[1] != "grep" {
		t.Fatalf("allowed tools = %v", at.AllowedTools)
	}
	if !strings.Contains(at.AppendPrompt, "先定位再总结") {
		t.Fatalf("append prompt = %q", at.AppendPrompt)
	}
	if !strings.Contains(st.Description(), "explorer") {
		t.Fatalf("Description should list file-loaded type, got: %s", st.Description())
	}
	// 类型名匹配大小写不敏感，与工具名比较风格一致。
	if got := st.lookupAgentType("EXPLORER"); got.Name != "explorer" {
		t.Fatalf("case-insensitive lookup failed, got %+v", got)
	}
}

// TestSubagentTool_ProgrammaticOverridesFileType 验证编程注册覆盖同名文件定义——
// 嵌入场景的显式配置优先于项目目录残留文件。
func TestSubagentTool_ProgrammaticOverridesFileType(t *testing.T) {
	root := writeAgentTypeFile(t, "explorer.md", `---
name: explorer
description: 文件版描述
---

文件版提示词。
`)

	st := newSubagentTool(Options{
		ProjectRoot: root,
		AgentTypes: []AgentType{{
			Name:         "explorer",
			Description:  "编程版描述",
			AppendPrompt: "编程版提示词。",
		}},
	})
	at := st.lookupAgentType("explorer")
	if at.Description != "编程版描述" {
		t.Fatalf("programmatic registration should override file definition, got %q", at.Description)
	}
	if !strings.Contains(at.AppendPrompt, "编程版提示词") {
		t.Fatalf("append prompt should come from programmatic registration, got %q", at.AppendPrompt)
	}
}

// TestSubagentTool_SkipsMalformedAgentTypeFile 验证坏定义文件被跳过且不影响其他
// 类型加载——单个文件写错不应让整个类型系统不可用。
func TestSubagentTool_SkipsMalformedAgentTypeFile(t *testing.T) {
	root := writeAgentTypeFile(t, "broken.md", `---
description: 没有 name 的坏文件
---
`)
	if err := os.WriteFile(filepath.Join(root, ".agents", "subagents", "good.md"), []byte(`---
name: reviewer
description: 审查型
---

审查要严格。
`), 0o644); err != nil {
		t.Fatalf("write good.md: %v", err)
	}

	st := newSubagentTool(Options{ProjectRoot: root})
	if got := st.lookupAgentType("reviewer"); got.Name != "reviewer" {
		t.Fatalf("good file should still load, got %+v", got)
	}
	if len(st.opts.AgentTypes) != 1 {
		t.Fatalf("only good file should produce a type, got %d: %+v", len(st.opts.AgentTypes), st.opts.AgentTypes)
	}
}

// TestSubagentTool_NoAgentTypesDir 验证没有类型目录时不产生任何类型，默认行为不变。
func TestSubagentTool_NoAgentTypesDir(t *testing.T) {
	st := newSubagentTool(Options{ProjectRoot: t.TempDir()})
	if len(st.opts.AgentTypes) != 0 {
		t.Fatalf("no dir should yield no types, got %+v", st.opts.AgentTypes)
	}
}

func newInMemoryMCPSession(t *testing.T) *mcp.ClientSession {
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "subagent-mcp-server", Version: "test"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "echo text",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	serverCtx, serverCancel := context.WithCancel(context.Background())
	serverSession, err := server.Connect(serverCtx, serverTransport, nil)
	if err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	t.Cleanup(func() {
		_ = serverSession.Close()
		serverCancel()
	})

	client := mcp.NewClient(&mcp.Implementation{Name: "subagent-mcp-client", Version: "test"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func TestSubagentTool_ChildRuntimeInheritsMCPServers(t *testing.T) {
	origBase := patchedNewMCPClient
	origOpts := patchedNewMCPClientWithOptions
	fake := func(context.Context, string, tool.MCPServerOptions, func(context.Context, *mcp.ClientSession)) (*mcp.ClientSession, error) {
		return newInMemoryMCPSession(t), nil
	}
	patchedNewMCPClient = func(ctx context.Context, spec string, handler func(context.Context, *mcp.ClientSession)) (*mcp.ClientSession, error) {
		return newInMemoryMCPSession(t), nil
	}
	patchedNewMCPClientWithOptions = fake
	t.Cleanup(func() {
		patchedNewMCPClient = origBase
		patchedNewMCPClientWithOptions = origOpts
	})

	ctx := context.Background()
	root := t.TempDir()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         root,
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read", "subagent"},
		MCPServers:          []string{"stdio://fake"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	ctxWithSession := WithToolSessionID(ctx, sessionID)
	if _, err := st.Execute(ctxWithSession, map[string]any{
		"name":        "analyzer",
		"instruction": "use echo",
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if len(mm.calls) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(mm.calls))
	}
	mainTools := toolNames(mm.calls[0].Tools)
	childTools := toolNames(mm.calls[len(mm.calls)-1].Tools)

	if !hasTool(mainTools, "echo") {
		t.Fatalf("main runtime missing mcp echo tool, got %v", mainTools)
	}
	if !hasTool(mainTools, "subagent") {
		t.Fatalf("main runtime missing subagent tool, got %v", mainTools)
	}
	if !hasTool(childTools, "echo") {
		t.Fatalf("child runtime missing mcp echo tool, got %v", childTools)
	}
	// 新契约：子 runtime 同样暴露 subagent tool，与主 runtime 工具面一致。
	if !hasTool(childTools, "subagent") {
		t.Fatalf("child runtime should expose subagent tool, got %v", childTools)
	}
}

func TestSubagentTool_LogsToFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	logDir := filepath.Join(root, ".agents", "logs")
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         root,
		LogDir:              logDir,
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	ctxWithSession := WithToolSessionID(ctx, sessionID)
	if _, err := st.Execute(ctxWithSession, map[string]any{
		"name":        "analyzer",
		"instruction": "summarize",
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	logPath := filepath.Join(logDir, fmt.Sprintf("agentsdk-%s.log", time.Now().Format("20060102")))
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read subagent log file %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), "[subagent] 子 agent 执行成功") {
		t.Fatalf("subagent success log missing from %s:\n%s", logPath, string(data))
	}
}

// eventRecorder 并发安全地记录 SubagentStart/SubagentComplete 事件，供生命周期
// 事件测试观测 hooks 总线（参照 subagent_async_test.go 的模式）。异步路径的
// middleware 在子 goroutine 中执行，快照读取必须加锁。
type eventRecorder struct {
	mu     sync.Mutex
	events []hooks.Event
}

func (r *eventRecorder) middleware() hooks.Middleware {
	return func(next hooks.MiddlewareHandler) hooks.MiddlewareHandler {
		return func(ctx context.Context, evt hooks.Event) error {
			switch evt.Type {
			case hooks.SubagentStart, hooks.SubagentComplete:
				r.mu.Lock()
				r.events = append(r.events, evt)
				r.mu.Unlock()
			}
			return next(ctx, evt)
		}
	}
}

func (r *eventRecorder) snapshot() []hooks.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hooks.Event(nil), r.events...)
}

// waitFor 等待至少记录到 n 条事件后返回快照；超时返回当前快照供断言失败信息使用。
func (r *eventRecorder) waitFor(n int, timeout time.Duration) []hooks.Event {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := r.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r.snapshot()
}

// TestSubagentTool_PublishesLifecycleEvents 验证同步派发的完整生命周期都进入
// hooks 总线：开始与成功各一发事件，载荷带齐主/子会话标识、类型名、耗时、输出长度。
// 这条测试守住的是平台侧的审计与监控入口——事件丢失或字段缺失时 tool 本身照常
// 工作，只有断言事件才能发现观测链断裂。mock 注入 10ms 睡眠，使耗时不依赖
// 时钟粒度也可稳定测得（见 recordingMockModel.delay 注释）。
func TestSubagentTool_PublishesLifecycleEvents(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{delay: 10 * time.Millisecond}

	recorder := &eventRecorder{}
	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		AgentTypes:          []AgentType{{Name: "explorer", Description: "只读搜索型"}},
		HookMiddleware:      []hooks.Middleware{recorder.middleware()},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "summarize",
		"type":        "explorer",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected subagent success, got: %s", res.Output)
	}

	dataMap, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("subagent result data is not map[string]any: %T", res.Data)
	}
	subSessionID, _ := dataMap["sub_session_id"].(string)
	if subSessionID == "" {
		t.Fatal("missing sub_session_id in result data")
	}

	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("lifecycle events = %d, want 2 (start+complete): %+v", len(events), events)
	}
	if events[0].Type != hooks.SubagentStart {
		t.Fatalf("first event type = %s, want SubagentStart", events[0].Type)
	}
	if events[0].SessionID != sessionID {
		t.Fatalf("start event session = %q, want main session %q", events[0].SessionID, sessionID)
	}
	startPayload, ok := events[0].Payload.(hooks.SubagentStartPayload)
	if !ok {
		t.Fatalf("start payload type = %T", events[0].Payload)
	}
	if startPayload.AgentID != subSessionID {
		t.Fatalf("start payload agent id = %q, want %q", startPayload.AgentID, subSessionID)
	}
	if startPayload.AgentType != "explorer" {
		t.Fatalf("start payload agent type = %q, want explorer", startPayload.AgentType)
	}

	if events[1].Type != hooks.SubagentComplete {
		t.Fatalf("second event type = %s, want SubagentComplete", events[1].Type)
	}
	if events[1].SessionID != sessionID {
		t.Fatalf("complete event session = %q, want main session %q", events[1].SessionID, sessionID)
	}
	completePayload, ok := events[1].Payload.(hooks.SubagentCompletePayload)
	if !ok {
		t.Fatalf("complete payload type = %T", events[1].Payload)
	}
	if completePayload.TaskID != subSessionID {
		t.Fatalf("complete payload task id = %q, want %q", completePayload.TaskID, subSessionID)
	}
	if completePayload.Status != "success" {
		t.Fatalf("complete payload status = %q, want success", completePayload.Status)
	}
	if completePayload.AgentType != "explorer" {
		t.Fatalf("complete payload agent type = %q, want explorer", completePayload.AgentType)
	}
	if completePayload.Output != res.Output {
		t.Fatalf("complete payload output = %q, want %q", completePayload.Output, res.Output)
	}
	if completePayload.OutputLength != len(res.Output) {
		t.Fatalf("complete payload output length = %d, want %d", completePayload.OutputLength, len(res.Output))
	}
	if completePayload.Duration < 5*time.Millisecond {
		t.Fatalf("complete payload duration = %s, want >= 5ms（mock 睡眠 10ms，耗时应为实测值而非占位）", completePayload.Duration)
	}
}

// TestSubagentTool_PublishesFailureEvent 验证子 agent 执行失败时同样发出完成事件
// （Status=error 且带错误信息）——失败不能被静默吞掉，否则平台侧只能看到工具调用
// 失败，无从判断子 agent 内部发生了什么。
func TestSubagentTool_PublishesFailureEvent(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}
	failingModel := &failingMockModel{recordingMockModel: mm, failAfter: 1}

	recorder := &eventRecorder{}
	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: failingModel},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HookMiddleware:      []hooks.Middleware{recorder.middleware()},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "this will fail",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res.Success {
		t.Fatalf("expected failure when child runtime fails, got success: %s", res.Output)
	}

	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("lifecycle events = %d, want 2 (start+complete): %+v", len(events), events)
	}
	payload, ok := events[1].Payload.(hooks.SubagentCompletePayload)
	if !ok {
		t.Fatalf("complete payload type = %T", events[1].Payload)
	}
	if payload.Status != "error" {
		t.Fatalf("failure event status = %q, want error", payload.Status)
	}
	if payload.Error == "" {
		t.Fatal("failure event missing error message")
	}
}

// TestSubagentTool_EventPublishFailureDoesNotAffectDispatch 验证事件发布失败时
// 派发结果与回传不受影响——观测链路故障不应让子 agent 任务本身失败。
func TestSubagentTool_EventPublishFailureDoesNotAffectDispatch(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	breakPublish := func(next hooks.MiddlewareHandler) hooks.MiddlewareHandler {
		return func(ctx context.Context, evt hooks.Event) error {
			if evt.Type == hooks.SubagentStart || evt.Type == hooks.SubagentComplete {
				return errors.New("event bus unavailable")
			}
			return next(ctx, evt)
		}
	}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HookMiddleware:      []hooks.Middleware{breakPublish},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "summarize",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("dispatch should succeed despite event publish failure, got: %s", res.Output)
	}
	if res.Output == "" {
		t.Fatal("dispatch output missing")
	}
}

// gatedMockModel 从第 gateAt 次调用起阻塞，直到 release 关闭或 ctx 取消，
// 用于确定性地验证异步派发时序：Execute 先返回、子 agent 后执行。
type gatedMockModel struct {
	mu      sync.Mutex
	calls   int
	gateAt  int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *gatedMockModel) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	if n >= m.gateAt {
		m.once.Do(func() { close(m.entered) })
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &model.Response{
		Message:    model.Message{Role: "assistant", Content: "async output"},
		StopReason: "end_turn",
	}, nil
}

func (m *gatedMockModel) CompleteStream(ctx context.Context, req model.Request, cb model.StreamHandler) error {
	resp, err := m.Complete(ctx, req)
	if err != nil {
		return err
	}
	if cb != nil {
		if err := cb(model.StreamResult{Final: true, Response: resp}); err != nil {
			return err
		}
	}
	return nil
}

// TestSubagentTool_AsyncDispatchReturnsImmediately 验证后台派发不阻塞当前对话：
// Execute 在子 agent 尚未产出结果时即返回（含任务标识），子 agent 放行后经事件
// 总线回报成功，载荷契约与同步路径一致。这条测试守住的是 IM 场景首响延迟不被
// 长耗时子任务拖住这一核心收益。
func TestSubagentTool_AsyncDispatchReturnsImmediately(t *testing.T) {
	ctx := context.Background()
	gm := &gatedMockModel{gateAt: 2, entered: make(chan struct{}), release: make(chan struct{})}
	recorder := &eventRecorder{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: gm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HookMiddleware:      []hooks.Middleware{recorder.middleware()},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)

	type execOutcome struct {
		res *tool.ToolResult
		err error
	}
	execDone := make(chan execOutcome, 1)
	go func() {
		res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
			"name":        "analyzer",
			"instruction": "long running task",
			"background":  true,
		})
		execDone <- execOutcome{res: res, err: err}
	}()

	var res *tool.ToolResult
	select {
	case outcome := <-execDone:
		if outcome.err != nil {
			t.Fatalf("Execute returned error: %v", outcome.err)
		}
		res = outcome.res
	case <-time.After(5 * time.Second):
		t.Fatal("async Execute blocked waiting for child result")
	}
	if !res.Success {
		t.Fatalf("expected async dispatch success, got: %s", res.Output)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("result data is not map[string]any: %T", res.Data)
	}
	if data["async"] != true {
		t.Fatalf("async flag missing in result data: %+v", data)
	}
	subSessionID, _ := data["sub_session_id"].(string)
	if subSessionID == "" {
		t.Fatal("async dispatch missing sub_session_id")
	}
	if !strings.Contains(res.Output, subSessionID) {
		t.Fatalf("async output missing task id %q: %s", subSessionID, res.Output)
	}

	// 子 agent 此刻应在后台阻塞于 model 调用；放行后应收到成功完成事件。
	select {
	case <-gm.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("async child never started")
	}
	close(gm.release)

	events := recorder.waitFor(2, 2*time.Second)
	if len(events) != 2 {
		t.Fatalf("lifecycle events = %d, want 2: %+v", len(events), events)
	}
	payload, ok := events[1].Payload.(hooks.SubagentCompletePayload)
	if !ok {
		t.Fatalf("complete payload type = %T", events[1].Payload)
	}
	if payload.Status != "success" {
		t.Fatalf("async complete status = %q, want success", payload.Status)
	}
	if payload.TaskID != subSessionID {
		t.Fatalf("async complete task id = %q, want %q", payload.TaskID, subSessionID)
	}
	if payload.Output != "async output" || payload.OutputLength != len("async output") {
		t.Fatalf("async complete payload = %+v", payload)
	}
}

// TestSubagentTool_AsyncFailureEvent 验证异步子 agent 执行失败时同样发出失败
// 事件且带错误信息——后台路径的失败不能被静默吞掉。
func TestSubagentTool_AsyncFailureEvent(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}
	failingModel := &failingMockModel{recordingMockModel: mm, failAfter: 1}
	recorder := &eventRecorder{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: failingModel},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HookMiddleware:      []hooks.Middleware{recorder.middleware()},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "this will fail",
		"background":  true,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("async dispatch should report accepted, got: %s", res.Output)
	}

	events := recorder.waitFor(2, 2*time.Second)
	if len(events) != 2 {
		t.Fatalf("lifecycle events = %d, want 2: %+v", len(events), events)
	}
	payload, ok := events[1].Payload.(hooks.SubagentCompletePayload)
	if !ok {
		t.Fatalf("complete payload type = %T", events[1].Payload)
	}
	if payload.Status != "error" {
		t.Fatalf("async failure status = %q, want error", payload.Status)
	}
	if payload.Error == "" {
		t.Fatal("async failure event missing error message")
	}
}

// TestSubagentTool_AsyncShutdownOnRuntimeClose 验证主 Runtime 关闭时在途的异步
// 子任务被取消且 Close 不挂死——否则进程退出时会泄漏 goroutine，IM 服务重启时
// 表现为主进程卡住。
func TestSubagentTool_AsyncShutdownOnRuntimeClose(t *testing.T) {
	ctx := context.Background()
	gm := &gatedMockModel{gateAt: 2, entered: make(chan struct{}), release: make(chan struct{})}
	recorder := &eventRecorder{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: gm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HookMiddleware:      []hooks.Middleware{recorder.middleware()},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "long running task",
		"background":  true,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected async dispatch success, got: %s", res.Output)
	}

	select {
	case <-gm.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("async child never started")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- mainRT.Close() }()
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on in-flight async task")
	}

	// 任务被取消后应发出失败事件，失败不静默。
	events := recorder.waitFor(2, 2*time.Second)
	if len(events) != 2 {
		t.Fatalf("lifecycle events = %d, want 2: %+v", len(events), events)
	}
	payload, ok := events[1].Payload.(hooks.SubagentCompletePayload)
	if !ok {
		t.Fatalf("complete payload type = %T", events[1].Payload)
	}
	if payload.Status != "error" {
		t.Fatalf("cancelled task status = %q, want error", payload.Status)
	}
	if payload.Error == "" {
		t.Fatal("cancelled task event missing error message")
	}
}

// TestSubagentTool_ConcurrentAsyncDispatchesAreIsolated 验证并发后台派发各自
// 隔离：子会话标识唯一，各自的子 history 只包含自己的 [Subtask] 消息——
// 并行任务之间串数据会让多个子 agent 互相污染上下文。
func TestSubagentTool_ConcurrentAsyncDispatchesAreIsolated(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}
	recorder := &eventRecorder{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HookMiddleware:      []hooks.Middleware{recorder.middleware()},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)

	resA, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "alpha",
		"instruction": "task-alpha",
		"background":  true,
	})
	if err != nil {
		t.Fatalf("Execute alpha returned error: %v", err)
	}
	resB, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "beta",
		"instruction": "task-beta",
		"background":  true,
	})
	if err != nil {
		t.Fatalf("Execute beta returned error: %v", err)
	}

	subA, _ := resA.Data.(map[string]any)["sub_session_id"].(string)
	subB, _ := resB.Data.(map[string]any)["sub_session_id"].(string)
	if subA == "" || subB == "" {
		t.Fatalf("missing sub session ids: %q %q", subA, subB)
	}
	if subA == subB {
		t.Fatalf("concurrent dispatches reused sub session id: %s", subA)
	}

	events := recorder.waitFor(4, 3*time.Second)
	if len(events) != 4 {
		t.Fatalf("lifecycle events = %d, want 4: %+v", len(events), events)
	}

	var alphaReq, betaReq *model.Request
	for i := range mm.calls {
		if subtaskMessage(&mm.calls[i], "alpha") != "" {
			alphaReq = &mm.calls[i]
		}
		if subtaskMessage(&mm.calls[i], "beta") != "" {
			betaReq = &mm.calls[i]
		}
	}
	if alphaReq == nil || betaReq == nil {
		t.Fatal("both children should have been invoked")
	}
	alphaSubtask := subtaskMessage(alphaReq, "alpha")
	betaSubtask := subtaskMessage(betaReq, "beta")
	if !strings.Contains(alphaSubtask, "task-alpha") || subtaskMessage(alphaReq, "beta") != "" {
		t.Fatalf("alpha child history not isolated: %q", alphaSubtask)
	}
	if !strings.Contains(betaSubtask, "task-beta") || subtaskMessage(betaReq, "alpha") != "" {
		t.Fatalf("beta child history not isolated: %q", betaSubtask)
	}
}

// subtaskMessage 返回请求中属于指定子任务的 [Subtask] 消息内容，不存在时返回空串。
// [Subtask] 消息位于 fork 历史的末尾、子 agent 自身 prompt 之前，不在最后一条。
func subtaskMessage(req *model.Request, name string) string {
	for _, msg := range req.Messages {
		if strings.Contains(msg.Content, "[Subtask] "+name+":") {
			return msg.Content
		}
	}
	return ""
}

// requestContent 把请求的全部消息内容（正文 + tool 执行结果）拼接成一个字符串，
// 供跨消息断言检索。tool 结果只存在于 ToolCalls[].Result，不拼进来会漏掉工具
// 回传内容（如 subagent no-op 的拒绝文案）。
func requestContent(req *model.Request) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		b.WriteString(msg.Content)
		b.WriteString("\n")
		for _, call := range msg.ToolCalls {
			b.WriteString(call.Result)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestSubagentTool_ResumeCarriesPriorHistory 验证携带子会话标识追问时，子 agent
// 带着此前完整工作记忆续跑：追问请求同时包含第一轮与第二轮的 [Subtask] 指令，
// 复用同一子会话标识，且主会话历史不被污染。这条测试守住的是"追问=接续而非
// 重开"——若续跑退化成重新 fork 主上下文，token 被浪费、此前的分析脉络丢失，
// 只有比对两轮指令是否同处一个请求才能发现。
func TestSubagentTool_ResumeCarriesPriorHistory(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	ctxWithSession := WithToolSessionID(ctx, sessionID)

	res1, err := st.Execute(ctxWithSession, map[string]any{
		"name":        "analyzer",
		"instruction": "第一轮分析",
	})
	if err != nil {
		t.Fatalf("first Execute returned error: %v", err)
	}
	if !res1.Success {
		t.Fatalf("expected first dispatch success, got: %s", res1.Output)
	}
	data1, ok := res1.Data.(map[string]any)
	if !ok {
		t.Fatalf("first result data is not map[string]any: %T", res1.Data)
	}
	subSessionID, _ := data1["sub_session_id"].(string)
	if subSessionID == "" {
		t.Fatal("first dispatch missing sub_session_id")
	}

	mainBefore, _ := mainRT.SessionHistory(sessionID)

	res2, err := st.Execute(ctxWithSession, map[string]any{
		"name":           "analyzer",
		"instruction":    "第二轮追问",
		"sub_session_id": subSessionID,
	})
	if err != nil {
		t.Fatalf("resume Execute returned error: %v", err)
	}
	if !res2.Success {
		t.Fatalf("expected resume success, got: %s", res2.Output)
	}
	data2, ok := res2.Data.(map[string]any)
	if !ok {
		t.Fatalf("resume result data is not map[string]any: %T", res2.Data)
	}
	if got, _ := data2["sub_session_id"].(string); got != subSessionID {
		t.Fatalf("resume reused different sub session id: got %q, want %q", got, subSessionID)
	}

	// 追问的子请求（最后一次 model 调用）必须同时带着两轮子任务指令。
	resumeReq := mm.calls[len(mm.calls)-1]
	joined := requestContent(&resumeReq)
	for _, want := range []string{
		"[Subtask] analyzer: 第一轮分析",
		"[Subtask] analyzer: 第二轮追问",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("resume request missing %q, got:\n%s", want, joined)
		}
	}

	// 多轮追问不污染主会话历史。
	mainAfter, _ := mainRT.SessionHistory(sessionID)
	if len(mainAfter) != len(mainBefore) {
		t.Fatalf("main history mutated by resume: before=%d after=%d", len(mainBefore), len(mainAfter))
	}
}

// TestSubagentTool_ResumeRejectsForeignSubSessionID 验证归属校验：别的会话派发的
// 子会话标识、以及本会话从未派发过的合法格式标识，都必须被明确拒绝，且不派发任何
// 子 agent。这条测试守住的是跨会话隔离——丢了它，一个会话就能读到另一个会话
// 子 agent 的工作记忆。
func TestSubagentTool_ResumeRejectsForeignSubSessionID(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)

	res1, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "分析",
	})
	if err != nil {
		t.Fatalf("first Execute returned error: %v", err)
	}
	if !res1.Success {
		t.Fatalf("expected first dispatch success, got: %s", res1.Output)
	}
	data1, _ := res1.Data.(map[string]any)
	subSessionID, _ := data1["sub_session_id"].(string)

	callsBefore := len(mm.calls)

	// 别的会话携带本会话派发的子会话标识 → 拒绝。
	res2, err := st.Execute(WithToolSessionID(ctx, "other-session"), map[string]any{
		"name":           "analyzer",
		"instruction":    "追问",
		"sub_session_id": subSessionID,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res2.Success {
		t.Fatal("expected rejection for foreign sub session id, got success")
	}
	if !strings.Contains(res2.Output, "不属于当前会话") {
		t.Fatalf("foreign rejection message unclear: %q", res2.Output)
	}

	// 格式合法但本会话未派发过的标识 → 拒绝。
	res3, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":           "analyzer",
		"instruction":    "追问",
		"sub_session_id": "test-session-sub-00000000-0000-0000-0000-000000000000",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res3.Success {
		t.Fatal("expected rejection for unknown sub session id, got success")
	}
	if !strings.Contains(res3.Output, "找不到子会话") {
		t.Fatalf("unknown-id rejection message unclear: %q", res3.Output)
	}

	if len(mm.calls) != callsBefore {
		t.Fatalf("rejected resume should not dispatch a sub agent: calls before=%d after=%d", callsBefore, len(mm.calls))
	}
}

// TestSubagentTool_ResumeKeepsTypeConfig 验证追问派发的类型配置与首次派发一致：
// 不传 type 参数时沿用首次的类型（追加提示词与拒绝换型），显式传同名类型也被
// 接受；system prompt 与工具面同样不漂移。这条测试守住恢复链上的类型语义——
// 追加提示词必须随缓存类型一起沿用；工具面则与主 agent 全量一致（白名单不再
// 收窄），且追问各轮之间不得漂移。
func TestSubagentTool_ResumeKeepsTypeConfig(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read", "write", "subagent"},
		CustomTools:         []tool.Tool{dummyCustomTool{}},
		AgentTypes: []AgentType{
			{Name: "explorer", Description: "只读搜索型", AllowedTools: []string{"read"}, AppendPrompt: "只做只读检索"},
			{Name: "writer", Description: "写入型", AllowedTools: []string{"write"}},
		},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)
	ctxWithSession := WithToolSessionID(ctx, sessionID)

	res1, err := st.Execute(ctxWithSession, map[string]any{
		"name":        "explorer",
		"instruction": "第一轮检索",
		"type":        "explorer",
	})
	if err != nil {
		t.Fatalf("first Execute returned error: %v", err)
	}
	if !res1.Success {
		t.Fatalf("expected first dispatch success, got: %s", res1.Output)
	}
	data1, _ := res1.Data.(map[string]any)
	subSessionID, _ := data1["sub_session_id"].(string)

	// 追问不传 type：沿用首次派发的 explorer 类型（追加提示词）。
	res2, err := st.Execute(ctxWithSession, map[string]any{
		"name":           "explorer",
		"instruction":    "追问一",
		"sub_session_id": subSessionID,
	})
	if err != nil || !res2.Success {
		t.Fatalf("resume Execute failed: err=%v res=%+v", err, res2)
	}

	// 追问显式传同名类型：同样被接受，配置不变。
	res3, err := st.Execute(ctxWithSession, map[string]any{
		"name":           "explorer",
		"instruction":    "追问二",
		"sub_session_id": subSessionID,
		"type":           "explorer",
	})
	if err != nil || !res3.Success {
		t.Fatalf("resume Execute with same type failed: err=%v res=%+v", err, res3)
	}

	firstReq := mm.calls[1]
	resumeReq := mm.calls[2]
	sameTypeReq := mm.calls[3]

	firstTools := strings.Join(toolNames(firstReq.Tools), ",")
	resumeTools := strings.Join(toolNames(resumeReq.Tools), ",")
	sameTypeTools := strings.Join(toolNames(sameTypeReq.Tools), ",")
	if resumeTools != firstTools || sameTypeTools != firstTools {
		t.Fatalf("resume tool face drifted: first=%q resume=%q sameType=%q", firstTools, resumeTools, sameTypeTools)
	}
	// 白名单不再收窄：追问请求带全量工具（含 subagent 与 customEcho），与首次
	// 派发逐项一致；类型配置的沿用体现在追加提示词上（见下）。
	for _, want := range []string{"read", "write", "customEcho", subagentToolName} {
		if !hasTool(toolNames(resumeReq.Tools), want) {
			t.Fatalf("resume request missing %q: %v", want, toolNames(resumeReq.Tools))
		}
	}

	// 追加提示词随缓存类型一起沿用。
	if !strings.Contains(requestContent(&resumeReq), "只做只读检索") {
		t.Fatalf("resume request missing cached append prompt:\n%s", requestContent(&resumeReq))
	}

	// system prompt 与首次派发/主 runtime 一致，不漂移。
	if resumeReq.System != mm.calls[0].System {
		t.Fatalf("resume system prompt drifted: resume=%q main=%q", resumeReq.System, mm.calls[0].System)
	}
}

// TestSubagentTool_ResumeRejectsTypeMismatch 验证追问时显式传入与首次派发不同的
// 类型会被拒绝。类型决定工具白名单，追问换型等于悄悄改写子 agent 的权限面，
// 必须显式报错而非静默切换。
func TestSubagentTool_ResumeRejectsTypeMismatch(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read", "write"},
		AgentTypes: []AgentType{
			{Name: "explorer", Description: "只读搜索型", AllowedTools: []string{"read"}},
			{Name: "writer", Description: "写入型", AllowedTools: []string{"write"}},
		},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)

	res1, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "explorer",
		"instruction": "第一轮检索",
		"type":        "explorer",
	})
	if err != nil {
		t.Fatalf("first Execute returned error: %v", err)
	}
	data1, _ := res1.Data.(map[string]any)
	subSessionID, _ := data1["sub_session_id"].(string)

	callsBefore := len(mm.calls)

	res2, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":           "explorer",
		"instruction":    "追问",
		"sub_session_id": subSessionID,
		"type":           "writer",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res2.Success {
		t.Fatal("expected rejection for type mismatch on resume, got success")
	}
	if !strings.Contains(res2.Output, "不一致") {
		t.Fatalf("type-mismatch rejection message unclear: %q", res2.Output)
	}
	if len(mm.calls) != callsBefore {
		t.Fatalf("rejected resume should not dispatch a sub agent: calls before=%d after=%d", callsBefore, len(mm.calls))
	}
}

// TestSubagentTool_CloseClearsSubSessionCache 验证主 Runtime 关闭后子会话缓存被
// 清理：此前可追问的子会话标识立即失效，且不会借机关出新的子 runtime。这条测试
// 守住的是缓存生命周期——缓存若活得比主 runtime 久，就是跨实例的内存泄漏与
// 上下文错认。
func TestSubagentTool_CloseClearsSubSessionCache(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)

	res1, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "分析",
	})
	if err != nil {
		t.Fatalf("first Execute returned error: %v", err)
	}
	data1, _ := res1.Data.(map[string]any)
	subSessionID, _ := data1["sub_session_id"].(string)

	if err := mainRT.Close(); err != nil {
		t.Fatalf("close main runtime: %v", err)
	}

	callsBefore := len(mm.calls)

	res2, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":           "analyzer",
		"instruction":    "追问",
		"sub_session_id": subSessionID,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res2.Success {
		t.Fatal("expected resume to fail after main runtime close")
	}
	if !strings.Contains(res2.Output, "找不到子会话") {
		t.Fatalf("post-close rejection message unclear: %q", res2.Output)
	}
	if len(mm.calls) != callsBefore {
		t.Fatalf("post-close resume dispatched a sub agent: calls before=%d after=%d", callsBefore, len(mm.calls))
	}
}

// TestRuntimeClearSubagentSession 验证显式清理入口：清除后该子会话不可追问，
// 重复清除与空标识安全返回 false，且不派发任何子 agent。
func TestRuntimeClearSubagentSession(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)

	res1, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "分析",
	})
	if err != nil {
		t.Fatalf("first Execute returned error: %v", err)
	}
	data1, _ := res1.Data.(map[string]any)
	subSessionID, _ := data1["sub_session_id"].(string)

	if !mainRT.ClearSubagentSession(subSessionID) {
		t.Fatal("ClearSubagentSession returned false for a cached sub session")
	}
	if mainRT.ClearSubagentSession(subSessionID) {
		t.Fatal("second ClearSubagentSession should return false")
	}
	if mainRT.ClearSubagentSession("") {
		t.Fatal("ClearSubagentSession with empty id should return false")
	}

	callsBefore := len(mm.calls)

	res2, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":           "analyzer",
		"instruction":    "追问",
		"sub_session_id": subSessionID,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res2.Success {
		t.Fatal("expected resume to fail after explicit clear")
	}
	if !strings.Contains(res2.Output, "找不到子会话") {
		t.Fatalf("post-clear rejection message unclear: %q", res2.Output)
	}
	if len(mm.calls) != callsBefore {
		t.Fatalf("rejected resume should not dispatch a sub agent: calls before=%d after=%d", callsBefore, len(mm.calls))
	}
}
