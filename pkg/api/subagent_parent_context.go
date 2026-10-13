package api

// subagentParentContext 是父 runtime 传给子 agent 派发/构建链的全部继承状态。
// 此前这组字段散落在 buildSubOptions 对 t.opts 与 t.runtime 的多点直读，新增
// 继承字段容易漏传（历史上修过禁用清单漏继承的问题）。
//
// 约定：新增继承字段时只改两处——parentContext()（唯一装配点）和该字段真正
// 的消费点；不要再往派发链上加散参数，也不要在链上直读 t.opts / t.runtime。
type subagentParentContext struct {
	// BaseOptions 是父 Options 快照，并已在装配点合并父 runtime 的实时禁用
	// 清单（runtime 为 nil 时保留快照原值，不被空值覆盖）。消费方从此字段
	// 派生子 Options，不得绕过装配点直取 t.opts。
	BaseOptions Options
}
