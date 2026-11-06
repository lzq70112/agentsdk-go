package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

func newTestMemoryRuntime(memDir string, extraDirs ...string) *memoryRuntime {
	return &memoryRuntime{
		cfg:      MemoryOptions{Dir: memDir, ExtraDirs: extraDirs},
		store:    &memoryStore{autoDir: memDir, extraDirs: extraDirs},
		selector: keywordSelector{},
		guard:    &memoryWriteGuard{autoDir: memDir, extraDirs: extraDirs},
	}
}

// TestMemoryDirHintInjectOncePerSession 验证 PerSession 会话桶路径提示：注入
// 恰好一条带会话桶路径的合成消息（历史去重防重复），且全局桶模式不注入——
// 没有这条提示主对话会把用户要求记住的信息写到召回扫不到的桶根（实机事故：
// 值班电话落桶根 + 防双写压制抽取兜底）。
func TestMemoryDirHintInjectOncePerSession(t *testing.T) {
	dir := t.TempDir()
	rt := &Runtime{memory: &memoryRuntime{
		cfg:   MemoryOptions{Dir: dir, PerSession: true},
		store: &memoryStore{autoDir: dir, perSession: true},
	}}

	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "请记住一些信息",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "dingtalk:user:1001", RequestID: "r1"},
	}
	rt.injectMemoryDirHint(&prep)
	rt.injectMemoryDirHint(&prep)
	if prep.history.Len() != 1 {
		t.Fatalf("hint must inject exactly once, len=%d", prep.history.Len())
	}
	msg := prep.history.All()[0]
	bucket := rt.memory.store.sessionBucketDir("dingtalk:user:1001")
	if !strings.Contains(msg.Content, filepath.ToSlash(bucket)) {
		t.Fatalf("hint must carry session bucket path %q, got %q", bucket, msg.Content)
	}
	if msg.Metadata[metadataSynthetic] != true || msg.Metadata[metadataSyntheticKey] != memoryOriginSessionDir {
		t.Fatalf("hint metadata = %v / %v", msg.Metadata[metadataSynthetic], msg.Metadata[metadataSyntheticKey])
	}

	// 已有提示的历史（如从存储恢复的旧会话）：不重复注入。
	rt.injectMemoryDirHint(&prep)
	if prep.history.Len() != 1 {
		t.Fatal("existing hint in history must suppress re-injection")
	}

	// 全局桶模式：不注入（常驻 memory-usage 提示已覆盖路径指引）。
	global := &Runtime{memory: &memoryRuntime{
		cfg:   MemoryOptions{Dir: dir},
		store: &memoryStore{autoDir: dir},
	}}
	gprep := preparedRun{
		ctx:        context.Background(),
		prompt:     "请记住一些信息",
		history:    message.NewHistory(),
		normalized: Request{SessionID: "s", RequestID: "r1"},
	}
	global.injectMemoryDirHint(&gprep)
	if gprep.history.Len() != 0 {
		t.Fatal("global mode must not inject the session dir hint")
	}
}

func TestMemoryQueryGate(t *testing.T) {
	tests := []struct {
		name string
		text string
		pass bool
	}{
		{"empty", "", false},
		{"single english word", "hello", false},
		{"two english words", "hello world", true},
		{"two cjk chars", "帮我", false},
		{"four cjk chars", "帮我修改", true},
		{"cjk plus tool", "修 bug", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := memoryQueryGate(tt.text); got != tt.pass {
				t.Fatalf("memoryQueryGate(%q) = %v, want %v", tt.text, got, tt.pass)
			}
		})
	}
}

func TestKeywordSelectorScoringAndTieBreak(t *testing.T) {
	now := time.Now()
	mk := func(filename, description string, age time.Duration) memoryHeader {
		return memoryHeader{
			Filename:    filename,
			Description: description,
			ModTime:     now.Add(-age),
		}
	}
	candidates := []memoryHeader{
		mk("low.md", "构建脚本约定", 1*time.Hour),         // 0 分
		mk("high.md", "用户偏好与协作约定", 3*time.Hour),     // 高分
		mk("older_tie.md", "用户名生成规则", 48*time.Hour), // 并列低分、较旧
		mk("newer_tie.md", "用户名规范", 1*time.Hour),    // 并列低分、较新
	}

	selected := keywordSelector{}.Select("用户偏好设置", candidates)
	got := make([]string, 0, len(selected))
	for _, h := range selected {
		got = append(got, h.Filename)
	}
	want := []string{"high.md", "newer_tie.md", "older_tie.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("selection = %v, want %v (score desc, tie by newest mtime)", got, want)
	}
}

