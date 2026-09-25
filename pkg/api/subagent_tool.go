package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	hooks "github.com/lzq70112/agentsdk-go/pkg/hooks"
	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

const subagentToolName = "subagent"

// defaultSubagentMaxConcurrent 是内置 subagent tool 后台任务的默认并发上限。
const defaultSubagentMaxConcurrent = 3

// subagentConcurrencyLimit 解析后台任务并发上限：优先取 Options.MaxConcurrentSubagents，
// 未设置（<=0）时回落到默认值。上限只约束后台任务；同步派发由 run loop 天然串行。
func subagentConcurrencyLimit(opts Options) int {
	if opts.MaxConcurrentSubagents > 0 {
		return opts.MaxConcurrentSubagents
	}
	return defaultSubagentMaxConcurrent
}

// subagentTool 让主 agent 能把一项自包含任务派发给子 agent 处理。
// 子 agent 采用隔离上下文：不继承主会话的对话历史，只带一条 delegation 消息
// 起步，并用自己的身份 system prompt 运行，从而避免被主会话内容带偏。
type subagentTool struct {
	opts    Options
	runtime *Runtime

	// disabled 为 true 时 Execute 直接返回拒绝。子 runtime 中该 tool 的
	// Name/Description/Schema 与主 runtime 一致，仅执行体变成 no-op，
	// 从机制上硬阻断孙 agent 派生（提示词只是软约束），最多 1 层。
	disabled bool

	// 异步派发状态：asyncCtx 与主 Runtime 同生命周期，Close 时取消并在
	// shutdownAsync 中等待全部在途任务退出，避免 goroutine 泄漏。
	asyncMu     sync.Mutex
	asyncWG     sync.WaitGroup
	asyncCtx    context.Context
	asyncCancel context.CancelFunc

	// 后台任务注册表：tasks 按子会话标识索引在途/已完成的后台任务，供
	// subagent_status / subagent_stop 查询与停止。生命周期与主 Runtime 绑定，
	// Close 时在 shutdownAsync 之后整体清空。
	taskMu sync.Mutex
	tasks  map[string]*asyncTask

	// 后台任务并发上限：bgSlots 容量为 subagentConcurrencyLimit(opts)，拿不到槽位的
	// 派发直接拒绝，避免无上限地派生后台子 agent。仅约束后台任务，同步派发不受影响。
	bgSlots chan struct{}

	// 子会话缓存：subSessions 按子会话标识索引已成功完成的子会话历史快照，
	// 供主 agent 携带标识追问时在子会话自身上下文续跑（不读主历史、
	// 不读文件存储）。缓存生命周期与主 Runtime 绑定，Close 时整体清理。
	subMu       sync.Mutex
	subSessions map[string]*subSessionEntry
}

// subSessionEntry 是一个可追问子会话的内存缓存条目。
type subSessionEntry struct {
	History   []message.Message // 子会话自身截止目前的完整历史
	AgentType AgentType         // 首次派发使用的类型配置，追问时保持一致
	TypeName  string            // 首次派发传入的类型名（空表示默认通用型）
}

// newSubagentTool 创建一个 subagent tool。
// opts 必须是创建主 Runtime 时使用的 Options；创建子 Runtime 时会以它为基础，
// 保留工具面并把本 tool 标记为禁用 no-op 防递归，同时替换身份 system prompt、
// 注入子会话自身 history。
// 项目目录 .agents/subagents/ 下的类型定义文件在此加载，编程注册同名时优先。
// runtime 可在主 Runtime 创建后通过 bindRuntime 补绑。
func newSubagentTool(opts Options) *subagentTool {
	opts.AgentTypes = mergeAgentTypes(loadAgentTypesFromDir(opts.ProjectRoot), opts.AgentTypes)
	return &subagentTool{
		opts:        opts,
		disabled:    opts.subagentDisabled,
		tasks:       make(map[string]*asyncTask),
		bgSlots:     make(chan struct{}, subagentConcurrencyLimit(opts)),
		subSessions: make(map[string]*subSessionEntry),
	}
}

// bindRuntime 补绑主 Runtime 引用（双向绑定：Runtime 关闭时据此取消在途异步任务），
// 并初始化异步派发的生命周期上下文。
func (t *subagentTool) bindRuntime(rt *Runtime) {
	if t == nil {
		return
	}
	t.runtime = rt
	if rt != nil {
		rt.subagent = t
	}
	t.asyncMu.Lock()
	defer t.asyncMu.Unlock()
	if t.asyncCancel == nil {
		t.asyncCtx, t.asyncCancel = context.WithCancel(context.Background())
	}
}

