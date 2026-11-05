package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

// waitForCond 轮询等待 cond 成立，超时返回当前值。供跨 goroutine 时序断言使用，
// 避免在每个用例里手写轮询循环。
func waitForCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// callCount 并发安全地返回 gatedMockModel 已收到的模型调用次数。
func (m *gatedMockModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// newDispatchTestRuntime 构造一个 subagent 内置工具已启用并绑定到 Runtime 的测试
// 运行时：RunSubagent 走 rt.subagent 公开链路，与宿主的真实使用方式一致，
// 同时覆盖"EnabledBuiltinTools 显式含 subagent 时 rt.subagent 自动可用"的绑定语义。
func newDispatchTestRuntime(t *testing.T, opts Options) *Runtime {
	t.Helper()
	rt, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// TestRunSubagent_ReturnsOutputAndSubSessionID 验证编程式派发的基础契约：返回子
// agent 的最终输出文本、子会话 ID 带 {主会话}-sub- 前缀、子 agent 在隔离上下文中
// 运行（delegation 消息包装指令 + 输出契约、不带主会话历史）、主会话历史不被污染。
// 这条测试守住工作流引擎 fan-out 的地基：输出与标识不可用，编排无从谈起。
func TestRunSubagent_ReturnsOutputAndSubSessionID(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{responses: []model.Response{
		// 第 1 条供主会话 Run 消耗；第 2 条才是子 agent 的最终输出。
		{Message: model.Message{Role: "assistant", Content: "主回复"}, StopReason: "end_turn"},
		{Message: model.Message{Role: "assistant", Content: "子任务完整结论与证据"}, StopReason: "end_turn"},
	}}

	sessionID := "test-session"
	rt := newDispatchTestRuntime(t, Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"subagent"},
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "MAIN_SECRET"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	})

	if _, err := rt.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}
	mainBefore, _ := rt.SessionHistory(sessionID)

	output, subSessionID, err := rt.RunSubagent(ctx, SubagentDispatchRequest{
		SessionID:   sessionID,
		Name:        "verifier",
		Instruction: "检查产物",
	})
	if err != nil {
		t.Fatalf("RunSubagent returned error: %v", err)
	}
	if output != "子任务完整结论与证据" {
		t.Fatalf("output = %q, want %q", output, "子任务完整结论与证据")
	}
	if !strings.HasPrefix(subSessionID, sessionID+"-sub-") {
		t.Fatalf("sub session id %q missing prefix %q", subSessionID, sessionID+"-sub-")
	}

	// 主会话历史不被编程式派发污染。
	mainAfter, _ := rt.SessionHistory(sessionID)
	if len(mainAfter) != len(mainBefore) {
		t.Fatalf("main history mutated: before=%d after=%d", len(mainBefore), len(mainAfter))
	}

	// 子 agent 请求：delegation 包装（指令 + 输出契约），且不带主会话历史（隔离上下文）。
	if len(mm.calls) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(mm.calls))
	}
	subReq := mm.calls[len(mm.calls)-1]
	joined := requestContent(&subReq)
	if !strings.Contains(joined, "[Subtask] verifier: 检查产物") {
		t.Fatalf("sub request missing delegation message, got:\n%s", joined)
	}
	if !strings.Contains(joined, "返回该子任务的完整执行结果") {
		t.Fatalf("sub request missing output contract, got:\n%s", joined)
	}
	if strings.Contains(joined, "MAIN_SECRET") {
		t.Fatalf("sub request leaked main session history:\n%s", joined)
	}
}

