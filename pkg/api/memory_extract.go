package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "embed"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// memoryExtractPromptTemplate 是抽取子代理的提示词（comate
// prompts/memory/extract.md 逐字移植，仅 Available tools 行按 spec §8-3
// 删去只读 Bash 措辞）。
//
//go:embed prompts/extract.md
var memoryExtractPromptTemplate string

// memoryTypeGuidePrompt 是记忆类型指南（comate prompts/memory/type_guide.md
// 逐字移植），渲染进 extract 提示词的 {{MEMORY_TYPE_GUIDE}}。
//
//go:embed prompts/type_guide.md
var memoryTypeGuidePrompt string

// 抽取子系统常量。
const (
	// memoryExtractMaxIterations 是抽取子代理的回合预算。原值 5（comate
	// max_turns=5）：抽取常需多轮 write 落盘多条记忆，5 轮打满即整批丢失
	// （失败后游标照推不重试），放宽到 12。
	memoryExtractMaxIterations = 12
	// memoryStateFileName 是记忆桶目录下的游标状态文件名。
	memoryStateFileName = ".memory_state.json"
	// memoryExtractTools 是抽取子代理的工具白名单：只读四件套 + 记忆写入两件，
	// 无 bash（spec §8-3 安全偏离）。
	memoryExtractTools = "read,write,edit,glob,grep"
)

// memoryStateFile 是 .memory_state.json 的落盘形态：游标记录已抽取处理过的
// 会话历史条数（SDK 消息无稳定 ID，用条数游标；压缩导致历史变短时游标收敛）。
type memoryStateFile struct {
	LastCursorMessageID int `json:"last_cursor_message_id"`
}

// memoryExtractToolWhitelist 返回抽取子代理的工具白名单切片。
func memoryExtractToolWhitelist() []string {
	return strings.Split(memoryExtractTools, ",")
}

// memoryExtractor 在回合正常结束后用后台子代理抽取记忆（spec §7.1）：
// 全局并发 1，抽取进行中又来新回合则记 pending、完成后自动补跑一轮；
// 抽取失败只 warn，绝不影响主回合。
type memoryExtractor struct {
	rt *Runtime

	// once 是单轮抽取的执行函数（默认 extractOnce），可替换以便测试注入
	// 确定的时序（gate/pending/Close 用例）。
	once func(sessionID string) bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	running bool
	pending bool
}

// statePathFor 返回游标文件路径：PerSession 时游标随会话桶隔离（多会话并发
// 抽取互不推进对方的游标），否则为桶根的全局游标（现状行为）。
func (e *memoryExtractor) statePathFor(sessionID string) string {
	return filepath.Join(e.rt.memory.store.sessionBucketDir(sessionID), memoryStateFileName)
}

// initExtractor 在 New() 完成 runtime 装配后初始化抽取器（Memory 禁用时不建）。
func (m *memoryRuntime) initExtractor(rt *Runtime) {
	if m == nil {
		return
	}
	m.extractor = &memoryExtractor{
		rt: rt,
	}
	m.extractor.once = m.extractor.extractOnce
	m.extractor.ctx, m.extractor.cancel = context.WithCancel(context.Background())
}

// loadCursor 读取游标；文件不存在或损坏按 0 处理（全量抽取一次，与恢复会话
// 的增量语义一致）。
func (e *memoryExtractor) loadCursor(sessionID string) int {
	statePath := e.statePathFor(sessionID)
	data, err := os.ReadFile(statePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			runtimeLogger.warnf("[api:memory-extract] load state failed: %v", err)
		}
		return 0
	}
	var state memoryStateFile
	if err := json.Unmarshal(data, &state); err != nil {
		runtimeLogger.warnf("[api:memory-extract] state file corrupted, reset cursor: %v", err)
		return 0
	}
	if state.LastCursorMessageID < 0 {
		return 0
	}
	return state.LastCursorMessageID
}

// saveCursor 原子落盘游标（tmp+rename，对齐 cron_store 模式）。
func (e *memoryExtractor) saveCursor(sessionID string, count int) {
	if err := e.saveCursorErr(sessionID, count); err != nil {
		runtimeLogger.warnf("[api:memory-extract] save state failed: %v", err)
	}
}

func (e *memoryExtractor) saveCursorErr(sessionID string, count int) error {
	statePath := e.statePathFor(sessionID)
	data, err := json.MarshalIndent(memoryStateFile{LastCursorMessageID: count}, "", "  ")
	if err != nil {
		return fmt.Errorf("api: memory state marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return fmt.Errorf("api: memory state dir: %w", err)
	}
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("api: memory state write: %w", err)
	}
	if err := os.Rename(tmp, statePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("api: memory state rename: %w", err)
	}
	return nil
}

// start 登记一次抽取请求：空闲则起后台 goroutine；进行中则置 pending，
// 当前一轮完成后自动补跑。
func (e *memoryExtractor) start(sessionID string) {
	if e == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		e.pending = true
		return
	}
	e.running = true
	e.wg.Add(1)
	go e.run(sessionID)
}

