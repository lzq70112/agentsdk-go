package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// 召回注入的 metadata 记账键（预算与去重靠扫描历史注入项累计，键名对齐 comate）。
const (
	memoryOriginRelevantMemory = "relevant_memory"

	memoryMetadataOrigin     = "origin"
	memoryMetadataPaths      = "memory_paths"
	memoryMetadataTotalBytes = "memory_total_bytes"
)

// memorySelector 从候选中挑出本回合要注入的记忆。一期是确定性 keyword 选择器，
// 二期将替换为 LLM 选择器（接口不变，见 spec §6.4/§7.2）。
type memorySelector interface {
	Select(query string, candidates []memoryHeader) []memoryHeader
}

// keywordSelector 按回合文本与 description 的分词重叠打分：CJK 2-gram +
// 英文小写词；取分 >0 的前 5 条，分数并列按 mtime 新者优先。
type keywordSelector struct{}

func (keywordSelector) Select(query string, candidates []memoryHeader) []memoryHeader {
	queryTokens := memoryQueryTokens(query)
	if len(queryTokens) == 0 {
		return nil
	}
	type scored struct {
		header memoryHeader
		score  int
	}
	var hits []scored
	for _, c := range candidates {
		if s := memoryOverlapScore(queryTokens, c.Description); s > 0 {
			hits = append(hits, scored{header: c, score: s})
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if !hits[i].header.ModTime.Equal(hits[j].header.ModTime) {
			return hits[i].header.ModTime.After(hits[j].header.ModTime)
		}
		return hits[i].header.Filename < hits[j].header.Filename
	})
	if len(hits) > memoryMaxSelected {
		hits = hits[:memoryMaxSelected]
	}
	out := make([]memoryHeader, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.header)
	}
	return out
}

