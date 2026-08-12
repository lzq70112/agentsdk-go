package subagents

import (
	"context"
	"testing"
	"time"

	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/skills"
)

func TestManagerDispatchTeamRunsMembersAndAggregatesResults(t *testing.T) {
	t.Parallel()

	m := NewManager()
	started := make(chan string, 2)
	release := make(chan struct{})

	register := func(name string) {
		t.Helper()
		if err := m.Register(Definition{Name: name}, HandlerFunc(func(ctx context.Context, subCtx Context, req Request) (Result, error) {
			started <- name
			<-release
			return Result{Output: req.Instruction}, nil
		})); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	register("alpha")
	register("beta")

	done := make(chan TeamResult, 1)
	errs := make(chan error, 1)
	go func() {
		res, err := m.DispatchTeam(context.Background(), TeamRequest{
			Members: []TeamMember{
				{Name: "alpha", Instruction: "scan-a"},
				{Name: "beta", Instruction: "scan-b"},
			},
			MaxConcurrency: 2,
		})
		if err != nil {
			errs <- err
			return
		}
		done <- res
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for first team member")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for second team member")
	}

	close(release)

	select {
	case err := <-errs:
		t.Fatalf("DispatchTeam: %v", err)
	case res := <-done:
		if len(res.Members) != 2 {
			t.Fatalf("members len = %d, want 2", len(res.Members))
		}
		if res.Members[0].Name != "alpha" || res.Members[0].Output != "scan-a" {
			t.Fatalf("unexpected alpha result: %+v", res.Members[0])
		}
		if res.Members[1].Name != "beta" || res.Members[1].Output != "scan-b" {
			t.Fatalf("unexpected beta result: %+v", res.Members[1])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for team result")
	}
}

func TestManagerDispatchTeamAutoSelectsMatchingSubagents(t *testing.T) {
	t.Parallel()

	m := NewManager()
	register := func(name string, priority int, score float64) {
		t.Helper()
		if err := m.Register(Definition{
			Name:     name,
			Priority: priority,
			Matchers: []skills.Matcher{
				skills.MatcherFunc(func(ctx skills.ActivationContext) skills.MatchResult {
					return skills.MatchResult{Matched: true, Score: score}
				}),
			},
		}, HandlerFunc(func(ctx context.Context, subCtx Context, req Request) (Result, error) {
			return Result{Output: name + ":" + req.Instruction}, nil
		})); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	register("planner", 20, 0.8)
	register("explorer", 10, 0.9)
	register("critic", 5, 0.7)

	res, err := m.DispatchTeam(context.Background(), TeamRequest{
		Instruction: "inspect",
		Activation:  skills.ActivationContext{Prompt: "inspect repo"},
		MaxAgents:   2,
	})
	if err != nil {
		t.Fatalf("DispatchTeam: %v", err)
	}
	if len(res.Members) != 2 {
		t.Fatalf("members len = %d, want 2", len(res.Members))
	}
	if res.Members[0].Name != "planner" {
		t.Fatalf("first team member = %q, want planner", res.Members[0].Name)
	}
	if res.Members[1].Name != "explorer" {
		t.Fatalf("second team member = %q, want explorer", res.Members[1].Name)
	}
}
