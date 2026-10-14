package toolbuiltin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/sandbox"
)

func TestGlobToolExecute(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tool := NewGlobToolWithRoot(root)
	tool.SetRespectGitignore(false)
	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"pattern": "*.txt",
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !res.Success || !strings.Contains(res.Output, "a.txt") {
		t.Fatalf("unexpected output %q", res.Output)
	}
}

func TestParseGlobPatternErrorsBuiltin(t *testing.T) {
	t.Parallel()

	if _, err := parseGlobPattern(nil); err == nil {
		t.Fatalf("expected nil params error")
	}
	if _, err := parseGlobPattern(map[string]interface{}{}); err == nil {
		t.Fatalf("expected missing pattern error")
	}
	if _, err := parseGlobPattern(map[string]interface{}{"pattern": 1}); err == nil {
		t.Fatalf("expected pattern type error")
	}
	if _, err := parseGlobPattern(map[string]interface{}{"pattern": ""}); err == nil {
		t.Fatalf("expected empty pattern error")
	}
}

func TestGlobToolExecuteErrorBranches(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	var nilTool *GlobTool
	if _, err := nilTool.Execute(context.Background(), map[string]interface{}{"pattern": "*"}); err == nil {
		t.Fatalf("expected nil tool error")
	}

	tool := NewGlobToolWithRoot(root)
	if _, err := tool.Execute(nil, map[string]interface{}{"pattern": "*"}); err == nil {
		t.Fatalf("expected nil context error")
	}
	if _, err := tool.Execute(context.Background(), nil); err == nil {
		t.Fatalf("expected params error")
	}
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "["}); err == nil {
		t.Fatalf("expected filepath.Glob error")
	}
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*", "path": 1}); err == nil {
		t.Fatalf("expected path type error")
	}
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*", "path": ".."}); err == nil {
		t.Fatalf("expected validate path error for escaped root")
	}
}

func TestGlobToolExecuteValidatePathMismatchSandbox(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	otherRoot := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(otherRoot); err == nil {
		otherRoot = resolved
	}
	tool := NewGlobToolWithSandbox(root, sandbox.NewFileSystemAllowList(otherRoot))
	tool.SetRespectGitignore(false)

	if _, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*.txt"}); err == nil {
		t.Fatalf("expected sandbox validate path error")
	}
}

func TestGlobToolExecuteValidatePathRejectsSymlinkMatch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	outside := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(outside); err == nil {
		outside = resolved
	}
	targetPath := filepath.Join(outside, "x.txt")
	if err := os.WriteFile(targetPath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(targetPath, filepath.Join(root, "escape.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	tool := NewGlobToolWithRoot(root)
	tool.SetRespectGitignore(false)
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*.txt"}); err == nil {
		t.Fatalf("expected validate path error for symlink match")
	}
}

func TestGlobToolExecuteRespectsGitignore(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("a.txt\n"), 0o600); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tool := NewGlobToolWithRoot(root)
	tool.SetRespectGitignore(true)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*.txt"})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	// P8 透明化：命中被 gitignore 全部隐藏时空结果必须说明原因。
	if !res.Success || strings.TrimSpace(res.Output) != "no matches (1 paths hidden by .gitignore)" {
		t.Fatalf("expected hidden-by-gitignore output, got %q", res.Output)
	}
	if data, ok := res.Data.(map[string]interface{}); ok {
		if hidden, _ := data["hidden_by_gitignore"].(int); hidden != 1 {
			t.Fatalf("expected hidden_by_gitignore=1, got %v", data["hidden_by_gitignore"])
		}
	}
}

func TestFormatGlobOutputAndDisplayPathFallback(t *testing.T) {
	t.Parallel()

	if got := formatGlobOutput(nil, false, 0); got != "no matches" {
		t.Fatalf("unexpected output %q", got)
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if got := displayPath(filepath.Dir(root), root); got == "." {
		t.Fatalf("expected fallback to absolute path for escaped root, got %q", got)
	}
}
