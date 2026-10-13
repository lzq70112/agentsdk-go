package api

import (
	"context"
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// syntheticFact 构造带压缩豁免标记的环境事实消息（与注入路由写入 history
// 的形态一致：user 角色 + synthetic metadata）。
func syntheticFact(content string) message.Message {
	return message.Message{
		Role:     "user",
		Content:  content,
		Metadata: map[string]any{metadataSynthetic: true, metadataSyntheticKey: "subagent_result"},
	}
}

// TestCompactPreservesSyntheticMessages 意图：长会话压缩后 agent 仍能看到
// 收到过的环境事实（子 agent 结果等注入消息）。防的事故是"压缩后失忆"——
// 注入事实被卷进有损摘要后，agent 不知道平台注入过什么。断言是逐字节保留
// + 在压缩后 history 中可见，而不是只看消息数量。
func TestCompactPreservesSyntheticMessages(t *testing.T) {
	t.Parallel()

	hist := message.NewHistory()
	hist.Append(message.Message{Role: "user", Content: "u0"})
	hist.Append(message.Message{Role: "assistant", ToolCalls: []message.ToolCall{{ID: "t1", Name: "bash", Arguments: map[string]any{"cmd": "one"}}}})
	hist.Append(message.Message{Role: "tool", ToolCalls: []message.ToolCall{{ID: "t1", Name: "bash", Result: "TOOL_OUTPUT_ONE"}}})
	hist.Append(syntheticFact("ENV_FACT_环境事实-πλü-必须逐字保留"))
	hist.Append(message.Message{Role: "user", Content: "u1"})
	hist.Append(message.Message{Role: "assistant", ToolCalls: []message.ToolCall{{ID: "t2", Name: "bash", Arguments: map[string]any{"cmd": "two"}}}})
	hist.Append(message.Message{Role: "tool", ToolCalls: []message.ToolCall{{ID: "t2", Name: "bash", Result: "TOOL_OUTPUT_TWO"}}})
	hist.Append(message.Message{Role: "user", Content: "tail-1"})
	hist.Append(message.Message{Role: "user", Content: "tail-2"})

	comp := newCompactor(CompactConfig{Enabled: true, PreserveCount: 2, Threshold: 0.1}, 1)
	stub := &compactStubModel{resp: "summary"}
	ok, err := comp.maybeCompact(context.Background(), hist, stub, "", nil)
	if err != nil || !ok {
		t.Fatalf("maybeCompact ok=%v err=%v", ok, err)
	}

	msgs := hist.All()
	if len(msgs) != 4 {
		t.Fatalf("compacted history len = %d, want 4 (summary + fact + 2 tails): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "assistant" || !strings.Contains(msgs[0].Content, "summary") {
		t.Fatalf("expected summary first, got %+v", msgs[0])
	}
	fact := msgs[1]
	if fact.Content != "ENV_FACT_环境事实-πλü-必须逐字保留" {
		t.Fatalf("synthetic fact corrupted after compaction: %q", fact.Content)
	}
	if !isSyntheticMessage(fact) {
		t.Fatalf("synthetic metadata lost after compaction: %+v", fact.Metadata)
	}
	if msgs[2].Content != "tail-1" || msgs[3].Content != "tail-2" {
		t.Fatalf("tail messages corrupted: %+v %+v", msgs[2], msgs[3])
	}

	// 环境事实不参与摘要生成：摘要输入里不应出现它的内容。
	combined := ""
	for _, m := range stub.last.Messages {
		combined += m.Role + ":" + m.Content + "\n"
	}
	if strings.Contains(combined, "ENV_FACT") {
		t.Fatalf("synthetic fact should not feed the summary, got %q", combined)
	}
}

// TestCompactSkipsModelWhenOnlySyntheticInRange 固化边界：待压缩区间全是
// 环境事实时没有可压缩内容，不调模型、历史原样保留。
func TestCompactSkipsModelWhenOnlySyntheticInRange(t *testing.T) {
	t.Parallel()

	hist := message.NewHistory()
	for i := 0; i < 7; i++ {
		hist.Append(syntheticFact("fact-" + string(rune('a'+i))))
	}

	comp := newCompactor(CompactConfig{Enabled: true, PreserveCount: 2, Threshold: 0.1}, 1)
	stub := &compactStubModel{resp: "summary"}
	ok, err := comp.maybeCompact(context.Background(), hist, stub, "", nil)
	if err != nil {
		t.Fatalf("maybeCompact err=%v", err)
	}
	if ok {
		t.Fatal("compaction should not run when summary range holds only synthetic messages")
	}
	if stub.last.Messages != nil {
		t.Fatal("model must not be called when there is nothing to summarize")
	}
	if hist.Len() != 7 {
		t.Fatalf("history must stay intact, len = %d", hist.Len())
	}
}

// TestReactiveCompactPreservesSyntheticMessages 固化反应式压缩（prompt too long 触发的兜底路径）同样豁免环境事实。
func TestReactiveCompactPreservesSyntheticMessages(t *testing.T) {
	t.Parallel()

	hist := message.NewHistory()
	hist.Append(message.Message{Role: "user", Content: "u0"})
	hist.Append(message.Message{Role: "assistant", ToolCalls: []message.ToolCall{{ID: "t1", Name: "bash", Arguments: map[string]any{"cmd": "one"}}}})
	hist.Append(message.Message{Role: "tool", ToolCalls: []message.ToolCall{{ID: "t1", Name: "bash", Result: "TOOL_OUTPUT_ONE"}}})
	hist.Append(syntheticFact("ENV_FACT_反应式路径-πλü-保留"))
	hist.Append(message.Message{Role: "user", Content: "u1"})
	hist.Append(message.Message{Role: "assistant", ToolCalls: []message.ToolCall{{ID: "t2", Name: "bash", Arguments: map[string]any{"cmd": "two"}}}})
	hist.Append(message.Message{Role: "tool", ToolCalls: []message.ToolCall{{ID: "t2", Name: "bash", Result: "TOOL_OUTPUT_TWO"}}})
	hist.Append(message.Message{Role: "user", Content: "tail-1"})
	hist.Append(message.Message{Role: "user", Content: "tail-2"})

	comp := newCompactor(CompactConfig{Enabled: true, PreserveCount: 4, Threshold: 0.1}, 1)
	stub := &compactStubModel{resp: "summary"}
	ok, err := comp.reactiveCompact(context.Background(), hist, stub)
	if err != nil || !ok {
		t.Fatalf("reactiveCompact ok=%v err=%v", ok, err)
	}

	msgs := hist.All()
	if len(msgs) != 4 {
		t.Fatalf("compacted history len = %d, want 4: %+v", len(msgs), msgs)
	}
	if msgs[1].Content != "ENV_FACT_反应式路径-πλü-保留" {
		t.Fatalf("synthetic fact corrupted in reactive path: %q", msgs[1].Content)
	}
	if !isSyntheticMessage(msgs[1]) {
		t.Fatal("synthetic metadata lost in reactive path")
	}
}

// TestMicroCompactPreservesSyntheticMessages 固化微压缩路径同样豁免：合成
// 消息的 ReasoningContent、多模态块不被修剪，普通消息照常修剪。
func TestMicroCompactPreservesSyntheticMessages(t *testing.T) {
	t.Parallel()

	hist := message.NewHistory()
	hist.Append(message.Message{
		Role:             "user",
		Content:          "FACT-πλü",
		ContentBlocks:    []message.ContentBlock{{Type: message.ContentBlockImage}},
		ReasoningContent: "fact-reasoning",
		Metadata:         map[string]any{metadataSynthetic: true},
	})
	hist.Append(message.Message{
		Role:             "user",
		Content:          "plain",
		ContentBlocks:    []message.ContentBlock{{Type: message.ContentBlockImage}},
		ReasoningContent: "plain-reasoning",
	})
	hist.Append(message.Message{Role: "tool", ToolCalls: []message.ToolCall{{ID: "t1", Name: "bash", Result: "big-output"}}})
	hist.Append(message.Message{Role: "user", Content: "tail-1"})
	hist.Append(message.Message{Role: "user", Content: "tail-2"})
	hist.Append(message.Message{Role: "user", Content: "tail-3"})
	hist.Append(message.Message{Role: "user", Content: "tail-4"})

	comp := newCompactor(CompactConfig{Enabled: true, PreserveCount: 2, MicroPreserveCount: 2, Threshold: 0.1}, 1)
	if !comp.maybeMicroCompact(hist) {
		t.Fatal("expected micro compaction to run")
	}

	msgs := hist.All()
	fact := msgs[0]
	if fact.Content != "FACT-πλü" || fact.ReasoningContent != "fact-reasoning" || len(fact.ContentBlocks) != 1 {
		t.Fatalf("synthetic message was pruned by micro compaction: %+v", fact)
	}
	if msgs[1].ReasoningContent != "" || len(msgs[1].ContentBlocks) != 0 {
		t.Fatalf("plain message should be pruned: %+v", msgs[1])
	}
	if !isMicroCompactedToolResult(msgs[2].ToolCalls[0].Result) {
		t.Fatalf("plain tool result should be truncated: %q", msgs[2].ToolCalls[0].Result)
	}
}
