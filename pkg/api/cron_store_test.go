package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCronStoreSaveLoadRoundTrip 固化落盘往返：字段无损（含零值 LastRun）。
func TestCronStoreSaveLoadRoundTrip(t *testing.T) {
	store := newCronStore(t.TempDir())
	created := at(2026, 10, 14, 10, 0)
	next := at(2026, 10, 15, 9, 0)
	last := at(2026, 10, 14, 9, 0)
	want := []CronTask{
		{ID: "cron-1", SessionID: "s1", Expr: "0 9 * * *", Prompt: "晨检", Recurring: true, Durable: true, CreatedAt: created, NextRun: next, LastRun: last},
		{ID: "cron-2", SessionID: "s2", Expr: "30 14 28 2 *", Prompt: "单次", Durable: true, CreatedAt: created, NextRun: next},
	}
	if err := store.save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := store.load()
	if len(got) != 2 {
		t.Fatalf("期望 2 条, 得到 %d", len(got))
	}
	for i, task := range got {
		src := want[i]
		if task.ID != src.ID || task.SessionID != src.SessionID || task.Expr != src.Expr ||
			task.Prompt != src.Prompt || task.Recurring != src.Recurring ||
			!task.CreatedAt.Equal(src.CreatedAt) || !task.NextRun.Equal(src.NextRun) || !task.LastRun.Equal(src.LastRun) {
			t.Fatalf("条目 %d 往返不符: got %+v want %+v", i, task, src)
		}
	}
}

