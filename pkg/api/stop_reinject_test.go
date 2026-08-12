package api

import (
	"context"
	"testing"

	hooks "github.com/stellarlinkco/agentsdk-go/pkg/hooks"
	"github.com/stellarlinkco/agentsdk-go/pkg/message"
	"github.com/stellarlinkco/agentsdk-go/pkg/middleware"
	"github.com/stellarlinkco/agentsdk-go/pkg/model"
	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
)

func TestRunLoopReinjectsStopHookBlockingErrorAndContinues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	countFile := dir + "/stop-count.txt"
	script := writeScript(t, dir, "stop-block.sh", shScript(
		"#!/bin/sh\n"+
			"count=0\n"+
			"if [ -f '"+countFile+"' ]; then count=$(cat '"+countFile+"'); fi\n"+
			"count=$((count+1))\n"+
			"printf '%s' \"$count\" > '"+countFile+"'\n"+
			"if [ \"$count\" -le 2 ]; then printf '{\"decision\":\"block\",\"reason\":\"missing fix\"}'; else printf '{}'; fi\n",
		"@setlocal EnableExtensions\r\n"+
			"@set file="+countFile+"\r\n"+
			"@if exist \"%file%\" (set /p count=<\"%file%\") else (set count=0)\r\n"+
			"@set /a count=%count%+1\r\n"+
			"@>\"%file%\" <nul set /p =%count%\r\n"+
			"@if %count% LEQ 2 goto block\r\n"+
			"@echo {}\r\n"+
			"@goto end\r\n"+
			":block\r\n"+
			"@echo {\"decision\":\"block\",\"reason\":\"missing fix\"}\r\n"+
			":end\r\n",
	))

	hookExec := hooks.NewExecutor()
	hookExec.Register(hooks.ShellHook{Event: hooks.Stop, Command: script})

	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: "draft-1"}, StopReason: "done"},
		{Message: model.Message{Role: "assistant", Content: "draft-2"}, StopReason: "done"},
		{Message: model.Message{Role: "assistant", Content: "final"}, StopReason: "done"},
	}}

	rt := &Runtime{
		opts: Options{StopReinjectionLimit: 3},
	}
	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "hi",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s"},
	}

	resp, err := rt.runLoop(prep, mdl, &runtimeHookAdapter{executor: hookExec, recorder: defaultHookRecorder()}, &runtimeToolExecutor{executor: tool.NewExecutor(nil, nil), hooks: &runtimeHookAdapter{}, host: "localhost"}, middleware.NewChain(nil), false)
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if resp == nil || resp.Message.Content != "final" {
		t.Fatalf("unexpected final response: %+v", resp)
	}
	if len(mdl.requests) != 3 {
		t.Fatalf("expected 3 model requests, got %d", len(mdl.requests))
	}

	msgs := prep.history.All()
	blocked := 0
	for _, msg := range msgs {
		if msg.Role == "user" && msg.Content == "[System] Stop blocked: missing fix. Please address this issue." {
			blocked++
		}
	}
	if blocked != 2 {
		t.Fatalf("expected 2 reinjected stop messages, got %d in %+v", blocked, msgs)
	}
}
