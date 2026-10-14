package api

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// scheduledFirePrefix 是 cron 到期注入消息的文案前缀，与 [Reminder] 同风格，
// 让模型能分辨该回合消息来自定时任务而非用户新输入。
const scheduledFirePrefix = "[Scheduled] "

// CronTask 描述一个定时任务。Expr 为标准 5 字段 cron 表达式（分 时 日 月 星期）；
// Recurring 为 true 按表达式周期重复触发，为 false 在下一个匹配时刻触发一次后
// 自动删除。Durable 标记持久化意图：落盘与重启恢复由 cron_store 提供（当前
// 仅内存，进程重启即失）。
type CronTask struct {
	ID        string
	SessionID string
	Expr      string
	Prompt    string
	Recurring bool
	Durable   bool
	CreatedAt time.Time
	NextRun   time.Time
	LastRun   time.Time // 零值表示尚未触发过
}

// CronTaskSpec 是创建定时任务的入参。
type CronTaskSpec struct {
	SessionID string
	Expr      string
	Prompt    string
	Recurring bool
	Durable   bool
}

// cronTaskState 是引擎内部的任务状态：任务快照 + 已解析表达式（表达式不可变，
// 可在锁外安全复用）。
type cronTaskState struct {
	task CronTask
	expr *cronExpr
}

// dueCronTask 是一次到期判定的快照：fire 期间不持锁，全部字段在快照内。
type dueCronTask struct {
	id        string
	sessionID string
	prompt    string
	recurring bool
	durable   bool
	expr      *cronExpr
}

// cronEngine 是内存态 cron 调度引擎。引擎只负责计算"哪些到期"并把到期任务
// 经统一注入入口写进会话（SCHEDULED kind），不直接驱动模型运行——宿主经
// Options.ScheduledFireHandler 感知到期并发起唤醒回合。
//
// 推进语义与 reminderEngine 一致：注入成功才推进（recurring 重算 NextRun /
// one-shot 删除），注入失败不推进，下个 tick 重试——防"注入被拒后任务永不触发"。
type cronEngine struct {
	inject func(sessionID string, in Injection) error
	onFire func(CronTask)
	store  *cronStore // 非 nil 时 durable 任务随变更落盘并在启动时恢复

	mu    sync.Mutex
	tasks map[string]*cronTaskState
	seq   int

	clock func() time.Time // 可注入时钟，默认 time.Now
	tick  time.Duration    // 循环扫描间隔，生产 30s，测试可调小

	wake      chan struct{}
	started   atomic.Bool
	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	done      chan struct{}
}

