package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/model"
)

func TestMemoryExtractGuardStrictMode(t *testing.T) {
	memDir := t.TempDir()
	outside := t.TempDir()
	g := &memoryWriteGuard{autoDir: memDir, sandboxRoot: outside, strict: true}

	t.Run("topic md inside memory dir passes", func(t *testing.T) {
		err := g.preToolUse("write", map[string]any{
			"file_path": filepath.Join(memDir, "user_role.md"),
			"content":   "---\nname: u\ndescription: ok\n---\nbody",
		})
		if err != nil {
			t.Fatalf("topic .md write must pass, got %v", err)
		}
	})
	t.Run("outside memory dir denied", func(t *testing.T) {
		err := g.preToolUse("write", map[string]any{
			"file_path": filepath.Join(outside, "evil.md"),
			"content":   "x",
		})
		if !errors.Is(err, ErrToolUseDenied) {
			t.Fatalf("outside write must be denied, got %v", err)
		}
	})
	t.Run("MEMORY.md denied case-insensitively", func(t *testing.T) {
		for _, name := range []string{"MEMORY.md", "memory.md"} {
			err := g.preToolUse("write", map[string]any{
				"file_path": filepath.Join(memDir, name),
				"content":   "x",
			})
			if !errors.Is(err, ErrToolUseDenied) {
				t.Fatalf("%s write must be denied, got %v", name, err)
			}
		}
	})
	t.Run("non-md denied", func(t *testing.T) {
		err := g.preToolUse("edit", map[string]any{
			"file_path":  filepath.Join(memDir, "state.json"),
			"new_string": "x",
		})
		if !errors.Is(err, ErrToolUseDenied) {
			t.Fatalf("non-.md write must be denied, got %v", err)
		}
	})
	t.Run("secret scan still applies", func(t *testing.T) {
		err := g.preToolUse("write", map[string]any{
			"file_path": filepath.Join(memDir, "leak.md"),
			"content":   "key = sk-abcdefghijklmnopqrstuvwxyz012345",
		})
		if !errors.Is(err, ErrToolUseDenied) {
			t.Fatalf("secret in memory write must be denied, got %v", err)
		}
	})
	t.Run("non-write tools untouched", func(t *testing.T) {
		if err := g.preToolUse("read", map[string]any{"file_path": filepath.Join(outside, "x")}); err != nil {
			t.Fatalf("read must not be intercepted, got %v", err)
		}
	})
	t.Run("non-strict guard keeps phase-one behavior", func(t *testing.T) {
		soft := &memoryWriteGuard{autoDir: memDir, sandboxRoot: outside}
		if err := soft.preToolUse("write", map[string]any{
			"file_path": filepath.Join(outside, "evil.md"),
			"content":   "x",
		}); err != nil {
			t.Fatalf("non-strict guard must not deny outside writes, got %v", err)
		}
	})
}

// TestMaybeStartMemoryExtractionGate 验证触发的门控条件：子代理上下文、空
// prompt、scheduled 前缀、Memory 禁用一律不发起抽取；正常回合恰好发起一次。
func TestMaybeStartMemoryExtractionGate(t *testing.T) {
	memDir := memoryRecallFixture(t)
	newGateRuntime := func() (*Runtime, *memoryExtractor) {
		rt := &Runtime{memory: newTestMemoryRuntime(memDir)}
		rt.memory.initExtractor(rt)
		return rt, rt.memory.extractor
	}

	t.Run("normal turn starts extraction once", func(t *testing.T) {
		rt, e := newGateRuntime()
		var calls int
		done := make(chan struct{})
		e.once = func(sessionID string) bool {
			calls++
			close(done)
			return false
		}
		rt.maybeStartMemoryExtraction("sess", "用户要求整理项目约定")
		<-done // 抽取为后台 goroutine：阻塞等待其触发 once（select/default 会与调度竞速）
		e.wg.Wait()
		if calls != 1 {
			t.Fatalf("extraction ran %d times, want 1", calls)
		}
	})

	t.Run("subagent context skipped", func(t *testing.T) {
		rt, e := newGateRuntime()
		e.once = func(string) bool { t.Fatal("must not extract"); return false }
		rt.opts.subagentDisabled = true
		rt.maybeStartMemoryExtraction("sess", "正常用户消息")
	})

	t.Run("empty prompt skipped", func(t *testing.T) {
		rt, e := newGateRuntime()
		e.once = func(string) bool { t.Fatal("must not extract"); return false }
		rt.maybeStartMemoryExtraction("sess", "   ")
	})

	t.Run("scheduled wake-up skipped", func(t *testing.T) {
		rt, e := newGateRuntime()
		e.once = func(string) bool { t.Fatal("must not extract"); return false }
		rt.maybeStartMemoryExtraction("sess", scheduledFirePrefix+"巡检任务")
	})

	t.Run("memory disabled skipped", func(t *testing.T) {
		rt := &Runtime{} // rt.memory == nil（抽取子代理即此形态，防递归）
		rt.maybeStartMemoryExtraction("sess", "正常用户消息")
	})
}

