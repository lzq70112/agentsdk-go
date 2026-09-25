//go:build integration
// +build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/api"
	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

// 本文件是 spec《Subagent 上下文策略——fork 全量 + 全量工具 + 末尾注入》的实测入口：
// 观察默认无差异化类型与带追加提示词的类型派发时，子 runtime 首次请求的服务端
// prefix cache 命中情况，验证"保留全量工具 + 末尾注入"策略下父前缀可被子请求命中。
// 指标必须来自真实模型服务端（Anthropic 的 cache_read_input_tokens /
// cache_creation_input_tokens），因此：
//   - 仅在设置 ANTHROPIC_API_KEY 时运行，未设置则跳过；
//   - 主 runtime 的模型侧用脚本化响应确定性地派发固定参数的子任务，
//     子 runtime 的请求透传给真实模型并记录服务端返回的 usage；
//   - 主侧脚本化只为保证参数可控，被测对象（子 runtime 首次请求的前缀与
//     服务端缓存行为）是完整真实的。

// kvcRecord 记录一次子 runtime 模型调用的请求特征与服务端缓存指标。
type kvcRecord struct {
	seq            int    // 同一场景内的第几次子调用（从 1 开始）
	tools          string // 逗号分隔的工具名
	messageCount   int
	inputChars     int64
	lastMessageTip string // 最后一条消息内容前 48 字符，用于定位前缀差异点
	usage          model.Usage
}

// hybridKVCacheModel 按请求内容路由：含 [Subtask] 消息的请求来自子 runtime，
// 透传真实模型并记录指标；其余请求来自主 runtime，返回脚本化的 subagent
// 工具调用（按 dispatchQueue 顺序消费）或收尾文本。
type hybridKVCacheModel struct {
	real model.Model
	rec  *kvcRecorder

	mu           sync.Mutex
	dispatchArgs []map[string]any // 主侧脚本化工具调用的参数队列
	subCalls     int              // 子 runtime 调用计数
}

type kvcRecorder struct {
	mu      sync.Mutex
	records []kvcRecord
}

func (r *kvcRecorder) add(rec kvcRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
}

func (r *kvcRecorder) snapshot() []kvcRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]kvcRecord(nil), r.records...)
}

func hasSubtaskMessage(req model.Request) bool {
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "[Subtask] ") {
			return true
		}
	}
	return false
}

func (m *hybridKVCacheModel) withRecorder(r *kvcRecorder) { m.rec = r }

func requestFingerprint(req model.Request) (chars int64, lastTip string) {
	for _, m := range req.Messages {
		chars += int64(len(m.Content))
	}
	if n := len(req.Messages); n > 0 {
		last := req.Messages[n-1].Content
		if len(last) > 48 {
			lastTip = last[:48]
		} else {
			lastTip = last
		}
	}
	return chars, lastTip
}

func (m *hybridKVCacheModel) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	if hasSubtaskMessage(req) {
		m.mu.Lock()
		m.subCalls++
		seq := m.subCalls
		m.mu.Unlock()
		resp, err := m.real.Complete(ctx, req)
		if err != nil {
			return nil, err
		}
		chars, lastTip := requestFingerprint(req)
		tools := make([]string, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, t.Name)
		}
		if m.rec != nil {
			m.rec.add(kvcRecord{
				seq:            seq,
				tools:          strings.Join(tools, ","),
				messageCount:   len(req.Messages),
				inputChars:     chars,
				lastMessageTip: lastTip,
				usage:          resp.Usage,
			})
		}
		return resp, nil
	}
	return m.scriptedMainResponse(), nil
}

// scriptedMainResponse 消费派发参数队列：队列非空时产出 subagent 工具调用，
// 耗尽后产出收尾文本。
func (m *hybridKVCacheModel) scriptedMainResponse() *model.Response {
	m.mu.Lock()
	var args map[string]any
	if len(m.dispatchArgs) > 0 {
		args = m.dispatchArgs[0]
		m.dispatchArgs = m.dispatchArgs[1:]
	}
	m.mu.Unlock()

	if args == nil {
		return &model.Response{
			Message:    model.Message{Role: "assistant", Content: "子任务已处理完毕。"},
			StopReason: "end_turn",
		}
	}
	return &model.Response{
		Message: model.Message{
			Role: "assistant",
			ToolCalls: []model.ToolCall{{
				ID:        fmt.Sprintf("call-%d", time.Now().UnixNano()),
				Name:      "subagent",
				Arguments: args,
			}},
		},
		StopReason: "tool_use",
	}
}

