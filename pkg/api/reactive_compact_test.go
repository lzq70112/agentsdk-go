package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stellarlinkco/agentsdk-go/pkg/message"
	"github.com/stellarlinkco/agentsdk-go/pkg/middleware"
	"github.com/stellarlinkco/agentsdk-go/pkg/model"
	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
)

type reactiveCompactModel struct {
	streamCalls int
	requests    []model.Request
}

func (m *reactiveCompactModel) Complete(_ context.Context, req model.Request) (*model.Response, error) {
	return &model.Response{Message: model.Message{Role: "assistant", Content: "summary"}}, nil
}

func (m *reactiveCompactModel) CompleteStream(_ context.Context, req model.Request, cb model.StreamHandler) error {
	m.streamCalls++
	m.requests = append(m.requests, req)
	if m.streamCalls == 1 {
		return errors.New("prompt_too_long: HTTP 413")
	}
	return cb(model.StreamResult{Final: true, Response: &model.Response{
		Message:    model.Message{Role: "assistant", Content: "done"},
		StopReason: "done",
	}})
}

func TestRunLoopReactiveCompactsAndRetriesOnPromptTooLong(t *testing.T) {
	t.Parallel()

	hist := message.NewHistory()
	for i := 0; i < 8; i++ {
		hist.Append(message.Message{Role: "user", Content: strings.Repeat("u", 400)})
	}
	hist.Append(message.Message{
		Role:             "assistant",
		Content:          "analysis",
		ReasoningContent: "private reasoning",
	})
	hist.Append(message.Message{
		Role: "tool",
		ToolCalls: []message.ToolCall{{
			ID:     "t1",
			Name:   "read",
			Result: strings.Repeat("x", 400),
		}},
	})

	rt := &Runtime{
		opts: Options{},
		compactor: newCompactor(CompactConfig{
			Enabled:            true,
			PreserveCount:      4,
			MicroPreserveCount: 2,
			Threshold:          0.95,
		}, defaultClaudeContextLimit),
	}
	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "hi",
		history:    hist,
		normalized: Request{SessionID: "s"},
	}
	mdl := &reactiveCompactModel{}

	resp, err := rt.runLoop(prep, mdl, &runtimeHookAdapter{}, &runtimeToolExecutor{executor: tool.NewExecutor(nil, nil), hooks: &runtimeHookAdapter{}, history: hist, host: "localhost"}, middleware.NewChain(nil), false)
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if resp == nil || resp.Message.Content != "done" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if mdl.streamCalls != 2 {
		t.Fatalf("expected retry after compaction, got %d stream calls", mdl.streamCalls)
	}
	msgs := hist.All()
	if len(msgs) == 0 || msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, "## Summary") {
		t.Fatalf("expected summary message after reactive compaction, got %+v", msgs)
	}
}