// TestRunSubagent_TypeIdentityResolvesRegisteredTypes 验证编程注册的 AgentType
// 经 RunSubagent 的 Type 字段可解析：类型身份 system prompt 替换子 runtime 的
// identity 段（类型文件路径的解析已由既有测试覆盖，这里守住编程入口同一语义）。
func TestRunSubagent_TypeIdentityResolvesRegisteredTypes(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	rt := newDispatchTestRuntime(t, Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are the main assistant",
		EnabledBuiltinTools: []string{"subagent"},
		AgentTypes: []AgentType{{
			Name:         "reviewer",
			Description:  "代码审查",
			SystemPrompt: "先列出风险点，再给出结论",
		}},
		AutoCompact: CompactConfig{Enabled: false},
	})

	if _, _, err := rt.RunSubagent(ctx, SubagentDispatchRequest{
		SessionID:   "test-session",
		Name:        "reviewer",
		Instruction: "审查本次变更",
		Type:        "reviewer",
	}); err != nil {
		t.Fatalf("RunSubagent returned error: %v", err)
	}

	if len(mm.calls) < 1 {
		t.Fatal("expected at least 1 model call")
	}
	childReq := mm.calls[len(mm.calls)-1]
	if !strings.Contains(childReq.System, "先列出风险点，再给出结论") {
		t.Fatalf("child system prompt missing type identity, got: %q", childReq.System)
	}
	if strings.Contains(childReq.System, "you are the main assistant") {
		t.Fatalf("child system prompt leaked main identity: %q", childReq.System)
	}
}

// TestRunSubagent_CompletionCallbackFiresExactlyOnceOnSuccess 验证成功路径上宿主
// 回调恰好触发一次且带齐归属信息——工作流引擎据此推进节点状态，回调丢失或重复
// 触发都会让状态机错乱。
func TestRunSubagent_CompletionCallbackFiresExactlyOnceOnSuccess(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	var got []SubagentCompletion
	rt := newDispatchTestRuntime(t, Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"subagent"},
		AutoCompact:         CompactConfig{Enabled: false},
		SubagentCompletionHandler: func(c SubagentCompletion) {
			got = append(got, c)
		},
	})

	_, subSessionID, err := rt.RunSubagent(ctx, SubagentDispatchRequest{
		SessionID:   "test-session",
		Name:        "analyzer",
		Instruction: "分析数据",
	})
	if err != nil {
		t.Fatalf("RunSubagent returned error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 completion callback, got %d", len(got))
	}
	c := got[0]
	if c.MainSessionID != "test-session" {
		t.Fatalf("completion main session = %q, want test-session", c.MainSessionID)
	}
	if c.SubSessionID != subSessionID {
		t.Fatalf("completion sub session = %q, want %q", c.SubSessionID, subSessionID)
	}
	if c.Name != "analyzer" {
		t.Fatalf("completion name = %q, want analyzer", c.Name)
	}
	if c.Err != nil {
		t.Fatalf("completion err = %v, want nil", c.Err)
	}
	if len(c.History) == 0 {
		t.Fatal("completion history is empty")
	}
}

// TestRunSubagent_CompletionCallbackFiresExactlyOnceOnFailure 验证失败路径上回调
// 同样恰好触发一次且带错误、派发错误向上传播——失败的子任务恰恰是最需要排查的
// 对象，漏掉它等于丢掉最有价值的诊断信息。
func TestRunSubagent_CompletionCallbackFiresExactlyOnceOnFailure(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}
	// 不先跑主会话：子 agent 的第 1 次模型调用即越过 failAfter，确定性失败。
	failingModel := &failingMockModel{recordingMockModel: mm, failAfter: 0}

	var got []SubagentCompletion
	rt := newDispatchTestRuntime(t, Options{
		ModelFactory:        recordingMockModelFactory{model: failingModel},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"subagent"},
		AutoCompact:         CompactConfig{Enabled: false},
		SubagentCompletionHandler: func(c SubagentCompletion) {
			got = append(got, c)
		},
	})

	output, _, err := rt.RunSubagent(ctx, SubagentDispatchRequest{
		SessionID:   "test-session",
		Name:        "analyzer",
		Instruction: "this will fail",
	})
	if err == nil {
		t.Fatal("RunSubagent should propagate child failure, got nil error")
	}
	if output != "" {
		t.Fatalf("output = %q on failure, want empty", output)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 completion callback on failure, got %d", len(got))
	}
	if got[0].Err == nil {
		t.Fatal("completion err = nil, want non-nil on failure")
	}
}

