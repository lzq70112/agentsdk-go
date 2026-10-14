package toolbuiltin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/lzq70112/agentsdk-go/pkg/gitignore"
	"github.com/lzq70112/agentsdk-go/pkg/sandbox"
	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

const (
	globResultLimit = 100
	// globToolDesc 对齐 Claude Code/Comate 的定位：Glob 是文件系统发现工具，
	// 不做 gitignore 语义过滤（agent 需要找到构建产物、.env、被忽略的文档）。
	globToolDesc = `
		Match file paths using glob patterns (e.g. \"**/*.js\", \"src/**/*.ts\").
		Results are relative to the sandbox root, sorted by modification time, and limited to 100 entries.
		Only files are returned; directories are not listed. Gitignored paths are included by default: .gitignore is a VCS concept and does not constrain file discovery.
		Version-control metadata directories (.git, .hg, .svn, .bzr, .jj, .sl) are always skipped.
	`
)

var globSchema = &tool.JSONSchema{
	Type: "object",
	Properties: map[string]interface{}{
		"pattern": map[string]interface{}{
			"type":        "string",
			"description": "The glob pattern to match files against",
		},
		"path": map[string]interface{}{
			"type":        "string",
			"description": "The directory to search in. If not specified, the current working directory will be used. IMPORTANT: Omit this field to use the default directory. DO NOT enter \"undefined\" or \"null\" - simply omit it for the default behavior. Must be a valid directory path if provided.",
		},
	},
	Required: []string{"pattern"},
}

// excludedVCSDirs 是遍历层硬剪枝的版本控制元数据目录：无文件发现价值且
// 体量巨大（.git 对象库），任何 pattern 下都不进入（对齐 Comate 的
// _SEARCH_EXCLUDED_DIRS）。
var excludedVCSDirs = []string{".bzr", ".git", ".hg", ".jj", ".sl", ".svn"}

var excludedVCSDirsSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(excludedVCSDirs))
	for _, name := range excludedVCSDirs {
		set[name] = struct{}{}
	}
	return set
}()

// GlobTool looks up files via glob patterns.
type GlobTool struct {
	policy           sandbox.FileSystemPolicy
	root             string
	maxResults       int
	respectGitignore bool
	gitignoreMatcher *gitignore.Matcher
}

// NewGlobTool builds a GlobTool rooted at the current directory.
func NewGlobTool() *GlobTool { return NewGlobToolWithRoot("") }

// NewGlobToolWithRoot builds a GlobTool rooted at the provided directory.
func NewGlobToolWithRoot(root string) *GlobTool {
	resolved := resolveRoot(root)
	return &GlobTool{
		policy:           sandbox.NewFileSystemAllowList(resolved),
		root:             resolved,
		maxResults:       globResultLimit,
		respectGitignore: false, // 对齐 Claude Code/Comate：gitignore 不约束文件发现
	}
}

// NewGlobToolWithSandbox builds a GlobTool using a custom sandbox.
// Used when sandbox needs to be pre-configured (e.g., disabled mode).
func NewGlobToolWithSandbox(root string, policy sandbox.FileSystemPolicy) *GlobTool {
	resolved := resolveRoot(root)
	return &GlobTool{
		policy:           policy,
		root:             resolved,
		maxResults:       globResultLimit,
		respectGitignore: false,
	}
}

// SetRespectGitignore configures whether the tool should respect .gitignore patterns.
// Off by default (file discovery should see gitignored paths); hosts may opt in.
func (g *GlobTool) SetRespectGitignore(respect bool) {
	g.respectGitignore = respect
	if respect && g.gitignoreMatcher == nil {
		g.gitignoreMatcher, _ = gitignore.NewMatcher(g.root) //nolint:errcheck // best-effort gitignore
	}
}

func (g *GlobTool) Name() string { return "glob" }

func (g *GlobTool) Description() string { return globToolDesc }

func (g *GlobTool) Schema() *tool.JSONSchema { return globSchema }

func (g *GlobTool) Metadata() tool.Metadata {
	return tool.Metadata{IsReadOnly: true, IsConcurrencySafe: true}
}

