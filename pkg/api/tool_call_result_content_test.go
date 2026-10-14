package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

// agent-tool-issues P7 回归：bash 链式命令失败时，已产生的 stdout/stderr 存放在
// ToolResult.Output 中（stderr 已合并），回传模型的 tool 结果必须保留这部分
// 输出——只回 {"error":"command failed: exit status N"} 会让调用方无从定位
// 失败原因。该行为在 toolCallResultContent 统一生效，所有工具的失败路径受益。
func TestToolCallResultContentKeepsPartialOutputOnError(t *testing.T) {
	runErr := errors.New("exit status 2")
	result := &tool.CallResult{
		Result: &tool.ToolResult{Success: false, Output: "dir-a\nls: cannot access '/nonexistent': No such file or directory"},
	}
	content := toolCallResultContent(result, runErr)
	if !strings.Contains(content, `"error":"exit status 2"`) {
		t.Fatalf("error JSON missing, got: %s", content)
	}
	if !strings.Contains(content, "dir-a") || !strings.Contains(content, "cannot access") {
		t.Fatalf("partial stdout/stderr must be preserved on failure, got: %s", content)
	}

	// 无部分输出时保持原样：只回错误 JSON。
	if got := toolCallResultContent(&tool.CallResult{}, runErr); got != `{"error":"exit status 2"}` {
		t.Fatalf("plain error content changed, got: %s", got)
	}
	// 成功路径不受影响：直接返回 Output。
	ok := &tool.CallResult{Result: &tool.ToolResult{Success: true, Output: "fine"}}
	if got := toolCallResultContent(ok, nil); got != "fine" {
		t.Fatalf("success content changed, got: %s", got)
	}
}
