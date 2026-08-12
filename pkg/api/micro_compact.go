package api

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/stellarlinkco/agentsdk-go/pkg/message"
)

const microCompactToolResultPrefix = "[output truncated, "

func (c *compactor) maybeMicroCompact(hist *message.History) bool {
	if c == nil || hist == nil || !c.cfg.Enabled || c.cfg.MicroPreserveCount <= 0 {
		return false
	}

	snapshot := hist.All()
	if len(snapshot) <= c.cfg.MicroPreserveCount+defaultMicroTriggerBuffer {
		return false
	}

	cut := len(snapshot) - c.cfg.MicroPreserveCount
	changed := false
	for i := 0; i < cut; i++ {
		if microCompactMessage(&snapshot[i]) {
			changed = true
		}
	}
	if !changed {
		return false
	}

	hist.Replace(snapshot)
	return true
}

func microCompactMessage(msg *message.Message) bool {
	if msg == nil {
		return false
	}

	changed := false
	if msg.ReasoningContent != "" {
		msg.ReasoningContent = ""
		changed = true
	}

	if len(msg.ContentBlocks) > 0 {
		blocks := msg.ContentBlocks[:0]
		removed := false
		for _, block := range msg.ContentBlocks {
			if block.Type == message.ContentBlockImage || block.Type == message.ContentBlockDocument {
				removed = true
				continue
			}
			blocks = append(blocks, block)
		}
		if removed {
			msg.ContentBlocks = append([]message.ContentBlock(nil), blocks...)
			if len(msg.ContentBlocks) == 0 {
				msg.ContentBlocks = nil
			}
			changed = true
		}
	}

	if msg.Role != "tool" {
		return changed
	}

	for i := range msg.ToolCalls {
		result := msg.ToolCalls[i].Result
		if strings.TrimSpace(result) == "" || isMicroCompactedToolResult(result) {
			continue
		}
		msg.ToolCalls[i].Result = truncatedToolResult(result)
		changed = true
	}
	return changed
}

func truncatedToolResult(result string) string {
	return fmt.Sprintf("[output truncated, %d chars]", utf8.RuneCountInString(result))
}

func isMicroCompactedToolResult(result string) bool {
	return strings.HasPrefix(result, microCompactToolResultPrefix) && strings.HasSuffix(result, " chars]")
}
