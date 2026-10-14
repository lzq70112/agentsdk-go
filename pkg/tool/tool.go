package tool

import "context"

type Metadata struct {
	IsReadOnly        bool
	IsDestructive     bool
	IsConcurrencySafe bool
}

type MetadataProvider interface {
	Metadata() Metadata
}

type DeferrableProvider interface {
	ShouldDefer() bool
}

// DispatchHider 是可选接口：实现者在特定状态下要求从模型下发列表中隐藏自己。
// 隐藏只影响下发（AvailableTools），注册保留——误调用仍会走到 Execute，
// 由实现返回明确的拒绝引导，而不是 "tool not found"。
type DispatchHider interface {
	HiddenFromDispatch() bool
}

type OutputLimiter interface {
	MaxOutputSize() int
}

// Tool represents an executable capability exposed to the agent runtime.
type Tool interface {
	// Name returns the unique identifier of the tool.
	Name() string

	// Description gives a short human readable summary.
	Description() string

	// Schema describes the tool parameters. Nil means the tool does not expect input.
	Schema() *JSONSchema

	// Execute runs the tool with validated parameters.
	Execute(ctx context.Context, params map[string]interface{}) (*ToolResult, error)
}

func MetadataOf(tool Tool) Metadata {
	if provider, ok := tool.(MetadataProvider); ok {
		return provider.Metadata()
	}
	return Metadata{}
}

func ShouldDefer(tool Tool) bool {
	if provider, ok := tool.(DeferrableProvider); ok {
		return provider.ShouldDefer()
	}
	return false
}

// HiddenFromDispatch 报告工具是否应从下发列表隐藏（未实现 DispatchHider 时为 false）。
func HiddenFromDispatch(t Tool) bool {
	h, ok := t.(DispatchHider)
	return ok && h.HiddenFromDispatch()
}

func MaxOutputSizeOf(tool Tool, fallback int) int {
	if provider, ok := tool.(OutputLimiter); ok {
		if limit := provider.MaxOutputSize(); limit > 0 {
			return limit
		}
	}
	return fallback
}
