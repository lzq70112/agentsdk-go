package api

import (
	"context"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// TestSubagentsRunningAndCancelSubagent 守住 Runtime 层后台子任务查询与单任务
// 取消的外部契约：列表只含本会话在途任务；取消仅对在途任务生效且真实触发
// 取消；终态、不存在、跨会话三种情形一律 false（会话归属校验防误伤）。
func TestSubagentsRunningAndCancelSubagent(t *testing.T) {
	ctx := context.Background()
	rt, err := New(ctx, Options{
		ModelFactory: recordingMockModelFactory{model: &recordingMockModel{}},
		ProjectRoot:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer rt.Close()

	if got := rt.SubagentsRunning("s1"); len(got) != 0 {
		t.Fatalf("expected empty running list, got %+v", got)
	}

	taskCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.subagent.registerTask(&asyncTask{
		id: "s1-sub-1", sessionID: "s1", name: "task-a", instruction: "do a",
		status: asyncTaskRunning, startedAt: time.Now(), cancel: cancel,
	})
	rt.subagent.registerTask(&asyncTask{
		id: "s2-sub-1", sessionID: "s2", name: "task-b",
		status: asyncTaskRunning, startedAt: time.Now(), cancel: func() {},
	})
	rt.subagent.registerTask(&asyncTask{
		id: "s1-sub-2", sessionID: "s1", name: "task-done",
		status: asyncTaskSuccess, startedAt: time.Now(),
	})

	running := rt.SubagentsRunning("s1")
	if len(running) != 1 {
		t.Fatalf("expected 1 running task for s1, got %+v", running)
	}
	if running[0].ID != "s1-sub-1" || running[0].Name != "task-a" || running[0].Status != asyncTaskRunning {
		t.Fatalf("unexpected task info: %+v", running[0])
	}

	if !rt.CancelSubagent("s1", "s1-sub-1") {
		t.Fatal("canceling a running task should report true")
	}
	if taskCtx.Err() != context.Canceled {
		t.Fatal("task context was not canceled")
	}

	if rt.CancelSubagent("s1", "s1-sub-2") {
		t.Fatal("canceling a finished task should report false")
	}
	if rt.CancelSubagent("s1", "nope") {
		t.Fatal("canceling a missing task should report false")
	}
	if rt.CancelSubagent("s1", "s2-sub-1") {
		t.Fatal("cross-session cancel must be rejected")
	}
}

// TestRunLoopStampsMessageTimestamps 守住历史消息时间戳约定：runLoop 写入的
// user/assistant 消息与 Inject 写入的注入消息都带 api.timestamp（UnixMilli），
// 上层据此渲染时间分隔线；无此键的旧历史由上层降级，SDK 不回填。
func TestRunLoopStampsMessageTimestamps(t *testing.T) {
	ctx := context.Background()
	rt, err := New(ctx, Options{
		ModelFactory: recordingMockModelFactory{model: &recordingMockModel{}},
		ProjectRoot:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer rt.Close()

	sessionID := "ts-session"
	events, err := rt.RunStream(ctx, Request{SessionID: sessionID, Prompt: "hello"})
	if err != nil {
		t.Fatalf("run stream: %v", err)
	}
	for range events { // 消费事件流：回合收尾后历史才可见
	}

	hist, ok := rt.SessionHistory(sessionID)
	if !ok || len(hist) == 0 {
		t.Fatalf("expected non-empty history, got ok=%v len=%d", ok, len(hist))
	}
	now := time.Now().UnixMilli()
	sawUser, sawAssistant := false, false
	for _, m := range hist {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		v, exists := m.Metadata[metadataTimestamp]
		if !exists {
			t.Fatalf("%s message missing %s: %+v", m.Role, metadataTimestamp, m.Metadata)
		}
		ts, isInt64 := v.(int64)
		if !isInt64 || ts <= 0 || ts > now+1000 {
			t.Fatalf("%s message has invalid timestamp %v (now=%d)", m.Role, v, now)
		}
		if m.Role == "user" {
			sawUser = true
		} else {
			sawAssistant = true
		}
	}
	if !sawUser || !sawAssistant {
		t.Fatalf("expected both user and assistant messages, sawUser=%v sawAssistant=%v", sawUser, sawAssistant)
	}

	if err := rt.Inject(sessionID, Injection{
		Kind:    InjectionKindReminder,
		Message: message.Message{Role: "user", Content: "injected reminder"},
	}); err != nil {
		t.Fatalf("inject: %v", err)
	}
	hist, _ = rt.SessionHistory(sessionID)
	last := hist[len(hist)-1]
	if v, exists := last.Metadata[metadataTimestamp]; !exists {
		t.Fatalf("injected message missing %s", metadataTimestamp)
	} else if _, isInt64 := v.(int64); !isInt64 {
		t.Fatalf("injected message timestamp is not int64: %T", v)
	}
}
