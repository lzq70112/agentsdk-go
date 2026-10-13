package api

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// ReminderClock 提醒引擎的双时钟，二者独立计数。
type ReminderClock int

const (
	// ClockRounds 按 assistant 消息条数计：每条已 commit 的 assistant 消息 +1。
	ClockRounds ReminderClock = iota
	// ClockUserTurns 按用户轮次计：每次已 commit 的用户输入 +1（一次 Run 请求一轮）。
	ClockUserTurns
)

func (c ReminderClock) String() string {
	switch c {
	case ClockRounds:
		return "rounds"
	case ClockUserTurns:
		return "user_turns"
	default:
		return "unknown"
	}
}

// ReminderRule 描述一条周期提醒规则。
//
// 触发语义：所在时钟每走 Every 个 tick 是一个周期点；处于冷却外（距上次
// 触发至少 Cooldown 个 tick）时在周期点触发。Cooldown 允许大于 Every——
// 触发后跳过若干周期点，防止频繁触发。
type ReminderRule struct {
	ID       string
	Clock    ReminderClock
	Every    int           // 周期 N（tick 数），必须 > 0
	Cooldown int           // 冷却（tick 数），0 表示触发后下一周期点即可再触发
	Text     string        // 提醒文案
	Policy   BarrierPolicy // 到期注入的 Barrier 语义（会话忙时排队或丢弃）
}

// dueReminder 是一次到期判定的结果：规则 + 触发时的 tick 值。
type dueReminder struct {
	rule   ReminderRule
	atTick int
}

// reminderEngine 按会话维护提醒规则状态。引擎只负责计算"哪些到期"，
// 不直接写会话——到期事件由调用方经统一注入入口以 REMINDER kind 进入。
type reminderEngine struct {
	mu    sync.Mutex
	rules map[string]map[string]*reminderState
}

type reminderState struct {
	rule      ReminderRule
	ticks     int
	fired     bool
	lastFired int
}

func newReminderEngine() *reminderEngine {
	return &reminderEngine{rules: map[string]map[string]*reminderState{}}
}

func (e *reminderEngine) register(sessionID string, rule ReminderRule) error {
	if e == nil {
		return errors.New("api: reminder engine is nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("api: reminder requires session id")
	}
	if strings.TrimSpace(rule.ID) == "" {
		return errors.New("api: reminder rule requires id")
	}
	if rule.Every <= 0 {
		return fmt.Errorf("api: reminder rule %s: every must be positive", rule.ID)
	}
	switch rule.Clock {
	case ClockRounds, ClockUserTurns:
	default:
		return fmt.Errorf("api: reminder rule %s: unknown clock %q", rule.ID, rule.Clock)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rules[sessionID] == nil {
		e.rules[sessionID] = map[string]*reminderState{}
	}
	if _, exists := e.rules[sessionID][rule.ID]; exists {
		return fmt.Errorf("api: reminder rule %s already registered on session %s", rule.ID, sessionID)
	}
	e.rules[sessionID][rule.ID] = &reminderState{rule: rule, lastFired: 0}
	return nil
}

func (e *reminderEngine) unregister(sessionID, ruleID string) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rules := e.rules[sessionID]
	if rules == nil {
		return false
	}
	if _, ok := rules[ruleID]; !ok {
		return false
	}
	delete(rules, ruleID)
	return true
}

func (e *reminderEngine) active(sessionID string) []ReminderRule {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []ReminderRule
	for _, st := range e.rules[sessionID] {
		out = append(out, st.rule)
	}
	return out
}

// advance 推进指定时钟并返回本 tick 到期的规则。只推进计数与判定，
// 不记录触发——冷却由 confirmFired 在注入成功（已 commit）后写入，
// 未 commit 的到期不重置冷却，下个周期点自然重试。
func (e *reminderEngine) advance(sessionID string, clock ReminderClock) []dueReminder {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var due []dueReminder
	for _, st := range e.rules[sessionID] {
		if st.rule.Clock != clock {
			continue
		}
		st.ticks++
		if st.ticks%st.rule.Every != 0 {
			continue
		}
		if st.fired && st.ticks-st.lastFired < st.rule.Cooldown {
			continue
		}
		due = append(due, dueReminder{rule: st.rule, atTick: st.ticks})
	}
	return due
}

// confirmFired 在到期注入成功后记录触发时刻（冷却起点）。注入失败时
// 不调用，规则在下个周期点重试——防"注入被丢弃后永不再触发"。
func (e *reminderEngine) confirmFired(sessionID, ruleID string, atTick int) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if st, ok := e.rules[sessionID][ruleID]; ok {
		st.fired = true
		st.lastFired = atTick
	}
}

// RegisterReminder 为会话注册一条周期提醒规则；同会话同 ID 重复注册报错。
func (rt *Runtime) RegisterReminder(sessionID string, rule ReminderRule) error {
	if rt == nil || rt.reminders == nil {
		return errors.New("api: runtime reminder engine unavailable")
	}
	return rt.reminders.register(sessionID, rule)
}

// UnregisterReminder 注销会话上的提醒规则，返回是否存在并被删除。
func (rt *Runtime) UnregisterReminder(sessionID, ruleID string) bool {
	if rt == nil || rt.reminders == nil {
		return false
	}
	return rt.reminders.unregister(sessionID, ruleID)
}

// ActiveReminders 返回会话当前活跃的提醒规则快照。
func (rt *Runtime) ActiveReminders(sessionID string) []ReminderRule {
	if rt == nil || rt.reminders == nil {
		return nil
	}
	return rt.reminders.active(sessionID)
}

// observeReminderTick 由 runLoop 在会话事实 commit 后喂入：用户输入落盘后
// 喂 ClockUserTurns，assistant 消息落盘后喂 ClockRounds。到期规则合并为
// 一条 REMINDER 注入经统一入口路由（引擎不直接写会话）。
func (rt *Runtime) observeReminderTick(sessionID string, clock ReminderClock) {
	if rt == nil || rt.reminders == nil {
		return
	}
	due := rt.reminders.advance(sessionID, clock)
	if len(due) == 0 {
		return
	}
	var builder strings.Builder
	for i, d := range due {
		if i > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString("[Reminder] ")
		builder.WriteString(d.rule.Text)
	}
	if injectErr := rt.Inject(sessionID, Injection{
		Kind:   InjectionKindReminder,
		Policy: mergedReminderPolicy(due),
		Message: message.Message{
			Role:    "user",
			Content: builder.String(),
		},
	}); injectErr != nil {
		// 注入未 commit（被拒绝/排队失败）：不记冷却，下个周期点重试。
		runtimeLogger.warnf("[api:reminder] session=%s clock=%s inject failed: %v", sessionID, clock, injectErr)
		return
	}
	for _, d := range due {
		rt.reminders.confirmFired(sessionID, d.rule.ID, d.atTick)
	}
}

// mergedReminderPolicy 合并到期批次的 Barrier 语义：按送达保证取最保守——
// 任一规则要求 DEFER 则整批排队，其次 REJECT，否则 DROP。
func mergedReminderPolicy(due []dueReminder) BarrierPolicy {
	policy := BarrierDrop
	for _, d := range due {
		switch d.rule.Policy {
		case BarrierDefer:
			return BarrierDefer
		case BarrierReject:
			policy = BarrierReject
		}
	}
	return policy
}
