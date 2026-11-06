package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// 记忆系统常量（对齐 comate memory_store.py / memory_runtime.py）。
const (
	memoryMaxScanFileCount       = 200 // 每个来源按 mtime 倒序最多扫描的文件数
	memoryMaxFrontmatterLines    = 30  // frontmatter 只读前 30 行
	memoryMaxSurfacedBytes       = 4096
	memoryMaxLines               = 200
	memoryMaxSelected            = 5     // 每回合最多注入的条数
	memorySessionBudgetBytes     = 60000 // 同会话累计注入上限
	memoryFreshnessThresholdDays = 2     // auto 来源超过该天龄附加过期警示
)

// memorySourceKind 区分记忆来源：auto 是 SDK 维护的记忆桶（带新鲜度警示），
// extra 是宿主挂载的只读知识目录（永不附警示）。
type memorySourceKind string

const (
	memorySourceAuto  memorySourceKind = "auto"
	memorySourceExtra memorySourceKind = "extra"
)

// memoryHeader 是一次扫描命中的一条记忆文件的元数据。
type memoryHeader struct {
	Filename    string // manifest 展示名：auto 为相对 posix 路径，extra 加 extra-{i}/ 前缀防撞
	Type        string
	Description string
	ModTime     time.Time
	Path        string // 绝对路径
	Source      memorySourceKind
}

// memoryStore 扫描 auto 目录与 extra 目录中的主题记忆文件。
type memoryStore struct {
	autoDir    string
	extraDirs  []string
	perSession bool
}

// sessionBucketDir 返回某会话的 auto 记忆目录：PerSession 时为
// <autoDir>/sessions/<sanitizePathComponent(sessionID)>（sanitizePathComponent
// 把 ':' 等非法目录字符替换为 '-'，如 dingtalk:user:123 → dingtalk-user-123；
// 不同会话 ID sanitize 后碰撞即共享桶，语义退化为共享模式，可接受），
// 否则恒为 autoDir（全局共享桶，现状行为）。
func (s *memoryStore) sessionBucketDir(sessionID string) string {
	if s == nil || !s.perSession {
		return s.autoDir
	}
	return filepath.Join(s.autoDir, "sessions", sanitizePathComponent(sessionID))
}

func (s *memoryStore) scan(sessionID string) []memoryHeader {
	if s == nil {
		return nil
	}
	headers := s.scanDir(s.sessionBucketDir(sessionID), "", memorySourceAuto)
	for i, dir := range s.extraDirs {
		headers = append(headers, s.scanDir(dir, fmt.Sprintf("extra-%d/", i), memorySourceExtra)...)
	}
	return headers
}

// scanDir 收集目录下 mtime 最新的 memoryMaxScanFileCount 个 .md 主题文件并解析
// frontmatter；MEMORY.md（大小写不敏感）是索引文件，不参与召回。
func (s *memoryStore) scanDir(dir, filenamePrefix string, source memorySourceKind) []memoryHeader {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	type candidate struct {
		path  string
		mtime time.Time
	}

	var files []candidate
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // 无权限等读侧问题：跳过该条目，绝不让扫描失败
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			return nil
		}
		if strings.EqualFold(d.Name(), "MEMORY.md") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, candidate{path: path, mtime: info.ModTime()})
		return nil
	})
	if err != nil {
		runtimeLogger.warnf("[api:memory] scan %s failed: %v", dir, err)
		return nil
	}
	if len(files) == 0 {
		return nil
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mtime.Equal(files[j].mtime) {
			return files[i].mtime.After(files[j].mtime)
		}
		return files[i].path < files[j].path
	})
	if len(files) > memoryMaxScanFileCount {
		files = files[:memoryMaxScanFileCount]
	}

	headers := make([]memoryHeader, 0, len(files))
	for _, f := range files {
		fm, ok := readMemoryFrontmatter(f.path)
		if !ok {
			continue
		}
		rel, err := filepath.Rel(dir, f.path)
		if err != nil {
			continue
		}
		headers = append(headers, memoryHeader{
			Filename:    filenamePrefix + filepath.ToSlash(rel),
			Type:        fm.Type,
			Description: fm.Description,
			ModTime:     f.mtime,
			Path:        f.path,
			Source:      source,
		})
	}
	return headers
}

// memoryFrontmatter 是召回所需的最小 frontmatter 子集。
type memoryFrontmatter struct {
	Type        string
	Description string
}