// TestRunSubagent_StaysOutOfBackgroundRegistryAndSlots 验证同步派发语义与内置
// subagent 工具一致：不进后台任务注册表（执行中也不出现在 SubagentsRunning）、
// 不占后台并发槽（唯一槽被后台任务占满时仍能同步派发成功）。这条测试守住的是
// 编程派发与后台派发的资源语义隔离——占槽会让宿主的后台 fan-out 被同步任务挤死。
func TestRunSubagent_StaysOutOfBackgroundRegistryAndSlots(t *testing.T) {
	ctx := context.Background()
	// 调用序：1=主 Run，2=后台子 agent（gate 阻塞），3=同步子 agent（gate 阻塞）。
	gm := &gatedMockModel{gateAt: 2, entered: make(chan struct{}), release: make(chan struct{})}

	sessionID := "test-session"
	rt := newDispatchTestRuntime(t, Options{
		ModelFactory:           recordingMockModelFactory{model: gm},
		ProjectRoot:            t.TempDir(),
		SystemPrompt:           "you are helpful",
		EnabledBuiltinTools:    []string{"subagent"},
		MaxConcurrentSubagents: 1,
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return []message.Message{{Role: "user", Content: "hello"}}, nil
			}
			return nil, nil
		},
		AutoCompact: CompactConfig{Enabled: false},
	})

	if _, err := rt.Run(ctx, Request{SessionID: sessionID, Prompt: "ping"}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	// 用内置工具的后台派发占住唯一并发槽。
	bgRes, err := rt.subagent.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":        "bg",
		"instruction": "长耗时后台任务",
		"background":  true,
	})
	if err != nil {
		t.Fatalf("background Execute returned error: %v", err)
	}
	if !bgRes.Success {
		t.Fatalf("expected background dispatch success, got: %s", bgRes.Output)
	}
	// 等后台子 agent 真正进入模型调用，确保槽已占住。
	select {
	case <-gm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("background child never started")
	}

	type runOutcome struct {
		output string
		id     string
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		output, id, err := rt.RunSubagent(ctx, SubagentDispatchRequest{
			SessionID:   sessionID,
			Name:        "sync",
			Instruction: "同步任务",
		})
		done <- runOutcome{output: output, id: id, err: err}
	}()
	// 等同步子 agent 进入模型调用：若实现错误地去抢后台槽，它此刻已被拒绝返回，
	// callCount 到不了 3，下面的等待超时即暴露。
	if !waitForCond(5*time.Second, func() bool { return gm.callCount() >= 3 }) {
		t.Fatalf("sync child never started; background slots may have blocked it, calls=%d", gm.callCount())
	}

	// 同步派发执行中不进后台任务注册表：列表里只有被占槽的后台任务。
	if got := rt.SubagentsRunning(sessionID); len(got) != 1 {
		t.Fatalf("SubagentsRunning = %d, want 1 (only the background task); %+v", len(got), got)
	}

	close(gm.release)
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("RunSubagent returned error: %v", out.err)
		}
		if !strings.HasPrefix(out.id, sessionID+"-sub-") {
			t.Fatalf("sub session id %q missing prefix %q", out.id, sessionID+"-sub-")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunSubagent did not return after gate release")
	}
	// 等后台任务退出注册表，避免 goroutine 与临时目录清理竞争。
	if !waitForCond(5*time.Second, func() bool { return len(rt.SubagentsRunning(sessionID)) == 0 }) {
		t.Fatalf("background task still registered: %+v", rt.SubagentsRunning(sessionID))
	}
}

// TestRunSubagent_ResultIsResumableViaSubagentTool 验证编程派发成功的子会话进入
// 追问缓存：既有 subagent 工具按 sub_session_id 追问时，子 agent 带着此前完整工作
// 记忆在原上下文续跑。这条测试守住编程入口与内置工具的会话资产互通——否则
// RunSubagent 产出的子会话对追问链路是黑盒。
func TestRunSubagent_ResultIsResumableViaSubagentTool(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{}

	sessionID := "test-session"
	rt := newDispatchTestRuntime(t, Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"subagent"},
		AutoCompact:         CompactConfig{Enabled: false},
	})

	output, subSessionID, err := rt.RunSubagent(ctx, SubagentDispatchRequest{
		SessionID:   sessionID,
		Name:        "analyzer",
		Instruction: "第一轮分析",
	})
	if err != nil {
		t.Fatalf("RunSubagent returned error: %v", err)
	}
	if output == "" {
		t.Fatal("first dispatch returned empty output")
	}

	res, err := rt.subagent.Execute(WithToolSessionID(ctx, sessionID), map[string]any{
		"name":           "analyzer",
		"instruction":    "第二轮追问",
		"sub_session_id": subSessionID,
	})
	if err != nil {
		t.Fatalf("resume Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected resume success, got: %s", res.Output)
	}

	// 追问的子请求必须同时带着两轮子任务指令：证明是在原上下文续跑而非重开。
	if len(mm.calls) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(mm.calls))
	}
	resumeReq := mm.calls[len(mm.calls)-1]
	joined := requestContent(&resumeReq)
	for _, want := range []string{
		"[Subtask] analyzer: 第一轮分析",
		"[Subtask] analyzer: 第二轮追问",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("resume request missing %q, got:\n%s", want, joined)
		}
	}
}