func newCronEngine(inject func(sessionID string, in Injection) error, onFire func(CronTask)) *cronEngine {
	return &cronEngine{
		inject: inject,
		onFire: onFire,
		tasks:  map[string]*cronTaskState{},
		clock:  time.Now,
		tick:   30 * time.Second,
		wake:   make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// add 校验并登记任务，返回带 ID 与初始 NextRun 的任务快照。立即到期的任务
// 经 wake 唤醒循环扫描，不必等下一个 tick。
func (e *cronEngine) add(spec CronTaskSpec, now time.Time) (CronTask, error) {
	if strings.TrimSpace(spec.SessionID) == "" {
		return CronTask{}, errors.New("api: cron 任务需要会话 id")
	}
	if strings.TrimSpace(spec.Prompt) == "" {
		return CronTask{}, errors.New("api: cron 任务需要提示词")
	}
	expr, err := parseCronExpr(spec.Expr)
	if err != nil {
		return CronTask{}, err
	}
	next := expr.next(now)
	if next.IsZero() {
		return CronTask{}, fmt.Errorf("api: cron 表达式 %q 未来 4 年内无匹配时刻", spec.Expr)
	}
	task, err := e.insert(spec, expr, next, now)
	if err != nil {
		return CronTask{}, err
	}
	if spec.Durable {
		e.persistDurable()
	}
	e.wakeSignal()
	return task, nil
}

// insert 在锁内登记任务并执行 durable 总量上限校验。
func (e *cronEngine) insert(spec CronTaskSpec, expr *cronExpr, next, now time.Time) (CronTask, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if spec.Durable {
		// durable 总量上限：超限拒绝创建（对齐 comate scheduled_prompts 语义）。
		count := 0
		for _, st := range e.tasks {
			if st.task.Durable {
				count++
			}
		}
		if count >= cronDurableLimit {
			return CronTask{}, fmt.Errorf("api: cron durable 任务已达上限 %d，请先删除部分任务", cronDurableLimit)
		}
	}
	e.seq++
	task := CronTask{
		ID:        "cron-" + strconv.Itoa(e.seq),
		SessionID: spec.SessionID,
		Expr:      spec.Expr,
		Prompt:    spec.Prompt,
		Recurring: spec.Recurring,
		Durable:   spec.Durable,
		CreatedAt: now,
		NextRun:   next,
	}
	e.tasks[task.ID] = &cronTaskState{task: task, expr: expr}
	return task, nil
}

func (e *cronEngine) remove(id string) bool {
	e.mu.Lock()
	st, ok := e.tasks[id]
	if ok {
		delete(e.tasks, id)
	}
	e.mu.Unlock()
	if ok && st.task.Durable && e.store != nil {
		e.persistDurable()
	}
	return ok
}

// list 返回任务快照（按创建时间排序，同刻按 ID 保证确定性）；sessionID 为
// 空时返回全部任务。
func (e *cronEngine) list(sessionID string) []CronTask {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []CronTask
	for _, st := range e.tasks {
		if sessionID == "" || st.task.SessionID == sessionID {
			out = append(out, st.task)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// start 启动扫描循环（幂等）。循环随 stopCh 退出并关闭 done。
func (e *cronEngine) start() {
	e.startOnce.Do(func() {
		e.started.Store(true)
		go e.loop()
	})
}

// stop 停止扫描循环并等待退出（未启动过则只置位停止标记）。
func (e *cronEngine) stop() {
	e.stopOnce.Do(func() { close(e.stopCh) })
	if e.started.Load() {
		<-e.done
	}
}

func (e *cronEngine) loop() {
	defer close(e.done)
	ticker := time.NewTicker(e.tick)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.fireDue()
		case <-e.wake:
			e.fireDue()
		}
	}
}

func (e *cronEngine) wakeSignal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// dueTasks 返回 NextRun <= now 的到期任务快照。只读判定，不推进状态。
func (e *cronEngine) dueTasks(now time.Time) []dueCronTask {
	e.mu.Lock()
	defer e.mu.Unlock()
	var due []dueCronTask
	for _, st := range e.tasks {
		if st.task.NextRun.IsZero() || st.task.NextRun.After(now) {
			continue
		}
		due = append(due, dueCronTask{
			id:        st.task.ID,
			sessionID: st.task.SessionID,
			prompt:    st.task.Prompt,
			recurring: st.task.Recurring,
			durable:   st.task.Durable,
			expr:      st.expr,
		})
	}
	return due
}

// fireDue 扫描一轮到期任务并逐个触发。由循环 goroutine 周期调用；
// 扫描前先清理超期 durable 任务（7 天 TTL）。
func (e *cronEngine) fireDue() {
	now := e.clock()
	if e.pruneExpired(now) {
		e.persistDurable()
	}
	for _, d := range e.dueTasks(now) {
		e.fireOne(d, now)
	}
}

// fireOne 注入到期任务；注入成功才推进任务状态，失败留给下个 tick 重试。
func (e *cronEngine) fireOne(d dueCronTask, now time.Time) {
	if err := e.inject(d.sessionID, Injection{
		Kind:   InjectionKindScheduled,
		Policy: BarrierDefer,
		Message: message.Message{
			Role:    "user",
			Content: scheduledFirePrefix + d.prompt,
		},
	}); err != nil {
		runtimeLogger.warnf("[api:cron] task=%s session=%s inject failed: %v", d.id, d.sessionID, err)
		return
	}
	snapshot, ok := e.advanceAfterFire(d, now)
	if !ok {
		return
	}
	if d.durable {
		e.persistDurable()
	}
	if e.onFire != nil {
		e.onFire(snapshot)
	}
}

// advanceAfterFire 推进已成功注入的任务：recurring 重算 NextRun（以触发时刻
// 为锚点，注入延迟大时不补跑错过周期），one-shot 记录 LastRun 后删除。任务
// 已被并发删除时返回 false。
func (e *cronEngine) advanceAfterFire(d dueCronTask, now time.Time) (CronTask, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.tasks[d.id]
	if !ok {
		return CronTask{}, false
	}
	st.task.LastRun = now
	if !d.recurring {
		snapshot := st.task
		delete(e.tasks, d.id)
		return snapshot, true
	}
	next := d.expr.next(now)
	if next.IsZero() {
		next = d.expr.next(now.Add(time.Minute))
	}
	st.task.NextRun = next
	return st.task, true
}

// attachStore 绑定持久化并立即执行一次恢复。恢复中有条目被清理时回写文件，
// 让落盘内容与内存一致（自愈坏条目与超期任务）。
func (e *cronEngine) attachStore(store *cronStore) {
	e.store = store
	if dropped := e.restore(); dropped > 0 {
		e.persistDurable()
	}
}

// restore 从落盘文件恢复 durable 任务，返回被清理的条目数：
//   - 超过 7 天（cronDurableTTL）的条目按过期清理；
//   - 错过触发点（NextRun <= now）的一次性任务按过期处理，不补跑；
//   - recurring 错过周期不补跑，NextRun 以重启时刻为锚点重算未来触发点；
//   - 表达式失效或字段缺失的坏条目跳过（load 层已容错一层）。
//
// 序号计数器推进到已恢复 ID 的最大值，避免新建任务与恢复任务撞 ID。
func (e *cronEngine) restore() int {
	if e.store == nil {
		return 0
	}
	now := e.clock()
	loaded := e.store.load()
	e.mu.Lock()
	maxSeq := 0
	for _, task := range loaded {
		if n := cronSeqNumber(task.ID); n > maxSeq {
			maxSeq = n
		}
	}
	dropped := 0
	for _, task := range loaded {
		if now.Sub(task.CreatedAt) > cronDurableTTL {
			dropped++
			continue
		}
		expr, err := parseCronExpr(task.Expr)
		if err != nil {
			runtimeLogger.warnf("[api:cron] durable 任务 %s 表达式失效，跳过: %v", task.ID, err)
			dropped++
			continue
		}
		if !task.NextRun.After(now) {
			if !task.Recurring {
				dropped++ // 重启期间错过触发点：过期处理，不补跑
				continue
			}
			next := expr.next(now)
			if next.IsZero() {
				dropped++
				continue
			}
			task.NextRun = next
		}
		e.tasks[task.ID] = &cronTaskState{task: task, expr: expr}
	}
	e.seq = maxSeq
	e.mu.Unlock()
	return dropped
}

// persistDurable 把当前 durable 任务快照落盘（引擎锁外做文件 I/O）。
// 失败只告警：内存态仍正确，下次变更会重新落盘。
func (e *cronEngine) persistDurable() {
	if e.store == nil {
		return
	}
	e.mu.Lock()
	tasks := make([]CronTask, 0, len(e.tasks))
	for _, st := range e.tasks {
		if st.task.Durable {
			tasks = append(tasks, st.task)
		}
	}
	e.mu.Unlock()
	if err := e.store.save(tasks); err != nil {
		runtimeLogger.warnf("[api:cron] durable save failed: %v", err)
	}
}

// pruneExpired 清理超过 7 天的 durable 任务，返回是否有清理发生。
func (e *cronEngine) pruneExpired(now time.Time) bool {
	if e.store == nil {
		return false
	}
	e.mu.Lock()
	pruned := false
	for id, st := range e.tasks {
		if st.task.Durable && now.Sub(st.task.CreatedAt) > cronDurableTTL {
			delete(e.tasks, id)
			pruned = true
		}
	}
	e.mu.Unlock()
	return pruned
}

// cronSeqNumber 解析 "cron-N" 形态 ID 的序号；不匹配返回 0。
func cronSeqNumber(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "cron-"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// AddCronTask 为会话创建定时任务并返回任务快照。Expr 为标准 5 字段 cron 表达式
// （分 时 日 月 星期，支持 *、区间、步进、逗号多值），非法表达式与未来 4 年
// 无匹配时刻的表达式在创建时即报错。Recurring 为 true 按周期重复触发，为 false
// 在下一个匹配时刻触发一次后自动删除。
//
// Durable 为 true 时任务落盘到 {ProjectRoot}/.agents/scheduled_prompts.json 并在
// 进程重启后恢复（7 天过期、总量上限 50、错过触发点的一次性任务不补跑）；
// 非 durable 仅内存，重启即失。
//
// 到期任务以 SCHEDULED 注入经统一入口进入会话：会话闲时直写 history，忙时
// （BarrierDefer）排队到回合结束交付，不与用户回合交错。注入成功后回调
// Options.ScheduledFireHandler（若配置），宿主可据此发起唤醒回合让到期任务
// 真正被模型执行。
func (rt *Runtime) AddCronTask(spec CronTaskSpec) (CronTask, error) {
	if rt == nil || rt.crons == nil {
		return CronTask{}, errors.New("api: runtime cron engine unavailable")
	}
	task, err := rt.crons.add(spec, rt.crons.clock())
	if err != nil {
		return CronTask{}, err
	}
	rt.crons.start()
	return task, nil
}

// RemoveCronTask 删除定时任务，返回是否存在并被删除。
func (rt *Runtime) RemoveCronTask(id string) bool {
	if rt == nil || rt.crons == nil {
		return false
	}
	return rt.crons.remove(id)
}

// CronTasks 返回会话的定时任务快照（按创建时间排序）；sessionID 为空返回全部。
func (rt *Runtime) CronTasks(sessionID string) []CronTask {
	if rt == nil || rt.crons == nil {
		return nil
	}
	return rt.crons.list(strings.TrimSpace(sessionID))
}
