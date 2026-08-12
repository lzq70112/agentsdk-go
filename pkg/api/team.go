package api

import (
	"context"
	"errors"

	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/subagents"
)

var ErrSubagentManagerUnavailable = errors.New("api: subagent manager is unavailable")

func (rt *Runtime) RunTeam(ctx context.Context, req subagents.TeamRequest) (subagents.TeamResult, error) {
	if rt == nil || rt.opts.subMgr == nil {
		return subagents.TeamResult{}, ErrSubagentManagerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return rt.opts.subMgr.DispatchTeam(ctx, req)
}
