package api

import (
	"context"
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/config"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

func mustCompile(t *testing.T, rules ...string) []denyRule {
	t.Helper()
	compiled, unenforceable := compileDenyRules(rules)
	if len(unenforceable) > 0 {
		t.Fatalf("unexpected unenforceable rules: %v", unenforceable)
	}
	return compiled
}

func TestCompileDenyRulesKinds(t *testing.T) {
	compiled, unenforceable := compileDenyRules([]string{
		"Bash(ssh:*)",
		"Bash(docker ps)",
		"Bash(docker * rm)",
		"Bash",
		"Read(~/.ssh/**)",
		"Bash(ssh",
		"",
	})
	if len(compiled) != 4 {
		t.Fatalf("expected 4 compiled rules, got %d: %+v", len(compiled), compiled)
	}
	if compiled[0].kind != denyPrefix || compiled[0].prefix != "ssh" {
		t.Errorf("rule 0 should be prefix ssh, got %+v", compiled[0])
	}
	if compiled[1].kind != denyExact || compiled[1].exact != "docker ps" {
		t.Errorf("rule 1 should be exact \"docker ps\", got %+v", compiled[1])
	}
	if compiled[2].kind != denyWildcard || compiled[2].re == nil {
		t.Errorf("rule 2 should be wildcard with regexp, got %+v", compiled[2])
	}
	if compiled[3].kind != denyBare || compiled[3].tool != "bash" {
		t.Errorf("rule 3 should be bare bash, got %+v", compiled[3])
	}
	// 非 bash 工具的 selector 与语法畸形项必须显式上报，不能静默吞掉。
	if len(unenforceable) != 2 || unenforceable[0] != "Read(~/.ssh/**)" || unenforceable[1] != "Bash(ssh" {
		t.Fatalf("expected [Read(~/.ssh/**) Bash(ssh)] as unenforceable, got %v", unenforceable)
	}
}

func TestMatchDenyBashPrefix(t *testing.T) {
	rules := mustCompile(t, "Bash(ssh:*)")
	hits := []string{
		"ssh root@192.168.1.1 hostname",
		"ssh   root@h",
		"MSYS_NO_PATHCONV=1 ssh -i /k/id_rsa -o BatchMode=yes root@h \"docker ps\"",
		"FOO=\"a b\" BAR=2 ssh x",
		"echo hi && ssh x",
		"cat file; ssh x",
		"find . | xargs ssh x",
		`ssh host "cd /x && systemctl restart nginx"`,
	}
	for _, cmd := range hits {
		if _, ok := matchDeny(rules, "bash", map[string]any{"command": cmd}); !ok {
			t.Errorf("expected deny hit for %q", cmd)
		}
	}
	misses := []string{
		"echo ssh",
		"gossh x",
		"sshd -T",
		"cat ssh",
		"echo 'run ssh later'",
	}
	for _, cmd := range misses {
		if _, ok := matchDeny(rules, "bash", map[string]any{"command": cmd}); ok {
			t.Errorf("expected no deny hit for %q", cmd)
		}
	}
}

func TestMatchDenyBashExactAndWildcard(t *testing.T) {
	exact := mustCompile(t, "Bash(docker ps)")
	for _, cmd := range []string{"docker ps", "  docker   ps  ", "FOO=1 docker ps"} {
		if _, ok := matchDeny(exact, "bash", map[string]any{"command": cmd}); !ok {
			t.Errorf("exact rule should hit %q", cmd)
		}
	}
	for _, cmd := range []string{"docker ps -a", "docker psx", "echo docker ps"} {
		if _, ok := matchDeny(exact, "bash", map[string]any{"command": cmd}); ok {
			t.Errorf("exact rule should not hit %q", cmd)
		}
	}

	wild := mustCompile(t, "Bash(docker * rm)")
	for _, cmd := range []string{"docker run --rm rm", "docker rm"} {
		_, hit := matchDeny(wild, "bash", map[string]any{"command": cmd})
		if want := cmd == "docker run --rm rm"; hit != want {
			t.Errorf("wildcard rule hit=%v for %q, want %v", hit, cmd, want)
		}
	}
}

func TestMatchDenyBareToolRule(t *testing.T) {
	rules := mustCompile(t, "WebFetch")
	if _, ok := matchDeny(rules, "WebFetch", nil); !ok {
		t.Error("bare WebFetch rule should deny the WebFetch tool")
	}
	if _, ok := matchDeny(rules, "bash", map[string]any{"command": "ssh x"}); ok {
		t.Error("bare WebFetch rule must not deny bash")
	}
	if _, ok := matchDeny(rules, "bash", nil); ok {
		t.Error("bare WebFetch rule must not deny bash even with nil params")
	}
}

// TestPermissionDenyBlocksBashViaSettings 走完整链路：settings 的 deny 规则
// 经桥接编译进 runtime，模型发起 bash ssh 调用时被拦截，拒绝原因以 tool
// result 回给模型（模型能读到原因并自行收尾，而不是整个回合崩掉）。
func TestPermissionDenyBlocksBashViaSettings(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{responses: []model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID:        "call-1",
			Name:      "bash",
			Arguments: map[string]any{"command": "ssh root@192.168.1.1 hostname"},
		}}}, StopReason: "tool_use"},
		{Message: model.Message{Role: "assistant", Content: "ssh 被禁，已停止"} , StopReason: "end_turn"},
	}}

	rt, err := New(ctx, Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "main",
		EnabledBuiltinTools: []string{"bash"},
		SettingsOverrides: &config.Settings{
			Permissions: &config.PermissionsConfig{Deny: []string{"Bash(ssh:*)"}},
		},
		AutoCompact: CompactConfig{Enabled: false},
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer rt.Close()

	if _, err := rt.Run(ctx, Request{SessionID: "s-deny", Prompt: "连上服务器看下主机名"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 拒绝原因必须以 tool result 落进历史，模型第二轮才能读到。
	denied := false
	for _, call := range mm.calls {
		for _, msg := range call.Messages {
			for _, tc := range msg.ToolCalls {
				if msg.Role == "tool" && strings.Contains(tc.Result, "denied by permissions.deny rule") {
					denied = true
				}
			}
		}
	}
	if !denied {
		t.Fatal("denial reason was not fed back to the model as a tool result")
	}
}

// TestPermissionDenyAllowsNonMatchingCommand 未命中 deny 的命令必须照常执行：
// deny 是精确拦截，不能把整个 bash 工具变成摆设。
func TestPermissionDenyAllowsNonMatchingCommand(t *testing.T) {
	ctx := context.Background()
	mm := &recordingMockModel{responses: []model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID:        "call-1",
			Name:      "bash",
			Arguments: map[string]any{"command": "echo ok"},
		}}}, StopReason: "tool_use"},
		{Message: model.Message{Role: "assistant", Content: "done"}, StopReason: "end_turn"},
	}}

	rt, err := New(ctx, Options{
		ModelFactory:        recordingMockModelFactory{model: mm},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "main",
		EnabledBuiltinTools: []string{"bash"},
		SettingsOverrides: &config.Settings{
			Permissions: &config.PermissionsConfig{Deny: []string{"Bash(ssh:*)"}},
		},
		AutoCompact: CompactConfig{Enabled: false},
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer rt.Close()

	if _, err := rt.Run(ctx, Request{SessionID: "s-allow", Prompt: "echo 一下"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	executed := false
	for _, call := range mm.calls {
		for _, msg := range call.Messages {
			for _, tc := range msg.ToolCalls {
				if msg.Role == "tool" && strings.Contains(tc.Result, "ok") {
					executed = true
				}
			}
		}
	}
	if !executed {
		t.Fatal("non-matching command should have executed and its output fed back")
	}
}
