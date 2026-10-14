package tool

import "context"

// ToolEvent 是工具向事件流主动推送的结构化事件。与 Call.StreamSink 的
// 增量文本流不同，ToolEvent 面向"一次状态变更、一份结构化负载"的场景
// （如 task 工具的清单全量快照）：由运行时把 sink 注入 ctx，工具在
// Execute 中取用，api 层将其适配为 StreamEvent 走既有事件流管道透传，
// 工具侧因此不依赖 api 包（避免循环引用）。
type ToolEvent struct {
	Type    string      // Type 是事件协议类型（如 "todo_updated"），由工具自身定义。
	Name    string      // Name 是来源工具名，便于消费方路由。
	Payload interface{} // Payload 是结构化负载，语义由事件类型约定。
}

// ToolEventSink 由运行时注入 ctx，工具经它把 ToolEvent 送入事件流。
type ToolEventSink func(ctx context.Context, event ToolEvent)

type toolEventSinkCtxKey struct{}

// WithToolEventSink 在 ctx 上挂载事件出口；nil sink 等于不挂载。
func WithToolEventSink(ctx context.Context, sink ToolEventSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, toolEventSinkCtxKey{}, sink)
}

// ToolEventSinkFromContext 取回事件出口；未挂载时返回 nil，工具据此跳过
// 事件发送（非流式运行/直连单测路径不发送事件，不影响工具结果）。
func ToolEventSinkFromContext(ctx context.Context) ToolEventSink {
	if ctx == nil {
		return nil
	}
	if sink, ok := ctx.Value(toolEventSinkCtxKey{}).(ToolEventSink); ok {
		return sink
	}
	return nil
}
