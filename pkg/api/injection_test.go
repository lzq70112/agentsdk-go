package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

func newInjectionTestRuntime(t *testing.T, sessionID string) (*Runtime, *message.History) {
	t.Helper()
	rt := &Runtime{histories: newHistoryStore(4, nil)}
	history, err := rt.histories.Get(sessionID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	return rt, history
}

// TestInjectRejectsUnknownKind 固化 Kind 为显式枚举：未登记类型在入口被
// 拒绝，防止调用方拼错 kind 字符串后消息绕过路由语义直接落盘。
func TestInjectRejectsUnknownKind(t *testing.T) {
	t.Parallel()
	rt, _ := newInjectionTestRuntime(t, "sess")
	err := rt.Inject("sess", Injection{
		Kind:    InjectionKind("bogus"),
		Policy:  BarrierDefer,
		Message: message.Message{Role: "user", Content: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown injection kind") {
		t.Fatalf("unknown kind must be rejected, err = %v", err)
	}
}

// TestInjectIdleSessionAppendsImmediately 固化空闲会话直通：无在途回合时
// 注入立即写入 history（与迁移前的直接 Append 行为一致）。
func TestInjectIdleSessionAppendsImmediately(t *testing.T) {
	t.Parallel()
	rt, history := newInjectionTestRuntime(t, "sess")
	err := rt.Inject("sess", Injection{
		Kind:    InjectionKindExternal,
		Policy:  BarrierDefer,
		Message: message.Message{Role: "user", Content: "external-fact"},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	msgs := history.All()
	if len(msgs) != 1 || msgs[0].Content != "external-fact" {
		t.Fatalf("injected message missing, msgs = %+v", msgs)
	}
}

// TestInjectDeferQueuesUntilRoundEnd 固化 DEFER 语义：会话忙时排队不落盘，
// 回合结束（endActiveRun）后按序交付。
func TestInjectDeferQueuesUntilRoundEnd(t *testing.T) {
	t.Parallel()
	rt, history := newInjectionTestRuntime(t, "sess")
	run := rt.beginActiveRun("sess", func() {}, nil)

	for _, content := range []string{"queued-1", "queued-2"} {
		if err := rt.Inject("sess", Injection{
			Kind:    InjectionKindReminder,
			Policy:  BarrierDefer,
			Message: message.Message{Role: "user", Content: content},
		}); err != nil {
			t.Fatalf("Inject defer: %v", err)
		}
	}
	if got := history.Len(); got != 0 {
		t.Fatalf("deferred injections must not land while busy, len = %d", got)
	}

	rt.endActiveRun(run)
	msgs := history.All()
	if len(msgs) != 2 || msgs[0].Content != "queued-1" || msgs[1].Content != "queued-2" {
		t.Fatalf("deferred injections must land in order after round end, msgs = %+v", msgs)
	}
}

// TestInjectRejectWhenBusy 固化 REJECT 语义：会话忙时明确拒绝并回报
// ErrSessionBusy，消息不落盘。
func TestInjectRejectWhenBusy(t *testing.T) {
	t.Parallel()
	rt, history := newInjectionTestRuntime(t, "sess")
	run := rt.beginActiveRun("sess", func() {}, nil)

	err := rt.Inject("sess", Injection{
		Kind:    InjectionKindScheduled,
		Policy:  BarrierReject,
		Message: message.Message{Role: "user", Content: "rejected"},
	})
	rt.endActiveRun(run)

	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("busy session with BarrierReject must return ErrSessionBusy, err = %v", err)
	}
	if history.Len() != 0 {
		t.Fatal("rejected injection must not land in history")
	}
}

// TestInjectDropWhenBusy 固化 DROP 语义：可合并的提醒类注入在会话忙时静默
// 丢弃，不报错、不落盘、不排队。
func TestInjectDropWhenBusy(t *testing.T) {
	t.Parallel()
	rt, history := newInjectionTestRuntime(t, "sess")
	run := rt.beginActiveRun("sess", func() {}, nil)

	err := rt.Inject("sess", Injection{
		Kind:    InjectionKindReminder,
		Policy:  BarrierDrop,
		Message: message.Message{Role: "user", Content: "droppable"},
	})
	rt.endActiveRun(run)

	if err != nil {
		t.Fatalf("BarrierDrop must be silent, err = %v", err)
	}
	if history.Len() != 0 {
		t.Fatal("dropped injection must not land in history")
	}
}

// TestInjectSyntheticMarkerAutoAttached 固化联动契约：非 user kind 的注入
// 自动携带 synthetic 标记（压缩豁免凭证），缺失时补齐、已有时不覆盖。
func TestInjectSyntheticMarkerAutoAttached(t *testing.T) {
	t.Parallel()
	rt, history := newInjectionTestRuntime(t, "sess")

	if err := rt.Inject("sess", Injection{
		Kind:    InjectionKindReminder,
		Policy:  BarrierDefer,
		Message: message.Message{Role: "user", Content: "no-metadata"},
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	msgs := history.All()
	if !isSyntheticMessage(msgs[0]) {
		t.Fatal("non-user injection must carry synthetic marker for compaction exemption")
	}
	if msgs[0].Metadata[metadataSyntheticKey] != string(InjectionKindReminder) {
		t.Fatalf("synthetic type should record the injection kind, got %v", msgs[0].Metadata[metadataSyntheticKey])
	}

	custom := message.Message{
		Role:     "user",
		Content:  "with-metadata",
		Metadata: map[string]any{metadataSynthetic: true, metadataSyntheticKey: "subagent_result"},
	}
	if err := rt.Inject("sess", Injection{Kind: InjectionKindSubagentResult, Policy: BarrierDefer, Message: custom}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	msgs = history.All()
	if msgs[1].Metadata[metadataSyntheticKey] != "subagent_result" {
		t.Fatalf("existing synthetic type must not be overwritten, got %v", msgs[1].Metadata[metadataSyntheticKey])
	}
}

// TestInjectUserKindNoSyntheticMarker 固化 USER kind 语义：用户输入类注入
// 不带 synthetic 标记（它是真实回合输入，不是环境事实）。
func TestInjectUserKindNoSyntheticMarker(t *testing.T) {
	t.Parallel()
	rt, history := newInjectionTestRuntime(t, "sess")
	if err := rt.Inject("sess", Injection{
		Kind:    InjectionKindUser,
		Policy:  BarrierDefer,
		Message: message.Message{Role: "user", Content: "user-turn"},
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	msgs := history.All()
	if isSyntheticMessage(msgs[0]) {
		t.Fatal("USER-kind injection must not carry synthetic marker")
	}
}

// TestInjectUnknownSessionRejected 固化边界：未加载会话的注入被拒绝，
// 不能凭空创建会话历史。
func TestInjectUnknownSessionRejected(t *testing.T) {
	t.Parallel()
	rt, _ := newInjectionTestRuntime(t, "sess")
	err := rt.Inject("ghost", Injection{
		Kind:    InjectionKindExternal,
		Policy:  BarrierDefer,
		Message: message.Message{Role: "user", Content: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("unknown session must be rejected, err = %v", err)
	}
}
