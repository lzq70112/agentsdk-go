package api

import (
	"context"
	"errors"
	"strings"
	"time"
)

// SubagentDispatchRequest 描述一次编程式子 agent 派发：宿主（如工作流引擎）从代码
// 里 fan-out 一个自包含子任务，不经模型的 subagent tool 调用。
type SubagentDispatchRequest struct {
	// SessionID 是主会话 ID：决定子会话 ID 前缀（{SessionID}-sub-{uuid}）与
	// 完成回调归属。
	SessionID string
	// Name 是子任务名，用于 delegation 消息与完成回调的标识。
	Name string
	// Instruction 是交给子 agent 的指令，必须自包含（子 agent 看不到主会话历史）。
	Instruction string
	// Type 是 AgentType 名（Options.AgentTypes 或 .agents/subagents/ 类型文件注册）；
	// 空串使用默认通用型。
	Type string
}

// RunSubagent 同步派发一个隔离子 agent 并返回其最终输出与子会话 ID。
//
// 复用内置 subagent 工具的同一派发链路：子 agent 在隔离上下文中运行（不继承主会话
// 历史，流式出口切断防污染主事件流）、身份 system prompt 由 Type 决定、指令包装成
// delegation 消息（含输出契约）。语义与内置工具的同步派发一致：不进后台任务注册表、
// 不占后台并发槽；成功后子会话进入追问缓存，可被 subagent 工具按 sub_session_id
// 追问续跑；完成回调（SubagentCompletionHandler）由派发链路在成功与失败路径上各
// 触发一次。ctx 取消/超时传播到子 runtime，随错误一起返回。
//
// subagent 内置工具未启用（rt.subagent 未绑定）时返回错误——派发能力随工具面声明，
// 不做隐式兜底。不走 subagentTool.Execute：那层是给模型的工具入口，带 ToolResult
// 语义与 session ctx 依赖。
func (rt *Runtime) RunSubagent(ctx context.Context, req SubagentDispatchRequest) (output string, subSessionID string, err error) {
	if rt == nil || rt.subagent == nil {
		return "", "", errors.New("RunSubagent 不可用：subagent 内置工具未启用（Options.EnabledBuiltinTools 需包含 subagent）")
	}
	sessionID := strings.TrimSpace(req.SessionID)
	name := strings.TrimSpace(req.Name)
	instruction := strings.TrimSpace(req.Instruction)
	if sessionID == "" || name == "" || instruction == "" {
		return "", "", errors.New("RunSubagent 需要非空的 SessionID、Name 和 Instruction 参数")
	}

	st := rt.subagent
	agentType := st.lookupAgentType(req.Type)
	subSessionID = newSubSessionID(sessionID)
	// 首次派发：history 传 nil，子 agent 从隔离上下文起步（buildSubOptions 注入空
	// history loader），与内置工具的同步路径同一入口。
	subOpts := st.buildSubOptions(agentType, nil)
	prompt := buildDelegationMessage(name, instruction)
	// 完成回调由 dispatch 内部触发（成功/失败/创建失败三路径各一次），此处不得
	// 重复调用 notifySubagentCompletion。
	output, err = st.dispatch(ctx, sessionID, subSessionID, name, prompt, agentType, time.Now(), subOpts)
	return output, subSessionID, err
}