func (m *hybridKVCacheModel) CompleteStream(ctx context.Context, req model.Request, cb model.StreamHandler) error {
	resp, err := m.Complete(ctx, req)
	if err != nil {
		return err
	}
	if cb != nil {
		return cb(model.StreamResult{Final: true, Response: resp})
	}
	return nil
}

type hybridKVFactory struct {
	mdl *hybridKVCacheModel
}

func (f hybridKVFactory) Model(ctx context.Context) (model.Model, error) { return f.mdl, nil }

// seedHistory 生成一份确定性的、体量足以体现前缀成本的主会话历史。
func seedHistory() []message.Message {
	msgs := make([]message.Message, 0, 14)
	for i := 0; i < 7; i++ {
		msgs = append(msgs,
			message.Message{
				Role: "user",
				Content: fmt.Sprintf("第 %d 轮提问：请审查这段配置解析代码的错误处理路径，重点关注 %s。",
					i+1, strings.Repeat("边界条件与默认值回退逻辑。", 30)),
			},
			message.Message{
				Role: "assistant",
				Content: fmt.Sprintf("第 %d 轮答复：错误处理路径整体完备，但 %s 建议补充显式校验与日志。",
					i+1, strings.Repeat("在配置缺省回退处", 25)),
			},
		)
	}
	return msgs
}

// runKVCacheCase 用独立的主 runtime 跑一个派发场景，返回记录到的子调用指标。
func runKVCacheCase(t *testing.T, real model.Model, agentTypes []api.AgentType, dispatchArgs map[string]any) []kvcRecord {
	t.Helper()

	sessionID := fmt.Sprintf("kvc-%s", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")))
	hybrid := &hybridKVCacheModel{real: real, dispatchArgs: []map[string]any{dispatchArgs}}
	rec := &kvcRecorder{}
	hybrid.withRecorder(rec)

	opts := api.Options{
		ModelFactory:        hybridKVFactory{mdl: hybrid},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "你是一个严谨的代码审查助理，先结论后证据。",
		EnabledBuiltinTools: []string{"read", "write", "subagent"},
		AgentTypes:          agentTypes,
		DefaultEnableCache:  true,
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return seedHistory(), nil
			}
			return nil, nil
		},
		AutoCompact: api.CompactConfig{Enabled: false},
	}

	rt, err := api.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("create main runtime: %v", err)
	}
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	enableCache := true
	if _, err := rt.Run(ctx, api.Request{
		SessionID:         sessionID,
		Prompt:            "请派发子 agent 对这段对话历史做一次完整审查。",
		EnablePromptCache: &enableCache,
	}); err != nil {
		t.Fatalf("main run: %v", err)
	}

	records := rec.snapshot()
	if len(records) == 0 {
		t.Fatal("子 runtime 的模型请求未被记录：派发链路可能未走到真实模型")
	}
	return records
}