// Name 返回 tool 的唯一标识。
func (t *subagentTool) Name() string { return subagentToolName }

// Description 返回给 LLM 看的 tool 说明。注册了 agent 类型时，末尾追加可用类型
// 清单，让主 agent 能按任务性质选择；未注册时返回基础文案，与历史行为一致。
func (t *subagentTool) Description() string {
	base := `当你判断当前对话中的某个任务需要专门能力、独立视角或更详尽分析时，调用此 tool 派生子 agent。
子 agent 在隔离的上下文中运行：它看不到当前对话历史，只依据你写的 instruction 工作，
并沿用项目的 rules/AGENTS.md 等上下文与工具面。因此 instruction 必须自包含。

何时使用：
- 需要对大量上下文做整体分析、审查、总结，且更适合作为一个独立任务完整执行
- 你希望某个任务被完整执行并返回可直接使用的结果，而不是自己一步步做完
- 任务耗时较长、当前对话不必等待其结果时，用 background=true 后台执行
- 对已派发的子 agent 追问时，把派发结果中的 sub_session_id 通过同名参数传回，子 agent 将带着此前完整工作记忆续跑，无需重述背景

何时不要使用：
- 简单可直接回答的问题，或只需一两次工具调用就能完成的事，亲自执行更快
- 还无法清楚描述任务目标时，先想清楚再派发

派发要求：
- instruction 必须自包含：目标、范围、期望的产出要写清楚，子 agent 会得到与 instruction 完整度相称的结果

结果预期：
- 子 agent 返回该任务的完整执行结果（结论、关键证据、遗留问题），可直接使用
- 不要重新执行子 agent 已完成的工作；如需校验，只核对关键结论或抽查它的实际变更`

	if len(t.opts.AgentTypes) == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n可用类型（通过 type 参数选择；不填使用默认通用型，全量继承主 agent 工具）：\n")
	for _, at := range t.opts.AgentTypes {
		name := strings.TrimSpace(at.Name)
		if name == "" {
			continue
		}
		desc := strings.TrimSpace(at.Description)
		if desc == "" {
			desc = "（无描述）"
		}
		fmt.Fprintf(&b, "- %s: %s\n", name, desc)
	}
	return b.String()
}

// subagentOutputContract 是追加给子 agent 的输出契约，确保子 agent 返回完整可用的结果
// 而不是一句话摘要——否则主 agent 只能返工甚至亲自重做，subagent 就失去了意义。
const subagentOutputContract = `【输出要求】返回该子任务的完整执行结果，主 agent 将直接使用：
1. 结论先行：第一句话说明你完成了什么、发现了什么；
2. 给出支撑结论的关键证据（涉及的文件路径、代码位置、命令输出、验证结果）；
3. 说明你具体做了什么、有哪些遗留问题或风险；
4. 不要只给一句话摘要。`

// Schema 描述 tool 参数。
func (t *subagentTool) Schema() *tool.JSONSchema {
	return &tool.JSONSchema{
		Type: "object",
		Properties: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "子 agent 的名称，用于标识本次子任务",
			},
			"instruction": map[string]any{
				"type":        "string",
				"description": "需要子 agent 执行的具体任务说明",
			},
			"type": map[string]any{
				"type":        "string",
				"description": "子 agent 类型名称；不填使用默认通用型",
			},
			"background": map[string]any{
				"type":        "boolean",
				"description": "是否后台执行：true 时立即返回任务标识，子 agent 在后台运行，完成后通过 hooks 事件通知；默认 false 同步执行并等待结果",
			},
			"sub_session_id": map[string]any{
				"type":        "string",
				"description": "要追问的既有子会话标识（即派发结果中的 sub_session_id）；携带该标识时子 agent 带着此前完整工作记忆在原上下文续跑，无需重述背景，类型配置沿用首次派发",
			},
		},
		Required: []string{"name", "instruction"},
	}
}

