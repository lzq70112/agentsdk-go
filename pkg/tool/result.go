package tool

// OutputRef describes where tool output has been persisted when it is too large
// (or otherwise undesirable) to embed directly in ToolResult.Output.
type OutputRef struct {
	Path      string `json:"path,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ToolResult captures the outcome of a tool invocation.
type ToolResult struct {
	Success   bool
	Output    string
	OutputRef *OutputRef
	Data      interface{}
	Error     error
	// EndTurn 表示工具要求本批工具调用执行完毕后回合就此收尾：运行时不再
	// 进行下一轮模型迭代，以当前响应干净结束（如提问超时需等待用户指示）。
	EndTurn bool
}
