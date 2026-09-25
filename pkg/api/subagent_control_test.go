package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// newControlTestTool 构造一个只用于任务注册表测试的 subagentTool。
func newControlTestTool(t *testing.T) *subagentTool {
	t.Helper()
	return newSubagentTool(Options{ProjectRoot: t.TempDir()})
}

// TestSubagentStatusTool_ListsRunningTasksForSession 守住状态查询的作用域语义：
// 只报告当前会话的在途任务，既不泄露已完成任务，也不跨会话串台。
func TestSubagentStatusTool_ListsRunningTasksForSession(t *testing.T) {
	st := newControlTestTool(t)
	now := time.Now()
	st.tasks["sub-a"] = &asyncTask{id: "sub-a", sessionID: "s1", name: "alpha", status: asyncTaskRunning, startedAt: now}
	st.tasks["sub-b"] = &asyncTask{id: "sub-b", sessionID: "s1", name: "beta", status: asyncTaskRunning, startedAt: now.Add(time.Second)}
	st.tasks["sub-c"] = &asyncTask{id: "sub-c", sessionID: "s1", name: "done", status: asyncTaskSuccess, startedAt: now}
	st.tasks["sub-d"] = &asyncTask{id: "sub-d", sessionID: "s2", name: "other", status: asyncTaskRunning, startedAt: now}

	statusTool := &subagentStatusTool{owner: st}
	res, err := statusTool.Execute(WithToolSessionID(context.Background(), "s1"), map[string]any{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Output)
	}
	for _, want := range []string{"sub-a", "sub-b", asyncTaskRunning} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("running list missing %q: %s", want, res.Output)
		}
	}
	for _, unwanted := range []string{"sub-c", "sub-d"} {
		if strings.Contains(res.Output, unwanted) {
			t.Fatalf("list leaked %q (completed or foreign session): %s", unwanted, res.Output)
		}
	}
}

// TestSubagentStatusTool_DetailByID 验证按标识查询返回单任务详情，且已结束任务
// 仍可查到最终状态与输出。
func TestSubagentStatusTool_DetailByID(t *testing.T) {
	st := newControlTestTool(t)
	st.tasks["sub-x"] = &asyncTask{
		id: "sub-x", sessionID: "s1", name: "x", instruction: "审查代码",
		status: asyncTaskSuccess, startedAt: time.Now(), output: "审查完成",
	}

	statusTool := &subagentStatusTool{owner: st}
	res, err := statusTool.Execute(WithToolSessionID(context.Background(), "s1"), map[string]any{"sub_session_id": "sub-x"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Output)
	}
	for _, want := range []string{"sub-x", asyncTaskSuccess, "审查完成"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("detail missing %q: %s", want, res.Output)
		}
	}
}

// TestSubagentControlTools_RequireSessionID 验证缺少会话上下文时拒绝执行——
// 没有 session id 就无法界定作用域，不能静默返回全量任务。
func TestSubagentControlTools_RequireSessionID(t *testing.T) {
	st := newControlTestTool(t)
	statusTool := &subagentStatusTool{owner: st}
	stopTool := &subagentStopTool{owner: st}

	if res, _ := statusTool.Execute(context.Background(), map[string]any{}); res.Success {
		t.Fatal("status should fail without session id")
	}
	if res, _ := stopTool.Execute(context.Background(), map[string]any{"sub_session_id": "sub-a"}); res.Success {
		t.Fatal("stop should fail without session id")
	}
}

// TestSubagentStopTool_CancelsRunningTask 守住停止的核心语义：对在途任务调用
// cancel，且任务结束后再次停止会提示无需停止。
func TestSubagentStopTool_CancelsRunningTask(t *testing.T) {
	st := newControlTestTool(t)
	taskCtx, cancel := context.WithCancel(context.Background())
	st.tasks["sub-s"] = &asyncTask{
		id: "sub-s", sessionID: "s1", name: "s", status: asyncTaskRunning, startedAt: time.Now(), cancel: cancel,
	}

	stopTool := &subagentStopTool{owner: st}
	res, err := stopTool.Execute(WithToolSessionID(context.Background(), "s1"), map[string]any{"sub_session_id": "sub-s"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Output)
	}
	if taskCtx.Err() != context.Canceled {
		t.Fatal("cancel was not propagated to the task context")
	}

	// 子 agent 退出后注册表转为 canceled，再次停止应提示已结束。
	st.finishTask("sub-s", "", context.Canceled)
	res2, _ := stopTool.Execute(WithToolSessionID(context.Background(), "s1"), map[string]any{"sub_session_id": "sub-s"})
	if !res2.Success || !strings.Contains(res2.Output, "已结束") {
		t.Fatalf("second stop should report finished, got: %s", res2.Output)
	}
}

