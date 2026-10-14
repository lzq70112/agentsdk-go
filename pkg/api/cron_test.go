package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/model"
)

// cronHarness 是同步驱动引擎的测试夹具：可注入时钟 + 记录型 inject/onFire。
// 到期触发经 fireDue 同步驱动，触发时刻断言不依赖真实睡眠。
type cronHarness struct {
	engine *cronEngine
	mu     sync.Mutex
	now    time.Time
	injs   []Injection
	fired  []CronTask
	injErr error
}

func newCronHarness(t *testing.T) *cronHarness {
	t.Helper()
	h := &cronHarness{now: at(2026, 10, 14, 10, 3)}
	h.engine = newCronEngine(
		func(_ string, in Injection) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.injErr != nil {
				return h.injErr
			}
			h.injs = append(h.injs, in)
			return nil
		},
		func(task CronTask) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.fired = append(h.fired, task)
		},
	)
	h.engine.clock = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	}
	t.Cleanup(h.engine.stop)
	return h
}

func (h *cronHarness) setTime(t time.Time) {
	h.mu.Lock()
	h.now = t
	h.mu.Unlock()
}

func (h *cronHarness) setInjectErr(err error) {
	h.mu.Lock()
	h.injErr = err
	h.mu.Unlock()
}

func (h *cronHarness) injections() []Injection {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Injection(nil), h.injs...)
}

func (h *cronHarness) firings() []CronTask {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]CronTask(nil), h.fired...)
}

// waitFor 轮询等待异步条件成立（仅用于 loop goroutine 的交付等待；
// 触发时刻本身由可注入时钟驱动，不依赖睡眠时长）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

// TestCronEngineFiresRecurringOnSchedule 固化 recurring 触发语义：到期才触发、
// 注入成功才推进 NextRun、到期回调携带推进后的快照。
func TestCronEngineFiresRecurringOnSchedule(t *testing.T) {
	h := newCronHarness(t)
	start := at(2026, 10, 14, 10, 3)
	task, err := h.engine.add(CronTaskSpec{SessionID: "s1", Expr: "*/1 * * * *", Prompt: "喝水提醒", Recurring: true}, start)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !task.NextRun.Equal(at(2026, 10, 14, 10, 4)) {
		t.Fatalf("初始 NextRun = %v, 期望 10:04", task.NextRun)
	}

	// 未到期：不触发
	h.engine.fireDue()
	if len(h.injections()) != 0 {
		t.Fatalf("未到期不应触发, 实际 %d 次", len(h.injections()))
	}

	// 到期（10:04）：触发一次，NextRun 推进到 10:05
	h.setTime(at(2026, 10, 14, 10, 4))
	h.engine.fireDue()
	injs := h.injections()
	if len(injs) != 1 {
		t.Fatalf("期望 1 次注入, 得到 %d", len(injs))
	}
	if injs[0].Kind != InjectionKindScheduled || injs[0].Policy != BarrierDefer {
		t.Fatalf("注入应为 SCHEDULED + BarrierDefer: %+v", injs[0])
	}
	if injs[0].Message.Content != scheduledFirePrefix+"喝水提醒" {
		t.Fatalf("注入内容 = %q", injs[0].Message.Content)
	}
	tasks := h.engine.list("s1")
	if len(tasks) != 1 || !tasks[0].NextRun.Equal(at(2026, 10, 14, 10, 5)) {
		t.Fatalf("NextRun 应推进到 10:05: %+v", tasks)
	}
	if !tasks[0].LastRun.Equal(at(2026, 10, 14, 10, 4)) {
		t.Fatalf("LastRun 应记录触发时刻: %+v", tasks[0])
	}
	fired := h.firings()
	if len(fired) != 1 || fired[0].ID != task.ID || !fired[0].NextRun.Equal(at(2026, 10, 14, 10, 5)) {
		t.Fatalf("到期回调快照不符: %+v", fired)
	}

	// 下一周期（10:05）：再次触发
	h.setTime(at(2026, 10, 14, 10, 5))
	h.engine.fireDue()
	if len(h.injections()) != 2 {
		t.Fatalf("期望第 2 次注入, 实际 %d 次", len(h.injections()))
	}
}

