package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/model"
	"github.com/lzq70112/agentsdk-go/pkg/tool"
	toolbuiltin "github.com/lzq70112/agentsdk-go/pkg/tool/builtin"
)

// TestQuestionAskerTimeoutPath 固化超时路径：超时后返回 ErrQuestionTimeout，
// 登记条目被清理（不悬挂）。
func TestQuestionAskerTimeoutPath(t *testing.T) {
	registry := newQuestionRegistry()
	adapter := &questionAskerAdapter{registry: registry, sessionID: "s1"}
	// 直接以极短超时驱动（20ms），不依赖真实长等待。
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Ask(context.Background(), tool.Question{Question: "q", Timeout: 20 * time.Millisecond})
		done <- err
	}()
	select {
	case err := <-done:
		if err != tool.ErrQuestionTimeout {
			t.Fatalf("期望 ErrQuestionTimeout, 得到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时路径未按时返回")
	}
	if _, ok := registry.bySession("s1"); ok {
		t.Fatal("超时后待答问题应被清理")
	}
}

// TestQuestionAskerAnswerDelivers 固化作答路径：AnswerQuestion 投递的答案
// 由挂起中的 Ask 收到，登记条目清理。
func TestQuestionAskerAnswerDelivers(t *testing.T) {
	registry := newQuestionRegistry()
	adapter := &questionAskerAdapter{registry: registry, sessionID: "s1"}
	var questionID string
	done := make(chan string, 1)
	go func() {
		answer, err := adapter.Ask(context.Background(), tool.Question{Question: "选哪个？", Options: []string{"A", "B"}})
		if err != nil {
			t.Errorf("Ask: %v", err)
			done <- ""
			return
		}
		done <- answer
	}()
	waitFor(t, 3*time.Second, func() bool {
		info, ok := registry.bySession("s1")
		if !ok {
			return false
		}
		questionID = info.ID
		return true
	})
	info, _ := rt_pendingSnapshot(registry, "s1")
	// 未显式限时的提问 Timeout 保持 0（无限等待），不再回填默认超时。
	if info.Options == nil || len(info.Options) != 2 || info.Timeout != 0 {
		t.Fatalf("待答快照不符: %+v", info)
	}
	if err := registry.answer(questionID, "B"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	select {
	case got := <-done:
		if got != "B" {
			t.Fatalf("答案不符: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("答案未送达挂起方")
	}
	if _, ok := registry.bySession("s1"); ok {
		t.Fatal("作答后待答问题应被清理")
	}
	// 重复作答同一问题：已摘除，报错
	if err := registry.answer(questionID, "B"); err == nil {
		t.Fatal("重复作答应报错")
	}
}

// rt_pendingSnapshot 便于测试取会话待答快照。
func rt_pendingSnapshot(r *questionRegistry, sessionID string) (QuestionInfo, bool) {
	return r.bySession(sessionID)
}

// TestRuntimeAnswerQuestionUnknown 固化 Runtime API 对未知问题的报错。
func TestRuntimeAnswerQuestionUnknown(t *testing.T) {
	rt := &Runtime{questions: newQuestionRegistry()}
	if err := rt.AnswerQuestion("q-999", "x"); err == nil {
		t.Fatal("未知问题应报错")
	}
	if _, ok := rt.PendingQuestion("s1"); ok {
		t.Fatal("无待答问题时应返回 false")
	}
}

// TestRunStreamSuspendsOnQuestion 端到端固化挂起与恢复：提问事件先于后续
// 执行到达，作答后回合携带答案继续完成。
func TestRunStreamSuspendsOnQuestion(t *testing.T) {
	root := newClaudeProject(t)
	askCall := model.ToolCall{
		ID:   "tool_1",
		Name: toolbuiltin.AskUserQuestionName,
		Arguments: map[string]any{
			"question": "部署到哪个环境？",
			"options":  []any{"测试环境", "生产环境"},
		},
	}
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{askCall}}},
		{Message: model.Message{Role: "assistant", Content: "已按你的选择继续"}},
	}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	stream, err := rt.RunStream(context.Background(), Request{Prompt: "请确认部署环境"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var (
		question  *QuestionInfo
		sessionID string
		finalText string
		sawAnswer bool
	)
	for evt := range stream {
		if evt.Type == toolbuiltin.QuestionAskedEventType {
			info, ok := evt.Output.(QuestionInfo)
			if !ok {
				t.Fatalf("提问事件负载类型 %T, 期望 QuestionInfo", evt.Output)
			}
			question = &info
			sessionID = evt.SessionID
			// 挂起中的回合：作答即恢复
			if err := rt.AnswerQuestion(info.ID, "生产环境"); err != nil {
				t.Fatalf("AnswerQuestion: %v", err)
			}
			sawAnswer = true
			continue
		}
		if evt.Delta != nil && evt.Delta.Type == "text_delta" {
			finalText += evt.Delta.Text
		}
		if evt.ContentBlock != nil && evt.ContentBlock.Type == "text" {
			finalText += evt.ContentBlock.Text
		}
	}
	if question == nil {
		t.Fatal("未收到提问事件")
	}
	if question.Question != "部署到哪个环境？" || len(question.Options) != 2 || question.ID == "" {
		t.Fatalf("提问快照不全: %+v", question)
	}
	if sessionID == "" {
		t.Fatal("提问事件缺少会话标识")
	}
	if !sawAnswer {
		t.Fatal("未完成作答")
	}
	if !strings.Contains(finalText, "已按你的选择继续") {
		t.Fatalf("作答后回合应继续完成, 实际收尾: %q", finalText)
	}
	// 工具结果进入历史：下一轮模型可见答案
	hist, ok := rt.SessionHistory(sessionID)
	if !ok {
		t.Fatalf("会话历史缺失: %q", sessionID)
	}
	found := false
	for _, msg := range hist {
		if strings.Contains(msg.Content, "用户回答：生产环境") {
			found = true
			break
		}
		for _, tc := range msg.ToolCalls {
			if strings.Contains(tc.Result, "用户回答：生产环境") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("历史中应包含答案工具结果")
	}
	// 挂起期间会话忙：提问期间 PendingQuestion 可见，结束后清空
	if _, ok := rt.PendingQuestion(sessionID); ok {
		t.Fatal("回合结束后不应再有 pending 问题")
	}
}

// TestQuestionAskerNoTimeoutWaitsForever 固化不限时语义：未显式限时的提问
// 永不超时——Ask 持续挂起等待答案，任何默认超时都不应摘除登记条目。废除
// 提问超时后这是提问的默认行为（用户何时回答都有效）。
func TestQuestionAskerNoTimeoutWaitsForever(t *testing.T) {
	registry := newQuestionRegistry()
	adapter := &questionAskerAdapter{registry: registry, sessionID: "s1"}
	done := make(chan string, 1)
	go func() {
		answer, err := adapter.Ask(context.Background(), tool.Question{Question: "不设时限的问题"})
		if err != nil {
			t.Errorf("Ask: %v", err)
			done <- ""
			return
		}
		done <- answer
	}()
	// 先等登记出现（goroutine 尚未执行时 registry 为空），再开观察窗口。
	waitFor(t, 3*time.Second, func() bool {
		_, ok := registry.bySession("s1")
		return ok
	})
	// 观察窗口远超原默认超时（5 分钟不可能在测试里等，取 300ms——旧默认
	// 回填语义下该窗口内会摘除登记并返回超时错误）。
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, ok := registry.bySession("s1"); !ok {
			t.Fatal("不限时提问在观察窗口内被摘除——不应超时")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := registry.answer(askFirstID(t, registry, "s1"), "终于回答了"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	select {
	case got := <-done:
		if got != "终于回答了" {
			t.Fatalf("答案不符: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("答案未送达挂起方")
	}
}

// askFirstID 返回会话当前待答问题 ID（测试辅助）。
func askFirstID(t *testing.T, registry *questionRegistry, sessionID string) string {
	t.Helper()
	info, ok := registry.bySession(sessionID)
	if !ok {
		t.Fatal("无待答问题")
	}
	return info.ID
}

// TestRunStreamQuestionTimeoutEndsTurn 端到端固化超时路径：超时说明进入
// 历史后回合立即收尾——不再进行下一轮模型迭代，等待用户给出指示。
// timeout_seconds: 1 → 约 1 秒真实等待。
func TestRunStreamQuestionTimeoutEndsTurn(t *testing.T) {
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID:   "tool_1",
			Name: toolbuiltin.AskUserQuestionName,
			Arguments: map[string]any{
				"question":        "需要更多信息吗？",
				"timeout_seconds": float64(1),
			},
		}}}},
		{Message: model.Message{Role: "assistant", Content: "超时后自主决策完成"}},
	}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	stream, err := rt.RunStream(context.Background(), Request{Prompt: "开工"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	sessionID := ""
	for evt := range stream {
		if sessionID == "" {
			sessionID = evt.SessionID
		}
	}
	if len(mdl.requests) != 1 {
		t.Fatalf("超时收尾后不应有下一轮模型调用，实际 %d 次", len(mdl.requests))
	}
	hist, ok := rt.SessionHistory(sessionID)
	if !ok {
		t.Fatalf("会话历史缺失: %q", sessionID)
	}
	found := false
	for _, msg := range hist {
		if strings.Contains(msg.Content, "1 秒内未回答") {
			found = true
			break
		}
		for _, tc := range msg.ToolCalls {
			if strings.Contains(tc.Result, "1 秒内未回答") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("历史中应包含超时说明")
	}
}
