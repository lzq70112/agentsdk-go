package toolbuiltin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetBashMaterialize 提供与解析器一致的 GOOS 接缝注入/还原。
func resetBashMaterializeGOOS(t *testing.T, goos string) {
	t.Helper()
	saved := bashResolverGOOS
	t.Cleanup(func() { bashResolverGOOS = saved })
	bashResolverGOOS = func() string { return goos }
}

// TestNormalizeBashNewlines 固化 CRLF 归一化：从 Windows 复制的带 \r\n 的
// 多行命令必须能正确执行——先归一化，不让 \r 混进脚本字节。
func TestNormalizeBashNewlines(t *testing.T) {
	t.Parallel()
	got := normalizeBashNewlines("echo one\r\necho two\recho three")
	if got != "echo one\necho two\necho three" {
		t.Fatalf("normalizeBashNewlines = %q", got)
	}
}

// TestBashMaterializeScriptByteFidelity 固化字节纪律：物化后的脚本内容 =
// 归一化后的命令 + 末尾补齐的换行（末尾已有换行则不重复补），无 BOM。
// 多行、heredoc、续行因此原样保留，不经过命令行转义层。
func TestBashMaterializeScriptByteFidelity(t *testing.T) {
	t.Parallel()

	command := "echo '第一行-πλü'\r\ncat <<'EOF'\nheredoc 体保持原样\nEOF"
	path, cleanup, err := bashMaterializeScript(command)
	if err != nil {
		t.Fatalf("bashMaterializeScript: %v", err)
	}
	defer cleanup()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	want := "echo '第一行-πλü'\ncat <<'EOF'\nheredoc 体保持原样\nEOF\n"
	if string(raw) != want {
		t.Fatalf("script bytes = %q, want %q", raw, want)
	}
}

// TestBashMaterializeScriptCleanupRemovesFile 固化清理契约：执行后（含
// 失败路径）临时脚本必须被删除，不在 temp 目录堆积。
func TestBashMaterializeScriptCleanupRemovesFile(t *testing.T) {
	t.Parallel()

	path, cleanup, err := bashMaterializeScript("echo ok")
	if err != nil {
		t.Fatalf("bashMaterializeScript: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("script should exist before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("script should be removed after cleanup, stat err = %v", err)
	}
	cleanup() // 幂等：重复调用不 panic
}

// TestRewriteWindowsNullRedirect 固化 >nul 改写：仅 Windows 生效、作用于
// 全量 payload、覆盖 >nul / 2>nul / >>nul 与常见分隔边界，且不误伤
// nul.txt 这类普通文件名。
func TestRewriteWindowsNullRedirect(t *testing.T) {
	cases := []struct {
		goos, in, want string
	}{
		{"windows", "echo a >nul", "echo a >/dev/null"},
		{"windows", "echo a > nul", "echo a > /dev/null"},
		{"windows", "cmd 2>nul && echo b", "cmd 2>/dev/null && echo b"},
		{"windows", "cmd >>nul\necho next", "cmd >>/dev/null\necho next"},
		{"windows", "cat f >nul.txt", "cat f >nul.txt"},
		{"windows", "echo nul", "echo nul"},
		{"linux", "echo a >nul", "echo a >nul"},
	}
	for _, tc := range cases {
		resetBashMaterializeGOOS(t, tc.goos)
		if got := rewriteWindowsNullRedirect(tc.in); got != tc.want {
			t.Fatalf("goos=%s rewrite(%q) = %q, want %q", tc.goos, tc.in, got, tc.want)
		}
	}
}

// TestBashExecuteHeredocMultiLine 意图级验证：一条业务上必须多行的命令
// （heredoc 生成文件）端到端可执行。旧的单行命令行传参通道下这类命令
// 无法可靠工作，多行支持是模型正确表达 shell 逻辑的前提。
func TestBashExecuteHeredocMultiLine(t *testing.T) {
	if diag := BashResolutionDiagnostic(); diag.Path == "" {
		t.Skip("bash 不可用")
	}
	b := NewBashToolWithRoot(t.TempDir())
	b.AllowShellMetachars(true)
	res, err := b.Execute(t.Context(), map[string]any{
		"command": "cat <<'EOF' > out.txt\nhello-多行\nworld\nEOF\ncat out.txt",
	})
	if err != nil {
		t.Fatalf("Execute heredoc: %v", err)
	}
	if !strings.Contains(res.Output, "hello-多行") || !strings.Contains(res.Output, "world") {
		t.Fatalf("heredoc content lost, output=%q", res.Output)
	}
	data, err := os.ReadFile(filepath.Join(b.root, "out.txt"))
	if err != nil || string(data) != "hello-多行\nworld\n" {
		t.Fatalf("out.txt = %q err=%v, want heredoc bytes", data, err)
	}
}

// TestBashExecuteLongScriptBeyondWindowsCommandLineLimit 意图级验证：超过
// Windows CreateProcess 32K 命令行上限的长脚本可执行（validator 上限经
// SetCommandLimits 抬高——那是代码生成场景的文档化出口）。旧通道下这条
// 命令在 Windows 上会直接 fork/exec 失败。
func TestBashExecuteLongScriptBeyondWindowsCommandLineLimit(t *testing.T) {
	if diag := BashResolutionDiagnostic(); diag.Path == "" {
		t.Skip("bash 不可用")
	}
	b := NewBashToolWithRoot(t.TempDir())
	b.AllowShellMetachars(true)
	b.SetCommandLimits(65536, 64)

	long := strings.Repeat("甲乙丙丁", 5000) // 20000 rune = 60000 字节：超 32K 命令行上限，低于测试自抬的 validator 上限
	res, err := b.Execute(t.Context(), map[string]any{
		"command": "echo '" + long + "' | wc -c",
	})
	if err != nil {
		t.Fatalf("Execute long script: %v", err)
	}
	if !strings.Contains(res.Output, "60001") {
		t.Fatalf("long payload corrupted, output=%q", res.Output)
	}
}

// TestBashStreamExecuteMultiLine 固化流式路径同样走物化通道：多行命令在
// StreamExecute 下与 Execute 行为一致。
func TestBashStreamExecuteMultiLine(t *testing.T) {
	if diag := BashResolutionDiagnostic(); diag.Path == "" {
		t.Skip("bash 不可用")
	}
	b := NewBashToolWithRoot(t.TempDir())
	b.AllowShellMetachars(true)
	var stdout strings.Builder
	_, err := b.StreamExecute(t.Context(), map[string]any{
		"command": "echo line-1\necho line-2",
	}, func(chunk string, isStderr bool) {
		if !isStderr {
			stdout.WriteString(chunk)
		}
	})
	if err != nil {
		t.Fatalf("StreamExecute: %v", err)
	}
	if !strings.Contains(stdout.String(), "line-1") || !strings.Contains(stdout.String(), "line-2") {
		t.Fatalf("stream output = %q", stdout.String())
	}
}
