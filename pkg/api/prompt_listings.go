package api

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lzq70112/agentsdk-go/pkg/runtime/skills"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/subagents"
)

// buildSkillsListing 将已注册技能渲染为 "# Available skills" 清单段。
// 返回空串表示无技能、不应添加该段。
func buildSkillsListing(reg *skills.Registry) string {
	if reg == nil {
		return ""
	}
	defs := reg.List()
	if len(defs) == 0 {
		return ""
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	var b strings.Builder
	b.WriteString("# Available skills\n\n")
	b.WriteString("Skills are packaged instructions for specific kinds of tasks. When the task at hand matches one of the descriptions below, or the user names one, invoke it via the Skill tool before answering:\n\n")
	for _, def := range defs {
		b.WriteString(fmt.Sprintf("- %s: %s\n", def.Name, def.Description))
	}
	return b.String()
}

// buildSubagentsListing 将已注册子 agent 类型渲染为 "# Available agent types" 清单段。
// 返回空串表示无子 agent 类型、不应添加该段。
func buildSubagentsListing(mgr *subagents.Manager) string {
	if mgr == nil {
		return ""
	}
	defs := mgr.List()
	if len(defs) == 0 {
		return ""
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	var b strings.Builder
	b.WriteString("# Available agent types\n\n")
	b.WriteString("Specialized agent types available via the Agent tool. Each starts fresh with its own context; brief it with a complete, self-contained task description:\n\n")
	for _, def := range defs {
		b.WriteString(fmt.Sprintf("- %s: %s\n", def.Name, def.Description))
	}
	return b.String()
}

// buildMCPInstructionsListing 渲染已配置的 MCP 服务器清单与工具命名约定。
// mcpServer 尚未接入协议层 server instructions，这里只报告清单与命名约定。
func buildMCPInstructionsListing(servers []mcpServer) string {
	if len(servers) == 0 {
		return ""
	}
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		if strings.TrimSpace(s.Name) != "" {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# MCP servers\n\n")
	b.WriteString("The following MCP servers are configured. Their tools appear in the tool list named mcp__<server>__<tool>; prefer a listed MCP tool over the generic fallback fetcher when one covers the URL:\n\n")
	for _, name := range names {
		b.WriteString(fmt.Sprintf("- %s\n", name))
	}
	return b.String()
}