// TestCronEngineOneShotFiresOnce 固化 one-shot 语义：触发一次后自动删除，
// 回调快照携带终态 LastRun，之后不再重复触发。
func TestCronEngineOneShotFiresOnce(t *testing.T) {
	h := newCronHarness(t)
	h.engine.add(CronTaskSpec{SessionID: "s1", Expr: "*/1 * * * *", Prompt: "单次巡检"}, at(2026, 10, 14, 10, 3))
	h.setTime(at(2026, 10, 14, 10, 4))
	h.engine.fireDue()
	if len(h.injections()) != 1 {
		t.Fatalf("期望 1 次注入, 得到 %d", len(h.injections()))
	}
	if tasks := h.engine.list("s1"); len(tasks) != 0 {
		t.Fatalf("one-shot 触发后应删除, 剩 %d 个", len(tasks))
	}
	fired := h.firings()
	if len(fired) != 1 || !fired[0].LastRun.Equal(at(2026, 10, 14, 10, 4)) {
		t.Fatalf("回调快照应含终态 LastRun: %+v", fired)
	}
	h.setTime(at(2026, 10, 14, 10, 5))
	h.engine.fireDue()
	if len(h.injections()) != 1 {
		t.Fatal("one-shot 不应重复触发")
	}
}

// TestCronEngineRetriesAfterInjectFailure 固化"注入成功才推进"：失败不推进
// NextRun、不回调，恢复后下轮扫描重试成功。
func TestCronEngineRetriesAfterInjectFailure(t *testing.T) {
	h := newCronHarness(t)
	h.engine.add(CronTaskSpec{SessionID: "s1", Expr: "*/1 * * * *", Prompt: "巡检", Recurring: true}, at(2026, 10, 14, 10, 3))
	h.setInjectErr(errors.New("session gone"))
	h.setTime(at(2026, 10, 14, 10, 4))
	h.engine.fireDue()
	h.engine.fireDue()
	if len(h.injections()) != 0 {
		t.Fatal("inject 失败时不应记录注入")
	}
	tasks := h.engine.list("s1")
	if len(tasks) != 1 || !tasks[0].NextRun.Equal(at(2026, 10, 14, 10, 4)) {
		t.Fatalf("注入失败不应推进 NextRun: %+v", tasks)
	}
	if len(h.firings()) != 0 {
		t.Fatal("注入失败不应回调 onFire")
	}
	h.setInjectErr(nil)
	h.engine.fireDue()
	if len(h.injections()) != 1 {
		t.Fatal("恢复后应重试成功")
	}
}

// TestCronEngineRemoveStopsFiring 固化删除语义：删除后不再触发，重复删除报 false。
func TestCronEngineRemoveStopsFiring(t *testing.T) {
	h := newCronHarness(t)
	task, err := h.engine.add(CronTaskSpec{SessionID: "s1", Expr: "*/1 * * * *", Prompt: "x", Recurring: true}, at(2026, 10, 14, 10, 3))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !h.engine.remove(task.ID) {
		t.Fatal("存在任务应删除成功")
	}
	if h.engine.remove(task.ID) {
		t.Fatal("重复删除应返回 false")
	}
	h.setTime(at(2026, 10, 14, 10, 4))
	h.engine.fireDue()
	if len(h.injections()) != 0 {
		t.Fatal("已删除任务不应触发")
	}
}

// TestCronEngineLoopFiresThroughTicker 验证扫描循环链路：时钟每次调用前进
// 1 分钟，短 ticker 驱动 fireDue，任务经循环真实触发。
func TestCronEngineLoopFiresThroughTicker(t *testing.T) {
	var mu sync.Mutex
	var injs []Injection
	var calls atomic.Int64
	base := at(2026, 10, 14, 10, 0)
	e := newCronEngine(func(_ string, in Injection) error {
		mu.Lock()
		defer mu.Unlock()
		injs = append(injs, in)
		return nil
	}, nil)
	e.tick = 10 * time.Millisecond
	e.clock = func() time.Time { return base.Add(time.Duration(calls.Add(1)) * time.Minute) }
	if _, err := e.add(CronTaskSpec{SessionID: "s1", Expr: "*/2 * * * *", Prompt: "loop", Recurring: true}, e.clock()); err != nil {
		t.Fatalf("add: %v", err)
	}
	e.start()
	t.Cleanup(e.stop)
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(injs) >= 1
	})
}

// TestCronEngineStartStopLifecycle 固化循环生命周期：stop 等待退出、幂等、
// 未启动引擎 stop 不阻塞。
func TestCronEngineStartStopLifecycle(t *testing.T) {
	e := newCronEngine(func(string, Injection) error { return nil }, nil)
	e.tick = 10 * time.Millisecond
	e.start()
	e.stop()
	select {
	case <-e.done:
	default:
		t.Fatal("stop 后循环未退出")
	}
	e.stop() // 幂等
	idle := newCronEngine(func(string, Injection) error { return nil }, nil)
	idle.stop() // 未启动过：直接返回
}

