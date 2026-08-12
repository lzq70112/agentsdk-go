package api

import (
	"context"
	"errors"
	"testing"

	"github.com/stellarlinkco/agentsdk-go/pkg/message"
	"github.com/stellarlinkco/agentsdk-go/pkg/middleware"
	"github.com/stellarlinkco/agentsdk-go/pkg/model"
	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
)

func TestRunLoopStopsWhenTokenBudgetExceeded(t *testing.T) {
	t.Parallel()

	rt := &Runtime{opts: Options{
		TokenBudget: TokenBudgetConfig{MaxTokens: 100},
	}}
	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "hi",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s"},
	}
	mdl := &stubModel{responses: []*model.Response{{
		Message: model.Message{Role: "assistant", Content: "done"},
		Usage:   model.Usage{InputTokens: 60, OutputTokens: 50, TotalTokens: 110},
	}}}

	resp, err := rt.runLoop(prep, mdl, &runtimeHookAdapter{}, &runtimeToolExecutor{executor: tool.NewExecutor(nil, nil), hooks: &runtimeHookAdapter{}, host: "localhost"}, middleware.NewChain(nil), false)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Fatalf("expected ErrTokenBudgetExceeded, got %v", err)
	}
	if resp == nil || resp.Message.Content != "done" {
		t.Fatalf("expected last response returned, got %+v", resp)
	}
}

func TestRunLoopStopsOnDiminishingReturns(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	echo := &echoTool{}
	if err := reg.Register(echo); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	exec := tool.NewExecutor(reg, nil)

	rt := &Runtime{opts: Options{
		TokenBudget: TokenBudgetConfig{DiminishingWindow: 3, DiminishingThreshold: 50},
	}}
	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "hi",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s"},
	}
	mdl := &stubModel{responses: []*model.Response{
		{
			Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "t1", Name: "echo", Arguments: map[string]any{"text": "one"}}}},
			Usage:   model.Usage{InputTokens: 10, OutputTokens: 40, TotalTokens: 50},
		},
		{
			Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "t2", Name: "echo", Arguments: map[string]any{"text": "two"}}}},
			Usage:   model.Usage{InputTokens: 10, OutputTokens: 30, TotalTokens: 40},
		},
		{
			Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "t3", Name: "echo", Arguments: map[string]any{"text": "three"}}}},
			Usage:   model.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
		},
	}}

	resp, err := rt.runLoop(prep, mdl, &runtimeHookAdapter{}, &runtimeToolExecutor{executor: exec, hooks: &runtimeHookAdapter{}, history: prep.history, host: "localhost"}, middleware.NewChain(nil), false)
	if !errors.Is(err, ErrDiminishingReturns) {
		t.Fatalf("expected ErrDiminishingReturns, got %v", err)
	}
	if resp == nil || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].ID != "t3" {
		t.Fatalf("expected third response returned, got %+v", resp)
	}
	if echo.calls != 3 {
		t.Fatalf("expected 3 tool executions before stop, got %d", echo.calls)
	}
	if len(mdl.requests) != 3 {
		t.Fatalf("expected 3 model requests before stop, got %d", len(mdl.requests))
	}
}
