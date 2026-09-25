package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

const (
	subagentStatusToolName = "subagent_status"
	subagentStopToolName   = "subagent_stop"
)

// 后台子任务的状态取值。
const (
	asyncTaskRunning  = "running"
	asyncTaskSuccess  = "success"
	asyncTaskError    = "error"
	asyncTaskCanceled = "canceled"
)

// asyncTask 跟踪一个后台派发的子 agent 任务，供 subagent_status / subagent_stop
// 查询与停止。cancel 为本任务专属的取消函数：调用后子 agent 在下一轮 runLoop
// 检查 ctx.Err() 时退出（协作式；若它正卡在不响应取消的工具调用中，退出会延迟）。
type asyncTask struct {
	id          string
	sessionID   string
	name        string
	instruction string
	status      string
	startedAt   time.Time
	finishedAt  time.Time
	output      string
	err         string
	cancel      context.CancelFunc
}

// registerTask 登记一个刚派发的后台任务。主 Runtime 已关闭时（tasks 为 nil）
// 仍可写入，因为派发前已由 dispatchAsync 校验过 asyncCtx。
func (t *subagentTool) registerTask(task *asyncTask) {
	if t == nil || task == nil || task.id == "" {
		return
	}
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	if t.tasks == nil {
		t.tasks = make(map[string]*asyncTask)
	}
	t.tasks[task.id] = task
}

// finishTask 记录后台任务终态。错误经 dispatch 的 %w 包装，errors.Is 仍可
// 识别取消，从而把主动停止与执行失败区分开。
func (t *subagentTool) finishTask(id, output string, err error) {
	if t == nil || id == "" {
		return
	}
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	task, ok := t.tasks[id]
	if !ok {
		return
	}
	task.finishedAt = time.Now()
	task.cancel = nil
	switch {
	case err == nil:
		task.status = asyncTaskSuccess
		task.output = output
	case errors.Is(err, context.Canceled):
		task.status = asyncTaskCanceled
		task.err = err.Error()
	default:
		task.status = asyncTaskError
		task.err = err.Error()
	}
}

// runningTasks 返回当前会话全部在途任务，按派发时间升序，保证输出稳定。
func (t *subagentTool) runningTasks(sessionID string) []asyncTask {
	if t == nil {
		return nil
	}
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	var out []asyncTask
	for _, task := range t.tasks {
		if task.sessionID == sessionID && task.status == asyncTaskRunning {
			out = append(out, *task)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].startedAt.Before(out[j].startedAt) })
	return out
}

// lookupTask 按子会话标识取任务，并校验归属：只允许访问当前会话派发的任务，
// 防止跨会话误查误停。
func (t *subagentTool) lookupTask(sessionID, id string) (asyncTask, bool) {
	if t == nil {
		return asyncTask{}, false
	}
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	task, ok := t.tasks[id]
	if !ok || task.sessionID != sessionID {
		return asyncTask{}, false
	}
	return *task, true
}

// cancelTask 停止当前会话的指定任务，返回其停止前状态。取消在锁外调用，避免
// 持有 taskMu 时触发子 agent 的退出路径再次取锁。
func (t *subagentTool) cancelTask(sessionID, id string) (string, bool) {
	if t == nil {
		return "", false
	}
	t.taskMu.Lock()
	task, ok := t.tasks[id]
	if !ok || task.sessionID != sessionID {
		t.taskMu.Unlock()
		return "", false
	}
	status := task.status
	cancel := task.cancel
	t.taskMu.Unlock()
	if status == asyncTaskRunning && cancel != nil {
		cancel()
	}
	return status, true
}

