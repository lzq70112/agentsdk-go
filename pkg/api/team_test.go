package api

import (
	"context"
	"testing"

	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/skills"
	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/subagents"
)

func TestRuntimeRunTeamAutoSelectDelegatesToSubagentManager(t *testing.T) {
	t.Parallel()

	mgr := subagents.NewManager()
	if err := mgr.Register(subagents.Definition{
		Name: "worker",
		Matchers: []skills.Matcher{
			skills.MatcherFunc(func(ctx skills.ActivationContext) skills.MatchResult {
				return skills.MatchResult{Matched: true, Score: 1}
			}),
		},
	}, subagents.HandlerFunc(func(ctx context.Context, subCtx subagents.Context, req subagents.Request) (subagents.Result, error) {
		return subagents.Result{Output: "ok"}, nil
	})); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	rt := &Runtime{opts: Options{subMgr: mgr}}
	res, err := rt.RunTeam(context.Background(), subagents.TeamRequest{
		Instruction: "inspect",
		Activation:  skills.ActivationContext{Prompt: "inspect"},
		MaxAgents:   1,
	})
	if err != nil {
		t.Fatalf("RunTeam: %v", err)
	}
	if len(res.Members) != 1 || res.Members[0].Name != "worker" || res.Members[0].Output != "ok" {
		t.Fatalf("unexpected team result: %+v", res)
	}
}

func TestRuntimeRunTeamCreatesWorkingTeamFromOptions(t *testing.T) {
	t.Parallel()

	root := newClaudeProject(t)
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       &stubModel{},
		Subagents: []SubagentRegistration{
			{
				Definition: subagents.Definition{Name: "alpha"},
				Handler: subagents.HandlerFunc(func(ctx context.Context, subCtx subagents.Context, req subagents.Request) (subagents.Result, error) {
					return subagents.Result{Output: "alpha:" + req.Instruction}, nil
				}),
			},
			{
				Definition: subagents.Definition{Name: "beta"},
				Handler: subagents.HandlerFunc(func(ctx context.Context, subCtx subagents.Context, req subagents.Request) (subagents.Result, error) {
					return subagents.Result{Output: "beta:" + req.Instruction}, nil
				}),
			},
		},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	res, err := rt.RunTeam(context.Background(), subagents.TeamRequest{
		Members: []subagents.TeamMember{
			{Name: "alpha", Instruction: "scan-a"},
			{Name: "beta", Instruction: "scan-b"},
		},
		MaxConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("RunTeam: %v", err)
	}
	if len(res.Members) != 2 {
		t.Fatalf("members len = %d, want 2", len(res.Members))
	}
	if res.Members[0].Name != "alpha" || res.Members[0].Output != "alpha:scan-a" {
		t.Fatalf("unexpected alpha result: %+v", res.Members[0])
	}
	if res.Members[1].Name != "beta" || res.Members[1].Output != "beta:scan-b" {
		t.Fatalf("unexpected beta result: %+v", res.Members[1])
	}
}
