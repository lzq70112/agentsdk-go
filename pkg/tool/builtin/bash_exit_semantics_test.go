package toolbuiltin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bash 非零退出码全量统一回归（对齐 Claude Code）：任何非零退出码都正常
// 返回（Success=true、输出末尾附 Exit code 标注、data 携带退出码），判断权
// 交模型；仅超时、取消与取不到可靠退出码的异常路径保留 error。
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

func TestBashChildEnv(t *testing.T) {
	t.Parallel()

	// 环境中已有 PYTHONIOENCODING：setdefault 语义，尊重用户显式设置。
	withVar := []string{"PATH=/bin", "PYTHONIOENCODING=gbk", "HOME=/root"}
	if got := bashChildEnv(withVar); len(got) != len(withVar) {
		t.Fatalf("expected env untouched when PYTHONIOENCODING present, got %v", got)
	}

	// 环境中缺失：追加 utf-8 默认值。
	withoutVar := []string{"PATH=/bin", "HOME=/root"}
	got := bashChildEnv(withoutVar)
	if len(got) != len(withoutVar)+1 {
		t.Fatalf("expected PYTHONIOENCODING appended, got %v", got)
	}
	if last := got[len(got)-1]; last != "PYTHONIOENCODING=utf-8" {
		t.Fatalf("last env entry = %q, want PYTHONIOENCODING=utf-8", last)
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

// grep 无匹配（exit 1）正常返回：输出保留、末尾附 Exit code 标注、data 携带
// 退出码，模型据此自行判断（无匹配即查询结论）。
func TestBashToolBenignExitCodeReturnsNormally(t *testing.T) {
	skipIfNoBash(t)
	dir := cleanTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	tool := NewBashToolWithRoot(dir)

	res, err := tool.Execute(context.Background(), map[string]any{"command": "grep zzzz sample.txt"})
	if err != nil {
		t.Fatalf("non-zero exit should not fail: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected Success=true for non-zero exit, output=%q", res.Output)
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

// 任意命令的非零退出码统一正常返回：false（exit 1）、exit 7、复合命令
// 一视同仁，模型从输出与退出码注记自行判断。
func TestBashToolAnyNonZeroExitReturnsNormally(t *testing.T) {
	skipIfNoBash(t)
	dir := cleanTempDir(t)
	tool := NewBashToolWithRoot(dir)

	res, err := tool.Execute(context.Background(), map[string]any{"command": "false"})
	if err != nil || res == nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if !res.Success {
		t.Fatalf("expected Success=true, output=%q", res.Output)
	}
	if !strings.Contains(res.Output, "Exit code: 1") {
		t.Fatalf("output should carry exit code note, got %q", res.Output)
	}

	res7, err := tool.Execute(context.Background(), map[string]any{"command": "exit 7"})
	if err != nil || res7 == nil {
		t.Fatalf("res7=%v err=%v", res7, err)
	}
	if !res7.Success || !strings.Contains(res7.Output, "Exit code: 7") {
		t.Fatalf("expected normal return with Exit code: 7, got success=%v output=%q", res7.Success, res7.Output)
	}

	compound := NewBashToolWithRoot(dir)
	compound.AllowShellMetachars(true)
	resC, err := compound.Execute(context.Background(), map[string]any{"command": "false && echo hi"})
	if err != nil || resC == nil {
		t.Fatalf("resC=%v err=%v", resC, err)
	}
	if !resC.Success || !strings.Contains(resC.Output, "Exit code: 1") {
		t.Fatalf("expected compound non-zero exit to return normally, got success=%v output=%q", resC.Success, resC.Output)
	}
}

// 环境注入回归：bash 子进程能观察到 PYTHONIOENCODING（宿主显式设置时尊重
// 现值，否则为 utf-8 兜底）。
func TestBashToolChildEnvCarriesPythonIOEncoding(t *testing.T) {
	skipIfNoBash(t)
	dir := cleanTempDir(t)
	tool := NewBashToolWithRoot(dir)
	tool.AllowShellMetachars(true)

	res, err := tool.Execute(context.Background(), map[string]any{"command": `printf '%s' "$PYTHONIOENCODING"`})
	if err != nil || res == nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	want := os.Getenv("PYTHONIOENCODING")
	if want == "" {
		want = "utf-8"
	}
	if strings.TrimSpace(res.Output) != want {
		t.Fatalf("child env PYTHONIOENCODING = %q, want %q", res.Output, want)
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
