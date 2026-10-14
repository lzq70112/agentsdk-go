package toolbuiltin

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// agent-tool-issues P4 回归：Windows 下 / 开头的 POSIX 风格绝对路径无法定位
// （filepath.IsAbs 为 false），历史上会被静默拼进沙箱根（/tmp/x → <root>\tmp\x），
// 在项目内创建目录污染工作区。新契约是显式拒绝并引导改用相对路径。
func TestWriteToolRejectsPOSIXAbsolutePathsOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("POSIX absolute path misresolution only affects Windows")
	}
	tool := NewWriteToolWithRoot(t.TempDir())
	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"file_path": "/tmp/agent_tool_test.txt",
		"content":   "x",
	})
	if err == nil {
		t.Fatalf("expected POSIX absolute path to be rejected, got result %+v", res)
	}
	if !strings.Contains(err.Error(), "POSIX-style absolute path") {
		t.Fatalf("error should explain the POSIX path rejection, got: %v", err)
	}
	if res != nil && strings.Contains(res.Output, "wrote") {
		t.Fatalf("nothing should be written, got output: %s", res.Output)
	}
}
