package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellarlinkco/agentsdk-go/pkg/model"
)

type maxTokensModel struct {
	streamCalls int
	requests    []model.Request
}

func (m *maxTokensModel) Complete(context.Context, model.Request) (*model.Response, error) {
	return nil, errors.New("unexpected non-streaming call")
}

func (m *maxTokensModel) CompleteStream(_ context.Context, req model.Request, cb model.StreamHandler) error {
	m.streamCalls++
	m.requests = append(m.requests, req)
	resp := &model.Response{
		Message:    model.Message{Role: "assistant", Content: "partial"},
		StopReason: "max_tokens",
	}
	if m.streamCalls > 1 {
		resp.Message.Content = "complete"
		resp.StopReason = "done"
	}
	return cb(model.StreamResult{Final: true, Response: resp})
}

func TestCompleteWithRecoveryEscalatesMaxTokens(t *testing.T) {
	t.Parallel()

	rt := &Runtime{opts: Options{
		MaxTokensEscalation: MaxTokensEscalationConfig{
			Enabled:     true,
			MaxAttempts: 3,
			Ceiling:     65536,
		},
	}}
	mdl := &maxTokensModel{}

	resp, err := rt.completeWithRecovery(context.Background(), mdl, model.Request{MaxTokens: 8192}, nil, nil, nil, Request{})
	if err != nil {
		t.Fatalf("completeWithRecovery: %v", err)
	}
	if resp == nil || resp.Message.Content != "complete" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if len(mdl.requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(mdl.requests))
	}
	if mdl.requests[1].MaxTokens != 16384 {
		t.Fatalf("expected retry at 16384 max tokens, got %d", mdl.requests[1].MaxTokens)
	}
}

type stalledStreamModel struct {
	streamCalls   int
	completeCalls int
}

func (m *stalledStreamModel) Complete(context.Context, model.Request) (*model.Response, error) {
	m.completeCalls++
	return &model.Response{
		Message:    model.Message{Role: "assistant", Content: "fallback"},
		StopReason: "done",
	}, nil
}

func (m *stalledStreamModel) CompleteStream(ctx context.Context, _ model.Request, cb model.StreamHandler) error {
	m.streamCalls++
	if err := cb(model.StreamResult{Delta: "a"}); err != nil {
		return err
	}
	if err := cb(model.StreamResult{Delta: "b"}); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestCompleteWithRecoveryFallsBackToCompleteOnStreamStall(t *testing.T) {
	t.Parallel()

	rt := &Runtime{opts: Options{
		StreamStall: StreamStallConfig{
			Timeout:         20 * time.Millisecond,
			FallbackEnabled: true,
		},
	}}
	mdl := &stalledStreamModel{}
	ctx := withStreamEmit(context.Background(), func(context.Context, StreamEvent) {})

	resp, err := rt.completeWithRecovery(ctx, mdl, model.Request{}, nil, nil, nil, Request{})
	if err != nil {
		t.Fatalf("completeWithRecovery: %v", err)
	}
	if resp == nil || resp.Message.Content != "fallback" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if mdl.streamCalls != 1 {
		t.Fatalf("expected 1 streaming attempt, got %d", mdl.streamCalls)
	}
	if mdl.completeCalls != 1 {
		t.Fatalf("expected 1 fallback complete call, got %d", mdl.completeCalls)
	}
}