// memoryQueryTokens 分词：连续 CJK 字符切成相邻 2-gram，其余连续字母数字
// 归一为小写词。
func memoryQueryTokens(text string) map[string]struct{} {
	tokens := map[string]struct{}{}
	var cjkRun []rune
	var wordRun []rune
	flushWord := func() {
		if len(wordRun) > 0 {
			tokens[string(wordRun)] = struct{}{}
			wordRun = wordRun[:0]
		}
	}
	flushCJK := func() {
		for i := 0; i+1 < len(cjkRun); i++ {
			tokens[string(cjkRun[i:i+2])] = struct{}{}
		}
		cjkRun = cjkRun[:0]
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			cjkRun = append(cjkRun, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			wordRun = append(wordRun, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return tokens
}

func memoryOverlapScore(queryTokens map[string]struct{}, description string) int {
	score := 0
	for token := range memoryQueryTokens(description) {
		if _, ok := queryTokens[token]; ok {
			score++
		}
	}
	return score
}

// memoryRuntime 是主 runtime 持有的记忆子系统装配（Memory 禁用时为 nil）。
type memoryRuntime struct {
	cfg       MemoryOptions
	store     *memoryStore
	selector  memorySelector
	guard     *memoryWriteGuard
	extractor *memoryExtractor

	// writeObserved 防双写标志：主对话自游标以来有成功的记忆目录写入
	// （工具执行器置位，回合结束抽取时消费清零）。
	writeObserved atomic.Bool

	// recallMu/recallJob 是 llmSelector 异步召回的单槽任务：beginMemoryRecall
	// 放入，consumeMemoryRecall 在首次模型调用前 try-consume。
	recallMu  sync.Mutex
	recallJob *memoryRecallJob

	// ctx/cancel/wg 管理本子系统全部后台 goroutine（异步召回 + 抽取），
	// shutdown 等待退出。
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// memoryRecallJob 是一次异步召回的完整上下文：以 sessionID+requestID 标记
// 归属回合，消费侧据此校验（多会话并发时互不串扰）。
type memoryRecallJob struct {
	sessionID   string
	requestID   string
	candidates  []memoryHeader
	consumed    int
	recentTools string
	done        chan struct{}
	selected    []memoryHeader
}

// newMemoryRuntime 在 New() 装配：确保记忆目录存在（提示词承诺"目录已存在，
// 直接写入"），并对齐沙箱的 symlink 解析口径。选择器：宿主为 SelectorTier
// 配置了池模型时用 LLM 选择器，否则用一期 keywordSelector。
func newMemoryRuntime(opts Options, sandboxRoot string) *memoryRuntime {
	if opts.Memory == nil || !opts.Memory.enabled() {
		return nil
	}
	dir := strings.TrimSpace(opts.Memory.Dir)
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		runtimeLogger.warnf("[api:memory] mkdir %s failed, auto memory disabled: %v", dir, err)
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && strings.TrimSpace(resolved) != "" {
		dir = resolved
	}

	cfg := *opts.Memory
	cfg.Dir = dir
	extraDirs := make([]string, 0, len(cfg.ExtraDirs))
	for _, extra := range cfg.ExtraDirs {
		extra = strings.TrimSpace(extra)
		if extra == "" {
			continue
		}
		if abs, err := filepathAbs(extra); err == nil {
			extra = abs
		}
		extraDirs = append(extraDirs, extra)
	}
	cfg.ExtraDirs = extraDirs

	selector := memorySelector(keywordSelector{})
	if ls := newLLMSelector(opts); ls != nil {
		selector = ls
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &memoryRuntime{
		cfg:      cfg,
		store:    &memoryStore{autoDir: dir, extraDirs: extraDirs},
		selector: selector,
		guard: &memoryWriteGuard{
			autoDir:     dir,
			extraDirs:   extraDirs,
			sandboxRoot: sandboxRoot,
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

// memoryQueryGate 回合门槛：文本 CJK ≥4 字符通过；否则需 >1 个空白分词。
// 单词短句（"继续"、"ok"）不值得召回。
func memoryQueryGate(text string) bool {
	if text == "" {
		return false
	}
	cjk := 0
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			cjk++
		}
	}
	if cjk >= 4 {
		return true
	}
	return len(strings.Fields(text)) > 1
}

// memoryAgeDays 计算 mtime 的天龄（对齐 comate：UTC 秒差整除 86400，负值归 0）。
func memoryAgeDays(mtime, now time.Time) int {
	d := now.Sub(mtime)
	if d <= 0 {
		return 0
	}
	days := int(d / (24 * time.Hour))
	if days < 0 {
		return 0
	}
	return days
}

// memoryFreshnessWarning 新鲜度警示（comate memory_runtime.py 逐字移植，
// 文案一个字符都不能改）。
func memoryFreshnessWarning(ageDays int) string {
	return fmt.Sprintf("This memory is %d days old. Memories are point-in-time observations, not live state — claims about code behavior or file:line citations may be outdated. Verify against current code before asserting as fact.", ageDays)
}

// memoryFreshnessLabel 每条记忆的来源标签行。
func memoryFreshnessLabel(source memorySourceKind, ageDays int) string {
	if source == memorySourceExtra {
		return "User-authored knowledge (always applies)"
	}
	switch {
	case ageDays <= 0:
		return "Memory (saved today)"
	case ageDays == 1:
		return "Memory (saved yesterday)"
	default:
		return fmt.Sprintf("Memory (saved %d days ago)", ageDays)
	}
}

// surfacedMemory 是一条待注入的记忆（元数据 + 截断后内容）。
type surfacedMemory struct {
	header  memoryHeader
	content string
}

func (m surfacedMemory) render(now time.Time) string {
	age := memoryAgeDays(m.header.ModTime, now)
	header := fmt.Sprintf("%s: %s", memoryFreshnessLabel(m.header.Source, age), filepath.ToSlash(m.header.Path))
	if m.header.Source == memorySourceAuto && age >= memoryFreshnessThresholdDays {
		header = memoryFreshnessWarning(age) + "\n\n" + header
	}
	return header + "\n\n" + m.content
}

// renderMemoryInjection 渲染合成 user 消息正文（对齐 comate 包裹格式）。
func renderMemoryInjection(entries []surfacedMemory, now time.Time) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.render(now))
	}
	return "<system-reminder>\nRelevant memory surfaced for this turn:\n\n" +
		strings.Join(parts, "\n\n") + "\n</system-reminder>"
}

// memorySessionUsage 扫描历史中既有注入项的 metadata：返回已注入文件集合
// （去重）与累计注入字节数（预算）。兼容 JSON 恢复历史的数值类型。
func memorySessionUsage(msgs []message.Message) (surfaced map[string]struct{}, consumed int) {
	surfaced = map[string]struct{}{}
	for _, m := range msgs {
		if m.Metadata == nil {
			continue
		}
		if origin, ok := m.Metadata[memoryMetadataOrigin].(string); !ok || origin != memoryOriginRelevantMemory {
			continue
		}
		if paths, ok := m.Metadata[memoryMetadataPaths].(string); ok {
			for _, p := range strings.Split(paths, "|") {
				if p != "" {
					surfaced[p] = struct{}{}
				}
			}
		}
		consumed += asMetadataInt(m.Metadata[memoryMetadataTotalBytes])
	}
	return surfaced, consumed
}

func asMetadataInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

// beginMemoryRecall 在用户消息落盘后、本回合首次模型调用前启动召回。直接向
// prep.history 追加合成 user 消息（不走 Runtime.Inject——DEFER 队列回合末才
// drain，会晚一回合）。合成标记沿用 SDK 现有机制获得压缩豁免。
//
// keywordSelector 保持一期同步语义（Select 后立即注入）；llmSelector 走异步
// 预取：goroutine 里做模型选择，consumeMemoryRecall 在首次模型调用前
// try-consume——未就绪则本回合放弃注入（comate consume_ready 语义）。
func (rt *Runtime) beginMemoryRecall(prep *preparedRun) {
	if rt == nil || rt.memory == nil {
		return
	}
	if rt.opts.subagentDisabled {
		return // 子 agent 上下文跳过（spec §6.1）
	}
	query := strings.TrimSpace(prep.prompt)
	if !memoryQueryGate(query) {
		return
	}

	surfaced, consumed := memorySessionUsage(prep.history.All())
	if consumed >= memorySessionBudgetBytes {
		return
	}
	candidates := filterSurfacedMemories(rt.memory.store.scan(), surfaced)
	if len(candidates) == 0 {
		return
	}

	if llm, ok := rt.memory.selector.(memoryLLMSelector); ok {
		rt.beginAsyncRecall(prep, llm, query, candidates, consumed)
		return
	}
	selected := rt.memory.selector.Select(query, candidates)
	if len(selected) == 0 {
		return
	}
	rt.appendRecallInjection(prep, selected, consumed, time.Now())
}

// beginAsyncRecall 为 llmSelector 发起预取 goroutine：单槽存放任务，goroutine
// 完成后关闭 done。结果由本回合的 consumeMemoryRecall 消费；无人消费则作废。
func (rt *Runtime) beginAsyncRecall(prep *preparedRun, llm memoryLLMSelector, query string, candidates []memoryHeader, consumed int) {
	job := &memoryRecallJob{
		sessionID:   strings.TrimSpace(prep.normalized.SessionID),
		requestID:   strings.TrimSpace(prep.normalized.RequestID),
		candidates:  candidates,
		consumed:    consumed,
		recentTools: memoryRecentToolsSection(collectRecentToolNames(prep.history.All(), memoryRecentToolsLimit)),
		done:        make(chan struct{}),
	}
	rt.memory.recallMu.Lock()
	rt.memory.recallJob = job
	rt.memory.recallMu.Unlock()
	rt.memory.wg.Add(1)
	go func() {
		defer rt.memory.wg.Done()
		job.selected = llm.SelectMemory(rt.memory.ctx, query, candidates, job.recentTools)
		close(job.done)
	}()
}

// consumeMemoryRecall 在本回合首次模型调用前尝试消费异步召回：任务就绪则
// 注入，未就绪则放弃（绝不阻塞、绝不晚到）。只消费归属本回合的任务，
// 其他会话/回合的迟到结果留在槽内由归属方处理或被覆盖作废。
func (rt *Runtime) consumeMemoryRecall(prep *preparedRun) {
	if rt == nil || rt.memory == nil {
		return
	}
	rt.memory.recallMu.Lock()
	job := rt.memory.recallJob
	if job != nil && (job.sessionID != strings.TrimSpace(prep.normalized.SessionID) ||
		job.requestID != strings.TrimSpace(prep.normalized.RequestID)) {
		job = nil // 归属其他回合：不动槽，由归属方消费或被覆盖作废
	}
	if job != nil {
		rt.memory.recallJob = nil
	}
	rt.memory.recallMu.Unlock()
	if job == nil {
		return
	}
	select {
	case <-job.done:
	default:
		runtimeLogger.printf("[api:memory] llm recall not ready, skip injection this turn session=%s", job.sessionID)
		return
	}
	if len(job.selected) == 0 {
		return
	}
	rt.appendRecallInjection(prep, job.selected, job.consumed, time.Now())
}

// appendRecallInjection 把选中的记忆按预算逐条 surface 并追加合成 user 消息；
// 预算放不下的整条放弃（comate 同款 break 语义）。
func (rt *Runtime) appendRecallInjection(prep *preparedRun, selected []memoryHeader, consumed int, now time.Time) {
	remaining := memorySessionBudgetBytes - consumed
	entries := make([]surfacedMemory, 0, len(selected))
	var paths []string
	total := 0
	for _, h := range selected {
		content, _, err := surfaceMemoryDocument(h.Path, filepath.ToSlash(h.Path))
		if err != nil {
			runtimeLogger.warnf("[api:memory] surface %s failed: %v", h.Path, err)
			continue
		}
		entry := surfacedMemory{header: h, content: content}
		size := len(entry.render(now))
		if total+size > remaining {
			break // 预算放不下的整条放弃（comate 同款 break 语义）
		}
		entries = append(entries, entry)
		paths = append(paths, filepath.ToSlash(h.Path))
		total += size
	}
	if len(entries) == 0 {
		return
	}

	prep.history.Append(message.Message{
		Role:    "user",
		Content: renderMemoryInjection(entries, now),
		Metadata: map[string]any{
			metadataTimestamp:        now.UnixMilli(),
			metadataSynthetic:        true,
			metadataSyntheticKey:     memoryOriginRelevantMemory,
			memoryMetadataOrigin:     memoryOriginRelevantMemory,
			memoryMetadataPaths:      strings.Join(paths, "|"),
			memoryMetadataTotalBytes: total,
		},
	})
}

func filterSurfacedMemories(headers []memoryHeader, surfaced map[string]struct{}) []memoryHeader {
	if len(surfaced) == 0 {
		return headers
	}
	out := make([]memoryHeader, 0, len(headers))
	for _, h := range headers {
		if _, ok := surfaced[filepath.ToSlash(h.Path)]; ok {
			continue
		}
		out = append(out, h)
	}
	return out
}
