package api

import (
	"context"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/model"
)

// TestSessionContextUsage 验证会话上下文占用查询：占用口径与 compact 触发
// 判定一致（历史 token + 系统提示与工具 overhead），limit 来自 TokenLimit；
// 会话不存在（未加载）时 ok=false。这是 Admin UI 上下文余量进度条的数据源，
// used 必须覆盖 compact 判定的全部组成部分，否则进度条会在真实压缩前显示
// 虚高的余量。
func TestSessionContextUsage(t *testing.T) {
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{ProjectRoot: root, Model: mdl, TokenLimit: 100000})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if _, _, ok := rt.SessionContextUsage("no_such_session"); ok {
		t.Fatal("未知会话应返回 ok=false")
	}

	if _, err := rt.Run(context.Background(), Request{SessionID: "sess_usage", Prompt: "hello"}); err != nil {
		t.Fatalf("run: %v", err)
	}

	used, limit, ok := rt.SessionContextUsage("sess_usage")
	if !ok {
		t.Fatal("已运行会话应返回 ok=true")
	}
	if limit != 100000 {
		t.Fatalf("limit 应取 TokenLimit=100000，实际 %d", limit)
	}
	if used <= 0 || used > 100000 {
		t.Fatalf("used 应在 (0, limit] 区间，实际 %d", used)
	}

	rt.ClearSession("sess_usage")
	if _, _, ok := rt.SessionContextUsage("sess_usage"); ok {
		t.Fatal("清空后的会话应返回 ok=false")
	}
}
