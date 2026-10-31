package api

import (
	_ "embed"
	"path/filepath"
	"strings"
)

// memoryUsagePromptTemplate 移植自 comate prompts/agent/memory_usage.md（逐字），
// 渲染时 {{MEMORY_DIR_PATH}} 替换为记忆目录绝对路径。提示词内含 markdown 围栏
// （反引号），无法用反引号字符串常量承载，故采用 go:embed（trace_template 先例）。
//
//go:embed prompts/memory_usage.md
var memoryUsagePromptTemplate string

// renderMemoryUsagePrompt 渲染常驻记忆使用说明 section。
func renderMemoryUsagePrompt(memoryDir string) string {
	return strings.ReplaceAll(memoryUsagePromptTemplate, "{{MEMORY_DIR_PATH}}", filepath.ToSlash(memoryDir))
}
