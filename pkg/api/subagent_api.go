package api

import "time"

// SubagentTaskInfo 是对外暴露的后台子任务快照（Runtime.SubagentsRunning 返回）。
type SubagentTaskInfo struct {
	// ID 是子会话 ID（形如 {sessionID}-sub-{uuid}），取消时作为 CancelSubagent 的入参。
	ID string
	// Name 是子任务名；Instruction 是派发指令。
	Name, Instruction string
	// Status 当前为 "running"；已结束的任务不会出现在 SubagentsRunning 列表中。
	Status string
	// StartedAt 是派发时间。
	StartedAt time.Time
}

// SubagentsRunning 返回会话当前全部在途的后台子任务，按派发时间升序。
// 同步派发的子任务不进任务注册表、不出现在列表中——它占着主回合，随主回合取消。
func (rt *Runtime) SubagentsRunning(sessionID string) []SubagentTaskInfo {
	if rt == nil || rt.subagent == nil {
		return nil
	}
	tasks := rt.subagent.runningTasks(sessionID)
	out := make([]SubagentTaskInfo, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, SubagentTaskInfo{
			ID:          t.id,
			Name:        t.name,
			Instruction: t.instruction,
			Status:      t.status,
			StartedAt:   t.startedAt,
		})
	}
	return out
}

// CancelSubagent 取消会话派发的指定后台子任务：仅当任务在途时发起取消并返回
// true；任务不存在、不属于该会话或已结束时返回 false。取消是协作式的：子 agent
// 在下一轮检查点退出；被取消的任务不注入结果、不触发完成唤醒，上层应自行标记终态。
func (rt *Runtime) CancelSubagent(sessionID, subSessionID string) bool {
	if rt == nil || rt.subagent == nil {
		return false
	}
	status, ok := rt.subagent.cancelTask(sessionID, subSessionID)
	return ok && status == asyncTaskRunning
}
