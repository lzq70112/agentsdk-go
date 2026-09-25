package api

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// activeRun 表示一个正在执行的回合（Run/RunStream）。
//
// cancel 取消该回合的 context，使 runLoop 在下一个检查点（迭代开头或模型/
// 工具调用）退出；snapshot 是回合开始前的 history 快照，用户取消时据此把
// history 回滚到回合开始前；rollback 由 CancelSession 置位，用于把「用户主动
// 取消」与「调用方 ctx 取消（如客户端断开）」区分开——只有前者才回滚。
type activeRun struct {
	sessionID string
	cancel    context.CancelFunc
	snapshot  []message.Message
	rollback  atomic.Bool
}

// beginActiveRun 登记一个在途回合，返回其句柄供结束/回滚时使用。
// sessionID 为空时不登记（无法按会话定位取消），返回 nil。
func (rt *Runtime) beginActiveRun(sessionID string, cancel context.CancelFunc, snapshot []message.Message) *activeRun {
	if rt == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	run := &activeRun{sessionID: sessionID, cancel: cancel, snapshot: snapshot}
	rt.activeMu.Lock()
	if rt.activeRuns == nil {
		rt.activeRuns = make(map[string]*activeRun)
	}
	rt.activeRuns[sessionID] = run
	rt.activeMu.Unlock()
	return run
}

// endActiveRun 注销在途回合。仅当注册表里仍是同一个 run 时才删除，
// 避免误删同一会话上后续登记的回合。
func (rt *Runtime) endActiveRun(run *activeRun) {
	if rt == nil || run == nil {
		return
	}
	rt.activeMu.Lock()
	if cur, ok := rt.activeRuns[run.sessionID]; ok && cur == run {
		delete(rt.activeRuns, run.sessionID)
	}
	rt.activeMu.Unlock()
}

// rollbackRequested 报告该回合是否被用户主动取消并要求回滚。
func (run *activeRun) rollbackRequested() bool {
	if run == nil {
		return false
	}
	return run.rollback.Load()
}

// CancelSession 取消指定会话当前在途的回合，并请求把该回合写入的历史回滚到
// 回合开始前；同时停止该会话在途的后台 subagent 任务。
//
// 返回 true 表示确实取消了一个在途回合；返回 false 表示该会话当前没有在途
// 回合（此时仍会尝试停止其后台 subagent，若有则返回 true）。
//
// 取消是协作式的：runLoop 在下一个检查点退出，因此正在执行且不响应取消的
// 工具调用可能使退出略有延迟。
func (rt *Runtime) CancelSession(sessionID string) bool {
	if rt == nil {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}

	rt.activeMu.Lock()
	run := rt.activeRuns[sessionID]
	if run != nil {
		run.rollback.Store(true)
	}
	rt.activeMu.Unlock()

	// 一并停止该会话在途的后台 subagent：取消回合不等于取消它已派发的后台任务。
	subagentsCanceled := rt.subagent.cancelTasksForSession(sessionID)

	if run == nil {
		return subagentsCanceled > 0
	}
	run.cancel()
	return true
}