func TestKeywordSelectorCapsAtFive(t *testing.T) {
	now := time.Now()
	var candidates []memoryHeader
	for i := 0; i < 8; i++ {
		candidates = append(candidates, memoryHeader{
			Filename:    fmt.Sprintf("m%d.md", i),
			Description: "部署流水线要点",
			ModTime:     now.Add(-time.Duration(i) * time.Hour),
		})
	}
	selected := keywordSelector{}.Select("部署流水线", candidates)
	if len(selected) != memoryMaxSelected {
		t.Fatalf("selected %d, want cap %d", len(selected), memoryMaxSelected)
	}
	if selected[0].Filename != "m0.md" {
		t.Fatalf("newest should rank first, got %q", selected[0].Filename)
	}
}

func TestMemoryFreshnessText(t *testing.T) {
	if got := memoryFreshnessLabel(memorySourceExtra, 0); got != "User-authored knowledge (always applies)" {
		t.Fatalf("extra label = %q", got)
	}
	if got := memoryFreshnessLabel(memorySourceAuto, 0); got != "Memory (saved today)" {
		t.Fatalf("today label = %q", got)
	}
	if got := memoryFreshnessLabel(memorySourceAuto, 1); got != "Memory (saved yesterday)" {
		t.Fatalf("yesterday label = %q", got)
	}
	if got := memoryFreshnessLabel(memorySourceAuto, 3); got != "Memory (saved 3 days ago)" {
		t.Fatalf("3-days label = %q", got)
	}
	want := "This memory is 3 days old. Memories are point-in-time observations, not live state — claims about code behavior or file:line citations may be outdated. Verify against current code before asserting as fact."
	if got := memoryFreshnessWarning(3); got != want {
		t.Fatalf("warning text drift:\n got %q\nwant %q", got, want)
	}
}

