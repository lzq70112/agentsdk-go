package api

import (
	"context"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// TestCancelSession_AbortsTurnAndRollsBackHistory 守住用户取消的核心语义：
// 取消会中止在途回合，并把 history 回滚到回合开始前（本轮用户消息一并回滚），
// 同时以 EventCanceled 事件收尾（而非泛化为 error）。
func TestCancelSession_AbortsTurnAndRollsBackHistory(t *testing.T) {
	ctx := context.Background()
	// gateAt=1：第一次模型调用即阻塞，确定性地制造「卡住的回合」。
	gm := &gatedMockModel{gateAt: 1, entered: make(chan struct{}), release: make(chan struct{})}
	sessionID := "cancel-session"

	rt, err := New(ctx, Options{
		ModelFactory: recordingMockModelFactory{model: gm},
		ProjectRoot:  t.TempDir(),
		SystemPrompt: "you are helpful",
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer rt.Close()

	events, err := rt.RunStream(ctx, Request{SessionID: sessionID, Prompt: "ping"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	select {
	case <-gm.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("model call never started; turn not in flight")
	}

	if !rt.CancelSession(sessionID) {
		t.Fatal("CancelSession should report an active run")
	}

	var canceled bool
	for evt := range events {
		if evt.Type == EventCanceled {
			canceled = true
		}
	}
	if !canceled {
		t.Fatal("expected an EventCanceled event to close the turn")
	}

	hist, ok := rt.SessionHistory(sessionID)
	if !ok {
		t.Fatal("session history missing after cancel")
	}
	if len(hist) != 1 || hist[0].Content != "hello" {
		t.Fatalf("history was not rolled back to the pre-turn snapshot: %+v", hist)
	}
}

// TestCancelSession_ReturnsFalseWhenNoActiveRun 验证没有在途回合时取消返回 false，
// 避免平台误报「已取消」。
func TestCancelSession_ReturnsFalseWhenNoActiveRun(t *testing.T) {
	ctx := context.Background()
	rt, err := New(ctx, Options{
		ModelFactory: recordingMockModelFactory{model: &recordingMockModel{}},
		ProjectRoot:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer rt.Close()

	if rt.CancelSession("nope") {
		t.Fatal("CancelSession on an idle session should return false")
	}
	if rt.CancelSession("   ") {
		t.Fatal("CancelSession with a blank session id should return false")
	}
}

// TestCancelSession_StopsBackgroundSubagents 验证取消回合时会一并停止该会话
// 在途的后台 subagent——即便此时没有在途回合，也应报告为「有任务被取消」。
func TestCancelSession_StopsBackgroundSubagents(t *testing.T) {
	ctx := context.Background()
	rt, err := New(ctx, Options{
		ModelFactory: recordingMockModelFactory{model: &recordingMockModel{}},
		ProjectRoot:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer rt.Close()

	st := newSubagentTool(rt.opts)
	st.bindRuntime(rt)

	taskCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.registerTask(&asyncTask{
		id: "sub-1", sessionID: "s1", name: "x", status: asyncTaskRunning, startedAt: time.Now(), cancel: cancel,
	})

	if !rt.CancelSession("s1") {
		t.Fatal("CancelSession should report true when it stopped a background subagent")
	}
	if taskCtx.Err() != context.Canceled {
		t.Fatal("background subagent context was not canceled")
	}
}

// TestRunStream_NormalTurnIsNotRolledBack 守住回滚的边界：只有用户主动取消才
// 回滚，正常完成的回合必须保留本轮写入的消息。
func TestRunStream_NormalTurnIsNotRolledBack(t *testing.T) {
	ctx := context.Background()
	sessionID := "normal-session"
	rt, err := New(ctx, Options{
		ModelFactory: recordingMockModelFactory{model: &recordingMockModel{}},
		ProjectRoot:  t.TempDir(),
		SystemPrompt: "you are helpful",
		HistoryLoader: func(sid string) ([]message.Message, error) {
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer rt.Close()

	events, err := rt.RunStream(ctx, Request{SessionID: sessionID, Prompt: "ping"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	for range events {
	}

	hist, ok := rt.SessionHistory(sessionID)
	if !ok {
		t.Fatal("session history missing")
	}
	foundUser := false
	for _, m := range hist {
		if m.Role == "user" && m.Content == "ping" {
			foundUser = true
		}
	}
	if !foundUser {
		t.Fatalf("a normally completed turn must keep its user message, got: %+v", hist)
	}
}