// readMemoryFrontmatter 读取并解析文件头部 frontmatter；返回 false 表示该文件
// 不能作为记忆参与召回（读失败、非 UTF-8、无有效 description、显式停用）。
func readMemoryFrontmatter(path string) (memoryFrontmatter, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		runtimeLogger.warnf("[api:memory] read %s failed: %v", path, err)
		return memoryFrontmatter{}, false
	}
	if !utf8.Valid(data) {
		runtimeLogger.warnf("[api:memory] skip non-utf8 memory file: %s", path)
		return memoryFrontmatter{}, false
	}
	return parseMemoryFrontmatter(string(data))
}

// parseMemoryFrontmatter 手写解析记忆文件的 YAML frontmatter 头（只认
// description/type/enabled 三个键，行序无关）。description 空白折叠后非空才
// 有效；type 缺省或非法值归 reference；enabled 只认显式 false/no/off/0 为停用。
func parseMemoryFrontmatter(content string) (memoryFrontmatter, bool) {
	lines := strings.Split(content, "\n")
	limit := len(lines)
	if limit > memoryMaxFrontmatterLines {
		limit = memoryMaxFrontmatterLines
	}
	if limit == 0 || strings.TrimSpace(lines[0]) != "---" {
		return memoryFrontmatter{}, false
	}

	fm := memoryFrontmatter{Type: "reference"}
	disabled := false
	for i := 1; i < limit; i++ {
		line := strings.TrimSpace(lines[i])
		if line == "---" || line == "..." {
			break
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = unquoteMemoryYAML(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "description":
			fm.Description = strings.Join(strings.Fields(value), " ")
		case "type":
			fm.Type = normalizeMemoryType(value)
		case "enabled":
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "false", "no", "off", "0":
				disabled = true
			}
		}
	}
	if disabled || fm.Description == "" {
		return memoryFrontmatter{}, false
	}
	return fm, true
}

func unquoteMemoryYAML(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func normalizeMemoryType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "user", "feedback", "project", "reference":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "reference"
	}
}

// buildMemoryManifest 渲染召回候选清单，行格式对齐 comate：
// `- [{type}] {filename} ({ISO8601 秒级 UTC Z}): {description}`。
func buildMemoryManifest(headers []memoryHeader) string {
	lines := make([]string, 0, len(headers))
	for _, h := range headers {
		lines = append(lines, fmt.Sprintf("- [%s] %s (%s): %s",
			h.Type, h.Filename, h.ModTime.UTC().Format("2006-01-02T15:04:05Z"), h.Description))
	}
	return strings.Join(lines, "\n")
}

// surfaceMemoryDocument 按存储协议截断读取单条记忆：先 200 行、再 4096 字节
// （多字节字符回退到 rune 边界）；任一截断发生即在尾部追加完整路径提示。
func surfaceMemoryDocument(absPath, displayPosixPath string) (string, bool, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", false, fmt.Errorf("api: read memory %s: %w", absPath, err)
	}
	if !utf8.Valid(data) {
		return "", false, fmt.Errorf("api: memory %s is not valid UTF-8", absPath)
	}
	text := string(data)
	truncated := false

	lines := strings.Split(text, "\n")
	if len(lines) > memoryMaxLines {
		text = strings.Join(lines[:memoryMaxLines], "\n")
		truncated = true
	}
	if len(text) > memoryMaxSurfacedBytes {
		cut := text[:memoryMaxSurfacedBytes]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		text = cut
		truncated = true
	}
	if truncated {
		text += fmt.Sprintf("\n\n> This memory file was truncated. Read the complete file at: %s", displayPosixPath)
	}
	return text, truncated, nil
}

// memoryBucketName 把 ProjectRoot 绝对路径规范化为记忆桶名：整体小写、
// `\` `/` `:` 替换为 `-`，保留 CJK（对齐 comate 桶名算法）。
func memoryBucketName(projectRoot string) string {
	lowered := strings.ToLower(projectRoot)
	return strings.NewReplacer("\\", "-", "/", "-", ":", "-").Replace(lowered)
}

// memoryHomeDir 解析用户主目录：HOME 优先、os.UserHomeDir 兜底（与
// settings_loader 的解析顺序一致，保证测试的 HOME 隔离在 Windows 上同样生效）。
func memoryHomeDir() (string, bool) {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home, true
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", false
	}
	return home, true
}

func defaultMemoryDir(home, projectRoot string) string {
	root := projectRoot
	if abs, err := filepathAbs(root); err == nil {
		root = abs
	}
	return filepath.Join(home, ".agents", "memory", memoryBucketName(root))
}