// TestMemoryExtractPendingMerge 固化并发语义：抽取进行中再来的请求合并为
// pending，当前一轮完成后自动补跑一轮；空闲时的两次触发只跑一轮。
func TestMemoryExtractPendingMerge(t *testing.T) {
	rt := &Runtime{memory: newTestMemoryRuntime(t.TempDir())}
	rt.memory.initExtractor(rt)
	e := rt.memory.extractor

	var mu sync.Mutex
	count := 0
	release := make(chan struct{})
	firstEntered := make(chan struct{})
	e.once = func(sessionID string) bool {
		mu.Lock()
		count++
		first := count == 1
		mu.Unlock()
		if first {
			close(firstEntered)
		}
		<-release
		return true
	}

	e.start("s")
	<-firstEntered // 第一轮进入（running）
	e.start("s")   // running → pending
	close(release)
	e.wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if count != 2 {
		t.Fatalf("pending merge must rerun exactly once, ran %d times", count)
	}
}

// extractGateModel 只对抽取子代理的请求设闸，用于控制后台抽取的时序。
type extractGateModel struct {
	inner   *stubModel
	release chan struct{}
}

func (g *extractGateModel) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	if strings.Contains(req.System, "memory extraction subagent") {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.inner.Complete(ctx, req)
}

func (g *extractGateModel) CompleteStream(ctx context.Context, req model.Request, cb model.StreamHandler) error {
	resp, err := g.Complete(ctx, req)
	if err != nil {
		return err
	}
	return cb(model.StreamResult{Final: true, Response: resp})
}

func writeMemoryToolCall(path, content string) model.ToolCall {
	return model.ToolCall{ID: "call-1", Name: "write", Arguments: map[string]any{
		"file_path": path,
		"content":   content,
	}}
}

func readMemoryCursor(t *testing.T, memDir string) memoryStateFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(memDir, memoryStateFileName))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var state memoryStateFile
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	return state
}