func (g *GlobTool) Execute(ctx context.Context, params map[string]interface{}) (*tool.ToolResult, error) {
	if ctx == nil {
		return nil, errors.New("context is nil")
	}
	if g == nil {
		return nil, errors.New("glob tool is not initialised")
	}

	pattern, err := parseGlobPattern(params)
	if err != nil {
		return nil, err
	}
	dir, err := g.resolveDir(params)
	if err != nil {
		return nil, err
	}
	relPattern, err := normalizeGlobPattern(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := validateGlobPatternParent(g.policy, dir, relPattern); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Initialize gitignore matcher lazily if needed
	if g.respectGitignore && g.gitignoreMatcher == nil {
		g.gitignoreMatcher, _ = gitignore.NewMatcher(g.root) //nolint:errcheck // best-effort gitignore
	}

	matches, hiddenByGitignore, err := g.collectMatches(dir, relPattern)
	if err != nil {
		return nil, err
	}

	sortGlobMatches(matches)

	truncated := len(matches) > g.maxResults
	if truncated {
		matches = matches[:g.maxResults]
	}

	results := make([]string, 0, len(matches))
	for _, match := range matches {
		results = append(results, displayPath(match, g.root))
	}

	return &tool.ToolResult{
		Success: true,
		Output:  formatGlobOutput(results, truncated, hiddenByGitignore),
		Data: map[string]interface{}{
			"pattern":            pattern,
			"path":               displayPath(dir, g.root),
			"matches":            results,
			"count":              len(results),
			"truncated":          truncated,
			"ignore_files":       ignorePolicyLabel(g.respectGitignore),
			"excluded_dirs":      excludedVCSDirs,
			"hidden_by_gitignore": hiddenByGitignore,
		},
	}, nil
}

// normalizeGlobPattern 把用户 pattern 规范化为相对 search dir 的 / 分隔形式：
// 绝对路径转相对（逃逸 sandbox 即报错）、Clean 去掉 ./ 冗余。
func normalizeGlobPattern(dir, pattern string) (string, error) {
	cleaned := filepath.Clean(pattern)
	if filepath.IsAbs(cleaned) {
		rel, err := filepath.Rel(dir, cleaned)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("pattern %q escapes the sandbox root", pattern)
		}
		cleaned = rel
	} else if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("pattern %q escapes the sandbox root", pattern)
	}
	return filepath.ToSlash(cleaned), nil
}

// validateGlobPatternParent 沿用原 combinePattern 的沙箱前置校验：pattern 的
// 无通配目录前缀必须落在 sandbox 内；前缀含通配时交给遍历期逐文件校验。
func validateGlobPatternParent(policy sandbox.FileSystemPolicy, dir, relPattern string) error {
	if policy == nil {
		return nil
	}
	prefix := ""
	if idx := strings.LastIndex(relPattern, "/"); idx >= 0 {
		prefix = relPattern[:idx]
	}
	if strings.ContainsAny(prefix, "*?[") {
		// 目录段带通配：无法静态定位前缀，遍历期逐文件校验兜底。
		return nil
	}
	abs := filepath.Join(dir, filepath.FromSlash(prefix))
	return policy.Validate(filepath.Clean(abs))
}

// vcsPruningFS 在 ReadDir 层滤掉 VCS 元数据目录：GlobWalk 通过 ReadDir 列举
// 目录，包装后这些目录对遍历完全不可见——天然剪枝（不进入 .git 等），且不
// 依赖回调返回 SkipDir（GlobWalk 的 leaf 层对 SkipDir 会终止整层遍历，有
// 漏掉同层兄弟 entry 的误伤）。
type vcsPruningFS struct {
	fs.ReadDirFS
}