// TestCronFireQueuesWhileSessionBusy 固化忙时会话的到期处置：SCHEDULED 注入
// 经统一入口排队到回合结束，不与用户回合交错，交付消息带 synthetic 标记。
func TestCronFireQueuesWhileSessionBusy(t *testing.T) {
	rt, history := newInjectionTestRuntime(t, "sess")
	run := rt.beginActiveRun("sess", func() {}, nil)

	fixed := at(2026, 10, 14, 10, 4)
	e := newCronEngine(rt.Inject, nil)
	e.clock = func() time.Time { return fixed }
	if _, err := e.add(CronTaskSpec{SessionID: "sess", Expr: "* * * * *", Prompt: "巡检"}, at(2026, 10, 14, 10, 3)); err != nil {
		t.Fatalf("add: %v", err)
	}
	e.fireDue()
	if got := history.Len(); got != 0 {
		t.Fatalf("忙时会话注入应排队, 实际已落盘 %d 条", got)
	}

	rt.endActiveRun(run)
	msgs := history.All()
	if len(msgs) != 1 || msgs[0].Content != scheduledFirePrefix+"巡检" {
		t.Fatalf("回合结束后应交付到期注入: %+v", msgs)
	}
	if !isSyntheticMessage(msgs[0]) || msgs[0].Metadata[metadataSyntheticKey] != string(InjectionKindScheduled) {
		t.Fatalf("注入消息应带 scheduled synthetic 标记: %+v", msgs[0].Metadata)
	}
}

// TestAddCronTaskValidatesInput 固化创建期校验：缺参、非法表达式、永不匹配
// 表达式都在创建时报错，不留死任务。
func TestAddCronTaskValidatesInput(t *testing.T) {
	rt := &Runtime{histories: newHistoryStore(4, nil)}
	rt.crons = newCronEngine(rt.Inject, nil)
	cases := []CronTaskSpec{
		{Expr: "* * * * *", Prompt: "x"},                  // 缺 session
		{SessionID: "s", Expr: "* * * * *"},               // 缺 prompt
		{SessionID: "s", Expr: "60 * * * *", Prompt: "x"}, // 非法表达式
		{SessionID: "s", Expr: "0 0 30 2 *", Prompt: "x"}, // 2 月 30 日永不匹配
	}
	for i, spec := range cases {
		if _, err := rt.AddCronTask(spec); err == nil {
			t.Errorf("用例 %d: 期望报错, 得到 nil", i)
		}
	}
}

// TestRuntimeCronTaskLifecycle 固化 Runtime API 生命周期：创建、按会话列举、
// 全量列举、删除。
func TestRuntimeCronTaskLifecycle(t *testing.T) {
	rt := &Runtime{histories: newHistoryStore(4, nil)}
	rt.crons = newCronEngine(rt.Inject, nil)
	t.Cleanup(rt.crons.stop)

	t1, err := rt.AddCronTask(CronTaskSpec{SessionID: "s1", Expr: "*/5 * * * *", Prompt: "a", Recurring: true})
	if err != nil {
		t.Fatalf("AddCronTask: %v", err)
	}
	t2, err := rt.AddCronTask(CronTaskSpec{SessionID: "s2", Expr: "0 9 * * *", Prompt: "b"})
	if err != nil {
		t.Fatalf("AddCronTask: %v", err)
	}
	if t1.ID == "" || t1.ID == t2.ID {
		t.Fatalf("任务 ID 应非空且唯一: %q %q", t1.ID, t2.ID)
	}
	if tasks := rt.CronTasks("s1"); len(tasks) != 1 || tasks[0].ID != t1.ID {
		t.Fatalf("按会话列举不符: %+v", tasks)
	}
	if all := rt.CronTasks(""); len(all) != 2 {
		t.Fatalf("全量列举应 2 个, 得到 %d", len(all))
	}
	if !rt.RemoveCronTask(t2.ID) {
		t.Fatal("删除应成功")
	}
	if rt.RemoveCronTask(t2.ID) {
		t.Fatal("重复删除应返回 false")
	}
}

// TestRuntimeWiresCronEngine 验证 New() 创建 cron 引擎并接线到期回调。
func TestRuntimeWiresCronEngine(t *testing.T) {
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: "ok"}},
	}}
	rt, err := New(context.Background(), Options{
		ProjectRoot:          root,
		Model:                mdl,
		ScheduledFireHandler: func(CronTask) {},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if rt.crons == nil {
		t.Fatal("New 应创建 cron 引擎")
	}
	if rt.crons.onFire == nil {
		t.Fatal("ScheduledFireHandler 未接线")
	}
}
