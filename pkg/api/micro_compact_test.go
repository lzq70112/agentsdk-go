package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

func TestCompactor_MicroCompactsOldMessagesBeforeLLMCompaction(t *testing.T) {
	t.Parallel()

	hist := message.NewHistory()
	for i := 0; i < defaultMicroTriggerBuffer+3; i++ {
		hist.Append(message.Message{Role: "user", Content: fmt.Sprintf("user-%d", i)})
	}
	hist.Append(message.Message{
		Role:             "assistant",
		Content:          "analysis",
		ReasoningContent: "private chain of thought",
		ContentBlocks: []message.ContentBlock{
			{Type: message.ContentBlockText, Text: "text"},
			{Type: message.ContentBlockImage, URL: "https://example.com/image.png"},
			{Type: message.ContentBlockDocument, URL: "https://example.com/doc.pdf"},
		},
	})
	hist.Append(message.Message{
		Role: "tool",
		ToolCalls: []message.ToolCall{{
			ID:     "t-old",
			Name:   "read",
			Result: "very old tool output that should be compacted",
		}},
	})
	hist.Append(message.Message{Role: "assistant", Content: "recent", ReasoningContent: "keep me"})
	hist.Append(message.Message{
		Role: "tool",
		ToolCalls: []message.ToolCall{{
			ID:     "t-new",
			Name:   "read",
			Result: "recent tool output should stay intact",
		}},
	})

	comp := newCompactor(CompactConfig{
		Enabled:            true,
		Threshold:          0.99,
		PreserveCount:      2,
		MicroPreserveCount: 2,
	}, defaultClaudeContextLimit)

	did, err := comp.maybeCompact(context.Background(), hist, nil, "", nil)
	if err != nil {
		t.Fatalf("maybeCompact returned error: %v", err)
	}
	if !did {
		t.Fatal("expected micro-compaction to run")
	}

	msgs := hist.All()
	oldOutput := "very old tool output that should be compacted"
	oldAssistant := msgs[len(msgs)-4]
	if oldAssistant.ReasoningContent != "" {
		t.Fatalf("expected old reasoning stripped, got %q", oldAssistant.ReasoningContent)
	}
	if len(oldAssistant.ContentBlocks) != 1 || oldAssistant.ContentBlocks[0].Type != message.ContentBlockText {
		t.Fatalf("expected only text blocks preserved, got %+v", oldAssistant.ContentBlocks)
	}

	oldTool := msgs[len(msgs)-3]
	if got, want := oldTool.ToolCalls[0].Result, fmt.Sprintf("[output truncated, %d chars]", len([]rune(oldOutput))); got != want {
		t.Fatalf("old tool result = %q, want %q", got, want)
	}

	recentAssistant := msgs[len(msgs)-2]
	if got := recentAssistant.ReasoningContent; got != "keep me" {
		t.Fatalf("recent reasoning = %q, want keep me", got)
	}
	recentTool := msgs[len(msgs)-1]
	if got := recentTool.ToolCalls[0].Result; got != "recent tool output should stay intact" {
		t.Fatalf("recent tool result changed: %q", got)
	}
}

func TestCompactor_MicroCompactionIsIdempotent(t *testing.T) {
	t.Parallel()

	hist := message.NewHistory()
	for i := 0; i < defaultMicroTriggerBuffer+3; i++ {
		hist.Append(message.Message{Role: "user", Content: fmt.Sprintf("user-%d", i)})
	}
	hist.Append(message.Message{
		Role:             "assistant",
		Content:          "analysis",
		ReasoningContent: "private chain of thought",
	})
	hist.Append(message.Message{
		Role: "tool",
		ToolCalls: []message.ToolCall{{
			ID:     "t-old",
			Name:   "read",
			Result: "very old tool output that should be compacted",
		}},
	})
	hist.Append(message.Message{Role: "assistant", Content: "recent"})
	hist.Append(message.Message{Role: "tool", ToolCalls: []message.ToolCall{{ID: "t-new", Name: "read", Result: "recent tool output should stay intact"}}})

	comp := newCompactor(CompactConfig{
		Enabled:            true,
		Threshold:          0.99,
		PreserveCount:      2,
		MicroPreserveCount: 2,
	}, defaultClaudeContextLimit)

	if did, err := comp.maybeCompact(context.Background(), hist, nil, "", nil); err != nil || !did {
		t.Fatalf("first maybeCompact did=%v err=%v", did, err)
	}
	first := hist.All()
	if did, err := comp.maybeCompact(context.Background(), hist, nil, "", nil); err != nil {
		t.Fatalf("second maybeCompact err=%v", err)
	} else if did {
		t.Fatal("expected second micro-compaction to be a no-op")
	}
	second := hist.All()
	if fmt.Sprintf("%+v", first) != fmt.Sprintf("%+v", second) {
		t.Fatalf("micro-compaction changed history on second pass\nfirst=%+v\nsecond=%+v", first, second)
	}
}