// cancelTasksForSession 取消指定会话全部在途后台任务，返回被取消的任务数。
// 取消在锁外逐个调用，避免持锁时触发子 agent 退出路径再次取锁。
func (t *subagentTool) cancelTasksForSession(sessionID string) int {
	if t == nil || strings.TrimSpace(sessionID) == "" {
		return 0
	}
	t.taskMu.Lock()
	var cancels []context.CancelFunc
	for _, task := range t.tasks {
		if task.sessionID == sessionID && task.status == asyncTaskRunning && task.cancel != nil {
			cancels = append(cancels, task.cancel)
		}
	}
	t.taskMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

// clearTasks 清空后台任务注册表，由主 Runtime Close 在 shutdownAsync 之后调用，
// 保证注册表生命周期与主 Runtime 一致。
func (t *subagentTool) clearTasks() {
	if t == nil {
		return
	}
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	t.tasks = nil
}

// injectAsyncResult 在后台子任务结束后把结果注入主会话 history，使主 agent 在
// 下一轮对话中直接感知任务结果。与 subMgr 的完成回注共用格式化实现，保证文案一致。
func (t *subagentTool) injectAsyncResult(sessionID, subSessionID, name, instruction, output string, err error) {
	if t == nil || t.runtime == nil || t.opts.DisableSubagentSummary {
		return
	}
	// 被取消的子任务不注入结果：取消往往伴随主回合的历史回滚，注入会把已回滚
	// 的历史重新污染。
	if err != nil && errors.Is(err, context.Canceled) {
		return
	}
	if t.runtime.histories == nil {
		return
	}
	history, ok := t.runtime.histories.Loaded(sessionID)
	if !ok {
		return
	}
	status := asyncTaskSuccess
	errText := ""
	if err != nil {
		status = asyncTaskError
		errText = err.Error()
	}
	history.Append(message.Message{
		Role:     "user",
		Content:  formatSubagentResultMessage(name, instruction, status, output, errText),
		Metadata: subagentResultMetadata(subSessionID, name),
	})
}

// subagentStatusTool 让主 agent 查询当前会话派发的后台子任务状态。
type subagentStatusTool struct {
	owner    *subagentTool
	disabled bool
}

func (t *subagentStatusTool) Name() string { return subagentStatusToolName }

func (t *subagentStatusTool) Description() string {
	return `查询当前会话中后台派发的子 agent 任务状态。
不传 sub_session_id 时列出当前会话所有在途（running）子任务；传入任务标识时返回该任务的当前状态（含已结束任务的最终结果）。`
}

func (t *subagentStatusTool) Schema() *tool.JSONSchema {
	return &tool.JSONSchema{
		Type: "object",
		Properties: map[string]any{
			"sub_session_id": map[string]any{
				"type":        "string",
				"description": "要查询的子任务标识（派发结果中的 sub_session_id）；不填则列出当前会话全部在途任务",
			},
		},
	}
}

func (t *subagentStatusTool) Execute(ctx context.Context, params map[string]any) (*tool.ToolResult, error) {
	if t.disabled {
		return &tool.ToolResult{Success: false, Output: subagentDisabledRefusal}, nil
	}
	sessionID, ok := ToolSessionIDFromContext(ctx)
	if !ok || strings.TrimSpace(sessionID) == "" {
		return &tool.ToolResult{Success: false, Output: "subagent_status tool 无法获取当前 session id"}, nil
	}
	if t.owner == nil {
		return &tool.ToolResult{Success: false, Output: "subagent_status tool 未绑定运行时"}, nil
	}

	id, _ := params["sub_session_id"].(string)
	id = strings.TrimSpace(id)
	if id != "" {
		task, found := t.owner.lookupTask(sessionID, id)
		if !found {
			return &tool.ToolResult{Success: false, Output: fmt.Sprintf("找不到子任务 %q（不属于当前会话或不存在）", id)}, nil
		}
		return &tool.ToolResult{Success: true, Output: formatTaskDetail(task)}, nil
	}

	running := t.owner.runningTasks(sessionID)
	if len(running) == 0 {
		return &tool.ToolResult{Success: true, Output: "当前会话没有在途的子任务。"}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "当前会话在途子任务 %d 个：\n", len(running))
	for _, task := range running {
		b.WriteString("- ")
		b.WriteString(formatTaskLine(task))
		b.WriteString("\n")
	}
	return &tool.ToolResult{Success: true, Output: strings.TrimRight(b.String(), "\n")}, nil
}

// subagentStopTool 让主 agent 停止当前会话中一个在途的后台子任务。
type subagentStopTool struct {
	owner    *subagentTool
	disabled bool
}

func (t *subagentStopTool) Name() string { return subagentStopToolName }

func (t *subagentStopTool) Description() string {
	return `停止当前会话中一个在途的后台子 agent 任务。
停止是协作式的：子 agent 会在下一个执行轮次检查到取消信号后退出；若它正卡在不响应取消的工具调用中，退出会延迟。`
}

func (t *subagentStopTool) Schema() *tool.JSONSchema {
	return &tool.JSONSchema{
		Type: "object",
		Properties: map[string]any{
			"sub_session_id": map[string]any{
				"type":        "string",
				"description": "要停止的子任务标识（派发结果中的 sub_session_id）",
			},
		},
		Required: []string{"sub_session_id"},
	}
}

func (t *subagentStopTool) Execute(ctx context.Context, params map[string]any) (*tool.ToolResult, error) {
	if t.disabled {
		return &tool.ToolResult{Success: false, Output: subagentDisabledRefusal}, nil
	}
	sessionID, ok := ToolSessionIDFromContext(ctx)
	if !ok || strings.TrimSpace(sessionID) == "" {
		return &tool.ToolResult{Success: false, Output: "subagent_stop tool 无法获取当前 session id"}, nil
	}
	if t.owner == nil {
		return &tool.ToolResult{Success: false, Output: "subagent_stop tool 未绑定运行时"}, nil
	}

	id, _ := params["sub_session_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return &tool.ToolResult{Success: false, Output: "subagent_stop 需要 sub_session_id 参数"}, nil
	}

	status, found := t.owner.cancelTask(sessionID, id)
	if !found {
		return &tool.ToolResult{Success: false, Output: fmt.Sprintf("找不到子任务 %q（不属于当前会话或不存在）", id)}, nil
	}
	if status != asyncTaskRunning {
		return &tool.ToolResult{Success: true, Output: fmt.Sprintf("子任务 %s 已结束（status=%s），无需停止。", id, status)}, nil
	}
	return &tool.ToolResult{Success: true, Output: fmt.Sprintf("已请求停止子任务 %s。停止是协作式的，子 agent 将在下一轮检查到取消信号后退出。", id)}, nil
}

// formatTaskLine 输出单行任务摘要，供在途列表使用。
func formatTaskLine(task asyncTask) string {
	return fmt.Sprintf("id=%s name=%s status=%s 已运行=%s", task.id, task.name, task.status, formatTaskDuration(task))
}

// formatTaskDetail 输出单个任务的完整状态，含指令、输出与错误。
func formatTaskDetail(task asyncTask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id=%s\nname=%s\nstatus=%s\n已运行=%s", task.id, task.name, task.status, formatTaskDuration(task))
	if task.instruction != "" {
		fmt.Fprintf(&b, "\ninstruction=%s", task.instruction)
	}
	if task.output != "" {
		fmt.Fprintf(&b, "\noutput=%s", truncateString(task.output, subagentOutputLimit))
	}
	if task.err != "" {
		fmt.Fprintf(&b, "\nerror=%s", task.err)
	}
	return b.String()
}

// formatTaskDuration 以秒为单位给出耗时；未结束时按当前时刻计算已运行时长。
func formatTaskDuration(task asyncTask) string {
	end := task.finishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(task.startedAt).Truncate(time.Second).String()
}