// run 串行消费抽取请求：每轮结束后若 pending 置位则补跑一轮。
func (e *memoryExtractor) run(sessionID string) {
	defer e.wg.Done()
	for {
		cont := e.once(sessionID)
		e.mu.Lock()
		again := cont && e.pending
		e.pending = false
		if !again {
			e.running = false
		}
		e.mu.Unlock()
		if !again {
			return
		}
		if err := e.ctx.Err(); err != nil {
			return
		}
	}
}

// extractOnce 执行一轮抽取：计算新增消息 → 起子代理跑 extract 提示词 →
// 推进游标。返回 false 表示无新内容（不触发 pending 补跑）。
func (e *memoryExtractor) extractOnce(sessionID string) bool {
	rt := e.rt
	if rt == nil || rt.memory == nil {
		return false
	}
	// 防双写（comate memory_extract.py 同款）：游标以来主对话已有成功的记忆
	// 目录写入 → 本轮跳过（标志消费清零，游标推进，避免重复考虑同一批消息）。
	if rt.memory.writeObserved.Swap(false) {
		runtimeLogger.printf("[api:memory-extract] session=%s skip: main conversation wrote memory this cycle", sessionID)
		if history, ok := rt.SessionHistory(sessionID); ok {
			e.saveCursor(sessionID, len(history))
		}
		return true
	}
	history, ok := rt.SessionHistory(sessionID)
	if !ok || len(history) == 0 {
		return false
	}
	histLen := len(history)
	cursor := e.loadCursor(sessionID)
	if cursor > histLen {
		// 压缩等导致历史变短：游标收敛到当前长度（摘要视作已处理）。
		cursor = histLen
		e.saveCursor(sessionID, cursor)
	}
	newCount := histLen - cursor
	if newCount <= 0 {
		return false
	}

	// 抽取目标桶：PerSession 时为会话子桶（写前确保存在——extract 提示词
	// 承诺"目录已存在，直接写入"），否则为桶根（现状行为）。
	bucket := rt.memory.store.sessionBucketDir(sessionID)
	if rt.memory.cfg.PerSession {
		if err := os.MkdirAll(bucket, 0o755); err != nil {
			runtimeLogger.warnf("[api:memory-extract] mkdir session bucket %s failed: %v", bucket, err)
			return true
		}
	}
	manifest := buildMemoryManifest(rt.memory.store.scan(sessionID))
	prompt := renderMemoryExtractPrompt(extractPromptParams{
		MemoryDir:        bucket,
		NewMessageCount:  newCount,
		TypeGuide:        memoryTypeGuidePrompt,
		ExistingMemories: manifest,
	})

	// 只喂游标之后的新消息：fork 全量历史会令子代理输入随会话线性膨胀
	// （实测 231 条消息 ≈6.2 万 tokens/轮），旧消息还会诱导模型反复调工具
	// 打满迭代上限，整批记忆随游标推进而丢失。
	subOpts := e.buildSubOptions(sessionID, history[cursor:histLen], prompt)
	subSessionID := newSubSessionID(sessionID)

	subRuntime, err := New(e.ctx, subOpts)
	if err != nil {
		runtimeLogger.warnf("[api:memory-extract] create sub runtime failed session=%s: %v", sessionID, err)
		e.saveCursor(sessionID, histLen)
		return true
	}
	defer subRuntime.Close()

	if _, err := subRuntime.Run(e.ctx, Request{
		SessionID:     subSessionID,
		Prompt:        prompt,
		ToolWhitelist: memoryExtractToolWhitelist(),
	}); err != nil {
		// 抽取失败只 warn（游标照常推进，不重试，绝不影响主回合）。
		runtimeLogger.warnf("[api:memory-extract] subagent run failed session=%s: %v", sessionID, err)
	}
	e.saveCursor(sessionID, histLen)
	return true
}

// extractPromptParams 是 extract.md 的占位符取值。
type extractPromptParams struct {
	MemoryDir        string
	NewMessageCount  int
	TypeGuide        string
	ExistingMemories string
}

// renderMemoryExtractPrompt 渲染抽取提示词：comate prompts/memory/extract.md
// 逐字移植（仅 Available tools 行按 spec §8-3 删去只读 Bash 措辞），四占位符替换。
func renderMemoryExtractPrompt(p extractPromptParams) string {
	out := memoryExtractPromptTemplate
	out = strings.ReplaceAll(out, "{{MEMORY_DIR_PATH}}", filepath.ToSlash(p.MemoryDir))
	out = strings.ReplaceAll(out, "{{NEW_MESSAGE_COUNT}}", fmt.Sprintf("%d", p.NewMessageCount))
	out = strings.ReplaceAll(out, "{{MEMORY_TYPE_GUIDE}}", p.TypeGuide)
	out = strings.ReplaceAll(out, "{{EXISTING_MEMORIES}}", p.ExistingMemories)
	return out
}

