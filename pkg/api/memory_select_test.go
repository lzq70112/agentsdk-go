package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

func TestParseMemorySelectorResponse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain json", `{"selected_memories": ["a.md", "b.md"]}`, []string{"a.md", "b.md"}},
		{"fenced json", "```json\n{\"selected_memories\": [\"a.md\"]}\n```", []string{"a.md"}},
		{"prose around json", "Sure! {\"selected_memories\":[\"x.md\"]} hope that helps", []string{"x.md"}},
		{"empty list", `{"selected_memories": []}`, nil},
		{"garbage", "I cannot help with that", nil},
		{"broken json", `{"selected_memories": ["a.md"`, nil},
		{"blank names dropped", `{"selected_memories": ["", "a.md", "  "]}`, []string{"a.md"}},
		{"capped at five", `{"selected_memories": ["1.md","2.md","3.md","4.md","5.md","6.md","7.md"]}`,
			[]string{"1.md", "2.md", "3.md", "4.md", "5.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMemorySelectorResponse(tt.in)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("parse = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCollectRecentToolNames(t *testing.T) {
	msgs := []message.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []message.ToolCall{{Name: "read"}, {Name: "grep"}}},
		{Role: "tool", ToolCalls: []message.ToolCall{{Name: "grep"}}},
		{Role: "assistant", ToolCalls: []message.ToolCall{{Name: "write"}}},
		{Role: "assistant", Content: "done"},
	}
	got := collectRecentToolNames(msgs, 8)
	want := []string{"write", "grep", "read"} // 倒序、去重（tool 消息的 grep 不重复计）
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("recent tools = %v, want %v", got, want)
	}

	limited := collectRecentToolNames(msgs, 1)
	if len(limited) != 1 || limited[0] != "write" {
		t.Fatalf("limit=1 should keep only newest, got %v", limited)
	}
	if got := collectRecentToolNames(nil, 8); got != nil {
		t.Fatalf("empty history should give nil, got %v", got)
	}
	if got := memoryRecentToolsSection(nil); got != "" {
		t.Fatalf("no tools must render empty section, got %q", got)
	}
	if got := memoryRecentToolsSection([]string{"read", "glob"}); got != "\n\nRecently used tools: read, glob" {
		t.Fatalf("tools section = %q", got)
	}
}

func TestLLMSelectorSelectsByFilename(t *testing.T) {
	candidates := []memoryHeader{
		{Filename: "a.md", Path: "/mem/a.md"},
		{Filename: "b.md", Path: "/mem/b.md"},
		{Filename: "c.md", Path: "/mem/c.md"},
	}
	stub := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: `{"selected_memories": ["b.md", "missing.md", "b.md"]}`}},
	}}
	got := (&llmSelector{complete: stub.Complete}).SelectMemory(context.Background(), "q", candidates, "")
	if len(got) != 1 || got[0].Filename != "b.md" {
		t.Fatalf("selected = %v, want only b.md (miss ignored, dup dropped)", got)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("selector must make exactly one Complete call, got %d", len(stub.requests))
	}
}

func TestLLMSelectorUserMessageFormat(t *testing.T) {
	candidates := []memoryHeader{
		{Filename: "a.md", Type: "user", Description: "用户角色", ModTime: time.Now(), Path: "/mem/a.md"},
	}
	stub := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: `{"selected_memories": []}`}},
	}}
	(&llmSelector{complete: stub.Complete}).SelectMemory(context.Background(), "帮我改部署脚本", candidates, "\n\nRecently used tools: read")
	req := stub.requests[0]
	if req.System != memorySelectRelevantPrompt {
		t.Fatal("selector request must embed select_relevant prompt as system")
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("selector must send single user message, got %+v", req.Messages)
	}
	content := req.Messages[0].Content
	for _, fragment := range []string{
		"Query:\n帮我改部署脚本\n",
		"Available memories:\n- [user] a.md (",
		"): 用户角色",
		"\n\nRecently used tools: read",
	} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("selector user message missing %q:\n%s", fragment, content)
		}
	}
}

func TestLLMSelectorFailureSilent(t *testing.T) {
	candidates := []memoryHeader{{Filename: "a.md", Path: "/mem/a.md"}}
	t.Run("garbage response", func(t *testing.T) {
		stub := &stubModel{responses: []*model.Response{
			{Message: model.Message{Role: "assistant", Content: "no memory seems relevant"}},
		}}
		got := (&llmSelector{complete: stub.Complete}).SelectMemory(context.Background(), "q", candidates, "")
		if got != nil {
			t.Fatalf("garbage response must return nil, got %v", got)
		}
	})
	t.Run("complete error", func(t *testing.T) {
		stub := &stubModel{err: errors.New("model down")}
		got := (&llmSelector{complete: stub.Complete}).SelectMemory(context.Background(), "q", candidates, "")
		if got != nil {
			t.Fatalf("complete error must return nil, got %v", got)
		}
	})
}

