package toolbuiltin

import (
	"context"
	"fmt"
	"strings"

	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

// taskToolName 是 task 工具的注册名。
const taskToolName = "task"

// TodoUpdatedEventType 是 task 工具的清单变更事件协议类型，负载为
// TodoSnapshot 全量快照。api 层将其适配为同名 StreamEvent 经既有
// 事件流管道透传给消费方（IM 进展卡片等）。
const TodoUpdatedEventType = "todo_updated"

// TaskTool 让 agent 维护一份任务清单并在每次变更时推送全量快照。
// 写入语义借鉴 claude code TodoWrite：单工具全量覆盖，调用方每次传完整
// 清单（新增=重写含新条目，完成=重写对应条目状态），不维护条目 id。
// 每次写入后经 ctx 上的 ToolEventSink 发出 TodoUpdatedEventType 事件，
// 同时 ToolResult.Data 携带同一快照供同步消费。
type TaskTool struct {
	store *TaskStore
}

// NewTaskTool 创建 task 工具；store 为权威状态源，nil 时内部新建独立实例。
func NewTaskTool(store *TaskStore) *TaskTool {
	if store == nil {
		store = &TaskStore{}
	}
	return &TaskTool{store: store}
}

// Store 返回工具绑定的权威状态源。
func (t *TaskTool) Store() *TaskStore { return t.store }

// Name 返回 tool 的唯一标识。
func (t *TaskTool) Name() string { return taskToolName }

// Description 返回给 LLM 看的 tool 说明。
func (t *TaskTool) Description() string {
	return `维护当前会话的任务清单，并向用户界面推送进展。
适用于多步骤任务开始时列出计划、执行中随进展更新状态。

写入规则：items 永远传完整清单（全量覆盖，不是增量追加）——
- 新增步骤：重写清单并附上新条目（status 留空或 pending）
- 开始某步：把该条 status 改为 in_progress，建议同时只有一个 in_progress
- 完成某步：把该条 status 改为 completed
每次调用后界面收到全量清单快照并整卡刷新。`
}

// Schema 描述工具入参。
func (t *TaskTool) Schema() *tool.JSONSchema {
	return &tool.JSONSchema{
		Type: "object",
		Properties: map[string]interface{}{
			"subject": map[string]interface{}{
				"type":        "string",
				"description": "清单主题，一句话说明这组步骤要达成什么。",
			},
			"items": map[string]interface{}{
				"type":        "array",
				"description": "完整清单条目（全量覆盖，非增量追加）。",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{
							"type":        "string",
							"description": "条目内容，祈使句描述一个可验证的步骤。",
						},
						"status": map[string]interface{}{
							"type":        "string",
							"enum":        []interface{}{string(TodoStatusPending), string(TodoStatusInProgress), string(TodoStatusCompleted)},
							"description": "条目状态，缺省 pending。",
						},
					},
					"required": []interface{}{"content"},
				},
			},
		},
		Required: []string{"items"},
	}
}

// Execute 全量覆盖清单，推送事件并返回快照。
func (t *TaskTool) Execute(ctx context.Context, params map[string]interface{}) (*tool.ToolResult, error) {
	subject, items, err := parseTaskParams(params)
	if err != nil {
		return nil, err
	}
	snap := t.store.Replace(subject, items)

	// 事件出口未挂载（非流式运行/直连单测）时跳过推送，不影响工具结果。
	if sink := tool.ToolEventSinkFromContext(ctx); sink != nil {
		sink(ctx, tool.ToolEvent{Type: TodoUpdatedEventType, Name: taskToolName, Payload: snap})
	}

	return &tool.ToolResult{
		Success: true,
		Output:  renderTodoSnapshot(snap),
		Data:    snap,
	}, nil
}

// parseTaskParams 解析并校验入参：items 必填且每个条目必须有 content，
// status 可缺省、出现时必须是合法状态。
func parseTaskParams(params map[string]interface{}) (string, []TodoItem, error) {
	subject := ""
	if v, ok := params["subject"]; ok {
		s, ok := v.(string)
		if !ok {
			return "", nil, fmt.Errorf("task: subject 必须是字符串")
		}
		subject = strings.TrimSpace(s)
	}

	rawItems, ok := params["items"]
	if !ok {
		return "", nil, fmt.Errorf("task: 缺少必填参数 items")
	}
	list, ok := rawItems.([]interface{})
	if !ok {
		return "", nil, fmt.Errorf("task: items 必须是数组")
	}
	items := make([]TodoItem, 0, len(list))
	for i, raw := range list {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			return "", nil, fmt.Errorf("task: items[%d] 必须是对象", i)
		}
		content, _ := entry["content"].(string)
		content = strings.TrimSpace(content)
		if content == "" {
			return "", nil, fmt.Errorf("task: items[%d].content 不能为空", i)
		}
		item := TodoItem{Content: content, Status: TodoStatusPending}
		if statusRaw, ok := entry["status"]; ok && statusRaw != nil {
			status, ok := statusRaw.(string)
			if !ok {
				return "", nil, fmt.Errorf("task: items[%d].status 必须是字符串", i)
			}
			st := TodoStatus(strings.TrimSpace(status))
			if !validTodoStatus(st) {
				return "", nil, fmt.Errorf("task: items[%d].status 非法: %q（允许 pending/in_progress/completed）", i, status)
			}
			item.Status = st
		}
		items = append(items, item)
	}
	return subject, items, nil
}

// renderTodoSnapshot 生成给 LLM 看的文本渲染（状态标记 + 完成计数）。
func renderTodoSnapshot(snap TodoSnapshot) string {
	var b strings.Builder
	if snap.Subject != "" {
		fmt.Fprintf(&b, "清单：%s\n", snap.Subject)
	}
	for i, it := range snap.Items {
		mark := "[ ]"
		switch it.Status {
		case TodoStatusInProgress:
			mark = "[~]"
		case TodoStatusCompleted:
			mark = "[x]"
		}
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, mark, it.Content)
	}
	fmt.Fprintf(&b, "完成 %d/%d。", snap.CompletedCount(), len(snap.Items))
	return b.String()
}
