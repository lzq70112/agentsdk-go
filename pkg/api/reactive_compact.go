package api

import (
	"context"
	"strings"

	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

func isPromptTooLongError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "prompt_too_long") || strings.Contains(msg, "http 413") || strings.Contains(msg, " 413")
}

func (c *compactor) reactiveCompact(ctx context.Context, hist *message.History, mdl model.Model) (bool, error) {
	if c == nil || hist == nil || mdl == nil {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	didMicroCompact := c.maybeMicroCompact(hist)

	snapshot := hist.All()
	preserve := c.cfg.PreserveCount / 2
	if preserve < 1 {
		preserve = 1
	}
	if preserve >= len(snapshot) {
		return didMicroCompact, nil
	}

	cut := len(snapshot) - preserve
	for _, span := range toolTransactionSpans(snapshot) {
		if span.start < cut && cut < span.end {
			cut = span.start
			break
		}
	}
	if cut <= 0 {
		return didMicroCompact, nil
	}

	// 环境事实类合成消息永不压缩：从摘要输入中分离出来，压缩后原样保留。
	summarySource, rest := partitionSynthetic(snapshot[:cut])
	if len(rest) == 0 {
		return didMicroCompact, nil
	}
	summary, err := compressMessages(ctx, mdl, stripToolIO(rest))
	if err != nil {
		return false, err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return didMicroCompact, nil
	}

	out := make([]message.Message, 0, 1+len(summarySource)+len(snapshot[cut:]))
	out = append(out, message.Message{Role: "system", Content: "## Summary\n\n" + summary})
	out = append(out, summarySource...)
	out = append(out, snapshot[cut:]...)
	hist.Replace(out)
	return true, nil
}