func TestNewMemoryRuntimeSelectorWiring(t *testing.T) {
	dir := t.TempDir()
	t.Run("pool tier present uses llm selector", func(t *testing.T) {
		opts := Options{
			// 生产路径经 withDefaults 已填 SelectorTier=ModelTierLow；此处直调
			// newMemoryRuntime，需显式给定以对齐生产取值。
			Memory:    &MemoryOptions{Dir: dir, SelectorTier: ModelTierLow},
			ModelPool: map[ModelTier]model.Model{ModelTierLow: &stubModel{}},
		}
		rt := newMemoryRuntime(opts, dir)
		if rt == nil {
			t.Fatal("runtime must build")
		}
		if _, ok := rt.selector.(*llmSelector); !ok {
			t.Fatalf("pool tier must wire llmSelector, got %T", rt.selector)
		}
	})
	t.Run("no pool keeps keyword selector", func(t *testing.T) {
		opts := Options{Memory: &MemoryOptions{Dir: dir}}
		rt := newMemoryRuntime(opts, dir)
		if rt == nil {
			t.Fatal("runtime must build")
		}
		if _, ok := rt.selector.(keywordSelector); !ok {
			t.Fatalf("no pool must keep keywordSelector, got %T", rt.selector)
		}
	})
}

func TestLLMRecallAsyncReadyConsumed(t *testing.T) {
	memDir := memoryRecallFixture(t)
	rt := &Runtime{memory: newTestMemoryRuntime(memDir)}
	rt.memory.selector = &llmSelector{complete: (&stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: `{"selected_memories": ["user_role.md"]}`}},
	}}).Complete}
	rt.memory.ctx, rt.memory.cancel = context.WithCancel(context.Background())
	t.Cleanup(rt.memory.cancel)

	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "请讲讲用户角色与协作偏好",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s", RequestID: "r1"},
	}
	rt.beginMemoryRecall(&prep)
	rt.memory.wg.Wait() // 预取 goroutine 完成（stub 即时返回）
	rt.consumeMemoryRecall(&prep)
	if prep.history.Len() != 1 {
		t.Fatalf("ready recall must inject, len=%d", prep.history.Len())
	}
	injected := prep.history.All()[0]
	if origin, ok := injected.Metadata[memoryMetadataOrigin].(string); !ok || origin != memoryOriginRelevantMemory {
		t.Fatalf("injected origin = %v", injected.Metadata[memoryMetadataOrigin])
	}
	// 消费后槽已清空：再消费是无操作。
	rt.consumeMemoryRecall(&prep)
	if prep.history.Len() != 1 {
		t.Fatal("consume must drain the job slot")
	}
}

func TestLLMRecallAsyncNotReadyAbandoned(t *testing.T) {
	memDir := memoryRecallFixture(t)
	rt := &Runtime{memory: newTestMemoryRuntime(memDir)}
	release := make(chan struct{})
	rt.memory.selector = &llmSelector{complete: func(ctx context.Context, req model.Request) (*model.Response, error) {
		<-release
		return &model.Response{Message: model.Message{Role: "assistant", Content: `{"selected_memories": ["user_role.md"]}`}}, nil
	}}
	rt.memory.ctx, rt.memory.cancel = context.WithCancel(context.Background())
	t.Cleanup(rt.memory.cancel)

	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "请讲讲用户角色与协作偏好",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s", RequestID: "r1"},
	}
	rt.beginMemoryRecall(&prep)
	rt.consumeMemoryRecall(&prep) // 未就绪：绝不阻塞、本回合放弃
	if prep.history.Len() != 0 {
		t.Fatal("not-ready recall must not inject")
	}
	close(release)
	rt.memory.wg.Wait()
	// 放弃后同回合不再消费（槽已清）。
	rt.consumeMemoryRecall(&prep)
	if prep.history.Len() != 0 {
		t.Fatal("abandoned job must stay abandoned")
	}
}

func TestLLMRecallCrossTurnJobNotStolen(t *testing.T) {
	memDir := memoryRecallFixture(t)
	rt := &Runtime{memory: newTestMemoryRuntime(memDir)}
	rt.memory.selector = &llmSelector{complete: (&stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: `{"selected_memories": ["user_role.md"]}`}},
	}}).Complete}
	rt.memory.ctx, rt.memory.cancel = context.WithCancel(context.Background())
	t.Cleanup(rt.memory.cancel)

	stale := preparedRun{
		ctx:        context.Background(),
		prompt:     "请讲讲用户角色与协作偏好",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s", RequestID: "old"},
	}
	rt.beginMemoryRecall(&stale)
	rt.memory.wg.Wait()

	fresh := preparedRun{
		ctx:        context.Background(),
		prompt:     "请讲讲部署流水线要点",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s", RequestID: "new"},
	}
	// 上一回合的迟到结果不归本回合消费（slot 不被偷走）。
	rt.consumeMemoryRecall(&fresh)
	if fresh.history.Len() != 0 {
		t.Fatal("stale job must not inject into a new turn")
	}
	// 归属回合（同 requestID）仍可消费。
	rt.consumeMemoryRecall(&stale)
	if stale.history.Len() != 1 {
		t.Fatal("owning turn must consume its own job")
	}
}
