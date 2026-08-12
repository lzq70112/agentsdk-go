package subagents

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/skills"
)

var ErrEmptyTeam = errors.New("subagents: team has no members")

type TeamMember struct {
	Name          string
	Instruction   string
	Metadata      map[string]any
	ToolWhitelist []string
}

type TeamRequest struct {
	Instruction    string
	Members        []TeamMember
	Activation     skills.ActivationContext
	Metadata       map[string]any
	ToolWhitelist  []string
	MaxAgents      int
	MaxConcurrency int
}

type TeamResult struct {
	Members []Status
}

func (m *Manager) DispatchTeam(ctx context.Context, req TeamRequest) (TeamResult, error) {
	members := cloneTeamRequestMembers(req)
	if len(members) == 0 {
		if strings.TrimSpace(req.Instruction) == "" {
			return TeamResult{}, ErrEmptyTeam
		}
		matches := m.matching(req.Activation)
		if len(matches) == 0 {
			return TeamResult{}, ErrNoMatchingSubagent
		}
		maxAgents := req.MaxAgents
		if maxAgents <= 0 || maxAgents > len(matches) {
			maxAgents = min(len(matches), m.dispatchLimit(req.MaxConcurrency, len(matches)))
		}
		members = make([]TeamMember, 0, maxAgents)
		for _, match := range matches[:maxAgents] {
			members = append(members, TeamMember{
				Name:        match.definition.Name,
				Instruction: req.Instruction,
			})
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	limit := m.dispatchLimit(req.MaxConcurrency, len(members))
	sem := make(chan struct{}, limit)
	statuses := make([]Status, len(members))
	errs := make([]error, len(members))

	var wg sync.WaitGroup
	for i, member := range members {
		i := i
		member := member
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				statuses[i] = Status{
					Name:        strings.ToLower(strings.TrimSpace(member.Name)),
					Instruction: strings.TrimSpace(member.Instruction),
					SessionID:   sessionID(ctx, mergedMetadata(req.Metadata, member.Metadata)),
					State:       StatusError,
					Error:       ctx.Err().Error(),
				}
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()

			dispatchReq := Request{
				Target:        member.Name,
				Instruction:   member.Instruction,
				Activation:    req.Activation.Clone(),
				ToolWhitelist: mergedWhitelist(req.ToolWhitelist, member.ToolWhitelist),
				Metadata:      mergedMetadata(req.Metadata, member.Metadata),
			}
			result, err := m.Dispatch(ctx, dispatchReq)

			status := Status{
				Name:        strings.ToLower(strings.TrimSpace(member.Name)),
				Instruction: strings.TrimSpace(member.Instruction),
				SessionID:   sessionID(ctx, dispatchReq.Metadata),
				Result:      result.clone(),
				State:       StatusSuccess,
			}
			if result.Subagent != "" {
				status.Name = result.Subagent
			}
			if result.Output != nil {
				status.Output = strings.TrimSpace(fmt.Sprint(result.Output))
			}
			if err != nil {
				status.State = StatusError
				if result.Error != "" {
					status.Error = strings.TrimSpace(result.Error)
				} else {
					status.Error = err.Error()
				}
				errs[i] = err
			}
			statuses[i] = status
		}()
	}
	wg.Wait()

	return TeamResult{Members: statuses}, errors.Join(errs...)
}

func (m *Manager) dispatchLimit(requested, total int) int {
	if total <= 0 {
		return 1
	}
	limit := requested
	if limit <= 0 {
		m.mu.RLock()
		limit = m.maxConcurrentBackground
		m.mu.RUnlock()
	}
	if limit <= 0 {
		limit = defaultMaxConcurrentBackground
	}
	if limit > total {
		limit = total
	}
	if limit <= 0 {
		return 1
	}
	return limit
}

func mergedMetadata(base, extra map[string]any) map[string]any {
	switch {
	case len(base) == 0 && len(extra) == 0:
		return nil
	case len(base) == 0:
		return maps.Clone(extra)
	case len(extra) == 0:
		return maps.Clone(base)
	}
	out := maps.Clone(base)
	maps.Copy(out, extra)
	return out
}

func mergedWhitelist(base, extra []string) []string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	merged := append([]string(nil), base...)
	merged = append(merged, extra...)
	return normalizeTools(merged)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func cloneTeamRequestMembers(req TeamRequest) []TeamMember {
	if len(req.Members) == 0 {
		return nil
	}
	out := make([]TeamMember, len(req.Members))
	copy(out, req.Members)
	return out
}
