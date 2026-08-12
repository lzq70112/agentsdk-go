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

func MaxOutputSizeOf(tool Tool, fallback int) int {
	if provider, ok := tool.(OutputLimiter); ok {
		if limit := provider.MaxOutputSize(); limit > 0 {
			return limit
		}
	}
	return fallback
}