// parseSubagentParams 解析并校验 tool 入参，返回去空白后的 name、instruction、
// 类型名、后台开关与要追问的子会话标识（空串表示首次派发）。
func parseSubagentParams(params map[string]any) (name, instruction, typeName string, background bool, subSessionID string, err error) {
	name, _ = params["name"].(string)
	instruction, _ = params["instruction"].(string)
	typeName, _ = params["type"].(string)
	background, _ = params["background"].(bool)
	subSessionID, _ = params["sub_session_id"].(string)
	name = strings.TrimSpace(name)
	instruction = strings.TrimSpace(instruction)
	typeName = strings.TrimSpace(typeName)
	subSessionID = strings.TrimSpace(subSessionID)
	if name == "" || instruction == "" {
		return "", "", "", false, "", errors.New("subagent tool 需要非空的 name 和 instruction 参数")
	}
	return name, instruction, typeName, background, subSessionID, nil
}

// defaultSubagentIdentityPrompt 是未指定 agent 类型时子 agent 的身份 system
// prompt。子 agent 不继承主 agent 的 system prompt，只拿到这份身份声明 + 项目
// rules/memory，因此必须在第一句明确它"是子 agent、不是主 agent、看不到主会话
// 历史"，避免它把自己当成主 agent 继续派发。
const defaultSubagentIdentityPrompt = `你是一个由主 agent 派发的子 agent，负责完成一项自包含的任务。

你不是主 agent，也看不到主会话的对话历史；你只能依据本次收到的任务说明工作，不要假设自己拥有任务说明之外的背景。

工作要求：
1. 自主使用可用工具完成任务，先检索、再动手；
2. 只聚焦被交付的任务范围，不要擅自扩大范围或执行任务说明之外的动作；
3. 完成后返回该任务的完整结果：结论先行，附关键证据（文件路径、命令输出、验证结果），并说明遗留问题或风险；
4. 不要只给一句话摘要。`

// subagentDisabledRefusal 是子 runtime 内 subagent tool 被调用时返回的拒绝文案。
// 它只是给模型的软提示；硬保证是 disabled 分支本身——调用不会触达派发逻辑。
// 测试复用同一常量，避免文案漂移。
const subagentDisabledRefusal = "你已经是子 agent，不能再派生子 agent；请直接使用现有工具完成当前任务。"

// buildDelegationMessage 构造交给子 agent 的任务消息：主 agent 写的 instruction
// 加上输出契约，确保子 agent 返回完整可用的结果。子 agent 的上下文从这条消息
// 开始，不再 fork 主会话历史。
func buildDelegationMessage(name, instruction string) string {
	return fmt.Sprintf("[Subtask] %s: %s\n\n%s", name, instruction, subagentOutputContract)
}

// newSubSessionID 生成子 agent 的独立 session id，避免与主 session 的持久化文件冲突。
func newSubSessionID(sessionID string) string {
	return fmt.Sprintf("%s-sub-%s", sessionID, uuid.New().String())
}

// buildSubOptions 基于主 Options 构造子 Runtime 选项：子 agent 采用隔离上下文，
// 不 fork 主会话历史；身份 system prompt 由 agent 类型决定（为空时用默认），替换
// 子 runtime 的 identity 段，但保留 rules/memory 等项目上下文。子 runtime 内的
// subagent tool 标记为禁用 no-op，硬阻断孙 agent 派生（最多 1 层）。
// history 是子会话自身的历史：首次派发为 nil（全新上下文），追问续跑时传回先前快照。
func (t *subagentTool) buildSubOptions(agentType AgentType, history []message.Message) Options {
	subOpts := t.opts
	subOpts.subagentDisabled = true
	identity := strings.TrimSpace(agentType.SystemPrompt)
	if identity == "" {
		identity = defaultSubagentIdentityPrompt
	}
	subOpts.SystemPrompt = identity
	subOpts.HistoryLoader = func(string) ([]message.Message, error) {
		return message.CloneMessages(history), nil
	}
	return subOpts
}

// subagentStreamContext 返回子 agent 专用的隔离 context：保留取消、超时与
// session 标识，但把流式事件出口换成 no-op，并换上独立的 streamForwardState。
// 主会话用 RunStream 时，子 agent 的文本 delta 与工具输出若沿用主会话的出口，
// 会被推入主事件流、混进用户可见的主回复，并置位主会话的转发状态；这里切断这条
// 通路。出口保持非 nil 是刻意的：detectStall 由「出口是否存在」推导，保留它才能
// 让子 agent 继续享有流式卡死检测与回退。
func subagentStreamContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, streamEmitCtxKey, streamEmitFunc(func(context.Context, StreamEvent) {}))
	ctx = context.WithValue(ctx, streamForwardStateCtxKey, &streamForwardState{})
	return ctx
}

