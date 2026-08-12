package api

import (
	"context"
	"testing"

	"github.com/stellarlinkco/agentsdk-go/pkg/model"
	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/skills"
	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/subagents"
)

func TestRuntimeRunUsesTeamRequest(t *testing.T) {
	t.Parallel()

	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "done"}}}}
	mgr := subagents.NewManager()
	for _, name := range []string{"alpha", "beta"} {
		name := name
		if err := mgr.Register(subagents.Definition{Name: name}, subagents.HandlerFunc(func(ctx context.Context, subCtx subagents.Context, req subagents.Request) (subagents.Result, error) {
			return subagents.Result{Output: name + ":" + req.Instruction}, nil
		})); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl, Subagents: []SubagentRegistration{}})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	rt.opts.subMgr = mgr
	t.Cleanup(func() { _ = rt.Close() })

	resp, err := rt.Run(context.Background(), Request{
		Prompt: "inspect",
		TeamMembers: []subagents.TeamMember{
			{Name: "alpha", Instruction: "scan-a"},
			{Name: "beta", Instruction: "scan-b"},
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Team == nil || len(resp.Team.Members) != 2 {
		t.Fatalf("unexpected team response: %+v", resp.Team)
	}
	if len(mdl.requests) == 0 || mdl.requests[0].Messages[len(mdl.requests[0].Messages)-1].Content == "inspect" {
		t.Fatalf("expected team output to feed model prompt, requests=%+v", mdl.requests)
	}
}

func TestRuntimeRunUsesAutoSelectedTeam(t *testing.T) {
	t.Parallel()

	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "done"}}}}
	mgr := subagents.NewManager()
	if err := mgr.Register(subagents.Definition{
		Name: "worker",
		Matchers: []skills.Matcher{
			skills.MatcherFunc(func(ctx skills.ActivationContext) skills.MatchResult {
				return skills.MatchResult{Matched: true, Score: 1}
			}),
		},
	}, subagents.HandlerFunc(func(ctx context.Context, subCtx subagents.Context, req subagents.Request) (subagents.Result, error) {
		return subagents.Result{Output: "worker:" + req.Instruction}, nil
	})); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl, Subagents: []SubagentRegistration{}})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	rt.opts.subMgr = mgr
	t.Cleanup(func() { _ = rt.Close() })

	resp, err := rt.Run(context.Background(), Request{
		Prompt:        "inspect",
		TeamMaxAgents: 1,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Team == nil || len(resp.Team.Members) != 1 || resp.Team.Members[0].Name != "worker" {
		t.Fatalf("unexpected team response: %+v", resp.Team)
	}
}

func TestRuntimeRunCreatesTeamFromRegisteredSubagents(t *testing.T) {
	t.Parallel()

	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "done"}}}}
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       mdl,
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

	resp, err := rt.Run(context.Background(), Request{
		Prompt: "inspect",
		TeamMembers: []subagents.TeamMember{
			{Name: "alpha", Instruction: "scan-a"},
			{Name: "beta", Instruction: "scan-b"},
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Team == nil || len(resp.Team.Members) != 2 {
		t.Fatalf("unexpected team response: %+v", resp.Team)
	}
	if resp.Team.Members[0].Output != "alpha:scan-a" || resp.Team.Members[1].Output != "beta:scan-b" {
		t.Fatalf("unexpected team outputs: %+v", resp.Team.Members)
	}
}