// TestRunSubagent_PropagatesCancelToChildRuntime 验证派发方 ctx 取消传播到子
// runtime 并以错误返回：取消后子 agent 的模型调用被中断、回调以失败触发。
// 子 runtime 的退出由 dispatch 内 defer Close 保证（Run 返回即已关闭）。
func TestRunSubagent_PropagatesCancelToChildRuntime(t *testing.T) {
	// 不先跑主会话：子 agent 的第 1 次模型调用即进入 gate，ctx 取消解除阻塞。
	gm := &gatedMockModel{gateAt: 1, entered: make(chan struct{}), release: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []SubagentCompletion
	rt := newDispatchTestRuntime(t, Options{
		ModelFactory:        recordingMockModelFactory{model: gm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"subagent"},
		AutoCompact:         CompactConfig{Enabled: false},
		SubagentCompletionHandler: func(c SubagentCompletion) {
			got = append(got, c)
		},
	})

	type runOutcome struct {
		output string
		id     string
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		output, id, err := rt.RunSubagent(ctx, SubagentDispatchRequest{
			SessionID:   "test-session",
			Name:        "analyzer",
			Instruction: "长耗时任务",
		})
		done <- runOutcome{output: output, id: id, err: err}
	}()

	select {
	case <-gm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child never started")
	}
	cancel()

	select {
	case out := <-done:
		if out.err == nil {
			t.Fatal("RunSubagent should return error on ctx cancel, got nil")
		}
		if !errors.Is(out.err, context.Canceled) {
			t.Fatalf("error should wrap context.Canceled, got: %v", out.err)
		}
		if out.output != "" {
			t.Fatalf("output = %q on cancel, want empty", out.output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunSubagent did not return after cancel")
	}
	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("expected exactly 1 failure completion on cancel, got %+v", got)
	}
}

// TestRunSubagent_RequiresBoundSubagentToolAndValidRequest 验证未绑定/非法入参时
// 返回明确错误而非 panic 或静默零值：subagent 内置工具未启用、SessionID/Name/
// Instruction 缺失都是宿主可自查的配置错误，报错必须指名道姓。
func TestRunSubagent_RequiresBoundSubagentToolAndValidRequest(t *testing.T) {
	ctx := context.Background()

	// EnabledBuiltinTools 不含 subagent → rt.subagent 未绑定。
	mm := &recordingMockModel{}
	rt := newDispatchTestRuntime(t, Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"read"},
		AutoCompact:         CompactConfig{Enabled: false},
	})
	if rt.subagent != nil {
		t.Fatal("precondition: rt.subagent should be nil without subagent builtin tool")
	}
	_, _, err := rt.RunSubagent(ctx, SubagentDispatchRequest{
		SessionID:   "test-session",
		Name:        "analyzer",
		Instruction: "分析",
	})
	if err == nil {
		t.Fatal("RunSubagent should fail explicitly when subagent tool is not bound")
	}

	// 绑定就绪但入参缺失 → 明确报错。
	rt2 := newDispatchTestRuntime(t, Options{
		ModelFactory:        recordingMockModelFactory{model: &recordingMockModel{}},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "you are helpful",
		EnabledBuiltinTools: []string{"subagent"},
		AutoCompact:         CompactConfig{Enabled: false},
	})
	for name, req := range map[string]SubagentDispatchRequest{
		"empty sessionID": {Name: "analyzer", Instruction: "分析"},
		"empty name":      {SessionID: "test-session", Instruction: "分析"},
		"empty instruction": {SessionID: "test-session",
			Name: "analyzer"},
	} {
		if _, _, err := rt2.RunSubagent(ctx, req); err == nil {
			t.Errorf("%s: RunSubagent should reject empty field, got nil error", name)
		}
	}
}