// TestCronStoreLoadCorruptFile 固化降级启动：整体损坏忽略全部条目，
// 单条损坏只跳过该条，均不 crash。
func TestCronStoreLoadCorruptFile(t *testing.T) {
	dir := t.TempDir()
	store := newCronStore(dir)

	// 整体损坏
	if err := os.MkdirAll(filepath.Dir(store.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := store.load(); len(got) != 0 {
		t.Fatalf("整体损坏应降级为空, 得到 %d 条", len(got))
	}

	// 单条损坏：第二条字段类型错误 + 第三条字段缺失，仅首条存活
	content := `{"version":1,"tasks":[
		{"id":"cron-1","session_id":"s1","expr":"0 9 * * *","prompt":"ok","recurring":true,"created_at":"2026-10-14T10:00:00+08:00","next_run":"2026-10-15T09:00:00+08:00"},
		{"id":123,"session_id":"s1","expr":"0 9 * * *","prompt":"bad-type"},
		{"id":"cron-3","session_id":"","expr":"0 9 * * *","prompt":"missing"}
	]}`
	if err := os.WriteFile(store.path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := store.load()
	if len(got) != 1 || got[0].ID != "cron-1" {
		t.Fatalf("应只保留首条有效任务: %+v", got)
	}
}

// TestCronEngineRestoreDurable 固化恢复语义：recurring 错过周期重算未来、
// 一次性任务未到期保留、错过触发点的一次性任务不补跑、超 7 天清理、
// 新建任务 ID 不与恢复任务冲突。
func TestCronEngineRestoreDurable(t *testing.T) {
	dir := t.TempDir()
	store := newCronStore(dir)
	restart := at(2026, 10, 14, 10, 0)
	if err := store.save([]CronTask{
		{ID: "cron-1", SessionID: "s1", Expr: "*/5 * * * *", Prompt: "周期", Recurring: true, CreatedAt: restart.Add(-time.Hour), NextRun: restart.Add(-5 * time.Minute)},
		{ID: "cron-2", SessionID: "s1", Expr: "30 14 28 2 *", Prompt: "未来单次", CreatedAt: restart.Add(-time.Hour), NextRun: at(2027, 2, 28, 14, 30)},
		{ID: "cron-3", SessionID: "s1", Expr: "*/5 * * * *", Prompt: "错过单次", CreatedAt: restart.Add(-time.Hour), NextRun: restart.Add(-time.Minute)},
		{ID: "cron-4", SessionID: "s1", Expr: "*/5 * * * *", Prompt: "超期", Recurring: true, CreatedAt: restart.Add(-8 * 24 * time.Hour), NextRun: restart.Add(time.Hour)},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	e := newCronEngine(func(string, Injection) error { return nil }, nil)
	e.clock = func() time.Time { return restart }
	e.attachStore(store)
	t.Cleanup(e.stop)

	tasks := e.list("")
	if len(tasks) != 2 {
		t.Fatalf("期望恢复 2 条, 得到 %d: %+v", len(tasks), tasks)
	}
	// cron-1：错过周期不补跑，NextRun 以重启时刻重算（10:00 → 10:05）
	if tasks[0].ID != "cron-1" || !tasks[0].NextRun.Equal(at(2026, 10, 14, 10, 5)) {
		t.Fatalf("recurring 恢复应重算未来触发点: %+v", tasks[0])
	}
	// cron-2：未到期的 one-shot 原样保留
	if tasks[1].ID != "cron-2" || !tasks[1].NextRun.Equal(at(2027, 2, 28, 14, 30)) {
		t.Fatalf("未到期 one-shot 应保留: %+v", tasks[1])
	}

	// 被清理条目回写文件，落盘内容与内存一致
	if got := store.load(); len(got) != 2 {
		t.Fatalf("恢复后落盘应只剩 2 条, 得到 %d", len(got))
	}

	// 新建任务 ID 推进到最大恢复序号之后，不冲突
	task, err := e.add(CronTaskSpec{SessionID: "s", Expr: "*/1 * * * *", Prompt: "new", Durable: true}, restart)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if task.ID != "cron-5" {
		t.Fatalf("新建任务 ID = %q, 期望 cron-5（不与恢复任务冲突）", task.ID)
	}
}

// TestAddCronTaskDurableLimit 固化上限语义：第 51 个 durable 任务拒绝创建并
// 明确报错，非 durable 不受约束。
func TestAddCronTaskDurableLimit(t *testing.T) {
	rt := &Runtime{histories: newHistoryStore(4, nil)}
	rt.crons = newCronEngine(rt.Inject, nil)
	t.Cleanup(rt.crons.stop)
	for i := 0; i < cronDurableLimit; i++ {
		spec := CronTaskSpec{SessionID: "s", Expr: "0 12 * * *", Prompt: "x", Durable: true}
		if _, err := rt.AddCronTask(spec); err != nil {
			t.Fatalf("第 %d 个 durable 任务不应报错: %v", i+1, err)
		}
	}
	_, err := rt.AddCronTask(CronTaskSpec{SessionID: "s", Expr: "0 12 * * *", Prompt: "x", Durable: true})
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超上限应明确报错: %v", err)
	}
	if _, err := rt.AddCronTask(CronTaskSpec{SessionID: "s", Expr: "0 12 * * *", Prompt: "x"}); err != nil {
		t.Fatalf("非 durable 不受上限约束: %v", err)
	}
}

// TestCronEnginePersistsDurableChanges 固化变更即存：durable 增删与触发推进
// 都落盘；非 durable 不落盘；durable one-shot 触发后从文件消失。
func TestCronEnginePersistsDurableChanges(t *testing.T) {
	dir := t.TempDir()
	store := newCronStore(dir)
	h := newCronHarness(t)
	h.engine.attachStore(store)

	// durable recurring 创建即落盘
	rec, err := h.engine.add(CronTaskSpec{SessionID: "s1", Expr: "*/1 * * * *", Prompt: "周期", Recurring: true, Durable: true}, at(2026, 10, 14, 10, 3))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := store.load(); len(got) != 1 || got[0].ID != rec.ID {
		t.Fatalf("创建后应落盘: %+v", got)
	}

	// 非 durable 只在内存
	if _, err := h.engine.add(CronTaskSpec{SessionID: "s1", Expr: "0 9 * * *", Prompt: "内存"}, at(2026, 10, 14, 10, 3)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := store.load(); len(got) != 1 {
		t.Fatalf("非 durable 不应落盘: %+v", got)
	}

	// 触发推进落盘：NextRun 更新
	h.setTime(at(2026, 10, 14, 10, 4))
	h.engine.fireDue()
	if got := store.load(); len(got) != 1 || !got[0].NextRun.Equal(at(2026, 10, 14, 10, 5)) {
		t.Fatalf("触发后 NextRun 应落盘: %+v", got)
	}

	// durable one-shot 触发后从文件消失
	if _, err := h.engine.add(CronTaskSpec{SessionID: "s1", Expr: "*/1 * * * *", Prompt: "单次", Durable: true}, at(2026, 10, 14, 10, 4)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := store.load(); len(got) != 2 {
		t.Fatalf("one-shot 创建后应落盘 2 条: %+v", got)
	}
	h.setTime(at(2026, 10, 14, 10, 5))
	h.engine.fireDue()
	if got := store.load(); len(got) != 1 || got[0].ID != rec.ID {
		t.Fatalf("one-shot 触发后应从文件消失: %+v", got)
	}

	// 删除落盘
	if !h.engine.remove(rec.ID) {
		t.Fatal("删除应成功")
	}
	if got := store.load(); len(got) != 0 {
		t.Fatalf("删除后文件应为空: %+v", got)
	}
}

// TestCronEnginePrunesExpiredInScan 固化运行中过期清理：durable 任务超 7 天
// 在扫描轮被清理并落盘。
func TestCronEnginePrunesExpiredInScan(t *testing.T) {
	dir := t.TempDir()
	store := newCronStore(dir)
	h := newCronHarness(t)
	h.engine.attachStore(store)

	start := at(2026, 10, 14, 10, 3)
	if _, err := h.engine.add(CronTaskSpec{SessionID: "s1", Expr: "*/1 * * * *", Prompt: "长跑", Recurring: true, Durable: true}, start); err != nil {
		t.Fatalf("add: %v", err)
	}
	// 时钟推进 8 天 + 1 分钟：TTL 到期先于触发被清理
	h.setTime(start.Add(8*24*time.Hour + time.Minute))
	h.engine.fireDue()
	if tasks := h.engine.list(""); len(tasks) != 0 {
		t.Fatalf("超 7 天 durable 任务应被清理: %+v", tasks)
	}
	if got := store.load(); len(got) != 0 {
		t.Fatalf("清理后文件应为空: %+v", got)
	}
}
