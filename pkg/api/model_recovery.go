package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

var ErrStreamStall = errors.New("api: streaming stall detected")

const defaultMaxTokensEscalationBase = 8192

type streamOutcome struct {
	resp *model.Response
	err  error
}

func (rt *Runtime) completeWithRecovery(ctx context.Context, mdl model.Model, req model.Request, hist *message.History, tracer Tracer, agentSpan SpanContext, normalized Request) (*model.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if mdl == nil {
		return nil, errors.New("api: model is nil")
	}

	escalation := rt.opts.MaxTokensEscalation.withDefaults()
	reactiveRetried := false
	escalationAttempts := 0

	for {
		resp, err := rt.completeOnce(ctx, mdl, req, tracer, agentSpan, normalized)
		if err != nil {
			if !reactiveRetried && isPromptTooLongError(err) {
				reactiveRetried = true
				if comp := rt.reactiveCompactor(); comp != nil && hist != nil {
					if _, compactErr := comp.reactiveCompact(ctx, hist, mdl); compactErr != nil {
						return nil, compactErr
					}
					req.Messages = convertMessages(hist.All())
				}
				continue
			}
			return nil, err
		}
		if !shouldEscalateMaxTokens(resp, escalation, escalationAttempts) {
			return resp, nil
		}

		nextMaxTokens := nextEscalatedMaxTokens(req.MaxTokens, escalation.Ceiling)
		if nextMaxTokens <= 0 || nextMaxTokens == req.MaxTokens {
			return resp, nil
		}
		req.MaxTokens = nextMaxTokens
		escalationAttempts++
	}
}

func (rt *Runtime) completeOnce(ctx context.Context, mdl model.Model, req model.Request, tracer Tracer, agentSpan SpanContext, normalized Request) (*model.Response, error) {
	modelSpan := SpanContext(nil)
	if tracer != nil {
		modelSpan = tracer.StartModelSpan(agentSpan, strings.TrimSpace(req.Model))
	}
	resp, err := completeViaStream(ctx, mdl, req, rt.opts.StreamStall.withDefaults(), streamEmitFromContext(ctx) != nil)
	if tracer != nil {
		attrs := map[string]any{
			"session_id": strings.TrimSpace(normalized.SessionID),
			"request_id": strings.TrimSpace(normalized.RequestID),
		}
		if resp != nil {
			attrs["stop_reason"] = strings.TrimSpace(resp.StopReason)
			attrs["input_tokens"] = resp.Usage.InputTokens
			attrs["output_tokens"] = resp.Usage.OutputTokens
			attrs["total_tokens"] = resp.Usage.TotalTokens
		}
		tracer.EndSpan(modelSpan, attrs, err)
	}
	return resp, err
}

func completeViaStream(ctx context.Context, mdl model.Model, req model.Request, cfg StreamStallConfig, detectStall bool) (*model.Response, error) {
	if !detectStall {
		return collectStreamResponse(ctx, mdl, req)
	}
	cfg = cfg.withDefaults()
	if !cfg.FallbackEnabled {
		return collectStreamWithTimeout(ctx, mdl, req, cfg.Timeout)
	}

	resp, err := collectStreamWithTimeout(ctx, mdl, req, cfg.Timeout)
	if !errors.Is(err, ErrStreamStall) {
		return resp, err
	}
	return mdl.Complete(ctx, req)
}

func collectStreamResponse(ctx context.Context, mdl model.Model, req model.Request) (*model.Response, error) {
	outcome := runStreamCollection(ctx, mdl, req, nil)
	if outcome.err != nil {
		return nil, outcome.err
	}
	return outcome.resp, nil
}

func collectStreamWithTimeout(ctx context.Context, mdl model.Model, req model.Request, timeout time.Duration) (*model.Response, error) {
	if timeout <= 0 {
		timeout = defaultStreamStallTimeout
	}

	progress := make(chan struct{}, 1)
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan streamOutcome, 1)
	go func() {
		done <- runStreamCollection(streamCtx, mdl, req, progress)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			cancel()
			outcome := <-done
			if outcome.err != nil && !errors.Is(outcome.err, context.Canceled) {
				return nil, outcome.err
			}
			return nil, ctx.Err()
		case outcome := <-done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return outcome.resp, outcome.err
		case <-progress:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			cancel()
			<-done
			return nil, ErrStreamStall
		}
	}
}

func runStreamCollection(ctx context.Context, mdl model.Model, req model.Request, progress chan<- struct{}) streamOutcome {
	var final *model.Response
	err := mdl.CompleteStream(ctx, req, func(sr model.StreamResult) error {
		if progress != nil {
			select {
			case progress <- struct{}{}:
			default:
			}
		}
		// 透传底层真 SSE delta：有 streamEmit 通道时，把 delta 作为 text_delta 事件直推，
		// 并置位 forwarded 标记，供 AfterAgent 跳过假重放。
		if sr.Delta != "" {
			emit := streamEmitFromContext(ctx)
			state, _ := ctx.Value(streamForwardStateCtxKey).(*streamForwardState)
			if emit != nil && state != nil {
				// 首个 delta 时发出 content_block_start（idx=0）。CAS 保证升级循环下
				// 多次 CompleteStream 只 start 一次。
				idx := 0
				if state.started.CompareAndSwap(false, true) {
					emit(ctx, StreamEvent{
						Type:         EventContentBlockStart,
						Index:        &idx,
						ContentBlock: &ContentBlock{Type: "text"},
					})
				}
				emit(ctx, StreamEvent{
					Type:  EventContentBlockDelta,
					Index: &idx,
					Delta: &Delta{Type: "text_delta", Text: sr.Delta},
				})
				state.forwarded.Store(true)
			}
		}
		if sr.Final && sr.Response != nil {
			final = sr.Response
		}
		return nil
	})
	if err != nil {
		return streamOutcome{err: err}
	}
	if final == nil {
		return streamOutcome{err: errors.New("api: model returned no final response")}
	}
	return streamOutcome{resp: final}
}

func (rt *Runtime) reactiveCompactor() *compactor {
	if rt != nil && rt.compactor != nil {
		return rt.compactor
	}
	cfg := CompactConfig{Enabled: true}
	if rt != nil {
		cfg = rt.opts.AutoCompact
	}
	cfg.Enabled = true
	return newCompactor(cfg, defaultTokenLimit(rt))
}

func defaultTokenLimit(rt *Runtime) int {
	if rt == nil || rt.opts.TokenLimit <= 0 {
		return defaultClaudeContextLimit
	}
	return rt.opts.TokenLimit
}

func shouldEscalateMaxTokens(resp *model.Response, cfg MaxTokensEscalationConfig, attempts int) bool {
	if resp == nil || !cfg.Enabled || attempts >= cfg.MaxAttempts {
		return false
	}
	return strings.TrimSpace(resp.StopReason) == "max_tokens"
}

func nextEscalatedMaxTokens(current, ceiling int) int {
	if ceiling <= 0 {
		ceiling = defaultEscalationCeiling
	}
	if current <= 0 {
		if defaultMaxTokensEscalationBase > ceiling {
			return ceiling
		}
		return defaultMaxTokensEscalationBase
	}
	next := current * 2
	if next < defaultMaxTokensEscalationBase {
		next = defaultMaxTokensEscalationBase
	}
	if next > ceiling {
		next = ceiling
	}
	if next <= current {
		return current
	}
	return next
}

func (s streamOutcome) String() string {
	if s.err != nil {
		return s.err.Error()
	}
	if s.resp == nil {
		return "nil"
	}
	return fmt.Sprintf("stop=%s", s.resp.StopReason)
}
