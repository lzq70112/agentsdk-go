package api

import (
	"context"
	"testing"

	hooks "github.com/lzq70112/agentsdk-go/pkg/hooks"
)

// captureSubagentEvents 构造一个经 middleware 捕获全部事件的 hooks 执行器。
func captureSubagentEvents(t *testing.T) (*hooks.Executor, func() []hooks.EventType) {
	t.Helper()
	var types []hooks.EventType
	exec := hooks.NewExecutor(hooks.WithMiddleware(func(next hooks.MiddlewareHandler) hooks.MiddlewareHandler {
		return func(ctx context.Context, evt hooks.Event) error {
			types = append(types, evt.Type)
			return next(ctx, evt)
		}
	}))
	return exec, func() []hooks.EventType { return types }
}

// TestSubagentEventForwardWhitelist 固化子 agent → 父会话的事件转发白名单：
// 登记类型（SubagentStart/SubagentComplete）转发，未登记类型 default 拒绝。
// 防的事故：新增 hooks 事件类型后被子 agent 链路无差别转发，事件洪水污染
// 父会话的 UI 与会话记录。
func TestSubagentEventForwardWhitelist(t *testing.T) {
	t.Parallel()
	exec, captured := captureSubagentEvents(t)

	forwardSubagentEvent(exec, hooks.Event{Type: hooks.SubagentStart, Payload: hooks.SubagentStartPayload{Name: "w"}})
	forwardSubagentEvent(exec, hooks.Event{Type: hooks.SubagentComplete, Payload: hooks.SubagentCompletePayload{Name: "w"}})
	forwardSubagentEvent(exec, hooks.Event{Type: hooks.PreToolUse, Payload: map[string]any{"tool": "bash"}})
	forwardSubagentEvent(exec, hooks.Event{Type: hooks.SessionStart})
	forwardSubagentEvent(exec, hooks.Event{Type: hooks.Stop})

	got := captured()
	if len(got) != 2 {
		t.Fatalf("whitelist must forward exactly 2 events, got %v", got)
	}
	if got[0] != hooks.SubagentStart || got[1] != hooks.SubagentComplete {
		t.Fatalf("forwarded events = %v, want [SubagentStart SubagentComplete]", got)
	}
}

// TestSubagentEventForwardNilExecutor 固化边界：hooks 未配置时转发静默返回，
// 不 panic（子 agent 派发不因观测链路缺位而失败）。
func TestSubagentEventForwardNilExecutor(t *testing.T) {
	t.Parallel()
	forwardSubagentEvent(nil, hooks.Event{Type: hooks.SubagentComplete})
}
