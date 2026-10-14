package toolbuiltin

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

// collectTaskEvents 挂载一个收集型事件出口，返回 ctx 与读取函数。
func collectTaskEvents() (context.Context, func() []tool.ToolEvent) {
	var mu sync.Mutex
	var events []tool.ToolEvent
	ctx := tool.WithToolEventSink(context.Background(), func(_ context.Context, ev tool.ToolEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	})
	return ctx, func() []tool.ToolEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]tool.ToolEvent(nil), events...)
	}
}

// taskParams 构造一次 task 工具调用的入参：每条 [content, status]。
func taskParams(subject string, entries ...[2]string) map[string]interface{} {
	items := make([]interface{}, 0, len(entries))
	for _, e := range entries {
		entry := map[string]interface{}{"content": e[0]}
		if e[1] != "" {
			entry["status"] = e[1]
		}
		items = append(items, entry)
	}
	return map[string]interface{}{"subject": subject, "items": items}
}

func TestTaskStoreReplaceSnapshot(t *testing.T) {
	store := &TaskStore{}
	snap := store.Replace("发版巡检", []TodoItem{
		{Content: "拉取告警", Status: TodoStatusInProgress},
		{Content: "核对指标", Status: TodoStatusPending},
	})
	if snap.Subject != "发版巡检" {
		t.Errorf("Subject = %q, 期望 发版巡检", snap.Subject)
	}
	if len(snap.Items) != 2 || snap.Items[0].Status != TodoStatusInProgress || snap.Items[1].Status != TodoStatusPending {
		t.Errorf("快照条目不符: %+v", snap.Items)
	}
	if snap.UpdatedAt.IsZero() {
		t.Error("UpdatedAt 未刷新")
	}
	if got := snap.CompletedCount(); got != 0 {
		t.Errorf("CompletedCount = %d, 期望 0", got)
	}

	// 再次覆盖：store 权威态随之变化。
	snap = store.Replace("新主题", []TodoItem{{Content: "唯一步骤", Status: TodoStatusCompleted}})
	if snap.Subject != "新主题" || len(snap.Items) != 1 || snap.CompletedCount() != 1 {
		t.Errorf("覆盖后快照不符: %+v", snap)
	}
}

func TestTaskStoreSnapshotDeepCopy(t *testing.T) {
	store := &TaskStore{}
	store.Replace("主题", []TodoItem{{Content: "a", Status: TodoStatusPending}, {Content: "b", Status: TodoStatusPending}})

	snap := store.Snapshot()
	snap.Items[0].Status = TodoStatusCompleted
	snap.Items[0].Content = "篡改"

	again := store.Snapshot()
	if again.Items[0].Status != TodoStatusPending || again.Items[0].Content != "a" {
		t.Errorf("快照未深拷贝，store 内部状态被外部修改: %+v", again.Items)
	}
}

func TestTaskToolEmitsFullSnapshotSequence(t *testing.T) {
	ctx, events := collectTaskEvents()
	impl := NewTaskTool(nil)

	steps := []map[string]interface{}{
		taskParams("发版巡检", [2]string{"拉取告警", ""}, [2]string{"核对指标", ""}, [2]string{"输出报告", ""}),
		taskParams("发版巡检", [2]string{"拉取告警", "in_progress"}, [2]string{"核对指标", ""}, [2]string{"输出报告", ""}),
		taskParams("发版巡检", [2]string{"拉取告警", "completed"}, [2]string{"核对指标", "in_progress"}, [2]string{"输出报告", ""}),
	}
	for i, params := range steps {
		if _, err := impl.Execute(ctx, params); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}

	evts := events()
	if len(evts) != len(steps) {
		t.Fatalf("期望 %d 个事件, 得到 %d", len(steps), len(evts))
	}
	wantStates := [][]TodoStatus{
		{TodoStatusPending, TodoStatusPending, TodoStatusPending},
		{TodoStatusInProgress, TodoStatusPending, TodoStatusPending},
		{TodoStatusCompleted, TodoStatusInProgress, TodoStatusPending},
	}
	for i, ev := range evts {
		if ev.Type != TodoUpdatedEventType {
			t.Errorf("事件 %d Type = %q, 期望 %q", i, ev.Type, TodoUpdatedEventType)
		}
		if ev.Name != taskToolName {
			t.Errorf("事件 %d Name = %q, 期望 %q", i, ev.Name, taskToolName)
		}
		snap, ok := ev.Payload.(TodoSnapshot)
		if !ok {
			t.Fatalf("事件 %d 负载类型 %T, 期望 TodoSnapshot", i, ev.Payload)
		}
		if snap.Subject != "发版巡检" {
			t.Errorf("事件 %d Subject = %q", i, snap.Subject)
		}
		// 硬约束：每个事件都携带全量清单（3 条），而非增量 diff。
		if len(snap.Items) != 3 {
			t.Fatalf("事件 %d 快照条目数 = %d, 期望全量 3", i, len(snap.Items))
		}
		for j, want := range wantStates[i] {
			if snap.Items[j].Status != want {
				t.Errorf("事件 %d 条目 %d 状态 = %q, 期望 %q", i, j, snap.Items[j].Status, want)
			}
		}
		if snap.UpdatedAt.IsZero() {
			t.Errorf("事件 %d UpdatedAt 为零值", i)
		}
	}
}