// TestMemoryExtractEndToEnd 验证 completed 回合触发后台抽取：子代理把会话内容
// 落成记忆文件、游标推进、提取提示词占位符全部渲染。
func TestMemoryExtractEndToEnd(t *testing.T) {
	memDir := t.TempDir()
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: "好的，我了解了"}}, // 主回合
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{
			writeMemoryToolCall(filepath.Join(memDir, "extracted.md"),
				"---\nname: extracted\ndescription: 会话抽取的项目记忆\ntype: project\n---\n\n抽取内容"),
		}}},
		{Message: model.Message{Role: "assistant", Content: "已保存记忆"}}, // 子代理收尾
	}}
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       mdl,
		Memory:      &MemoryOptions{Dir: memDir},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if _, err := rt.Run(context.Background(), Request{Prompt: "hi", SessionID: "sess-x"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.memory.extractor.wg.Wait() // 抽取为后台 goroutine：等待其收敛

	data, err := os.ReadFile(filepath.Join(memDir, "extracted.md"))
	if err != nil {
		t.Fatalf("extracted memory missing: %v", err)
	}
	if !strings.Contains(string(data), "抽取内容") {
		t.Fatalf("extracted content wrong: %s", data)
	}
	if state := readMemoryCursor(t, memDir); state.LastCursorMessageID != 2 {
		t.Fatalf("cursor = %d, want 2 (user + assistant)", state.LastCursorMessageID)
	}
	if len(mdl.requests) != 3 {
		t.Fatalf("model calls = %d, want 3 (main + extract 2 turns)", len(mdl.requests))
	}
	subReq := mdl.requests[1]
	if !strings.Contains(subReq.System, "memory extraction subagent") {
		t.Fatalf("subagent system prompt missing extract template:\n%s", subReq.System)
	}
	if strings.Contains(subReq.System, "{{") {
		t.Fatalf("extract prompt placeholders not rendered:\n%s", subReq.System)
	}
}

// TestMemoryExtractErrorAndScheduledTurns 验证 error 回合与 scheduled 唤醒回合
// 都不触发抽取（无子代理模型调用、无游标文件）。
func TestMemoryExtractErrorAndScheduledTurns(t *testing.T) {
	memDir := t.TempDir()
	root := newClaudeProject(t)

	t.Run("error turn does not extract", func(t *testing.T) {
		mdl := &stubModel{err: errors.New("model down")}
		rt, err := New(context.Background(), Options{
			ProjectRoot: root,
			Model:       mdl,
			Memory:      &MemoryOptions{Dir: memDir},
		})
		if err != nil {
			t.Fatalf("runtime: %v", err)
		}
		t.Cleanup(func() { _ = rt.Close() })
		if _, err := rt.Run(context.Background(), Request{Prompt: "hi", SessionID: "s"}); err == nil {
			t.Fatal("expected run error")
		}
		rt.memory.extractor.wg.Wait()
		if len(mdl.requests) != 1 {
			t.Fatalf("model calls = %d, want 1 (no extraction subagent)", len(mdl.requests))
		}
		if _, err := os.Stat(filepath.Join(memDir, memoryStateFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cursor file must not exist after failed turn, got %v", err)
		}
	})

	t.Run("scheduled wake-up turn skipped", func(t *testing.T) {
		mdl := &stubModel{responses: []*model.Response{
			{Message: model.Message{Role: "assistant", Content: "收到"}},
		}}
		rt, err := New(context.Background(), Options{
			ProjectRoot: root,
			Model:       mdl,
			Memory:      &MemoryOptions{Dir: memDir},
		})
		if err != nil {
			t.Fatalf("runtime: %v", err)
		}
		t.Cleanup(func() { _ = rt.Close() })
		if _, err := rt.Run(context.Background(), Request{Prompt: scheduledFirePrefix + "巡检", SessionID: "s"}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if len(mdl.requests) != 1 {
			t.Fatalf("model calls = %d, want 1 (scheduled turn must not extract)", len(mdl.requests))
		}
		if _, err := os.Stat(filepath.Join(memDir, memoryStateFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cursor file must not exist for scheduled turn, got %v", err)
		}
	})
}

// TestMemoryExtractSkipsWhenMainWroteMemory 验证防双写：主对话本回合成功写入
// 记忆目录 → 跳过本轮抽取（无子代理调用），游标照常推进。
func TestMemoryExtractSkipsWhenMainWroteMemory(t *testing.T) {
	memDir := t.TempDir()
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{
			writeMemoryToolCall(filepath.Join(memDir, "main_note.md"),
				"---\nname: main\ndescription: 主对话显式保存\ntype: user\n---\n\n主对话内容"),
		}}},
		{Message: model.Message{Role: "assistant", Content: "已记录"}},
	}}
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       mdl,
		Memory:      &MemoryOptions{Dir: memDir},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if _, err := rt.Run(context.Background(), Request{Prompt: "记住这一点", SessionID: "sess"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.memory.extractor.wg.Wait()

	if _, err := os.Stat(filepath.Join(memDir, "main_note.md")); err != nil {
		t.Fatalf("main conversation memory write missing: %v", err)
	}
	entries, err := os.ReadDir(memDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == "main_note.md" || entry.Name() == memoryStateFileName {
			continue
		}
		t.Fatalf("extraction must be skipped, unexpected entry: %s", entry.Name())
	}
	if state := readMemoryCursor(t, memDir); state.LastCursorMessageID <= 0 {
		t.Fatalf("cursor must advance even when skipping, got %d", state.LastCursorMessageID)
	}
	if len(mdl.requests) != 2 {
		t.Fatalf("model calls = %d, want 2 (main turn only; extraction skipped)", len(mdl.requests))
	}
}

// TestMemoryExtractSubagentNoRecursionAndNoRecall 验证抽取子代理自身不再触发
// 抽取（防递归）、其请求也不带召回注入（Memory 整链路禁用）。
func TestMemoryExtractSubagentNoRecursionAndNoRecall(t *testing.T) {
	memDir := t.TempDir()
	root := newClaudeProject(t)
	writeMemoryFile(t, memDir, "existing.md", memoryDoc("e", "既有记忆"), "内容", 0)
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: "主回合结束"}}, // 主回合（prompt "hi" 不过召回门槛，无注入）
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{
			writeMemoryToolCall(filepath.Join(memDir, "extracted.md"),
				"---\nname: ex\ndescription: 抽取\ntype: project\n---\n\n内容"),
		}}},
		{Message: model.Message{Role: "assistant", Content: "收尾"}}, // 子代理收尾后不再有第 4 次调用
	}}
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       mdl,
		Memory:      &MemoryOptions{Dir: memDir},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if _, err := rt.Run(context.Background(), Request{Prompt: "hi", SessionID: "s"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.memory.extractor.wg.Wait()

	if len(mdl.requests) != 3 {
		t.Fatalf("model calls = %d, want 3 (recursion would add more)", len(mdl.requests))
	}
	subReq := mdl.requests[1]
	for _, m := range subReq.Messages {
		if strings.Contains(m.Content, "<system-reminder>") {
			t.Fatal("extraction subagent must never receive recall injection")
		}
	}
}

// TestMemoryExtractOutsideWriteDenied 验证抽取子代理对目录外路径的写入被
// 守卫拒绝：目标文件不存在，子代理收到 deny 后仍正常收尾。
func TestMemoryExtractOutsideWriteDenied(t *testing.T) {
	memDir := t.TempDir()
	root := newClaudeProject(t)
	outside := filepath.Join(root, "evil.md")
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: "主回合结束"}},
		{Message: model.Message{Role: "assistant", ToolCalls: []model.ToolCall{
			writeMemoryToolCall(outside, "---\nname: e\ndescription: x\n---\n越权内容"),
		}}},
		{Message: model.Message{Role: "assistant", Content: "收尾"}},
	}}
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       mdl,
		Memory:      &MemoryOptions{Dir: memDir},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if _, err := rt.Run(context.Background(), Request{Prompt: "hi", SessionID: "s"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.memory.extractor.wg.Wait()

	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside write must be denied, file state: %v", err)
	}
	if len(mdl.requests) != 3 {
		t.Fatalf("model calls = %d, want 3 (deny must not abort the subagent)", len(mdl.requests))
	}
}

// TestMemoryCloseStopsExtractor 验证 Close 停掉在途抽取并收敛 goroutine。
func TestMemoryCloseStopsExtractor(t *testing.T) {
	memDir := t.TempDir()
	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{
		{Message: model.Message{Role: "assistant", Content: "主回合结束"}},
	}}
	gated := &extractGateModel{inner: mdl, release: make(chan struct{})}
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       gated,
		Memory:      &MemoryOptions{Dir: memDir},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}

	if _, err := rt.Run(context.Background(), Request{Prompt: "hi", SessionID: "s"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 抽取子代理已卡在模型调用（gated）。Close 必须 cancel 并等待其退出。
	done := make(chan error, 1)
	go func() { done <- rt.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	default:
		// Close 可能需要一点时间；给它一次机会后强制失败。
		<-done
	}
	rt.memory.extractor.wg.Wait() // Close 已等待过：立即返回即通过
}