func TestSurfacedMemoryRender(t *testing.T) {
	now := time.Now()
	t.Run("auto fresh today", func(t *testing.T) {
		e := surfacedMemory{header: memoryHeader{Path: "/mem/today.md", Source: memorySourceAuto, ModTime: now}, content: "内容 A"}
		got := e.render(now)
		want := "Memory (saved today): /mem/today.md\n\n内容 A"
		if got != want {
			t.Fatalf("render =\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("auto stale carries warning above label", func(t *testing.T) {
		e := surfacedMemory{
			header:  memoryHeader{Path: "/mem/old.md", Source: memorySourceAuto, ModTime: now.Add(-74 * time.Hour)},
			content: "内容 B",
		}
		got := e.render(now)
		want := memoryFreshnessWarning(3) + "\n\nMemory (saved 3 days ago): /mem/old.md\n\n内容 B"
		if got != want {
			t.Fatalf("render =\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("extra never warns even when old", func(t *testing.T) {
		e := surfacedMemory{
			header:  memoryHeader{Path: "/mem/manual.md", Source: memorySourceExtra, ModTime: now.Add(-74 * time.Hour)},
			content: "内容 C",
		}
		got := e.render(now)
		want := "User-authored knowledge (always applies): /mem/manual.md\n\n内容 C"
		if got != want {
			t.Fatalf("extra must never carry staleness warning, got %q", got)
		}
	})
}

func TestRenderMemoryInjectionWrapper(t *testing.T) {
	now := time.Now()
	entries := []surfacedMemory{
		{header: memoryHeader{Path: "/mem/a.md", Source: memorySourceAuto}, content: "A"},
		{header: memoryHeader{Path: "/mem/b.md", Source: memorySourceExtra}, content: "B"},
	}
	got := renderMemoryInjection(entries, now)
	want := "<system-reminder>\nRelevant memory surfaced for this turn:\n\n" +
		entries[0].render(now) + "\n\n" + entries[1].render(now) +
		"\n</system-reminder>"
	if got != want {
		t.Fatalf("wrapper mismatch:\n%q\nwant\n%q", got, want)
	}
}

func TestMemorySessionUsageAccumulation(t *testing.T) {
	hist := message.NewHistory()
	hist.Append(message.Message{Role: "user", Content: "普通消息"})
	hist.Append(message.Message{Role: "user", Content: "注入一", Metadata: map[string]any{
		memoryMetadataOrigin:     string(memoryOriginRelevantMemory),
		memoryMetadataPaths:      "/mem/a.md|/mem/b.md",
		memoryMetadataTotalBytes: 100,
	}})
	hist.Append(message.Message{Role: "user", Content: "注入二（JSON 恢复后的 float64）", Metadata: map[string]any{
		memoryMetadataOrigin:     string(memoryOriginRelevantMemory),
		memoryMetadataPaths:      "/mem/b.md",
		memoryMetadataTotalBytes: float64(50),
	}})

	surfaced, consumed := memorySessionUsage(hist.All())
	if consumed != 150 {
		t.Fatalf("consumed = %d, want 150", consumed)
	}
	for _, p := range []string{"/mem/a.md", "/mem/b.md"} {
		if _, ok := surfaced[p]; !ok {
			t.Fatalf("surfaced set missing %q: %v", p, surfaced)
		}
	}
	if len(surfaced) != 2 {
		t.Fatalf("surfaced = %v, want 2 unique paths", surfaced)
	}
}

// memoryRecallFixture 建两个记忆文件并返回目录。
func memoryRecallFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeMemoryFile(t, dir, "user_role.md", memoryDoc("u", "用户角色与协作偏好"), "角色内容", 0)
	writeMemoryFile(t, dir, "deploy.md", memoryDoc("d", "部署流水线要点"), "部署内容", 0)
	return dir
}

func TestSurfaceRelevantMemoriesBudgetStop(t *testing.T) {
	memDir := memoryRecallFixture(t)
	hist := message.NewHistory()
	hist.Append(message.Message{Role: "user", Content: "早前注入", Metadata: map[string]any{
		memoryMetadataOrigin:     string(memoryOriginRelevantMemory),
		memoryMetadataPaths:      "/mem/earlier.md",
		memoryMetadataTotalBytes: memorySessionBudgetBytes,
	}})
	rt := &Runtime{memory: newTestMemoryRuntime(memDir)}
	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "请讲讲用户角色与协作偏好",
		history:    hist,
		normalized: Request{SessionID: "s"},
	}
	rt.beginMemoryRecall(&prep)
	if hist.Len() != 1 {
		t.Fatalf("budget exhausted session must not inject, len=%d msgs=%+v", hist.Len(), hist.All())
	}
}

func TestSurfaceRelevantMemoriesDedupsSurfacedFile(t *testing.T) {
	memDir := memoryRecallFixture(t)
	// query 同时覆盖两个 description；已注入的 user_role.md 必须被去重剔除，
	// 只有 deploy.md 进入本回合注入。
	surfacedPath := filepath.ToSlash(filepath.Join(memDir, "user_role.md"))
	hist := message.NewHistory()
	hist.Append(message.Message{Role: "user", Content: "早前注入", Metadata: map[string]any{
		memoryMetadataOrigin:     string(memoryOriginRelevantMemory),
		memoryMetadataPaths:      surfacedPath,
		memoryMetadataTotalBytes: 10,
	}})
	rt := &Runtime{memory: newTestMemoryRuntime(memDir)}
	prep := preparedRun{
		ctx:        context.Background(),
		prompt:     "请讲讲用户角色偏好与部署流水线",
		history:    hist,
		normalized: Request{SessionID: "s"},
	}
	rt.beginMemoryRecall(&prep)
	if hist.Len() != 2 {
		t.Fatalf("expected exactly one injection, len=%d", hist.Len())
	}
	injected := hist.All()[1]
	paths, ok := injected.Metadata[memoryMetadataPaths].(string)
	if !ok {
		t.Fatalf("memory_paths metadata missing: %+v", injected.Metadata)
	}
	deployPath := filepath.ToSlash(filepath.Join(memDir, "deploy.md"))
	if paths != deployPath {
		t.Fatalf("injected paths = %q, want only %q (user_role.md must be deduped)", paths, deployPath)
	}
}

func TestSurfaceRelevantMemoriesSkipsSubagentAndDisabled(t *testing.T) {
	memDir := memoryRecallFixture(t)

	t.Run("subagent context skipped", func(t *testing.T) {
		rt := &Runtime{memory: newTestMemoryRuntime(memDir)}
		rt.opts.subagentDisabled = true
		prep := preparedRun{
			ctx:        context.Background(),
			prompt:     "请讲讲用户角色与协作偏好",
			history:    message.NewHistory(),
			normalized: Request{SessionID: "s"},
		}
		rt.beginMemoryRecall(&prep)
		if prep.history.Len() != 0 {
			t.Fatal("subagent contexts must never recall memory")
		}
	})

	t.Run("no memory runtime skipped", func(t *testing.T) {
		rt := &Runtime{}
		prep := preparedRun{
			ctx:        context.Background(),
			prompt:     "请讲讲用户角色与协作偏好",
			history:    message.NewHistory(),
			normalized: Request{SessionID: "s"},
		}
		rt.beginMemoryRecall(&prep)
		if prep.history.Len() != 0 {
			t.Fatal("runtime without memory must not inject")
		}
	})

	t.Run("query below gate skipped", func(t *testing.T) {
		rt := &Runtime{memory: newTestMemoryRuntime(memDir)}
		prep := preparedRun{
			ctx:        context.Background(),
			prompt:     "修",
			history:    message.NewHistory(),
			normalized: Request{SessionID: "s"},
		}
		rt.beginMemoryRecall(&prep)
		if prep.history.Len() != 0 {
			t.Fatal("short queries must not trigger recall")
		}
	})
}

func TestMemoryRecallEndToEnd(t *testing.T) {
	memDir := t.TempDir()
	writeMemoryFile(t, memDir, "user_role.md", memoryDoc("u", "用户角色与协作偏好"), "角色内容", 0)
	writeMemoryFile(t, memDir, "feedback_style.md", memoryDoc("f", "代码风格反馈"), "风格内容", 1)
	writeMemoryFile(t, memDir, "project_deadline.md", memoryDoc("p", "项目交付时间安排"), "时间内容", 3)

	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       mdl,
		Memory:      &MemoryOptions{Dir: memDir},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if prompt := rt.GetSystemPrompt(); !strings.Contains(prompt, "<memory_usage>") {
		t.Fatalf("system prompt must embed memory usage section, got:\n%s", prompt)
	}
	if want := filepath.ToSlash(memDir); !strings.Contains(rt.GetSystemPrompt(), want) {
		t.Fatalf("memory usage prompt must render memory dir %q, got:\n%s", want, rt.GetSystemPrompt())
	}

	resp, err := rt.Run(context.Background(), Request{Prompt: "请帮我核对用户角色偏好、代码风格反馈与项目交付时间安排", SessionID: "sess-mem"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if len(mdl.requests) == 0 {
		t.Fatal("no model request captured")
	}
	req := mdl.requests[0]
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("last message role = %q, want user", last.Role)
	}
	for _, fragment := range []string{
		"<system-reminder>",
		"Relevant memory surfaced for this turn:",
		"Memory (saved today): ",
		"Memory (saved yesterday): ",
		"This memory is 3 days old. Memories are point-in-time observations, not live state — claims about code behavior or file:line citations may be outdated. Verify against current code before asserting as fact.",
		"Memory (saved 3 days ago): ",
		"</system-reminder>",
	} {
		if !strings.Contains(last.Content, fragment) {
			t.Fatalf("injected message missing %q:\n%s", fragment, last.Content)
		}
	}

	history, ok := rt.SessionHistory("sess-mem")
	if !ok {
		t.Fatal("session history missing")
	}
	var injected *message.Message
	for i := range history {
		if v, ok := history[i].Metadata[memoryMetadataOrigin].(string); ok && v == string(memoryOriginRelevantMemory) {
			m := history[i]
			injected = &m
		}
	}
	if injected == nil {
		t.Fatalf("no relevant_memory message in history: %+v", history)
	}
	if injected.Metadata[metadataSynthetic] != true {
		t.Fatal("injected memory message must carry synthetic flag (compaction exemption)")
	}
	if injected.Metadata[metadataSyntheticKey] != "relevant_memory" {
		t.Fatalf("synthetic type = %v", injected.Metadata[metadataSyntheticKey])
	}
	paths, ok := injected.Metadata[memoryMetadataPaths].(string)
	if !ok || paths == "" {
		t.Fatal("memory_paths metadata missing")
	}
	if total := asMetadataInt(injected.Metadata[memoryMetadataTotalBytes]); total <= 0 {
		t.Fatalf("memory_total_bytes = %v, want > 0", injected.Metadata[memoryMetadataTotalBytes])
	}
}

func TestMemoryDisabledLeavesNoTrace(t *testing.T) {
	memDir := memoryRecallFixture(t)
	extra := t.TempDir()

	root := newClaudeProject(t)
	mdl := &stubModel{responses: []*model.Response{{Message: model.Message{Role: "assistant", Content: "ok"}}}}
	rt, err := New(context.Background(), Options{
		ProjectRoot: root,
		Model:       mdl,
		Memory: &MemoryOptions{
			Enabled:   boolPtr(false),
			Dir:       memDir,
			ExtraDirs: []string{extra},
		},
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if prompt := rt.GetSystemPrompt(); strings.Contains(prompt, "<memory_usage>") {
		t.Fatalf("disabled memory must not inject usage section, got:\n%s", prompt)
	}
	if _, err := rt.Run(context.Background(), Request{Prompt: "请帮我核对用户角色偏好与项目交付时间安排"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, req := range mdl.requests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "<system-reminder>") {
				t.Fatal("disabled memory must not recall anything")
			}
		}
	}
}
