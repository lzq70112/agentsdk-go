package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	_ "embed"

	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/model"
)

// memorySelectRelevantPrompt 是 LLM 选择器的 system prompt（comate
// prompts/memory/select_relevant.md 逐字移植）。
//
//go:embed prompts/select_relevant.md
var memorySelectRelevantPrompt string

// memoryLLMSelector 是 memorySelector 的二期扩展：LLM 选择器需要回合上下文
// （ctx 与近期工具名），异步预取管线按此接口识别并调用；keywordSelector 的
// 一期同步语义（memorySelector）保持不变。
type memoryLLMSelector interface {
	SelectMemory(ctx context.Context, query string, candidates []memoryHeader, recentTools string) []memoryHeader
}

// llmSelector 用低档模型从 manifest 中挑选与查询相关的记忆（comate side_query
// 等价物）：单次 Complete 调用，不进主历史；响应解析失败或为空时静默返回空
// （本回合不注入，不做降级重试）。
type llmSelector struct {
	complete func(ctx context.Context, req model.Request) (*model.Response, error)
}

// newLLMSelector 从 Options 解析选择模型：仅当 ModelPool[SelectorTier] 存在时
// 启用 LLM 选择器（宿主显式为选择器配置了低档模型即明确意图）；无池返回 nil，
// 召回退回一期 keywordSelector。与 spec §3"无池则主模型"的偏离见实现报告：
// 无池时用主模型做选择器会在每回合多一次主模型调用，且使注入成败依赖竞态，
// 保守退回确定性选择器。
func newLLMSelector(opts Options) *llmSelector {
	var tier ModelTier
	if opts.Memory != nil {
		tier = opts.Memory.SelectorTier
	}
	if tier == "" {
		return nil
	}
	m, ok := opts.ModelPool[tier]
	if !ok || m == nil {
		return nil
	}
	return &llmSelector{complete: m.Complete}
}

// Select 实现 memorySelector 一期接口：同步直调（无回合 ctx、无近期工具段）。
// 生产召回路径走 memoryLLMSelector 的异步预取（beginMemoryRecall/
// consumeMemoryRecall），此方法用于接口满足性与同步场景。
func (s *llmSelector) Select(query string, candidates []memoryHeader) []memoryHeader {
	return s.SelectMemory(context.Background(), query, candidates, "")
}

// memoryRecentToolsLimit 是 user 消息中"近期工具"的最大条数（comate 同款）。
const memoryRecentToolsLimit = 8

// SelectMemory 渲染 user 消息、调用选择模型并按 Filename 回选。
// recentTools 是预拼好的 "Recently used tools: ..." 段（空串表示无）。
func (s *llmSelector) SelectMemory(ctx context.Context, query string, candidates []memoryHeader, recentTools string) []memoryHeader {
	if s == nil || s.complete == nil || len(candidates) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	user := fmt.Sprintf("Query:\n%s\n\nAvailable memories:\n%s%s",
		query, buildMemoryManifest(candidates), recentTools)
	req := model.Request{
		Messages: []model.Message{{Role: "user", Content: user}},
		System:   memorySelectRelevantPrompt,
	}
	resp, err := s.complete(ctx, req)
	if err != nil {
		runtimeLogger.warnf("[api:memory] llm selector complete failed: %v", err)
		return nil
	}
	names := parseMemorySelectorResponse(modelResponseText(resp))
	if len(names) == 0 {
		return nil
	}
	return selectMemoryHeadersByFilename(candidates, names)
}

// collectRecentToolNames 从历史倒序收集最近 limit 个去重工具名（assistant 与
// tool 消息的 ToolCalls 都算；comate memory_runtime 同款）。
func collectRecentToolNames(msgs []message.Message, limit int) []string {
	if limit <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for i := len(msgs) - 1; i >= 0 && len(out) < limit; i-- {
		for _, tc := range msgs[i].ToolCalls {
			name := strings.TrimSpace(tc.Name)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// memoryRecentToolsSection 拼接 user 消息的近期工具段：无工具时为空串。
func memoryRecentToolsSection(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	return "\n\nRecently used tools: " + strings.Join(tools, ", ")
}

func modelResponseText(resp *model.Response) string {
	if resp == nil {
		return ""
	}
	return strings.TrimSpace(resp.Message.Content)
}

// parseMemorySelectorResponse 宽松解析选择模型响应：容忍代码围栏与前后杂文，
// 取首个 '{' 到最后一个 '}' 之间解析 {"selected_memories": [...]}；截前
// memoryMaxSelected 个非空名。解析失败返回 nil（调用侧静默跳过注入）。
func parseMemorySelectorResponse(text string) []string {
	s := strings.TrimSpace(text)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil
	}
	var parsed struct {
		SelectedMemories []string `json:"selected_memories"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &parsed); err != nil {
		return nil
	}
	out := make([]string, 0, len(parsed.SelectedMemories))
	for _, name := range parsed.SelectedMemories {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, name)
		if len(out) >= memoryMaxSelected {
			break
		}
	}
	return out
}

// selectMemoryHeadersByFilename 按模型返回的文件名顺序回选候选，未命中忽略、
// 重名去重。
func selectMemoryHeadersByFilename(candidates []memoryHeader, names []string) []memoryHeader {
	byName := make(map[string]memoryHeader, len(candidates))
	for _, c := range candidates {
		byName[c.Filename] = c
	}
	seen := map[string]struct{}{}
	out := make([]memoryHeader, 0, len(names))
	for _, name := range names {
		h, ok := byName[name]
		if !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, h)
	}
	return out
}
