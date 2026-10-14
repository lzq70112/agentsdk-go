package toolbuiltin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agent-tool-issues 非零退出码语义回归：grep/diff/test 用 exit 1 表达正常
// 结果（无匹配/有差异/条件假），白名单内的单条简单命令按失败阈值判定；
// 复合命令与未知命令保守沿用 POSIX 默认（非零即失败），避免把真实失败
// 改写成成功结果。
func TestIsBashExitError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		command  string
		exitCode int
		want     bool
	}{
		{name: "zero is never error", command: "grep zzz f", exitCode: 0, want: false},
		{name: "negative sentinel is error", command: "grep zzz f", exitCode: -1, want: true},
		{name: "grep no match is benign", command: "grep zzz f", exitCode: 1, want: false},
		{name: "grep real error", command: "grep zzz f", exitCode: 2, want: true},
		{name: "ripgrep no match is benign", command: "rg zzz f", exitCode: 1, want: false},
		{name: "diff differences are benign", command: "diff a b", exitCode: 1, want: false},
		{name: "diff real error", command: "diff a b", exitCode: 2, want: true},
		{name: "test false is benign", command: "test -f missing", exitCode: 1, want: false},
		{name: "bracket test false is benign", command: "[ -f missing ]", exitCode: 1, want: false},
		{name: "unknown command stays POSIX", command: "make build", exitCode: 1, want: true},
		{name: "builtin false stays failure", command: "false", exitCode: 1, want: true},
		{name: "exit builtin stays failure", command: "exit 7", exitCode: 7, want: true},
		{name: "compound command stays failure", command: "grep zzz f && echo hi", exitCode: 1, want: true},
		{name: "pipeline stays failure", command: "cat f | grep zzz", exitCode: 1, want: true},
		{name: "or-list stays failure", command: "grep zzz f || echo miss", exitCode: 1, want: true},
		{name: "semicolon list stays failure", command: "grep zzz f; echo done", exitCode: 1, want: true},
		{name: "subshell stays failure", command: "(grep zzz f)", exitCode: 1, want: true},
		{name: "command substitution stays failure", command: "$(grep zzz f)", exitCode: 1, want: true},
		{name: "variable command stays failure", command: "$CMD f", exitCode: 1, want: true},
		{name: "timeout wrapper stays failure", command: "timeout 5 grep zzz f", exitCode: 1, want: true},
		{name: "quoted pipe does not split", command: `grep "a|b" f`, exitCode: 1, want: false},
		{name: "quoted semicolon does not split", command: `grep "a;b" f`, exitCode: 1, want: false},
		{name: "redirection keeps ownership", command: "grep zzz f > out.txt", exitCode: 1, want: false},
		{name: "command wrapper unwrapped", command: "command grep zzz f", exitCode: 1, want: false},
		{name: "env wrapper unwrapped", command: "env LC_ALL=C grep zzz f", exitCode: 1, want: false},
		{name: "env -u unwrapped", command: "env -u FOO grep zzz f", exitCode: 1, want: false},
		{name: "unknown wrapper stays failure", command: "sudo grep zzz f", exitCode: 1, want: true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isBashExitError(tc.command, tc.exitCode); got != tc.want {
				t.Fatalf("isBashExitError(%q, %d) = %v, want %v", tc.command, tc.exitCode, got, tc.want)
			}
		})
	}
}

func TestBashExitCodeHelper(t *testing.T) {
	t.Parallel()

	if got := bashExitCode(nil); got != 0 {
		t.Fatalf("bashExitCode(nil) = %d, want 0", got)
	}
	if got := bashExitCode(errors.New("start failed")); got != -1 {
		t.Fatalf("bashExitCode(non-exit error) = %d, want -1", got)
	}
	if got := bashExitCode(errors.Join(errors.New("stdout read: boom"), errors.New("stderr read: boom"))); got != -1 {
		t.Fatalf("bashExitCode(joined read errors) = %d, want -1", got)
	}
}

// skipIfNoBash 只在宿主上找不到可用 bash 时跳过：比 skipIfWindows 更精确，
// Windows + Git Bash 的组合（im-robot 生产形态）同样覆盖集成路径。
func skipIfNoBash(t *testing.T) {
	t.Helper()
	path, _ := ResolveBashExecutable()
	if strings.TrimSpace(path) == "" {
		t.Skip("no usable bash executable on this host")
	}
}

// 白名单命令的信号性退出码不再报工具失败：输出保留、末尾附 Exit code 标注、
// data 携带退出码，模型据此自行判断（如无匹配即查询结论）。
func TestBashToolBenignExitCodeReturnsNormally(t *testing.T) {
	skipIfNoBash(t)
	dir := cleanTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	tool := NewBashToolWithRoot(dir)

	res, err := tool.Execute(context.Background(), map[string]any{"command": "grep zzzz sample.txt"})
	if err != nil {
		t.Fatalf("benign exit should not fail: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected Success=true for benign exit code, output=%q", res.Output)
	}
	if !strings.Contains(res.Output, "Exit code: 1") {
		t.Fatalf("output should carry exit code note, got %q", res.Output)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("data is %T", res.Data)
	}
	if code, _ := data["exit_code"].(int); code != 1 {
		t.Fatalf("data exit_code = %v, want 1", data["exit_code"])
	}
}

// 白名单外命令保持 POSIX 默认：非零即失败，错误文案不变。
func TestBashToolNonWhitelistedFailureKeepsError(t *testing.T) {
	skipIfNoBash(t)
	dir := cleanTempDir(t)
	tool := NewBashToolWithRoot(dir)

	res, err := tool.Execute(context.Background(), map[string]any{"command": "false"})
	if err == nil || res == nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "command failed") {
		t.Fatalf("expected command failed error, got %v", err)
	}
}

// 复合命令保守按失败处理：即使其中一段是白名单命令，退出码也无法归属到
// 单条简单命令，不能把可能的真失败改写成成功结果。
func TestBashToolCompoundCommandStaysFailure(t *testing.T) {
	skipIfNoBash(t)
	dir := cleanTempDir(t)
	tool := NewBashToolWithRoot(dir)
	tool.AllowShellMetachars(true)

	res, err := tool.Execute(context.Background(), map[string]any{"command": "grep zzzz . && echo hi"})
	if err == nil || res == nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "command failed") {
		t.Fatalf("expected command failed error for compound command, got %v", err)
	}
}

// 成功路径的 data 同样携带 exit_code=0，保持契约一致。
func TestBashToolSuccessCarriesExitCodeZero(t *testing.T) {
	skipIfNoBash(t)
	dir := cleanTempDir(t)
	tool := NewBashToolWithRoot(dir)

	res, err := tool.Execute(context.Background(), map[string]any{"command": "printf ok"})
	if err != nil || res == nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("data is %T", res.Data)
	}
	if code, _ := data["exit_code"].(int); code != 0 {
		t.Fatalf("data exit_code = %v, want 0", data["exit_code"])
	}
	if strings.Contains(res.Output, "Exit code:") {
		t.Fatalf("success output should not carry exit code note, got %q", res.Output)
	}
}
