package api

import (
	"context"
	"testing"
)

// agent-tool-issues P1 回归：enabled_tools 配置的工具必须全部出现在主会话
// 下发列表（AvailableTools）中——注册与下发共享同一 registry，历史上出现过
// "模型自述缺工具被误判为 SDK 静默过滤"。该测试锁死配置↔注册↔下发的
// 一致性，任何引入静默过滤的改动都会在此暴露。
func TestEnabledToolsFullyExposedToMainSession(t *testing.T) {
	root := t.TempDir()
	enabled := []string{"write", "edit", "bash", "read", "glob", "grep", "skill", "subagent"}

	rt, err := New(context.Background(), Options{
		ProjectRoot:         root,
		EnabledBuiltinTools: enabled,
		Model:               &stubModel{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close() //nolint:errcheck

	exposed := map[string]bool{}
	for _, def := range rt.AvailableTools() {
		exposed[def.Name] = true
	}
	// 8 个配置工具 + subagent 命中时连带注册的 3 个查询/停止/取回工具。
	for _, name := range []string{
		"bash", "read", "write", "edit", "glob", "grep", "skill",
		"subagent", "subagent_status", "subagent_stop", "subagent_output",
	} {
		if !exposed[name] {
			t.Errorf("tool %q 配置启用但未出现在主会话下发列表", name)
		}
	}
}

// agent-tool-issues P5 回归：子 runtime 内 subagent 系（派发/查询/停止/取回）
// 不得下发——Execute 已硬阻断递归派发，下发只会误导调用方高估自身能力。
// 同时验证子会话对同一份 enabled_tools 仍拿到全部常规工具（P2 一致性）。
func TestSubagentToolsNotExposedInsideSubagent(t *testing.T) {
	root := t.TempDir()
	enabled := []string{"write", "edit", "bash", "read", "glob", "grep", "skill", "subagent"}

	rt, err := New(context.Background(), Options{
		ProjectRoot:         root,
		EnabledBuiltinTools: enabled,
		Model:               &stubModel{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close() //nolint:errcheck
	if rt.subagent == nil {
		t.Fatal("main runtime should register subagent tool")
	}

	subOpts := rt.subagent.buildSubOptions(AgentType{}, nil)
	subRT, err := New(context.Background(), subOpts)
	if err != nil {
		t.Fatalf("New sub runtime: %v", err)
	}
	defer subRT.Close() //nolint:errcheck

	exposed := map[string]bool{}
	for _, def := range subRT.AvailableTools() {
		exposed[def.Name] = true
	}
	for _, banned := range []string{"subagent", "subagent_status", "subagent_stop", "subagent_output"} {
		if exposed[banned] {
			t.Errorf("tool %q 不应下发到子会话（递归派发已被硬阻断）", banned)
		}
	}
	for _, required := range []string{"bash", "read", "write", "edit", "glob", "grep", "skill"} {
		if !exposed[required] {
			t.Errorf("tool %q 应下发到子会话，实际缺失", required)
		}
	}
}
