package api

import (
	"context"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/model"
	"github.com/lzq70112/agentsdk-go/pkg/tool"
	toolbuiltin "github.com/lzq70112/agentsdk-go/pkg/tool/builtin"
)

// TestRunStreamEmitsTodoUpdated 验证 task 工具的全量快照事件经运行时
// 适配后从事件流管道到达消费者：事件类型、负载快照、会话标识齐全。
func TestRunStreamEmitsTodoUpdated(t *testing.T) {
	root := newClaudeProject(t)
	taskTool := toolbuiltin.NewTaskTool(nil)
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{
			Role: "assistant",
			ToolCalls: []model.ToolCall{{
				ID:   "tool_1",
				Name: taskTool.Name(),
				Arguments: map[string]any{
					"subject": "发版巡检",
					"items": []any{
						map[string]any{"content": "拉取告警"},
						map[string]any{"content": "核对指标", "status": "in_progress"},
					},
				},
			}},
		}},
		{Message: model.Message{Role: "assistant", Content: "done"}},
	}}

	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl, Tools: []tool.Tool{taskTool}})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	stream, err := rt.RunStream(context.Background(), Request{Prompt: "go"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var snapshots []toolbuiltin.TodoSnapshot
	var sessionIDs []string
	for evt := range stream {
		if evt.Type != toolbuiltin.TodoUpdatedEventType {
			continue
		}
		snap, ok := evt.Output.(toolbuiltin.TodoSnapshot)
		if !ok {
			t.Fatalf("事件负载类型 %T, 期望 toolbuiltin.TodoSnapshot", evt.Output)
		}
		snapshots = append(snapshots, snap)
		sessionIDs = append(sessionIDs, evt.SessionID)
	}

	if len(snapshots) != 1 {
		t.Fatalf("期望 1 个 todo_updated 事件, 得到 %d", len(snapshots))
	}
	snap := snapshots[0]
	// 快照为全量清单：主题 + 全部条目及状态 + 更新时间。
	if snap.Subject != "发版巡检" || len(snap.Items) != 2 || snap.UpdatedAt.IsZero() {
		t.Fatalf("快照不全量: %+v", snap)
	}
	if snap.Items[1].Status != toolbuiltin.TodoStatusInProgress {
		t.Errorf("条目 1 状态 = %q, 期望 in_progress", snap.Items[1].Status)
	}
	if sessionIDs[0] == "" {
		t.Error("事件缺少会话标识")
	}
}

// TestBuiltinRegistrationIncludesTask 验证默认工具面注册 task 工具。
func TestBuiltinRegistrationIncludesTask(t *testing.T) {
	reg := tool.NewRegistry()
	if _, err := registerTools(reg, Options{}, nil, nil); err != nil {
		t.Fatalf("registerTools: %v", err)
	}
	impl, err := reg.Get("task")
	if err != nil {
		t.Fatalf("task 工具未注册: %v", err)
	}
	if impl.Name() != "task" {
		t.Errorf("Name = %q, 期望 task", impl.Name())
	}
}