// dispatch 创建子 Runtime 并同步执行子任务，返回子 agent 的输出文本。
// prompt 是发给子 agent 的完整任务消息（delegation 包装后的指令）。执行成功后把
// 子会话历史快照写入缓存，供后续追问续跑。
func (t *subagentTool) dispatch(ctx context.Context, sessionID, subSessionID, name, prompt string, agentType AgentType, start time.Time, subOpts Options) (string, error) {
	subCtx := subagentStreamContext(ctx)
	subRuntime, err := New(subCtx, subOpts)
	if err != nil {
		runtimeLogger.warnf("[subagent] 创建子 Runtime 失败 main_session=%s name=%s: %v", sessionID, name, err)
		t.notifySubagentCompletion(sessionID, subSessionID, name, agentType, nil, err)
		return "", fmt.Errorf("创建子 agent 失败: %w", err)
	}
	defer subRuntime.Close()

	res, err := subRuntime.Run(subCtx, Request{
		SessionID: subSessionID,
		Prompt:    prompt,
	})
	// 无论成功失败都取一次子会话历史快照：成功用于追问续跑与落盘，失败用于排查。
	// SessionHistory 返回克隆，可安全交给回调与缓存。
	history, hasHistory := subRuntime.SessionHistory(subSessionID)
	if err != nil {
		runtimeLogger.warnf("[subagent] 子 agent 执行失败 main_session=%s sub_session=%s name=%s duration=%s: %v",
			sessionID, subSessionID, name, time.Since(start), err)
		t.notifySubagentCompletion(sessionID, subSessionID, name, agentType, history, err)
		return "", fmt.Errorf("子 agent 执行失败: %w", err)
	}
	// 缓存子会话历史供后续追问续跑：历史取自子 runtime 的内存快照
	// （SessionHistory 返回克隆），不经过文件存储。
	if hasHistory {
		t.storeSubSession(subSessionID, agentType, history)
	}
	t.notifySubagentCompletion(sessionID, subSessionID, name, agentType, history, nil)

	output := ""
	if res.Result != nil {
		output = res.Result.Output
	}
	return output, nil
}

// notifySubagentCompletion 调用宿主的子会话完成回调（若已配置）。回调在子会话历史
// 快照就绪后触发，同步派发与后台任务共用同一入口，保证宿主不漏接任何子会话。
func (t *subagentTool) notifySubagentCompletion(mainSessionID, subSessionID, name string, agentType AgentType, history []message.Message, err error) {
	handler := t.opts.SubagentCompletionHandler
	if handler == nil {
		return
	}
	handler(SubagentCompletion{
		MainSessionID: mainSessionID,
		SubSessionID:  subSessionID,
		Name:          name,
		AgentType:     agentType.Name,
		History:       history,
		Err:           err,
	})
}

// publishSubagentEvent 向 hooks 总线发布 subagent 生命周期事件，供平台侧审计与监控。
// 发布失败只告警：观测链路的问题不应影响派发本身的结果与回传。
func (t *subagentTool) publishSubagentEvent(evt hooks.Event) {
	if t == nil || t.runtime == nil || t.runtime.hooks == nil {
		return
	}
	if err := t.runtime.hooks.Publish(evt); err != nil {
		runtimeLogger.warnf("[subagent] 生命周期事件发布失败 type=%s: %v", evt.Type, err)
	}
}