func TestSubagentTypeKVCacheImpact(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set, skipping subagent KV cache measurement")
	}

	modelName := strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	if modelName == "" {
		modelName = "claude-sonnet-4-5-20250929"
	}
	real, err := model.NewAnthropic(model.AnthropicConfig{
		APIKey:    apiKey,
		BaseURL:   strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
		Model:     modelName,
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("create anthropic model: %v", err)
	}

	instruction := "请基于上下文对这段配置解析代码的审查结论做完整性核查，指出遗漏项。"

	// 场景 A：默认无差异化类型（不使用类型，全量继承工具面）。
	caseA := runKVCacheCase(t, real, nil, map[string]any{
		"name": "analyzer", "instruction": instruction,
	})

	// 场景 B：带工具白名单的类型。白名单已不再收窄子 agent 工具面，故其 tools
	// 应与场景 A 完全一致——本场景用于确认"白名单不再影响前缀"。
	caseB := runKVCacheCase(t, real, []api.AgentType{{
		Name: "explorer", Description: "只读检索型", AllowedTools: []string{"read"},
	}}, map[string]any{
		"name": "explorer", "instruction": instruction, "type": "explorer",
	})

	// 场景 C：仅带追加提示词的类型（不收窄工具）。
	caseC := runKVCacheCase(t, real, []api.AgentType{{
		Name: "auditor", Description: "审计型", AppendPrompt: "以最小惊讶原则输出审计发现。",
	}}, map[string]any{
		"name": "auditor", "instruction": instruction, "type": "auditor",
	})

	report := func(label string, records []kvcRecord) {
		for _, r := range records {
			t.Logf("[%s] seq=%d tools=[%s] messages=%d inputChars=%d input=%d output=%d cacheRead=%d cacheCreate=%d lastMsg=%q",
				label, r.seq, r.tools, r.messageCount, r.inputChars,
				r.usage.InputTokens, r.usage.OutputTokens, r.usage.CacheReadTokens, r.usage.CacheCreationTokens,
				r.lastMessageTip)
		}
	}
	report("A-default", caseA)
	report("B-whitelist", caseB)
	report("C-append-prompt", caseC)

	// 场景 B 的类型白名单已不再收窄工具面：A 与 B 的子请求工具集必须完全一致。
	if len(caseA) > 0 && len(caseB) > 0 && caseA[0].tools != caseB[0].tools {
		t.Errorf("白名单类型不应改变子请求工具面：A tools=[%s] B tools=[%s]", caseA[0].tools, caseB[0].tools)
	}

	// 场景 D（ sanity ）：同一主会话内以完全相同的参数派发两次，第二次应命中
	// 第一次写入的 prefix cache。这是 harness 自检——若服务端未回报任何缓存
	// 指标（cacheRead 与 cacheCreate 同时为 0），说明 provider/配置不支持，
	// 前面几组数字不可解释为缓存行为，必须显式失败。
	sessionID := "kvc-sanity"
	hybrid := &hybridKVCacheModel{real: real, dispatchArgs: []map[string]any{
		{"name": "analyzer", "instruction": instruction},
		{"name": "analyzer", "instruction": instruction},
	}}
	rec := &kvcRecorder{}
	hybrid.withRecorder(rec)
	sanityOpts := api.Options{
		ModelFactory:        hybridKVFactory{mdl: hybrid},
		ProjectRoot:         t.TempDir(),
		SystemPrompt:        "你是一个严谨的代码审查助理，先结论后证据。",
		EnabledBuiltinTools: []string{"read", "write", "subagent"},
		DefaultEnableCache:  true,
		HistoryLoader: func(sid string) ([]message.Message, error) {
			if sid == sessionID {
				return seedHistory(), nil
			}
			return nil, nil
		},
		AutoCompact: api.CompactConfig{Enabled: false},
	}
	rt, err := api.New(context.Background(), sanityOpts)
	if err != nil {
		t.Fatalf("create sanity main runtime: %v", err)
	}
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	enableCache := true
	if _, err := rt.Run(ctx, api.Request{
		SessionID:         sessionID,
		Prompt:            "请连续派发两个完全相同的子任务。",
		EnablePromptCache: &enableCache,
	}); err != nil {
		t.Fatalf("sanity main run: %v", err)
	}
	sanity := rec.snapshot()
	if len(sanity) < 2 {
		t.Fatalf("sanity case recorded %d sub calls, want >= 2", len(sanity))
	}
	report("D-sanity", sanity)
	second := sanity[1]
	if second.usage.CacheReadTokens == 0 && second.usage.CacheCreationTokens == 0 {
		t.Errorf("第二次相同前缀请求既无 cacheRead 也无 cacheCreate（input=%d）：provider 未回报缓存指标，本环境的测量结果不可用于缓存行为分析",
			second.usage.InputTokens)
	}
	if second.usage.CacheReadTokens == 0 {
		t.Errorf("第二次相同前缀请求 cacheRead=0（cacheCreate=%d）：未观察到前缀缓存命中，harness 或 provider 配置需排查",
			second.usage.CacheCreationTokens)
	}
}
