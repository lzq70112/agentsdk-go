package toolbuiltin

import "time"

// TodoStatus 描述清单条目的进行状态。
type TodoStatus string

const (
	TodoStatusPending    TodoStatus = "pending"
	TodoStatusInProgress TodoStatus = "in_progress"
	TodoStatusCompleted  TodoStatus = "completed"
)

// validTodoStatus 校验外部传入的状态值是否合法。
func validTodoStatus(s TodoStatus) bool {
	switch s {
	case TodoStatusPending, TodoStatusInProgress, TodoStatusCompleted:
		return true
	}
	return false
}

// TodoItem 是清单中的一个条目。数组顺序即展示顺序，不维护独立 id：
// 写入语义为全量覆盖（对齐 claude code TodoWrite），条目身份由位置承载。
type TodoItem struct {
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
}

// TodoSnapshot 是清单的全量快照：主题、全部条目及状态、更新时间。
// 硬约束：快照始终是全量清单而非增量 diff，消费方（IM 进展卡片）整卡重绘。
type TodoSnapshot struct {
	Subject   string     `json:"subject"`
	Items     []TodoItem `json:"items"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// CompletedCount 返回已完成条目数，供进展卡片渲染"N/M 步"。
func (s TodoSnapshot) CompletedCount() int {
	n := 0
	for _, it := range s.Items {
		if it.Status == TodoStatusCompleted {
			n++
		}
	}
	return n
}