func (f vcsPruningFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := f.ReadDirFS.ReadDir(name)
	if err != nil {
		return nil, err
	}
	filtered := entries[:0]
	for _, entry := range entries {
		if entry.IsDir() {
			if _, banned := excludedVCSDirsSet[entry.Name()]; banned {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	return filtered, nil
}

// collectMatches 用 doublestar.GlobWalk 遍历收集匹配文件（仅文件，对齐
// rg --files / Claude Code：目录不进结果）；gitignore 过滤开启时统计被隐藏数。
func (g *GlobTool) collectMatches(dir, relPattern string) ([]string, int, error) {
	base := os.DirFS(dir)
	var fsys fs.FS = base
	if rdr, ok := base.(fs.ReadDirFS); ok {
		fsys = vcsPruningFS{ReadDirFS: rdr}
	}

	var matches []string
	hidden := 0

	walkErr := doublestar.GlobWalk(fsys, relPattern, func(p string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		// 双保险：ReadDir 剪枝挡住了常规路径，这里兜底 pattern 显式指名
		// VCS 目录的场景（如 ".git/config"）。
		for _, seg := range strings.Split(p, "/") {
			if _, banned := excludedVCSDirsSet[seg]; banned {
				return nil
			}
		}
		abs := filepath.Join(dir, filepath.FromSlash(p))
		if g.policy != nil {
			if err := g.policy.Validate(abs); err != nil {
				return err
			}
		}
		if g.respectGitignore && g.gitignoreMatcher != nil {
			relPath := displayPath(abs, g.root)
			if g.gitignoreMatcher.Match(relPath, false) {
				hidden++
				return nil
			}
		}
		matches = append(matches, abs)
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, doublestar.ErrBadPattern) {
			return nil, 0, fmt.Errorf("glob failed: %w", walkErr)
		}
		return nil, 0, walkErr
	}
	return matches, hidden, nil
}

// sortGlobMatches 按修改时间升序排列（rg --sort=modified 语义），同刻按路径
// 稳定。stat 失败视作 0（最旧），与 Comate 的 _glob_sort_key 一致。
func sortGlobMatches(matches []string) {
	mtimes := make(map[string]int64, len(matches))
	for _, path := range matches {
		mtimes[path] = 0
		if info, err := os.Stat(path); err == nil {
			mtimes[path] = info.ModTime().UnixNano()
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		mi, mj := mtimes[matches[i]], mtimes[matches[j]]
		if mi != mj {
			return mi < mj
		}
		return matches[i] < matches[j]
	})
}

func ignorePolicyLabel(respectGitignore bool) string {
	if respectGitignore {
		return "respected"
	}
	return "bypassed"
}

func parseGlobPattern(params map[string]interface{}) (string, error) {
	if params == nil {
		return "", errors.New("params is nil")
	}
	raw, ok := params["pattern"]
	if !ok {
		return "", errors.New("pattern is required")
	}
	pattern, err := coerceString(raw)
	if err != nil {
		return "", fmt.Errorf("pattern must be string: %w", err)
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", errors.New("pattern cannot be empty")
	}
	return pattern, nil
}

func (g *GlobTool) resolveDir(params map[string]interface{}) (string, error) {
	dir := g.root
	if params != nil {
		if raw, ok := params["path"]; ok && raw != nil {
			value, err := coerceString(raw)
			if err != nil {
				return "", fmt.Errorf("path must be string: %w", err)
			}
			value = strings.TrimSpace(value)
			if value != "" {
				dir = value
			}
		}
	}

	if !filepath.IsAbs(dir) {
		dir = filepath.Join(g.root, dir)
	}
	dir = filepath.Clean(dir)
	if g.policy != nil {
		if err := g.policy.Validate(dir); err != nil {
			return "", err
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("stat dir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return dir, nil
}

func formatGlobOutput(matches []string, truncated bool, hiddenByGitignore int) string {
	if len(matches) == 0 {
		if hiddenByGitignore > 0 {
			return fmt.Sprintf("no matches (%d paths hidden by .gitignore)", hiddenByGitignore)
		}
		return "no matches"
	}
	output := strings.Join(matches, "\n")
	if truncated {
		output += fmt.Sprintf("\n... truncated to %d results", len(matches))
	}
	return output
}

func displayPath(path, root string) string {
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	if rel, err := filepath.Rel(cleanRoot, cleanPath); err == nil {
		switch {
		case rel == ".":
			return "."
		case strings.HasPrefix(rel, ".."):
			// Path escaped root; fall back to absolute path.
		default:
			return rel
		}
	}
	return cleanPath
}
