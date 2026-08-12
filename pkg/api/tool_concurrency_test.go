package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stellarlinkco/agentsdk-go/pkg/message"
	"github.com/stellarlinkco/agentsdk-go/pkg/middleware"
	"github.com/stellarlinkco/agentsdk-go/pkg/model"
	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
)

type timedTool struct {
	name  string
	delay time.Duration
	meta  tool.Metadata

	mu      sync.Mutex
	started []time.Time
	ended   []time.Time
}

func (t *timedTool) Name() string             { return t.name }
func (t *timedTool) Description() string      { return t.name }
func (t *timedTool) Schema() *tool.JSONSchema { return &tool.JSONSchema{Type: "object"} }
func (t *timedTool) Metadata() tool.Metadata  { return t.meta }
func (t *timedTool) Execute(context.Context, map[string]any) (*tool.ToolResult, error) {
	t.mu.Lock()
	t.started = append(t.started, time.Now())
	t.mu.Unlock()
	time.Sleep(t.delay)
	t.mu.Lock()
	t.ended = append(t.ended, time.Now())
	t.mu.Unlock()
	return &tool.ToolResult{Success: true, Output: t.name}, nil
}

func (t *timedTool) firstStart() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.started) == 0 {
		return time.Time{}
	}
	return t.started[0]
}

func (t *timedTool) lastEnd() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.ended) == 0 {
		return time.Time{}
	}
	return t.ended[len(t.ended)-1]
}

type countedCancelingConcurrentTool struct {
	name  string
	delay time.Duration
	calls int
	mu    sync.Mutex
}

func (t *countedCancelingConcurrentTool) Name() string        { return t.name }
func (t *countedCancelingConcurrentTool) Description() string { return t.name }
func (t *countedCancelingConcurrentTool) Schema() *tool.JSONSchema {
	return &tool.JSONSchema{Type: "object"}
}
func (t *countedCancelingConcurrentTool) Metadata() tool.Metadata {
	return tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true}
}
func (t *countedCancelingConcurrentTool) Execute(context.Context, map[string]any) (*tool.ToolResult, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if t.delay > 0 {
		time.Sleep(t.delay)
	}
	return nil, context.Canceled
}
func (t *countedCancelingConcurrentTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func TestRunLoopExecutesReadOnlyConcurrencySafeToolsInParallel(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	read1 := &timedTool{name: "read1", delay: 120 * time.Millisecond, meta: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true}}
	read2 := &timedTool{name: "read2", delay: 120 * time.Millisecond, meta: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true}}
	read3 := &timedTool{name: "read3", delay: 120 * time.Millisecond, meta: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true}}
	write := &timedTool{name: "write", delay: 120 * time.Millisecond}
	for _, impl := range []tool.Tool{read1, read2, read3, write} {
		if err := reg.Register(impl); err != nil {
			t.Fatalf("register %s: %v", impl.Name(), err)
		}
	}

	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: "r1", Name: "read1", Arguments: map[string]any{}},
			{ID: "r2", Name: "read2", Arguments: map[string]any{}},
			{ID: "r3", Name: "read3", Arguments: map[string]any{}},
			{ID: "w1", Name: "write", Arguments: map[string]any{}},
		}}},
		{Message: model.Message{Role: "assistant", Content: "done"}},
	}}

	rt := &Runtime{
		opts:     Options{ToolConcurrency: 3},
		registry: reg,
		executor: tool.NewExecutor(reg, nil),
	}
	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "hi",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s", RequestID: "r"},
	}

	start := time.Now()
	resp, err := rt.runLoop(prep, mdl, &runtimeHookAdapter{}, &runtimeToolExecutor{executor: rt.executor, hooks: &runtimeHookAdapter{}, history: prep.history, host: "localhost"}, middleware.NewChain(nil), false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if resp == nil || resp.Message.Content != "done" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if elapsed >= 360*time.Millisecond {
		t.Fatalf("expected concurrent execution to finish faster, elapsed=%s", elapsed)
	}

	latestReadEnd := read1.lastEnd()
	for _, tool := range []*timedTool{read2, read3} {
		if end := tool.lastEnd(); end.After(latestReadEnd) {
			latestReadEnd = end
		}
	}
	if write.firstStart().Before(latestReadEnd) {
		t.Fatalf("write tool started before read-only batch completed: write=%s latestReadEnd=%s", write.firstStart(), latestReadEnd)
	}
}

func TestRunLoopPreservesSerialBoundariesBetweenConcurrentSegments(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	read := &timedTool{name: "read", delay: 80 * time.Millisecond, meta: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true}}
	bash := &timedTool{name: "bash", delay: 80 * time.Millisecond}
	grep := &timedTool{name: "grep", delay: 80 * time.Millisecond, meta: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true}}
	for _, impl := range []tool.Tool{read, bash, grep} {
		if err := reg.Register(impl); err != nil {
			t.Fatalf("register %s: %v", impl.Name(), err)
		}
	}

	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: "r1", Name: "read", Arguments: map[string]any{}},
			{ID: "b1", Name: "bash", Arguments: map[string]any{}},
			{ID: "g1", Name: "grep", Arguments: map[string]any{}},
		}}},
		{Message: model.Message{Role: "assistant", Content: "done"}},
	}}

	rt := &Runtime{
		opts:     Options{ToolConcurrency: 2},
		registry: reg,
		executor: tool.NewExecutor(reg, nil),
	}
	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "hi",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s", RequestID: "r"},
	}

	resp, err := rt.runLoop(prep, mdl, &runtimeHookAdapter{}, &runtimeToolExecutor{executor: rt.executor, hooks: &runtimeHookAdapter{}, history: prep.history, host: "localhost"}, middleware.NewChain(nil), false)
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if resp == nil || resp.Message.Content != "done" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	if bash.firstStart().Before(read.lastEnd()) {
		t.Fatalf("bash started before preceding read segment completed: bash=%s readEnd=%s", bash.firstStart(), read.lastEnd())
	}
	if grep.firstStart().Before(bash.lastEnd()) {
		t.Fatalf("grep crossed serial boundary: grep=%s bashEnd=%s", grep.firstStart(), bash.lastEnd())
	}
}

