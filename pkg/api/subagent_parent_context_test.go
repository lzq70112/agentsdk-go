package api

import (
	"reflect"
	"testing"
)

// TestSubagentParentContextKeepsSnapshotWhenRuntimeNil 固化继承语义的一半：
// runtime 为 nil（如纯构造场景）时，子选项保留 t.opts 快照里的禁用清单，
// 不得被空值覆盖。与 TestBuildSubOptionsInheritsDisabledLists（实时清单
// 覆盖快照 + 克隆防污染）共同锁住 buildSubOptions 的合并契约，收敛到
// subagentParentContext 单点装配时两条都必须原样成立。
func TestSubagentParentContextKeepsSnapshotWhenRuntimeNil(t *testing.T) {
	t.Parallel()

	stale := &subagentTool{opts: Options{
		DisabledSkills: []string{"ssh-secured"},
		DisabledMCP:    []string{"context7"},
	}}

	subOpts := stale.buildSubOptions(AgentType{Name: "general"}, nil)
	if len(subOpts.DisabledSkills) != 1 || subOpts.DisabledSkills[0] != "ssh-secured" {
		t.Fatalf("runtime 为 nil 时应保留快照禁用清单，got %+v", subOpts.DisabledSkills)
	}
	if len(subOpts.DisabledMCP) != 1 || subOpts.DisabledMCP[0] != "context7" {
		t.Fatalf("runtime 为 nil 时应保留快照 MCP 禁用清单，got %+v", subOpts.DisabledMCP)
	}
	if !subOpts.subagentDisabled {
		t.Fatal("child options must keep subagent disabled")
	}
}

// TestSubagentParentContextAssemblyIsIdempotent 固化装配点语义：parentContext()
// 可重复调用，实时清单每次取自 runtime 的克隆，派发链上多处消费不产生状态
// 漂移；修改装配结果不得穿透回 t.opts 快照。
func TestSubagentParentContextAssemblyIsIdempotent(t *testing.T) {
	t.Parallel()

	rt := &Runtime{opts: Options{DisabledSkills: []string{"ssh-secured"}, DisabledMCP: []string{"context7"}}}
	stale := &subagentTool{opts: Options{DisabledSkills: []string{"snapshot-skill"}}}
	stale.bindRuntime(rt)

	pc1 := stale.parentContext()
	pc2 := stale.parentContext()
	if !reflect.DeepEqual(pc1.BaseOptions.DisabledSkills, pc2.BaseOptions.DisabledSkills) {
		t.Fatalf("两次装配的禁用清单不一致: %v vs %v", pc1.BaseOptions.DisabledSkills, pc2.BaseOptions.DisabledSkills)
	}
	if !reflect.DeepEqual(pc1.BaseOptions.DisabledMCP, pc2.BaseOptions.DisabledMCP) {
		t.Fatalf("两次装配的 MCP 禁用清单不一致: %v vs %v", pc1.BaseOptions.DisabledMCP, pc2.BaseOptions.DisabledMCP)
	}

	// 装配结果与 t.opts 快照隔离：改装配结果不污染快照。
	pc1.BaseOptions.DisabledSkills[0] = "tampered"
	if stale.opts.DisabledSkills[0] != "snapshot-skill" {
		t.Fatalf("parentContext 装配污染了 t.opts 快照: %+v", stale.opts.DisabledSkills)
	}
}