// Execute 被 SDK tool executor 调用：为子 agent 准备隔离上下文（全新历史 + 身份
// system prompt），运行子任务并返回结果。子 runtime 中的本 tool 是 no-op：调用即
// 拒绝——这是阻断孙 agent 派生的硬保证（最多 1 层），不依赖模型自觉。
func (t *subagentTool) Execute(ctx context.Context, params map[string]any) (*tool.ToolResult, error) {
	if t.disabled {
		return &tool.ToolResult{
			Success: false,
			Output:  subagentDisabledRefusal,
		}, nil
	}
	start := time.Now()

	sessionID, ok := ToolSessionIDFromContext(ctx)
	if !ok || strings.TrimSpace(sessionID) == "" {
		return &tool.ToolResult{
			Success: false,
			Output:  "subagent tool 无法获取当前 session id",
		}, nil
	}

	name, instruction, typeName, background, resumeID, err := parseSubagentParams(params)
	if err != nil {
		return &tool.ToolResult{
			Success: false,
			Output:  err.Error(),
		}, nil
	}

	var agentType AgentType
	var history []message.Message
	var subSessionID string
	if resumeID != "" {
		// 追问路径：取回子会话自身的历史在原上下文续跑（不是主会话历史）。
		entry, entryErr := t.lookupSubSession(sessionID, resumeID, typeName)
		if entryErr != nil {
			return &tool.ToolResult{
				Success: false,
				Output:  entryErr.Error(),
			}, nil
		}
		agentType = entry.AgentType
		history = entry.History
		subSessionID = resumeID
	} else {
		// 首次派发：子 agent 从隔离上下文起步，历史为空，只带一条 delegation 消息。
		agentType = t.lookupAgentType(typeName)
		subSessionID = newSubSessionID(sessionID)
	}
	// 首次派发与追问都把指令包装成 delegation 消息（[Subtask] + 输出契约），
	// 追问时它作为新的一轮用户消息追加在子会话既有历史之后。
	prompt := buildDelegationMessage(name, instruction)

	t.publishSubagentEvent(hooks.Event{
		Type:      hooks.SubagentStart,
		SessionID: sessionID,
		Payload: hooks.SubagentStartPayload{
			Name:      name,
			AgentID:   subSessionID,
			AgentType: agentType.Name,
		},
	})

	subOpts := t.buildSubOptions(agentType, history)
	if background {
		return t.dispatchAsync(sessionID, subSessionID, name, instruction, prompt, agentType, start, subOpts), nil
	}

	output, err := t.dispatch(ctx, sessionID, subSessionID, name, prompt, agentType, start, subOpts)
	t.reportCompletion(sessionID, subSessionID, name, agentType, output, err, time.Since(start))
	if err != nil {
		return &tool.ToolResult{
			Success: false,
			Output:  err.Error(),
		}, nil
	}
	return &tool.ToolResult{
		Success: true,
		Output:  output,
		Data:    map[string]any{"sub_session_id": subSessionID},
	}, nil
}

// reportCompletion 在子任务结束后统一发布完成事件并记录成功日志。同步与异步路径
// 共用同一实现，保证事件契约与日志内容一致。
func (t *subagentTool) reportCompletion(sessionID, subSessionID, name string, agentType AgentType, output string, err error, duration time.Duration) {
	payload := hooks.SubagentCompletePayload{
		TaskID:    subSessionID,
		Name:      name,
		AgentType: agentType.Name,
		Duration:  duration,
	}
	if err != nil {
		payload.Status = "error"
		payload.Error = err.Error()
	} else {
		payload.Status = "success"
		payload.Output = truncateString(output, subagentOutputLimit)
		payload.OutputLength = len(output)
	}
	t.publishSubagentEvent(hooks.Event{
		Type:      hooks.SubagentComplete,
		SessionID: sessionID,
		Payload:   payload,
	})
	if err != nil {
		return
	}
	runtimeLogger.printf("[subagent] 子 agent 执行成功 main_session=%s sub_session=%s name=%s duration=%s output_len=%d",
		sessionID, subSessionID, name, duration, len(output))
}