// TestSubagentStopTool_RejectsForeignOrUnknownTask 验证不能停止其它会话或不存在
// 的任务，避免跨会话误停。
func TestSubagentStopTool_RejectsForeignOrUnknownTask(t *testing.T) {
	st := newControlTestTool(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.tasks["sub-foreign"] = &asyncTask{
		id: "sub-foreign", sessionID: "s2", name: "f", status: asyncTaskRunning, startedAt: time.Now(), cancel: cancel,
	}

	stopTool := &subagentStopTool{owner: st}
	if res, _ := stopTool.Execute(WithToolSessionID(context.Background(), "s1"), map[string]any{"sub_session_id": "sub-foreign"}); res.Success {
		t.Fatal("should not stop a task from another session")
	}
	if res, _ := stopTool.Execute(WithToolSessionID(context.Background(), "s1"), map[string]any{"sub_session_id": "nope"}); res.Success {
		t.Fatal("should fail for unknown task")
	}
}

// TestSubagentControlTools_DisabledRefuse 验证子 runtime 中两工具退化为 no-op，
// 与 subagent tool 的递归阻断机制一致，且不触碰任务注册表。
func TestSubagentControlTools_DisabledRefuse(t *testing.T) {
	st := newControlTestTool(t)
	statusTool := &subagentStatusTool{owner: st, disabled: true}
	stopTool := &subagentStopTool{owner: st, disabled: true}
	ctx := WithToolSessionID(context.Background(), "s1")

	res, _ := statusTool.Execute(ctx, map[string]any{})
	if res.Success || res.Output != subagentDisabledRefusal {
		t.Fatalf("disabled status should refuse, got: %+v", res)
	}
	res, _ = stopTool.Execute(ctx, map[string]any{"sub_session_id": "sub-a"})
	if res.Success || res.Output != subagentDisabledRefusal {
		t.Fatalf("disabled stop should refuse, got: %+v", res)
	}
}

// TestSubagentAsyncDispatch_RegistersTaskAndInjectsHistory 端到端验证后台派发：
// 任务在途时状态查询可见，完成后注册表转 success，且结果被注入主会话 history，
// 使主 agent 在下一轮对话直接感知结果。
func TestSubagentAsyncDispatch_RegistersTaskAndInjectsHistory(t *testing.T) {
	ctx := context.Background()
	gm := &gatedMockModel{gateAt: 2, entered: make(chan struct{}), release: make(chan struct{})}
	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:        recordingMockModelFactory{model: gm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)

	res, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "analyzer",
		"instruction": "long running task",
		"background":  true,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected async dispatch success, got: %s", res.Output)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("result data is not map[string]any: %T", res.Data)
	}
	subSessionID, _ := data["sub_session_id"].(string)
	if subSessionID == "" {
		t.Fatal("async dispatch missing sub_session_id")
	}

	// 子 agent 此刻阻塞在 model 调用中：状态查询应报告为 running。
	select {
	case <-gm.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("async child never started")
	}
	statusTool := &subagentStatusTool{owner: st}
	sres, _ := statusTool.Execute(WithToolSessionID(ctx, sessionID), map[string]any{})
	if !sres.Success || !strings.Contains(sres.Output, subSessionID) || !strings.Contains(sres.Output, asyncTaskRunning) {
		t.Fatalf("running task not reported: %s", sres.Output)
	}

	close(gm.release)

	// 完成后：主会话 history 收到合成结果消息（状态先于注入落定，故直接等注入）。
	deadline := time.Now().Add(3 * time.Second)
	injected := false
	for time.Now().Before(deadline) {
		hist, _ := mainRT.SessionHistory(sessionID)
		for _, m := range hist {
			if strings.Contains(m.Content, "[Subagent Result: analyzer]") {
				if m.Metadata["api.synthetic_type"] != "subagent_result" {
					t.Fatalf("synthetic metadata missing: %+v", m.Metadata)
				}
				injected = true
				break
			}
		}
		if injected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !injected {
		t.Fatal("async result was not injected into main session history")
	}

	task, ok := st.lookupTask(sessionID, subSessionID)
	if !ok {
		t.Fatalf("task %q not found in registry", subSessionID)
	}
	if task.status != asyncTaskSuccess {
		t.Fatalf("task status = %q, want success", task.status)
	}
}

// TestSubagentAsyncDispatch_RejectsBeyondConcurrencyLimit 验证后台任务并发上限：
// 槽位占满时新派发被拒绝（而非无限起 goroutine），任务结束释放槽位后可再次派发。
func TestSubagentAsyncDispatch_RejectsBeyondConcurrencyLimit(t *testing.T) {
	ctx := context.Background()
	gm := &gatedMockModel{gateAt: 2, entered: make(chan struct{}), release: make(chan struct{})}
	sessionID := "test-session"
	mainOpts := Options{
		ModelFactory:           recordingMockModelFactory{model: gm},
		ProjectRoot:            t.TempDir(),
		SystemPrompt:           "you are helpful",
		EnabledBuiltinTools:    []string{"read"},
		MaxConcurrentSubagents: 1,
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	}

	mainRT, err := New(ctx, mainOpts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer mainRT.Close()

	if _, err := mainRT.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	st := newSubagentTool(mainOpts)
	st.bindRuntime(mainRT)

	first, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name": "first", "instruction": "t1", "background": true,
	})
	if err != nil || !first.Success {
		t.Fatalf("first dispatch failed: res=%+v err=%v", first, err)
	}

	select {
	case <-gm.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("child never started")
	}

	second, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name": "second", "instruction": "t2", "background": true,
	})
	if err != nil {
		t.Fatalf("second Execute error: %v", err)
	}
	if second.Success {
		t.Fatalf("expected rejection at concurrency limit, got: %s", second.Output)
	}
	if !strings.Contains(second.Output, "并发上限") {
		t.Fatalf("rejection message missing limit hint: %s", second.Output)
	}

	// 放行首个任务后槽位释放，再次派发应成功。
	close(gm.release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(st.bgSlots) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(st.bgSlots) > 0 {
		t.Fatal("slot was not released after task completion")
	}
	third, err := st.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name": "third", "instruction": "t3", "background": true,
	})
	if err != nil || !third.Success {
		t.Fatalf("expected re-dispatch success after slot release: res=%+v err=%v", third, err)
	}
}
