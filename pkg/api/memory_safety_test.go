package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hooks "github.com/lzq70112/agentsdk-go/pkg/hooks"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

func TestMemoryWriteGuardAllowsCleanMemoryWrite(t *testing.T) {
	memDir := t.TempDir()
	guard := &memoryWriteGuard{autoDir: memDir, sandboxRoot: t.TempDir()}
	err := guard.preToolUse("write", map[string]any{
		"file_path": filepath.Join(memDir, "user_role.md"),
		"content":   "---\nname: u\ndescription: 用户角色\n---\nbody",
	})
	if err != nil {
		t.Fatalf("clean memory write must pass, got %v", err)
	}
}

func TestMemoryWriteGuardDeniesSecrets(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		params   map[string]any
		contains string // 错误信息应点名的规则
	}{
		{
			name:     "aws key in write",
			tool:     "write",
			params:   map[string]any{"file_path": "MEM", "content": "key = AKIAIOSFODNN7EXAMPLE"},
			contains: "aws_access_key_id",
		},
		{
			name:     "openai key in write",
			tool:     "write",
			params:   map[string]any{"file_path": "MEM", "content": "OPENAI_API_KEY=sk-proj-abcdefghij0123456789ABCDEFGH"},
			contains: "openai_api_key",
		},
		{
			name:     "private key block in edit",
			tool:     "edit",
			params:   map[string]any{"file_path": "MEM", "old_string": "a", "new_string": "-----BEGIN RSA PRIVATE KEY-----\nMIIE"},
			contains: "private_key_block",
		},
		{
			name:     "github token in write",
			tool:     "write",
			params:   map[string]any{"file_path": "MEM", "content": "token: ghp_0123456789abcdefghijklmnopqrstuvwxyz"},
			contains: "github_token",
		},
		{
			name:     "generic credential assignment",
			tool:     "write",
			params:   map[string]any{"file_path": "MEM", "content": `api_key = "AbCdEf0123456789AbCdEf0123456789AbCdEf01"`},
			contains: "credential_assignment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memDir := t.TempDir()
			guard := &memoryWriteGuard{autoDir: memDir, sandboxRoot: t.TempDir()}
			tt.params["file_path"] = filepath.Join(memDir, "leak.md")
			err := guard.preToolUse(tt.tool, tt.params)
			if err == nil {
				t.Fatal("secret in memory write must be denied")
			}
			if !errors.Is(err, ErrToolUseDenied) {
				t.Fatalf("deny must wrap ErrToolUseDenied, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("error should name rule %q, got %q", tt.contains, err.Error())
			}
			// 错误信息不得包含密钥明文。
			for _, secret := range []string{"AKIAIOSFODNN7EXAMPLE", "sk-proj-abcdefghij0123456789ABCDEFGH", "-----BEGIN RSA PRIVATE KEY-----", "ghp_0123456789abcdefghijklmnopqrstuvwxyz", "AbCdEf0123456789AbCdEf0123456789AbCdEf01"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error message must not echo the secret: %q", err.Error())
				}
			}
		})
	}
}

func TestMemoryWriteGuardDeniesExtraDirWrites(t *testing.T) {
	extra := t.TempDir()
	guard := &memoryWriteGuard{autoDir: t.TempDir(), extraDirs: []string{extra}, sandboxRoot: t.TempDir()}
	err := guard.preToolUse("write", map[string]any{
		"file_path": filepath.Join(extra, "note.md"),
		"content":   "totally safe",
	})
	if err == nil || !errors.Is(err, ErrToolUseDenied) {
		t.Fatalf("extra dir write must be denied, got %v", err)
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("deny message should explain read-only, got %q", err.Error())
	}
}

func TestMemoryWriteGuardLeavesOtherPathsUnchanged(t *testing.T) {
	memDir := t.TempDir()
	outside := t.TempDir()
	guard := &memoryWriteGuard{autoDir: memDir, extraDirs: []string{t.TempDir()}, sandboxRoot: outside}

	t.Run("non-memory path untouched", func(t *testing.T) {
		if err := guard.preToolUse("write", map[string]any{
			"file_path": filepath.Join(outside, "code.go"),
			"content":   "AKIAIOSFODNN7EXAMPLE", // 工作区写入不归 memory 守卫管
		}); err != nil {
			t.Fatalf("non-memory write must be untouched, got %v", err)
		}
	})
	t.Run("non-write tool untouched", func(t *testing.T) {
		if err := guard.preToolUse("bash", map[string]any{
			"command": "echo " + filepath.Join(memDir, "x.md"),
		}); err != nil {
			t.Fatalf("bash must be untouched by memory guard, got %v", err)
		}
	})
	t.Run("missing file path untouched", func(t *testing.T) {
		if err := guard.preToolUse("write", map[string]any{"content": "x"}); err != nil {
			t.Fatalf("unresolvable target must defer to the tool's own validation, got %v", err)
		}
	})
}

func TestMemoryWriteGuardNilIsPassthrough(t *testing.T) {
	var guard *memoryWriteGuard
	if err := guard.preToolUse("write", map[string]any{"file_path": "/x", "content": "y"}); err != nil {
		t.Fatalf("nil guard must be passthrough, got %v", err)
	}
}

func TestRuntimeHookAdapterMemoryGuardRunsBeforeUserHookChain(t *testing.T) {
	memDir := t.TempDir()
	extra := t.TempDir()

	// executor 为 nil 即用户 hook 链路不存在：deny 仍然发生，说明 memory guard
	// 的检查位于用户 hook 消费之前（生产实现里 guard 在 executor.Execute 之前）。
	adapter := &runtimeHookAdapter{
		memoryGuard: &memoryWriteGuard{
			autoDir:     memDir,
			extraDirs:   []string{extra},
			sandboxRoot: t.TempDir(),
		},
	}
	_, err := adapter.PreToolUse(context.Background(), hooks.ToolUsePayload{
		Name: "write",
		Params: map[string]any{
			"file_path": filepath.Join(extra, "note.md"),
			"content":   "safe",
		},
	})
	if err == nil {
		t.Fatal("extra dir write must be denied")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("deny should come from memory guard, got %q", err.Error())
	}
}

func TestMemoryGuardEndToEndExtraDirWriteDenied(t *testing.T) {
	memDir := t.TempDir()
	extra := t.TempDir()

	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID:        "w1",
			Name:      "write",
			Arguments: map[string]any{"file_path": filepath.Join(extra, "note.md"), "content": "hello"},
		}}}},
		{Message: model.Message{Role: "assistant", Content: "done"}},
	}}
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       mdl,
		Memory:      &MemoryOptions{Dir: memDir, ExtraDirs: []string{extra}},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if _, err := rt.Run(context.Background(), Request{
		Prompt:        "write a note",
		ToolWhitelist: []string{"write"},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 工具被 deny：extra 目录不应产生文件。
	if _, statErr := os.Stat(filepath.Join(extra, "note.md")); statErr == nil {
		t.Fatal("write into extra dir must not reach the filesystem")
	}
}
