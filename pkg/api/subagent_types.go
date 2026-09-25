package api

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lzq70112/agentsdk-go/pkg/runtime/skills"
	"gopkg.in/yaml.v3"
)

// AgentType 描述一种预定义的 subagent 类型。主 agent 派发时通过 type 参数选择；
// 未选择或名称未注册时回退默认通用型（全量继承主 runtime 配置）。
type AgentType struct {
	// Name 是类型的唯一标识，主 agent 在 type 参数中用它选择类型。
	Name string
	// Description 说明该类型的用途，注入 tool 描述供主 agent 按任务性质匹配。
	Description string
	// AllowedTools 当前不再收窄子 agent 的工具面：子 runtime 的工具集与主
	// runtime 逐字节一致（保 KV/prefix cache），字段仅为兼容与规划保留，
	// 对子请求的工具集没有影响。
	AllowedTools []string
	// AppendPrompt 追加到子任务消息末尾的提示词，用于给该类型的子 agent 补充
	// 工作指引。注入只发生在 history 末尾消息，不改子 runtime 的 system prompt
	// 本体，以保持与主 runtime 的请求前缀一致（prefix cache 契约）。
	AppendPrompt string
}

// lookupAgentType 按名称查找已注册类型；名称为空或未找到时返回零值（默认通用型）。
func (t *subagentTool) lookupAgentType(name string) AgentType {
	name = strings.TrimSpace(name)
	if name == "" {
		return AgentType{}
	}
	for _, at := range t.opts.AgentTypes {
		if strings.EqualFold(strings.TrimSpace(at.Name), name) {
			return at
		}
	}
	return AgentType{}
}

// agentTypeFileMetadata mirrors the YAML frontmatter fields of an agent type file.
type agentTypeFileMetadata struct {
	Name         string          `yaml:"name"`
	Description  string          `yaml:"description"`
	AllowedTools skills.ToolList `yaml:"allowed-tools,omitempty"`
}

// parseAgentTypeFile 解析单个类型定义文件：frontmatter 元数据 + 正文（作为
// AppendPrompt 注入子任务消息末尾）。
func parseAgentTypeFile(content string) (AgentType, error) {
	trimmed := strings.TrimPrefix(content, "\uFEFF")
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return AgentType{}, errors.New("missing YAML frontmatter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return AgentType{}, errors.New("missing closing frontmatter separator")
	}

	var meta agentTypeFileMetadata
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &meta); err != nil {
		return AgentType{}, fmt.Errorf("decode YAML: %w", err)
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		return AgentType{}, errors.New("name is required")
	}
	desc := strings.TrimSpace(meta.Description)
	if desc == "" {
		return AgentType{}, errors.New("description is required")
	}

	return AgentType{
		Name:         name,
		Description:  desc,
		AllowedTools: []string(meta.AllowedTools),
		AppendPrompt: strings.TrimSpace(strings.Join(lines[end+1:], "\n")),
	}, nil
}

// loadAgentTypesFromDir 从 <root>/.agents/subagents/ 加载 *.md 类型定义文件。
// 单个文件读取或解析失败只告警跳过，不影响其他类型；目录不存在时返回 nil。
func loadAgentTypesFromDir(root string) []AgentType {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	dir := filepath.Join(root, ".agents", "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			runtimeLogger.warnf("[subagent] 读取类型定义目录失败 dir=%s: %v", dir, err)
		}
		return nil
	}

	var types []AgentType
	seen := map[string]struct{}{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			runtimeLogger.warnf("[subagent] 读取类型定义文件失败 path=%s: %v", path, err)
			continue
		}
		at, err := parseAgentTypeFile(string(data))
		if err != nil {
			runtimeLogger.warnf("[subagent] 跳过无效的类型定义文件 path=%s: %v", path, err)
			continue
		}
		key := strings.ToLower(at.Name)
		if _, dup := seen[key]; dup {
			runtimeLogger.warnf("[subagent] 跳过重复的类型定义文件 path=%s name=%s", path, at.Name)
			continue
		}
		seen[key] = struct{}{}
		types = append(types, at)
	}
	return types
}

// mergeAgentTypes 合并文件类型与编程注册类型；同名（大小写不敏感）时编程注册优先——
// 嵌入场景的显式配置应覆盖项目目录里的残留文件。
func mergeAgentTypes(fileTypes, programmatic []AgentType) []AgentType {
	if len(fileTypes) == 0 {
		return programmatic
	}
	override := make(map[string]struct{}, len(programmatic))
	for _, at := range programmatic {
		override[strings.ToLower(strings.TrimSpace(at.Name))] = struct{}{}
	}
	merged := make([]AgentType, 0, len(fileTypes)+len(programmatic))
	for _, at := range fileTypes {
		if _, ok := override[strings.ToLower(strings.TrimSpace(at.Name))]; ok {
			continue
		}
		merged = append(merged, at)
	}
	return append(merged, programmatic...)
}
