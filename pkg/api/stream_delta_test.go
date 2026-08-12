package api

import (
	"context"
	"strings"
	"testing"

	"github.com/stellarlinkco/agentsdk-go/pkg/model"
	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
)

// deltaModel 发射固定的 SSE delta 序列 + 一个 Final Response。
// 用于验证 RunStream 透传底层真 delta，而非 AfterAgent 假重放。
type deltaModel struct {
	deltas []string // 按顺序发射的真 SSE delta
	final  *model.Response
}

func (m *deltaModel) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	if m.final == nil {
		return &model.Response{Message: model.Message{Role: "assistant"}}, nil
	}
	return m.final, nil
}

func (m *deltaModel) CompleteStream(ctx context.Context, req model.Request, cb model.StreamHandler) error {
	for _, d := range m.deltas {
		if err := cb(model.StreamResult{Delta: d}); err != nil {
			return err
		}
	}
	resp, err := m.Complete(ctx, req)
	if err != nil {
		return err
	}
	return cb(model.StreamResult{Final: true, Response: resp})
}

// multiCallDeltaModel 支持多次 CompleteStream 调用（工具执行会触发多次迭代）。
// 第一次调用发 deltas + first（含 ToolCall）；第二次及以后只发 second（纯文本收尾，无 delta）。
type multiCallDeltaModel struct {
	deltas  []string
	first   *model.Response
	second  *model.Response
	callCnt int
}

func (m *multiCallDeltaModel) Complete(_ context.Context, _ model.Request) (*model.Response, error) {
	if m.callCnt == 0 {
		return m.first, nil
	}
	return m.second, nil
}

func (m *multiCallDeltaModel) CompleteStream(ctx context.Context, req model.Request, cb model.StreamHandler) error {
	m.callCnt++
	callNum := m.callCnt
	if callNum == 1 {
		for _, d := range m.deltas {
			if err := cb(model.StreamResult{Delta: d}); err != nil {
				return err
			}
		}
	}
	var resp *model.Response
	var err error
	if callNum == 1 {
		resp, err = m.first, nil
	} else {
		resp, err = m.second, nil
	}
	if err != nil {
		return err
	}
	return cb(model.StreamResult{Final: true, Response: resp})
}

