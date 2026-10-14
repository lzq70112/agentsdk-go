package api

import (
	"fmt"
	"strings"

	hooks "github.com/lzq70112/agentsdk-go/pkg/hooks"
	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/subagents"
)

// subagentOutputLimit 限制注入主会话的子 agent 输出长度（rune 数）。截断时
// formatTruncatedOutput 会追加恢复指引，主 agent 可用 subagent_output 工具
// 凭 sub_session_id 读取全文——截断是延迟加载，不是信息丢失。
const subagentOutputLimit = 50000

func (rt *Runtime) bindSubagentCallbacks() {
	if rt == nil || rt.opts.subMgr == nil {
		return
	}
	rt.opts.subMgr.SetMaxConcurrentBackground(rt.opts.MaxConcurrentSubagents)
	rt.opts.subMgr.SetCompletionHandler(rt.handleSubagentCompletion)
}

func (rt *Runtime) handleSubagentCompletion(status subagents.Status) {
	if rt == nil {
		return
	}
	payload := hooks.SubagentCompletePayload{
		TaskID: status.TaskID,
		Name:   strings.TrimSpace(status.Name),
		Status: completionStatus(status),
		Output: truncateString(status.Output, subagentOutputLimit),
		Error:  strings.TrimSpace(status.Error),
	}
	forwardSubagentEvent(rt.hooks, hooks.Event{
		Type:      hooks.SubagentComplete,
		SessionID: strings.TrimSpace(status.SessionID),
		Payload:   payload,
	})
	if rt.opts.DisableSubagentSummary {
		return
	}
	if err := rt.Inject(status.SessionID, Injection{
		Kind:   InjectionKindSubagentResult,
		Policy: BarrierDefer,
		Message: message.Message{
			Role:     "user",
			Content:  formatSubagentSummary(status),
			Metadata: subagentSummaryMetadata(status),
		},
	}); err != nil {
		runtimeLogger.warnf("[api:subagent] completion inject failed session=%s task=%s: %v", status.SessionID, status.TaskID, err)
	}
}

func completionStatus(status subagents.Status) string {
	if status.State == subagents.StatusError {
		return "error"
	}
	return "success"
}

func formatSubagentSummary(status subagents.Status) string {
	return formatSubagentResultMessage(status.Name, status.Instruction, completionStatus(status), status.Output, status.Error, "")
}

// formatSubagentResultMessage 拼装注入主会话 history 的子 agent 完成结果消息。
// subMgr 的同步派发与内置 subagent tool 的后台任务共用，保证文案一致。
// subSessionID 非空时，截断标记会附带 subagent_output 工具的取回指引；
// 传空（如 subMgr 链的 task id 语义不同）则只提示截断事实。
func formatSubagentResultMessage(name, instruction, status, output, errText, subSessionID string) string {
	var builder strings.Builder
	builder.WriteString("[Subagent Result: ")
	builder.WriteString(strings.TrimSpace(name))
	builder.WriteString("]\nTask: ")
	builder.WriteString(strings.TrimSpace(instruction))
	builder.WriteString("\nStatus: ")
	builder.WriteString(status)
	builder.WriteString("\nOutput: ")
	builder.WriteString(formatTruncatedOutput(output, subSessionID))
	if trimmed := strings.TrimSpace(errText); trimmed != "" {
		builder.WriteString("\nError: ")
		builder.WriteString(trimmed)
	}
	return builder.String()
}

// formatTruncatedOutput 把超限输出截断到 subagentOutputLimit 并追加截断标记：
// 主 agent 必须能明确知道输出被截、原文多大、如何取回全文，而不是靠
// "内容看起来没写完"自行猜测（截断无标记会诱发重复派发子 agent 补查）。
func formatTruncatedOutput(output, subSessionID string) string {
	runes := []rune(strings.TrimSpace(output))
	if len(runes) <= subagentOutputLimit {
		return string(runes)
	}
	var b strings.Builder
	b.WriteString(string(runes[:subagentOutputLimit]))
	fmt.Fprintf(&b, "\n[已截断：完整输出共 %d 字符，以上为前 %d 字符", len(runes), subagentOutputLimit)
	if subSessionID != "" {
		fmt.Fprintf(&b, "；可用 subagent_output 工具传 sub_session_id=%s 读取全文", subSessionID)
	}
	b.WriteString("]")
	return b.String()
}

func subagentSummaryMetadata(status subagents.Status) map[string]any {
	return subagentResultMetadata(status.TaskID, status.Name)
}

// subagentResultMetadata 标注合成消息的来源，供平台侧识别与过滤；
// synthetic 标记同时是压缩豁免凭证（见 compact_preserve.go）。
func subagentResultMetadata(taskID, name string) map[string]any {
	return map[string]any{
		metadataSynthetic:    true,
		metadataSyntheticKey: "subagent_result",
		"subagent.task_id":   taskID,
		"subagent.name":      name,
	}
}

func truncateString(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || text == "" {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
