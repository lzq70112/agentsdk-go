package api

import (
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

func newReminderTestRuntime(t *testing.T, sessionID string) (*Runtime, *message.History) {
	t.Helper()
	rt := &Runtime{histories: newHistoryStore(4, nil), reminders: newReminderEngine()}
	history, err := rt.histories.Get(sessionID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	return rt, history
}

// TestReminderDualClocksIndependent 固化双时钟独立计数：ROUNDS 规则只被
// assistant 消息推进，USER_TURNS 轮次不影响它，反之亦然。
func TestReminderDualClocksIndependent(t *testing.T) {
	t.Parallel()
	rt, history := newReminderTestRuntime(t, "sess")
	if err := rt.RegisterReminder("sess", ReminderRule{ID: "rounds-rule", Clock: ClockRounds, Every: 2, Text: "round-remind", Policy: BarrierDefer}); err != nil {
		t.Fatalf("register: %v", err)
	}

	rt.observeReminderTick("sess", ClockUserTurns)
	if history.Len() != 0 {
		t.Fatal("user turn must not advance the rounds clock")
	}
	rt.observeReminderTick("sess", ClockRounds)
	if history.Len() != 0 {
		t.Fatal("first round must not fire an every=2 rule")
	}
	rt.observeReminderTick("sess", ClockRounds)
	if history.Len() != 1 || !strings.Contains(history.All()[0].Content, "round-remind") {
		t.Fatalf("second round must fire, history = %+v", history.All())
	}
}

// TestReminderCooldownBlocksFrequentFiring 固化冷却契约的"防频繁触发"侧：
// 触发后在 Cooldown 个 tick 内即使到达周期点也不再触发。
func TestReminderCooldownBlocksFrequentFiring(t *testing.T) {
	t.Parallel()
	rt, history := newReminderTestRuntime(t, "sess")
	if err := rt.RegisterReminder("sess", ReminderRule{ID: "cd", Clock: ClockRounds, Every: 1, Cooldown: 3, Text: "cd-remind", Policy: BarrierDefer}); err != nil {
		t.Fatalf("register: %v", err)
	}

	rt.observeReminderTick("sess", ClockRounds) // 触发并 confirm
	if history.Len() != 1 {
		t.Fatalf("first tick must fire, history len = %d", history.Len())
	}
	rt.observeReminderTick("sess", ClockRounds) // 冷却内
	rt.observeReminderTick("sess", ClockRounds) // 冷却内
	if history.Len() != 1 {
		t.Fatalf("cooldown must block firing, history len = %d", history.Len())
	}
	rt.observeReminderTick("sess", ClockRounds) // 冷却过
	if history.Len() != 2 {
		t.Fatalf("cooldown expired must fire again, history len = %d", history.Len())
	}
}

// TestReminderInjectFailureDoesNotResetCooldown 固化冷却契约的"防永不到期"
// 侧：注入未 commit（会话忙 + REJECT 被拒）时不记录冷却，忙完后的下个周期
// 点照常触发。
func TestReminderInjectFailureDoesNotResetCooldown(t *testing.T) {
	t.Parallel()
	rt, history := newReminderTestRuntime(t, "sess")
	if err := rt.RegisterReminder("sess", ReminderRule{ID: "retry", Clock: ClockRounds, Every: 1, Text: "retry-remind", Policy: BarrierReject}); err != nil {
		t.Fatalf("register: %v", err)
	}

	run := rt.beginActiveRun("sess", func() {}, nil)
	rt.observeReminderTick("sess", ClockRounds) // 忙 + REJECT → 注入失败，不 confirm
	rt.endActiveRun(run)
	if history.Len() != 0 {
		t.Fatalf("rejected injection must not land, history len = %d", history.Len())
	}

	rt.observeReminderTick("sess", ClockRounds) // 空闲 → 重试成功
	if history.Len() != 1 || !strings.Contains(history.All()[0].Content, "retry-remind") {
		t.Fatalf("failed injection must retry on next period, history = %+v", history.All())
	}
}

// TestReminderDueEventsMerged 固化合并/节流：同一 tick 到期的多条规则合并
// 为一条 REMINDER 注入，而非多条消息刷屏。
func TestReminderDueEventsMerged(t *testing.T) {
	t.Parallel()
	rt, history := newReminderTestRuntime(t, "sess")
	for _, rule := range []ReminderRule{
		{ID: "a", Clock: ClockUserTurns, Every: 1, Text: "remind-a", Policy: BarrierDefer},
		{ID: "b", Clock: ClockUserTurns, Every: 1, Text: "remind-b", Policy: BarrierDefer},
	} {
		if err := rt.RegisterReminder("sess", rule); err != nil {
			t.Fatalf("register %s: %v", rule.ID, err)
		}
	}

	rt.observeReminderTick("sess", ClockUserTurns)
	msgs := history.All()
	if len(msgs) != 1 {
		t.Fatalf("due reminders must merge into one message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "remind-a") || !strings.Contains(msgs[0].Content, "remind-b") {
		t.Fatalf("merged message must carry both texts, got %q", msgs[0].Content)
	}
}

// TestReminderRoutesThroughInjectionRouter 固化引擎不直接写会话：到期走
// Inject(REMINDER)，合成标记自动补齐；忙时 DEFER 排队、回合结束交付。
func TestReminderRoutesThroughInjectionRouter(t *testing.T) {
	t.Parallel()
	rt, history := newReminderTestRuntime(t, "sess")
	if err := rt.RegisterReminder("sess", ReminderRule{ID: "routed", Clock: ClockUserTurns, Every: 1, Text: "routed-remind", Policy: BarrierDefer}); err != nil {
		t.Fatalf("register: %v", err)
	}

	run := rt.beginActiveRun("sess", func() {}, nil)
	rt.observeReminderTick("sess", ClockUserTurns)
	if history.Len() != 0 {
		t.Fatal("busy session must queue the reminder instead of writing directly")
	}
	rt.endActiveRun(run)

	msgs := history.All()
	if len(msgs) != 1 {
		t.Fatalf("deferred reminder must land at round boundary, history = %+v", msgs)
	}
	if !isSyntheticMessage(msgs[0]) || msgs[0].Metadata[metadataSyntheticKey] != string(InjectionKindReminder) {
		t.Fatalf("reminder must carry synthetic marker, metadata = %v", msgs[0].Metadata)
	}
}

// TestReminderRegistrationLifecycle 固化宿主接口：注册/注销/查询、重复
// 注册拒绝、非法参数拒绝。
func TestReminderRegistrationLifecycle(t *testing.T) {
	t.Parallel()
	rt, _ := newReminderTestRuntime(t, "sess")

	rule := ReminderRule{ID: "life", Clock: ClockUserTurns, Every: 5, Text: "life-remind", Policy: BarrierDrop}
	if err := rt.RegisterReminder("sess", rule); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := rt.RegisterReminder("sess", rule); err == nil {
		t.Fatal("duplicate id must be rejected")
	}
	active := rt.ActiveReminders("sess")
	if len(active) != 1 || active[0].ID != "life" || active[0].Every != 5 {
		t.Fatalf("active reminders = %+v", active)
	}
	if !rt.UnregisterReminder("sess", "life") {
		t.Fatal("unregister existing rule must return true")
	}
	if rt.UnregisterReminder("sess", "life") {
		t.Fatal("unregister missing rule must return false")
	}
	if rt.ActiveReminders("sess") != nil {
		t.Fatal("no active reminders expected")
	}
	if err := rt.RegisterReminder("sess", ReminderRule{ID: "bad", Clock: ClockUserTurns, Every: 0}); err == nil {
		t.Fatal("every=0 must be rejected")
	}
	if err := rt.RegisterReminder("sess", ReminderRule{ID: "bad", Clock: ReminderClock(9), Every: 1}); err == nil {
		t.Fatal("unknown clock must be rejected")
	}
}