// newDeltaRuntime 构造一个使用 deltaModel 的 Runtime。
func newDeltaRuntime(t *testing.T, deltas []string, finalContent string) *Runtime {
	t.Helper()
	root := newClaudeProject(t)
	mdl := &deltaModel{
		deltas: deltas,
		final:  &model.Response{Message: model.Message{Role: "assistant", Content: finalContent}},
	}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// collectDeltas 收集 channel 中所有 text_delta 类型的事件文本，保持顺序。
func collectDeltas(t *testing.T, stream <-chan StreamEvent) []string {
	t.Helper()
	var out []string
	for evt := range stream {
		if evt.Type != EventContentBlockDelta {
			continue
		}
		if evt.Delta == nil || evt.Delta.Type != "text_delta" {
			continue
		}
		out = append(out, evt.Delta.Text)
	}
	return out
}

// TestRunStreamForwardsRealDelta 验证意图：底层 CompleteStream 发射的真 SSE delta
// 必须原样到达 RunStream channel，而非被中间层丢弃后用整段 Response 假重放。
// 业务意义：流式 UI 才能在 AI 生成过程中实时显示，而不是干等完整结果。
// 同时验证真 delta 被完整的 content_block_start/stop 包络包裹（块生命周期完整）。
func TestRunStreamForwardsRealDelta(t *testing.T) {
	wantDeltas := []string{"Hello", " ", "World"}
	rt := newDeltaRuntime(t, wantDeltas, "Hello World")

	stream, err := rt.RunStream(context.Background(), Request{Prompt: "go"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var gotDeltas []string
	var blockStarts, blockStops []int
	for evt := range stream {
		switch evt.Type {
		case EventContentBlockStart:
			if evt.Index != nil {
				blockStarts = append(blockStarts, *evt.Index)
			}
		case EventContentBlockStop:
			if evt.Index != nil {
				blockStops = append(blockStops, *evt.Index)
			}
		case EventContentBlockDelta:
			if evt.Delta != nil && evt.Delta.Type == "text_delta" {
				gotDeltas = append(gotDeltas, evt.Delta.Text)
			}
		}
	}

	if len(gotDeltas) != len(wantDeltas) {
		t.Fatalf("期望收到 %d 个真 delta，实际 %d 个：%v", len(wantDeltas), len(gotDeltas), gotDeltas)
	}
	for i, want := range wantDeltas {
		if gotDeltas[i] != want {
			t.Fatalf("第 %d 个 delta 不匹配：期望 %q，实际 %q", i, want, gotDeltas[i])
		}
	}
	// 块生命周期：真 delta 必须被 start/stop 包裹，否则下游按 index 跟踪块的消费者会解析异常。
	if len(blockStarts) != 1 || blockStarts[0] != 0 {
		t.Fatalf("期望恰好 1 个 content_block_start(idx=0)，实际 %v", blockStarts)
	}
	if len(blockStops) != 1 || blockStops[0] != 0 {
		t.Fatalf("期望恰好 1 个 content_block_stop(idx=0)，实际 %v", blockStops)
	}
}

// TestRunStreamSkipsFakeReplay 验证意图：当底层已透传真 delta 时，
// AfterAgent 不得再用整段 Response.Content 逐字符重放，否则会重复推送。
// 业务意义：避免用户看到 "H","e","l","l","o",... 重复内容与真 delta 混在一起。
func TestRunStreamSkipsFakeReplay(t *testing.T) {
	realDeltas := []string{"AB", "CD"}
	rt := newDeltaRuntime(t, realDeltas, "ABCD")

	stream, err := rt.RunStream(context.Background(), Request{Prompt: "go"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	got := collectDeltas(t, stream)

	// 期望只收到真 delta 的拼接 = "AB" + "CD"，不能出现逐字符重放。
	wantJoined := strings.Join(realDeltas, "")
	gotJoined := strings.Join(got, "")
	if gotJoined != wantJoined {
		t.Fatalf("delta 拼接不匹配：期望 %q，实际 %q", wantJoined, gotJoined)
	}
	// 关键：不能出现单字符 delta（假重放的标志）。真 delta 是 2 个，假重放会是 4 个单字符。
	if len(got) != len(realDeltas) {
		t.Fatalf("delta 个数应为 %d（真 delta），实际 %d（疑似假重放）：%v",
			len(realDeltas), len(got), got)
	}
}

// TestRunNoStreamEmitUnchanged 验证回归保护：非流式 Run 路径不注入 streamEmit context，
// deltaModel 发的 delta 在 Run 中应被忽略（Run 不暴露 delta channel），行为与现状一致。
func TestRunNoStreamEmitUnchanged(t *testing.T) {
	rt := newDeltaRuntime(t, []string{"ignored-delta"}, "final answer")

	resp, err := rt.Run(context.Background(), Request{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Run 返回完整 Response，delta 不影响最终输出。
	if resp.Result == nil || resp.Result.Output != "final answer" {
		got := "<nil>"
		if resp.Result != nil {
			got = resp.Result.Output
		}
		t.Fatalf("期望 Result.Output=%q，实际 %q", "final answer", got)
	}
}

// TestCollectStreamResponseNoEmitUnchanged 验证：没有 streamEmit 的 context，
// collectStreamResponse 调用底层 CompleteStream 时不应把 delta 推到任何 channel
// （该函数只返回最终 Response）。回归保护：确保 delta 透传逻辑只在有 streamEmit 时生效。
func TestCollectStreamResponseNoEmitUnchanged(t *testing.T) {
	mdl := &deltaModel{
		deltas: []string{"should", "not", "leak"},
		final:  &model.Response{Message: model.Message{Role: "assistant", Content: "done"}},
	}
	// 普通 context，不带 streamEmit / forwarded 标记。
	ctx := context.Background()
	resp, err := collectStreamResponse(ctx, mdl, model.Request{})
	if err != nil {
		t.Fatalf("collectStreamResponse: %v", err)
	}
	if resp == nil || resp.Message.Content != "done" {
		t.Fatalf("期望返回最终 Response Content=%q，实际 %+v", "done", resp)
	}
}

// TestRunStreamForwardsDeltaWithToolCalls 验证意图：当一次响应既有文本 delta 又有 tool_call 时，
// 透传的真 delta 必须占据 index 0 的 text 块（含 start/stop），随后的 tool_use 块从 index 1 开始，
// 两者不能共用 index 0。业务意义：下游按 index 跟踪块生命周期的消费者（如钉钉卡片流式推送）
// 不会把文本与工具调用混在一个块里。
// 同时验证 spec 决策1 不变量："工具执行相关事件不受 delta 透传影响"。
func TestRunStreamForwardsDeltaWithToolCalls(t *testing.T) {
	root := newClaudeProject(t)
	echo := &echoTool{}
	// 迭代1：真 delta + 一个 tool_call；迭代2：工具执行后纯文本收尾（无 delta）。
	mdl := &multiCallDeltaModel{
		deltas: []string{"calling ", "tool"},
		first: &model.Response{Message: model.Message{
			Role: "assistant",
			Content: "calling tool",
			ToolCalls: []model.ToolCall{{
				ID: "tc_1", Name: echo.Name(), Arguments: map[string]any{"text": "hi"},
			}},
		}},
		second: &model.Response{Message: model.Message{Role: "assistant", Content: "done"}},
	}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl, Tools: []tool.Tool{echo}})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	stream, err := rt.RunStream(context.Background(), Request{Prompt: "go"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var textDeltas []string
	var textBlockStart, textBlockStop int
	toolUseStarts := map[int]string{} // idx -> tool name
	for evt := range stream {
		switch evt.Type {
		case EventContentBlockStart:
			if evt.ContentBlock == nil || evt.Index == nil {
				continue
			}
			if evt.ContentBlock.Type == "text" {
				textBlockStart++
			} else if evt.ContentBlock.Type == "tool_use" {
				toolUseStarts[*evt.Index] = evt.ContentBlock.Name
			}
		case EventContentBlockStop:
			if evt.Index != nil && *evt.Index == 0 {
				textBlockStop++
			}
		case EventContentBlockDelta:
			if evt.Delta != nil && evt.Delta.Type == "text_delta" {
				textDeltas = append(textDeltas, evt.Delta.Text)
			}
		}
	}

	// 真 delta 透传成功：前两个 delta 必须是底层的真 delta（不是逐字符假重放）。
	// 注意：第二次迭代（工具执行后）的 "done" 会经过 textBlock 假重放，这是预期行为。
	wantDeltas := []string{"calling ", "tool"}
	if len(textDeltas) < len(wantDeltas) {
		t.Fatalf("text delta 数量不足：期望至少 %d 个，实际 %d 个：%v", len(wantDeltas), len(textDeltas), textDeltas)
	}
	for i, want := range wantDeltas {
		if textDeltas[i] != want {
			t.Fatalf("第 %d 个 delta 不匹配：期望 %q，实际 %q", i, want, textDeltas[i])
		}
	}
	// text 块至少有一次完整生命周期（第一次迭代的真 delta 块）。
	// 第二次迭代还会有一个假重放的 text 块，所以总数 >= 1 即可。
	if textBlockStart < 1 || textBlockStop < 1 {
		t.Fatalf("期望至少 1 个 text 块 start + stop，实际 start=%d stop=%d", textBlockStart, textBlockStop)
	}
	// tool_use 块必须从 index 1 开始，不能与 text 块共用 index 0。
	if _, ok := toolUseStarts[0]; ok {
		t.Fatalf("tool_use 块出现在 index 0，与 text 块冲突：%v", toolUseStarts)
	}
	if _, ok := toolUseStarts[1]; !ok {
		t.Fatalf("期望 tool_use 块在 index 1，实际 tool 块分布：%v", toolUseStarts)
	}
}

// TestRunStreamEmptyDeltaFallsBackToFakeReplay 验证意图：当底层 CompleteStream
// 只发 Final 不发 delta（sr.Delta == ""）时，AfterAgent 必须走 textBlock 假重放路径，
// 否则 channel 中没有任何文本内容。
// 业务意义：兼容不发 delta 的 Model 实现（如某些旧版模型或 mock）。
func TestRunStreamEmptyDeltaFallsBackToFakeReplay(t *testing.T) {
	// deltaModel 发空 delta（Delta: ""），但 Final Response 有内容
	rt := newDeltaRuntime(t, []string{""}, "fallback text")

	stream, err := rt.RunStream(context.Background(), Request{Prompt: "go"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	got := collectDeltas(t, stream)

	// 空 delta 应走假重放：逐字符拆分 "fallback text"
	wantJoined := "fallback text"
	gotJoined := strings.Join(got, "")
	if gotJoined != wantJoined {
		t.Fatalf("假重放内容不匹配：期望 %q，实际 %q", wantJoined, gotJoined)
	}
	// 假重放的标志：单字符 delta（数量 > 1）
	if len(got) <= 1 {
		t.Fatalf("期望逐字符假重放（len > 1），实际 %d 个：%v", len(got), got)
	}
}

// TestRunStreamMaxTokensEscalationStartOnce 验证意图：当 completeOnce 因
// max_tokens 被多次调用（MaxTokensEscalation）时，content_block_start 只应发送一次。
// 业务意义：避免升级循环下重复发送 start 事件导致下游解析异常。
func TestRunStreamMaxTokensEscalationStartOnce(t *testing.T) {
	root := newClaudeProject(t)
	// escalationModel 第一次 CompleteStream 发 delta + max_tokens 响应，
	// 触发升级后第二次 CompleteStream 再发 delta + 正常响应。
	mdl := &escalationModel{
		deltas: [][]string{{"first "}, {"second "}},
		responses: []*model.Response{
			{Message: model.Message{Role: "assistant", Content: "first"}, StopReason: "max_tokens"},
			{Message: model.Message{Role: "assistant", Content: "second"}, StopReason: "end_turn"},
		},
	}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	defer rt.Close()

	stream, err := rt.RunStream(context.Background(), Request{Prompt: "go"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var starts []int
	for evt := range stream {
		if evt.Type == EventContentBlockStart {
			if evt.Index != nil {
				starts = append(starts, *evt.Index)
			}
		}
	}

	// 两次 CompleteStream 调用，但 start 只应出现一次（CAS 保证）
	if len(starts) != 1 {
		t.Fatalf("content_block_start 应只出现 1 次，实际 %d 次：%v", len(starts), starts)
	}
}

// escalationModel 模拟 MaxTokensEscalation 场景：多次 CompleteStream 调用。
type escalationModel struct {
	deltas    [][]string // 每次 CompleteStream 发的 delta
	responses []*model.Response
	callCnt   int
}

func (m *escalationModel) Complete(_ context.Context, _ model.Request) (*model.Response, error) {
	if m.callCnt >= len(m.responses) {
		return m.responses[len(m.responses)-1], nil
	}
	return m.responses[m.callCnt], nil
}

func (m *escalationModel) CompleteStream(_ context.Context, _ model.Request, cb model.StreamHandler) error {
	if m.callCnt < len(m.deltas) {
		for _, d := range m.deltas[m.callCnt] {
			if err := cb(model.StreamResult{Delta: d}); err != nil {
				return err
			}
		}
	}
	resp, err := m.Complete(context.Background(), model.Request{})
	if err != nil {
		return err
	}
	m.callCnt++
	return cb(model.StreamResult{Final: true, Response: resp})
}
