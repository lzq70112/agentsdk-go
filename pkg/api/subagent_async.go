package api

import (
	"log"
	"strings"

	hooks "github.com/stellarlinkco/agentsdk-go/pkg/hooks"
	"github.com/stellarlinkco/agentsdk-go/pkg/message"
	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/subagents"
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
			log.Printf("hooks: subagent completion publish failed: %v", err)
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
	var builder strings.Builder
	builder.WriteString("[Subagent Result: ")
	builder.WriteString(strings.TrimSpace(status.Name))
	builder.WriteString("]\nTask: ")
	builder.WriteString(strings.TrimSpace(status.Instruction))
	builder.WriteString("\nStatus: ")
	builder.WriteString(completionStatus(status))
	builder.WriteString("\nOutput: ")
	builder.WriteString(truncateString(status.Output, subagentOutputLimit))
	if errText := strings.TrimSpace(status.Error); errText != "" {
		builder.WriteString("\nError: ")
		builder.WriteString(errText)
	}
	return builder.String()
}

func subagentSummaryMetadata(status subagents.Status) map[string]any {
	return map[string]any{
		"api.synthetic":      true,
		"api.synthetic_type": "subagent_result",
		"subagent.task_id":   status.TaskID,
		"subagent.name":      status.Name,
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