// dispatchAsync 把子任务放到独立 goroutine 中执行，立即返回"已派发"结果。
// goroutine 使用与主 Runtime 同生命周期的 asyncCtx：主 Runtime 关闭时任务被取消，
// shutdownAsync 会等待其退出，不泄漏 goroutine。wg 登记与关闭检查在同一把锁内
// 完成，保证关闭后不会有新任务绕过等待。
func (t *subagentTool) dispatchAsync(sessionID, subSessionID, name, instruction, prompt string, agentType AgentType, start time.Time, subOpts Options) *tool.ToolResult {
	// 并发上限：拿不到槽位直接拒绝，不阻塞主 agent 当前对话。
	select {
	case t.bgSlots <- struct{}{}:
	default:
		return &tool.ToolResult{
			Success: false,
			Output:  fmt.Sprintf("后台子任务已达并发上限 %d 个，请等待现有任务完成，或先用 subagent_stop 停止一个再重试。", cap(t.bgSlots)),
		}
	}

	t.asyncMu.Lock()
	if t.asyncCancel == nil {
		t.asyncMu.Unlock()
		<-t.bgSlots
		return &tool.ToolResult{
			Success: false,
			Output:  "主 Runtime 已关闭，无法派发后台子任务",
		}
	}
	ctx, cancel := context.WithCancel(t.asyncCtx)
	t.asyncWG.Add(1)
	t.asyncMu.Unlock()

	t.registerTask(&asyncTask{
		id:          subSessionID,
		sessionID:   sessionID,
		name:        name,
		instruction: instruction,
		status:      asyncTaskRunning,
		startedAt:   start,
		cancel:      cancel,
	})

	go func() {
		defer t.asyncWG.Done()
		defer func() { <-t.bgSlots }()
		defer cancel()
		output, err := t.dispatch(ctx, sessionID, subSessionID, name, prompt, agentType, start, subOpts)
		t.finishTask(subSessionID, output, err)
		t.reportCompletion(sessionID, subSessionID, name, agentType, output, err, time.Since(start))
		t.injectAsyncResult(sessionID, subSessionID, name, instruction, output, err)
	}()

	return &tool.ToolResult{
		Success: true,
		Output:  fmt.Sprintf("已派发子任务 %s（任务标识 %s）。子 agent 后台执行中，完成后通过 hooks 事件通知，当前对话不被阻塞。", name, subSessionID),
		Data: map[string]any{
			"task_id":        subSessionID,
			"sub_session_id": subSessionID,
			"async":          true,
		},
	}
}

// shutdownAsync 取消全部进行中的异步子任务并等待它们退出，由主 Runtime Close 调用。
func (t *subagentTool) shutdownAsync() {
	if t == nil {
		return
	}
	t.asyncMu.Lock()
	cancel := t.asyncCancel
	t.asyncCancel = nil
	t.asyncCtx = nil
	t.asyncMu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.asyncWG.Wait()
}

// lookupSubSession 校验追问标识的归属（必须是当前主会话派发的），再从内存缓存
// 取回子会话条目。子会话标识形如 <主会话标识>-sub-<随机部分>，前缀校验先行，
// 防止跨会话误操作；typeName 非空时必须与首次派发的类型一致，保证追问时沿用的
// 类型追加提示词不漂移。
func (t *subagentTool) lookupSubSession(mainSessionID, subSessionID, typeName string) (*subSessionEntry, error) {
	if !strings.HasPrefix(subSessionID, mainSessionID+"-sub-") {
		return nil, fmt.Errorf("子会话标识 %q 不属于当前会话 %q，拒绝追问", subSessionID, mainSessionID)
	}
	t.subMu.Lock()
	defer t.subMu.Unlock()
	entry, ok := t.subSessions[subSessionID]
	if !ok {
		return nil, fmt.Errorf("找不到子会话 %q 的记录（标识无效或已被清理）", subSessionID)
	}
	if typeName != "" && !strings.EqualFold(typeName, entry.TypeName) {
		return nil, fmt.Errorf("子会话 %q 的类型为 %q，与本次请求的 %q 不一致，追问须沿用首次派发的类型", subSessionID, entry.TypeName, typeName)
	}
	return entry, nil
}

// storeSubSession 缓存子会话的历史快照与类型配置。条目发布后不再修改，
// 同一标识的再次存储以新条目整体替换旧条目，读取方无需额外同步。
func (t *subagentTool) storeSubSession(subSessionID string, agentType AgentType, history []message.Message) {
	if t == nil || subSessionID == "" {
		return
	}
	t.subMu.Lock()
	defer t.subMu.Unlock()
	if t.subSessions == nil {
		t.subSessions = make(map[string]*subSessionEntry)
	}
	t.subSessions[subSessionID] = &subSessionEntry{
		History:   history,
		AgentType: agentType,
		TypeName:  agentType.Name,
	}
}

// clearSubSessions 清空全部子会话缓存，由主 Runtime Close 调用，保证缓存
// 生命周期与主 runtime 一致，不跨 runtime 泄漏内存。
func (t *subagentTool) clearSubSessions() {
	if t == nil {
		return
	}
	t.subMu.Lock()
	defer t.subMu.Unlock()
	t.subSessions = nil
}

// forgetSubSession 删除单个子会话缓存，供 SDK 使用者显式清理。
func (t *subagentTool) forgetSubSession(subSessionID string) bool {
	if t == nil {
		return false
	}
	t.subMu.Lock()
	defer t.subMu.Unlock()
	if _, ok := t.subSessions[subSessionID]; !ok {
		return false
	}
	delete(t.subSessions, subSessionID)
	return true
}
