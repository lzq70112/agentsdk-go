package api

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// memoryWriteGuard 在 PreToolUse 内部拦截对记忆目录的写操作（SafetyCheck 之后、
// 用户 hook 之前）：auto 记忆目录需通过 secret 扫描后放行（免审批）；extra 只读
// 来源一律拒绝；其余路径行为完全不变。
type memoryWriteGuard struct {
	autoDir     string
	extraDirs   []string
	sandboxRoot string // 相对 file_path 的解析基准（与工具侧 fileSandbox.root 同源）
	// strict 抽取子代理专用模式（spec §7.1 工具校验）：write/edit 只允许落
	// autoDir 内的主题 .md——拒 MEMORY.md、拒目录外、拒非 .md。
	strict bool
}

// memorySecretPatterns 检出即 deny；错误信息只报规则名，绝不回显密钥明文。
var memorySecretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"aws_access_key_id", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"openai_api_key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)},
	{"github_token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`)},
	{"slack_token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"private_key_block", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{"credential_assignment", regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password)\b\s*[:=]\s*["']?[A-Za-z0-9+/=_-]{32,}`)},
}

// preToolUse 对 write/edit 且目标落在记忆目录内的调用做放行/拒绝判定；
// 其余情况返回 nil（行为不变）。
func (g *memoryWriteGuard) preToolUse(toolName string, params map[string]any) error {
	if g == nil {
		return nil
	}
	switch canonicalToolName(toolName) {
	case "write", "edit":
	default:
		return nil
	}
	target, err := g.resolveTarget(params)
	if err != nil {
		return nil // 解析不了目标（缺参数等）：交由工具自身的参数校验报错
	}
	for _, dir := range g.extraDirs {
		if withinDir(dir, target) {
			return fmt.Errorf("%w: memory source directory is read-only (user-authored extra source): %s",
				ErrToolUseDenied, filepath.ToSlash(target))
		}
	}
	if !withinDir(g.autoDir, target) {
		if g.strict {
			return fmt.Errorf("%w: extraction subagent may only write topic .md files inside the memory directory: %s",
				ErrToolUseDenied, filepath.ToSlash(target))
		}
		return nil
	}
	if g.strict {
		if strings.EqualFold(filepath.Base(target), "memory.md") {
			return fmt.Errorf("%w: MEMORY.md is the index file and must not be modified by extraction: %s",
				ErrToolUseDenied, filepath.ToSlash(target))
		}
		if !strings.EqualFold(filepath.Ext(target), ".md") {
			return fmt.Errorf("%w: extraction subagent may only write topic .md files: %s",
				ErrToolUseDenied, filepath.ToSlash(target))
		}
	}
	return g.scanSecrets(toolName, params)
}

// resolveTarget 解析 write/edit 的 file_path：与工具侧一致——绝对路径直接
// Clean，相对路径相对沙箱根。
func (g *memoryWriteGuard) resolveTarget(params map[string]any) (string, error) {
	raw, ok := params["file_path"]
	if !ok {
		return "", fmt.Errorf("file_path is required")
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("file_path must be a string")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("file_path cannot be empty")
	}
	if !filepath.IsAbs(s) {
		root := g.sandboxRoot
		if strings.TrimSpace(root) == "" {
			root = "."
		}
		s = filepath.Join(root, s)
	}
	return filepath.Clean(s), nil
}

func (g *memoryWriteGuard) scanSecrets(toolName string, params map[string]any) error {
	var contents []string
	switch canonicalToolName(toolName) {
	case "write":
		contents = append(contents, paramString(params, "content"))
	case "edit":
		contents = append(contents, paramString(params, "new_string"))
	}
	for _, content := range contents {
		if content == "" {
			continue
		}
		for _, p := range memorySecretPatterns {
			if p.re.MatchString(content) {
				return fmt.Errorf("%w: potential secret detected in memory write (rule: %s); remove the secret before saving to memory",
					ErrToolUseDenied, p.name)
			}
		}
	}
	return nil
}

func paramString(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	if s, ok := params[key].(string); ok {
		return s
	}
	return ""
}

// withinDir 判断 path 是否落在 root 内（与 pkg/sandbox 的 within 同构：
// Clean + 分隔符后缀前缀匹配）。
func withinDir(root, path string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(root, sep) {
		root += sep
	}
	return strings.HasPrefix(path+sep, root)
}
