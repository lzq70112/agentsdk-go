package api

import (
	"strings"

	hooks "github.com/lzq70112/agentsdk-go/pkg/hooks"
	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/subagents"
)

const subagentOutputLimit = 2000

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
	if rt.hooks != nil {
		if err := rt.hooks.Publish(hooks.Event{
			Type:      hooks.SubagentComplete,
			SessionID: strings.TrimSpace(status.SessionID),
			Payload:   payload,
		}); err != nil {
			runtimeLogger.warnf("hooks: subagent completion publish failed: %v", err)
		}
	}
	if rt.opts.DisableSubagentSummary || rt.histories == nil {
		return
	}
	history, ok := rt.histories.Loaded(status.SessionID)
	if !ok {
		return
	}
	history.Append(message.Message{
		Role:     "user",
		Content:  formatSubagentSummary(status),
		Metadata: subagentSummaryMetadata(status),
	})
}

func completionStatus(status subagents.Status) string {
	if status.State == subagents.StatusError {
		return "error"
	}
	return "success"
}

func formatSubagentSummary(status subagents.Status) string {
	return formatSubagentResultMessage(status.Name, status.Instruction, completionStatus(status), status.Output, status.Error)
}

// formatSubagentResultMessage 拼装注入主会话 history 的子 agent 完成结果消息。
// subMgr 的同步派发与内置 subagent tool 的后台任务共用，保证文案一致。
func formatSubagentResultMessage(name, instruction, status, output, errText string) string {
	var builder strings.Builder
	builder.WriteString("[Subagent Result: ")
	builder.WriteString(strings.TrimSpace(name))
	builder.WriteString("]\nTask: ")
	builder.WriteString(strings.TrimSpace(instruction))
	builder.WriteString("\nStatus: ")
	builder.WriteString(status)
	builder.WriteString("\nOutput: ")
	builder.WriteString(truncateString(output, subagentOutputLimit))
	if trimmed := strings.TrimSpace(errText); trimmed != "" {
		builder.WriteString("\nError: ")
		builder.WriteString(trimmed)
	}
	return builder.String()
}

func subagentSummaryMetadata(status subagents.Status) map[string]any {
	return subagentResultMetadata(status.TaskID, status.Name)
}

// subagentResultMetadata 标注合成消息的来源，供平台侧识别与过滤。
func subagentResultMetadata(taskID, name string) map[string]any {
	return map[string]any{
		"api.synthetic":      true,
		"api.synthetic_type": "subagent_result",
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