// buildSubOptions 为抽取子代理构造自建 Options（仿 dispatch 但不走 subagent
// tool）：继承父配置的工程设置，Memory 整链路禁用（防递归），工具收敛为白名单，
// System 换成渲染后的 extract 提示词，历史只含调用方切好的游标后新消息。
func (e *memoryExtractor) buildSubOptions(sessionID string, history []message.Message, extractPrompt string) Options {
	sub := e.rt.opts
	// 防递归：Memory 整链路关闭（无常驻提示/无召回/无守卫/无沙箱放行），
	// 子回合也就不会再触发抽取。
	sub.Memory = &MemoryOptions{Enabled: boolValuePtr(false)}
	sub.EnabledBuiltinTools = memoryExtractToolWhitelist()
	sub.MaxIterations = memoryExtractMaxIterations
	sub.SystemPrompt = extractPrompt
	sub.HistoryLoader = func(string) ([]message.Message, error) {
		return message.CloneMessages(history), nil
	}
	sub.subagentDisabled = true
	// 丢弃父 runtime 的 SystemPromptBuilder（其中含 memory-usage 等运行期段落，
	// 与"Memory 整链路禁用"矛盾）：子 runtime 从 SystemPrompt（extract 提示词）
	// 重建全新 builder，Memory 禁用时 memory-usage 段自然不注入。
	sub.SystemPromptBuilder = nil
	// 子 runtime 的 Memory 已禁用（memoryGuard 为 nil），改挂抽取专用的严格
	// 写守卫：write/edit 只允许落本会话记忆桶内的主题 .md（PerSession 时为
	// 会话子桶，否则为桶根）。
	bucket := e.rt.memory.store.sessionBucketDir(sessionID)
	sub.memoryExtractGuard = &memoryWriteGuard{
		autoDir:     bucket,
		sandboxRoot: e.rt.sbRoot,
		strict:      true,
	}
	// Memory 禁用时 buildSandboxManager 不放行记忆目录，显式加入允许路径，
	// 抽取写入才能落盘。
	sub.Sandbox.AllowedPaths = append(append([]string(nil), sub.Sandbox.AllowedPaths...), bucket)
	return sub
}

// maybeStartMemoryExtraction 在回合正常完成（completed）后触发后台抽取。
// 跳过条件：Memory 未启用 / 子代理上下文 / scheduled/cron 唤醒回合 /
// 无真实用户消息（prompt 为空）。判定依据见实现报告。
func (rt *Runtime) maybeStartMemoryExtraction(sessionID, prompt string) {
	if rt == nil || rt.memory == nil || rt.memory.extractor == nil {
		return
	}
	if rt.memory.cfg.autoDerived {
		// 宿主未配置 Memory（withDefaults 从 nil 派生的默认目录）：只保留一期
		// 召回语义，不静默发起抽取子代理（模型调用 + 记忆目录写盘副作用）。
		// 宿主显式配置 Memory（含 WithMemoryOptions）即启用完整抽取链路。
		return
	}
	if rt.opts.subagentDisabled {
		return // 子 agent 永不抽取（spec §7.1 防递归）
	}
	p := strings.TrimSpace(prompt)
	if p == "" {
		return // 无真实用户消息的回合不抽取
	}
	if strings.HasPrefix(p, scheduledFirePrefix) {
		return // scheduled/cron 唤醒回合跳过
	}
	rt.memory.extractor.start(sessionID)
}

// noteSuccessfulMemoryWrite 由工具执行器在 write/edit 成功执行后调用：
// 目标落在记忆目录内置位防双写标志（回合结束抽取时消费）。PerSession 模式下
// 只有落进当前会话桶的写入才算数——主对话写错位置（如桶根，PerSession 召回
// 扫不到）时不能压制抽取，由抽取子代理把内容正确落进会话桶兜底。
func (m *memoryRuntime) noteSuccessfulMemoryWrite(toolName string, params map[string]any, sessionID string) {
	if m == nil || m.guard == nil {
		return
	}
	switch canonicalToolName(toolName) {
	case "write", "edit":
	default:
		return
	}
	target, err := m.guard.resolveTarget(params)
	if err != nil {
		return
	}
	dir := m.guard.autoDir
	if m.cfg.PerSession && strings.TrimSpace(sessionID) != "" {
		dir = m.store.sessionBucketDir(sessionID)
	}
	if withinDir(dir, target) {
		m.writeObserved.Store(true)
	}
}

// shutdown 停掉 memory 子系统的全部后台工作（召回预取 + 抽取）并等待退出，
// 由 Runtime.Close 调用。
func (m *memoryRuntime) shutdown() {
	if m == nil {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	if e := m.extractor; e != nil {
		// 抽取子代理可能阻塞在模型调用上：cancel 其 ctx 使其退出后再等待，
		// 保证 Close 返回时无在途抽取 goroutine。
		if e.cancel != nil {
			e.cancel()
		}
		e.wg.Wait()
	}
}