func TestTaskToolNoSinkNoPanic(t *testing.T) {
	impl := NewTaskTool(nil)
	res, err := impl.Execute(context.Background(), taskParams("主题", [2]string{"一步", ""}))
	if err != nil {
		t.Fatalf("无事件出口时 Execute 不应失败: %v", err)
	}
	if !res.Success {
		t.Error("Success 应为 true")
	}
	if _, ok := res.Data.(TodoSnapshot); !ok {
		t.Errorf("Data 类型 %T, 期望 TodoSnapshot", res.Data)
	}
}

func TestTaskToolResultCarriesSnapshot(t *testing.T) {
	ctx, events := collectTaskEvents()
	impl := NewTaskTool(nil)
	res, err := impl.Execute(ctx, taskParams("发版巡检", [2]string{"拉取告警", "completed"}, [2]string{"核对指标", ""}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	snap, ok := res.Data.(TodoSnapshot)
	if !ok {
		t.Fatalf("Data 类型 %T, 期望 TodoSnapshot", res.Data)
	}
	if snap.CompletedCount() != 1 || len(snap.Items) != 2 {
		t.Errorf("Data 快照不符: %+v", snap)
	}
	if !strings.Contains(res.Output, "[x] 拉取告警") || !strings.Contains(res.Output, "完成 1/2") {
		t.Errorf("Output 渲染不符:\n%s", res.Output)
	}
	if len(events()) != 1 {
		t.Errorf("事件应发送一次, 得到 %d", len(events()))
	}
}

func TestTaskToolValidation(t *testing.T) {
	impl := NewTaskTool(nil)
	cases := []struct {
		name   string
		params map[string]interface{}
	}{
		{"缺 items", map[string]interface{}{"subject": "x"}},
		{"items 非数组", map[string]interface{}{"items": "no"}},
		{"条目非对象", map[string]interface{}{"items": []interface{}{"no"}}},
		{"缺 content", map[string]interface{}{"items": []interface{}{map[string]interface{}{"status": "pending"}}}},
		{"content 为空", map[string]interface{}{"items": []interface{}{map[string]interface{}{"content": "  "}}}},
		{"status 非法", map[string]interface{}{"items": []interface{}{map[string]interface{}{"content": "a", "status": "done"}}}},
		{"status 非字符串", map[string]interface{}{"items": []interface{}{map[string]interface{}{"content": "a", "status": 3}}}},
		{"subject 非字符串", map[string]interface{}{"subject": 1, "items": []interface{}{}}},
	}
	for _, tc := range cases {
		if _, err := impl.Execute(context.Background(), tc.params); err == nil {
			t.Errorf("%s: 期望报错, 得到 nil", tc.name)
		}
	}
}

func TestTaskToolSchema(t *testing.T) {
	schema := NewTaskTool(nil).Schema()
	if schema == nil || schema.Type != "object" {
		t.Fatalf("Schema 异常: %+v", schema)
	}
	if _, ok := schema.Properties["items"]; !ok {
		t.Error("缺少 items 属性")
	}
	if _, ok := schema.Properties["subject"]; !ok {
		t.Error("缺少 subject 属性")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "items" {
		t.Errorf("Required = %v, 期望 [items]", schema.Required)
	}
}
