package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/middleware"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

// TestChunkStringPreservesMultibyteRunes 验证分片不会切断多字节 UTF-8 字符。
// 工具入参经 chunkString 分片后逐片 json.Marshal 发出，消费端再解码拼接；
// 若切点落在 rune 中间，json.Marshal 会把半个字符替换为 U+FFFD，使中文等
// 非 ASCII 文本在流式展示中变成乱码（�/◆）。此测试锁定“重组后文本不变”。
func TestChunkStringPreservesMultibyteRunes(t *testing.T) {
	original := `{"name":"x","instruction":"你是一个只读探查任务，目标是把结论返回。"}`
	// size 覆盖大于、等于、小于单个中文字符（3 字节）的情形。
	for _, size := range []int{1, 2, 3, 4, 10, 64} {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			var reassembled strings.Builder
			for _, chunk := range chunkString(original, size) {
				encoded, err := json.Marshal(chunk)
				if err != nil {
					t.Fatalf("marshal chunk: %v", err)
				}
				var back string
				if err := json.Unmarshal(encoded, &back); err != nil {
					t.Fatalf("unmarshal chunk: %v", err)
				}
				reassembled.WriteString(back)
			}
			if got := reassembled.String(); got != original {
				t.Fatalf("分片重组后文本被破坏:\n got: %q\nwant: %q", got, original)
			}
		})
	}
}

// TestAfterAgentForwardsRealStopReason 验证 AfterAgent 透传 resp.StopReason
// 而非硬编码 "end_turn"。这是自动续写功能的前提：IM Robot 层需要从
// EventMessageDelta 事件检测到 "max_tokens" 截断信号。
func TestAfterAgentForwardsRealStopReason(t *testing.T) {
	cases := []struct {
		name       string
		stopReason string
		wantReason string
	}{
		{name: "max_tokens 截断", stopReason: "max_tokens", wantReason: "max_tokens"},
		{name: "空 StopReason 回退 end_turn", stopReason: "", wantReason: "end_turn"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newClaudeProject(t)
			resp := &model.Response{
				Message:    model.Message{Role: "assistant", Content: "done"},
				StopReason: tc.stopReason,
			}
			mdl := &stubModel{responses: []*model.Response{resp}}

			rt, err := New(context.Background(), Options{
				ProjectRoot: root,
				Model:       mdl,
			})
			if err != nil {
				t.Fatalf("runtime: %v", err)
			}
			t.Cleanup(func() { _ = rt.Close() })

			stream, err := rt.RunStream(context.Background(), Request{Prompt: "go"})
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}

			var gotReason string
			for evt := range stream {
				if evt.Type == EventMessageDelta && evt.Delta != nil {
					gotReason = evt.Delta.StopReason
				}
			}
			if gotReason != tc.wantReason {
				t.Fatalf("StopReason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

// TestAfterAgentEmitsCorrectStopReason 直接调用 AfterAgent，验证 StopReason 逻辑。
// 覆盖无工具调用时透传 resp.StopReason、为空时回退 end_turn、有工具调用时覆盖为 tool_use。
func TestAfterAgentEmitsCorrectStopReason(t *testing.T) {
	cases := []struct {
		name         string
		stopReason   string
		hasToolCalls bool
		wantReason   string
	}{
		{name: "max_tokens 截断", stopReason: "max_tokens", wantReason: "max_tokens"},
		{name: "空 StopReason 回退 end_turn", stopReason: "", wantReason: "end_turn"},
		{name: "有 ToolCalls 覆盖为 tool_use", stopReason: "max_tokens", hasToolCalls: true, wantReason: "tool_use"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan StreamEvent, 100)
			pm := newProgressMiddleware(ch)

			resp := &model.Response{
				Message:    model.Message{Role: "assistant", Content: "done"},
				StopReason: tc.stopReason,
			}
			if tc.hasToolCalls {
				resp.Message.ToolCalls = []model.ToolCall{{ID: "t1", Name: "noop"}}
			}
			st := &middleware.State{ModelOutput: resp, Iteration: 0}

			if err := pm.AfterAgent(context.Background(), st); err != nil {
				t.Fatalf("AfterAgent: %v", err)
			}
			close(ch)

			var gotReason string
			for evt := range ch {
				if evt.Type == EventMessageDelta && evt.Delta != nil {
					gotReason = evt.Delta.StopReason
				}
			}
			if gotReason != tc.wantReason {
				t.Fatalf("StopReason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}