func TestOptionsDefaultsLeaveToolConcurrencyUnlimited(t *testing.T) {
	t.Parallel()

	opts := Options{ProjectRoot: t.TempDir()}.withDefaults()

	if opts.ToolConcurrency != 0 {
		t.Fatalf("default ToolConcurrency = %d, want 0 for unlimited", opts.ToolConcurrency)
	}
}

func TestRunLoopDoesNotRecordEmptyResultForConcurrentToolSkippedByCancellation(t *testing.T) {
	for attempt := 0; attempt < 200; attempt++ {
		reg := tool.NewRegistry()
		cancel1 := &countedCancelingConcurrentTool{name: "cancel1", delay: time.Millisecond}
		cancel2 := &countedCancelingConcurrentTool{name: "cancel2", delay: time.Millisecond}
		for _, impl := range []tool.Tool{cancel1, cancel2} {
			if err := reg.Register(impl); err != nil {
				t.Fatalf("register %s: %v", impl.Name(), err)
			}
		}

		mdl := &stubModel{responses: []*model.Response{
			{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{
				{ID: "c1", Name: "cancel1", Arguments: map[string]any{}},
				{ID: "c2", Name: "cancel2", Arguments: map[string]any{}},
			}}},
		}}
		hist := message.NewHistory()
		rt := &Runtime{
			opts:     Options{ToolConcurrency: 1},
			registry: reg,
			executor: tool.NewExecutor(reg, nil),
		}
		prep := preparedRun{
			ctx:        context.Background(),
			prompt:     "hi",
			history:    hist,
			normalized: Request{SessionID: "s", RequestID: "r"},
		}

		_, err := rt.runLoop(prep, mdl, &runtimeHookAdapter{}, &runtimeToolExecutor{executor: rt.executor, hooks: &runtimeHookAdapter{}, history: hist, host: "localhost"}, middleware.NewChain(nil), false)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runLoop err = %v, want context.Canceled", err)
		}
		if got := cancel1.callCount() + cancel2.callCount(); got != 1 {
			t.Fatalf("attempt %d: started %d tools after segment cancellation; cancel1=%d cancel2=%d", attempt, got, cancel1.callCount(), cancel2.callCount())
		}

		var toolResults []message.ToolCall
		for _, msg := range hist.All() {
			if msg.Role == "tool" {
				toolResults = append(toolResults, msg.ToolCalls...)
			}
		}
		if len(toolResults) != 1 {
			t.Fatalf("attempt %d: tool result count = %d, want 1: %+v", attempt, len(toolResults), toolResults)
		}
		if toolResults[0].Result == "" {
			t.Fatalf("attempt %d: recorded empty tool result for cancellation: %+v", attempt, toolResults[0])
		}
	}
}

func TestExecuteToolCallsDoesNotStartConcurrentSegmentWhenContextAlreadyCanceled(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	read1 := &countedCancelingConcurrentTool{name: "read1"}
	read2 := &countedCancelingConcurrentTool{name: "read2"}
	for _, impl := range []tool.Tool{read1, read2} {
		if err := reg.Register(impl); err != nil {
			t.Fatalf("register %s: %v", impl.Name(), err)
		}
	}
	hist := message.NewHistory()
	rt := &Runtime{
		registry: reg,
		executor: tool.NewExecutor(reg, nil),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := rt.executeToolCalls(
		ctx,
		[]model.ToolCall{
			{ID: "r1", Name: "read1", Arguments: map[string]any{}},
			{ID: "r2", Name: "read2", Arguments: map[string]any{}},
		},
		&runtimeToolExecutor{executor: rt.executor, hooks: &runtimeHookAdapter{}, history: hist, host: "localhost"},
		middleware.NewChain(nil),
		nil,
		nil,
		nil,
		Request{SessionID: "s", RequestID: "r"},
	)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeToolCalls err = %v, want context.Canceled", err)
	}
	if got := read1.callCount() + read2.callCount(); got != 0 {
		t.Fatalf("started %d tools after context cancellation; read1=%d read2=%d", got, read1.callCount(), read2.callCount())
	}
	if hist.Len() != 0 {
		t.Fatalf("history len = %d, want 0", hist.Len())
	}
}

func TestExecuteToolCallsReturnsErrorForNilExecutorWithConcurrentSegment(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	if err := reg.Register(&timedTool{name: "read", meta: tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true}}); err != nil {
		t.Fatalf("register read: %v", err)
	}
	rt := &Runtime{registry: reg}

	err := rt.executeToolCalls(
		context.Background(),
		[]model.ToolCall{{ID: "r1", Name: "read", Arguments: map[string]any{}}},
		nil,
		middleware.NewChain(nil),
		nil,
		nil,
		nil,
		Request{SessionID: "s", RequestID: "r"},
	)

	if err == nil {
		t.Fatalf("expected nil executor error")
	}
}
