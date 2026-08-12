package api

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"

	"github.com/stellarlinkco/agentsdk-go/pkg/middleware"
	"github.com/stellarlinkco/agentsdk-go/pkg/model"
	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
)

type indexedToolCall struct {
	index int
	call  model.ToolCall
}

type toolCallSegment struct {
	concurrent bool
	calls      []indexedToolCall
}

type toolExecution struct {
	call      model.ToolCall
	result    *tool.CallResult
	err       error
	beforeErr error
	afterErr  error
	started   bool
}

func isConcurrentTool(registry *tool.Registry, call model.ToolCall) bool {
	if registry == nil {
		return false
	}
	impl, err := registry.Get(call.Name)
	if err != nil {
		return false
	}
	meta := tool.MetadataOf(impl)
	return meta.IsReadOnly && meta.IsConcurrencySafe
}

func partitionToolCallSegments(registry *tool.Registry, calls []model.ToolCall) []toolCallSegment {
	var segments []toolCallSegment
	var concurrent []indexedToolCall

	flushConcurrent := func() {
		if len(concurrent) == 0 {
			return
		}
		segmentCalls := append([]indexedToolCall(nil), concurrent...)
		segments = append(segments, toolCallSegment{concurrent: true, calls: segmentCalls})
		concurrent = concurrent[:0]
	}

	for i, call := range calls {
		item := indexedToolCall{index: i, call: call}
		if isConcurrentTool(registry, call) {
			concurrent = append(concurrent, item)
			continue
		}
		flushConcurrent()
		segments = append(segments, toolCallSegment{calls: []indexedToolCall{item}})
	}
	flushConcurrent()
	return segments
}

func cloneMiddlewareState(base *middleware.State) *middleware.State {
	if base == nil {
		return &middleware.State{Values: map[string]any{}}
	}
	cloned := &middleware.State{
		Iteration:   base.Iteration,
		Agent:       base.Agent,
		ModelInput:  base.ModelInput,
		ModelOutput: base.ModelOutput,
		ToolCall:    base.ToolCall,
		ToolResult:  base.ToolResult,
	}
	if len(base.Values) > 0 {
		cloned.Values = maps.Clone(base.Values)
	} else {
		cloned.Values = map[string]any{}
	}
	return cloned
}

func (rt *Runtime) executeSingleToolCall(ctx context.Context, call model.ToolCall, tools *runtimeToolExecutor, chain *middleware.Chain, baseState *middleware.State, tracer Tracer, agentSpan SpanContext, sessionID, requestID string) toolExecution {
	exec := toolExecution{call: call, started: true}
	state := cloneMiddlewareState(baseState)
	state.ToolCall = call
	if err := chain.Execute(ctx, middleware.StageBeforeTool, state); err != nil {
		exec.beforeErr = err
	}
	if tools == nil {
		exec.err = errors.New("api: tool executor is nil")
		return exec
	}
	toolSpan := SpanContext(nil)
	if tracer != nil {
		toolSpan = tracer.StartToolSpan(agentSpan, strings.TrimSpace(call.Name))
	}
	res, err := tools.Execute(ctx, call)
	if tracer != nil {
		tracer.EndSpan(toolSpan, map[string]any{
			"session_id":  strings.TrimSpace(sessionID),
			"request_id":  strings.TrimSpace(requestID),
			"tool_use_id": strings.TrimSpace(call.ID),
			"tool_name":   strings.TrimSpace(call.Name),
		}, err)
	}
	exec.result = res
	exec.err = err
	state.ToolResult = res
	if afterErr := chain.Execute(ctx, middleware.StageAfterTool, state); afterErr != nil {
		exec.afterErr = afterErr
	}
	return exec
}

func (rt *Runtime) executeToolCalls(ctx context.Context, calls []model.ToolCall, tools *runtimeToolExecutor, chain *middleware.Chain, baseState *middleware.State, tracer Tracer, agentSpan SpanContext, req Request) error {
	if len(calls) > 0 && tools == nil {
		return errors.New("api: tool executor is nil")
	}
	segments := partitionToolCallSegments(rt.registry, calls)

	var firstMiddlewareErr error
	recordMiddlewareErr := func(exec toolExecution) {
		if firstMiddlewareErr == nil && exec.beforeErr != nil {
			firstMiddlewareErr = exec.beforeErr
		}
		if firstMiddlewareErr == nil && exec.afterErr != nil {
			firstMiddlewareErr = exec.afterErr
		}
	}

	for _, segment := range segments {
		if len(segment.calls) == 0 {
			continue
		}
		if !segment.concurrent {
			exec := rt.executeSingleToolCall(ctx, segment.calls[0].call, tools, chain, baseState, tracer, agentSpan, req.SessionID, req.RequestID)
			recordMiddlewareErr(exec)
			if exec.err != nil && (errors.Is(exec.err, context.Canceled) || errors.Is(exec.err, context.DeadlineExceeded)) {
				return exec.err
			}
			continue
		}

		results := make([]toolExecution, len(segment.calls))
		groupCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		limit := rt.opts.ToolConcurrency
		var sem chan struct{}
		if limit > 0 {
			sem = make(chan struct{}, limit)
		}
		var wg sync.WaitGroup
		errCh := make(chan error, len(segment.calls))
		concurrentExec := tools.withoutHistory()
		for i, item := range segment.calls {
			i := i
			item := item
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := groupCtx.Err(); err != nil {
					results[i] = toolExecution{call: item.call, err: err}
					errCh <- err
					return
				}
				if sem != nil {
					select {
					case sem <- struct{}{}:
					case <-groupCtx.Done():
						results[i] = toolExecution{call: item.call, err: groupCtx.Err()}
						errCh <- groupCtx.Err()
						return
					}
					defer func() { <-sem }()
				}
				if err := groupCtx.Err(); err != nil {
					results[i] = toolExecution{call: item.call, err: err}
					errCh <- err
					return
				}
				results[i] = rt.executeSingleToolCall(groupCtx, item.call, concurrentExec, chain, baseState, tracer, agentSpan, req.SessionID, req.RequestID)
				if results[i].err != nil && (errors.Is(results[i].err, context.Canceled) || errors.Is(results[i].err, context.DeadlineExceeded)) {
					errCh <- results[i].err
					cancel()
					return
				}
			}()
		}
		wg.Wait()
		close(errCh)
		var groupErr error
		for err := range errCh {
			if err != nil {
				groupErr = err
				break
			}
		}
		for i, exec := range results {
			recordMiddlewareErr(exec)
			if !exec.started && (errors.Is(exec.err, context.Canceled) || errors.Is(exec.err, context.DeadlineExceeded)) {
				continue
			}
			tools.appendCallResult(segment.calls[i].call, exec.result, exec.err)
		}
		if groupErr != nil {
			return groupErr
		}
	}

	return firstMiddlewareErr
}
