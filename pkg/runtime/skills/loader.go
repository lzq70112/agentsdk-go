package skills

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/config"
	"gopkg.in/yaml.v3"
)

type fileOps struct {
	readFile func(string) ([]byte, error)
	openFile func(string) (fs.File, error)
	statFile func(string) (fs.FileInfo, error)
}

var (
	fileOpOverridesMu sync.RWMutex
	fileOpOverrides   = struct {
		read func(string) ([]byte, error)
		stat func(string) (fs.FileInfo, error)
	}{}
)

func readFileOverrideOrOS(path string) ([]byte, error) {
	fileOpOverridesMu.RLock()
	override := fileOpOverrides.read
	fileOpOverridesMu.RUnlock()
	if override != nil {
		return override(path)
	}
	return os.ReadFile(path)
}

func statFileOverrideOrOS(path string) (fs.FileInfo, error) {
	fileOpOverridesMu.RLock()
	override := fileOpOverrides.stat
	fileOpOverridesMu.RUnlock()
	if override != nil {
		return override(path)
	}
	return os.Stat(path)
}

type LoaderOptions struct {
	ProjectRoot string
	UserHome    string
	EnableUser  bool
	FS          *config.FS
	EmbedFS     fs.FS
}

type SkillFile struct {
	Name     string
	Path     string
	Metadata SkillMetadata
	fs       *config.FS
}

var readFile = os.ReadFile

type ToolList []string

func (t *ToolList) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Tag == "!!null" {
		*t = nil
		return nil
	}

	var tools []string
	switch value.Kind {
	case yaml.ScalarNode:
		for _, entry := range strings.Split(value.Value, ",") {
			tool := strings.TrimSpace(entry)
			if tool != "" {
				tools = append(tools, tool)
			}
		}
	case yaml.SequenceNode:
		for i, entry := range value.Content {
			if entry.Kind != yaml.ScalarNode {
				return fmt.Errorf("allowed-tools[%d]: expected string", i)
			}
			tool := strings.TrimSpace(entry.Value)
			if tool != "" {
				tools = append(tools, tool)
			}
		}
	default:
		return errors.New("allowed-tools: expected string or sequence")
	}

	seen := map[string]struct{}{}
	deduped := tools[:0]
	for _, tool := range tools {
		if _, ok := seen[tool]; ok {
			continue
		}
		seen[tool] = struct{}{}
		deduped = append(deduped, tool)
	}

	if len(deduped) == 0 {
		*t = nil
		return nil
	}
	*t = ToolList(deduped)
	return nil
}

type SkillMetadata struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	WhenToUse     string            `yaml:"when_to_use,omitempty"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty"`
	AllowedTools  ToolList          `yaml:"allowed-tools,omitempty"`
	Paths         []string          `yaml:"paths,omitempty"`
}

type SkillRegistration struct {
	Definition Definition
	Handler    Handler
}

// Skill names must be 1-64 characters, lowercase alphanumeric plus hyphens, and
// cannot start or end with a hyphen.
var skillNameRegexp = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

func isValidSkillName(name string) bool {
	return skillNameRegexp.MatchString(strings.TrimSpace(name))
}

// LoadFromFS loads skills from the filesystem. Errors are aggregated so one
// broken file will not block others. Duplicate names are skipped with a
// warning entry in the error list.
func LoadFromFS(opts LoaderOptions) ([]SkillRegistration, []error) {
	var (
		registrations []SkillRegistration
		errs          []error
		allFiles      []SkillFile
	)

	projectFS := opts.FS
	if projectFS == nil {
		projectFS = config.NewFS(opts.ProjectRoot, nil)
	}

	agentsDir := filepath.Join(opts.ProjectRoot, ".agents", "skills")
	files, loadErrs := loadSkillDirFn(agentsDir, projectFS)
	errs = append(errs, loadErrs...)
	allFiles = append(allFiles, files...)
	if opts.EmbedFS != nil {
		embedFiles, embedErrs := loadEmbeddedSkillDir(opts.ProjectRoot, opts.EmbedFS)
		errs = append(errs, embedErrs...)
		allFiles = append(allFiles, embedFiles...)
	}

	if len(allFiles) == 0 {
		return nil, errs
	}

	seen := map[string]string{}
	for _, file := range allFiles {
		if prev, ok := seen[file.Metadata.Name]; ok {
			errs = append(errs, fmt.Errorf("skills: duplicate skill %q at %s (already from %s)", file.Metadata.Name, file.Path, prev))
			continue
		}
		seen[file.Metadata.Name] = file.Path

		registrations = append(registrations, skillFileToRegistration(file))
	}
	sort.Slice(registrations, func(i, j int) bool {
		return registrations[i].Definition.Name < registrations[j].Definition.Name
	})

	return registrations, errs
}

// skillFileToRegistration converts a parsed SkillFile into a Definition +
// Handler pair. Shared by the bulk LoadFromFS path and the single-skill
// LoadSingleSkill path so both produce identical registrations.
func skillFileToRegistration(file SkillFile) SkillRegistration {
	def := Definition{
		Name:        file.Metadata.Name,
		Description: skillDisplayDescription(file.Metadata),
		Metadata:    buildDefinitionMetadata(file),
	}
	if len(file.Metadata.Paths) > 0 {
		def.Matchers = append(def.Matchers, PathMatcher{Patterns: append([]string(nil), file.Metadata.Paths...)})
	}
	return SkillRegistration{
		Definition: def,
		Handler:    buildHandler(file, resolveFileOps(file.fs)),
	}
}

// LoadSingleSkill loads one skill from a directory containing SKILL.md.
// skillDir must be the skill directory (e.g. ".agents/skills/my-skill"),
// not the SKILL.md file path. Used by Runtime.ReloadSkill to re-read a
// single modified skill at runtime without re-scanning the whole tree.
func LoadSingleSkill(skillDir string, fsLayer *config.FS) (SkillRegistration, error) {
	skillFile, err := filepath.Abs(filepath.Clean(skillDir))
	if err != nil {
		return SkillRegistration{}, fmt.Errorf("skills: resolve %s: %w", skillDir, err)
	}
	dirName := filepath.Base(skillFile)
	file, err := parseSkillFile(filepath.Join(skillFile, "SKILL.md"), dirName, fsLayer)
	if err != nil {
		return SkillRegistration{}, fmt.Errorf("skills: load single %s: %w", skillDir, err)
	}
	return skillFileToRegistration(file), nil
}

var loadSkillDirFn = loadSkillDir

func loadSkillDir(root string, fsLayer *config.FS) ([]SkillFile, []error) {
	var (
		results []SkillFile
		errs    []error
	)

	if fsLayer == nil {
		fsLayer = config.NewFS("", nil)
	}

	info, err := fsLayer.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("skills: stat %s: %w", root, err)}
	}
	if !info.IsDir() {
		return nil, []error{fmt.Errorf("skills: path %s is not a directory", root)}
	}
	if _, err := fsLayer.ReadDir(root); err != nil {
		return nil, []error{fmt.Errorf("skills: read dir %s: %w", root, err)}
	}

	if walkErr := fsLayer.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return fs.SkipDir
			}
			errs = append(errs, fmt.Errorf("skills: walk %s: %w", path, walkErr))
			return nil
		}
		if d == nil || d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}

		dirName := filepath.Base(filepath.Dir(path))
		file, parseErr := parseSkillFile(path, dirName, fsLayer)
		if parseErr != nil {
			if errors.Is(parseErr, fs.ErrNotExist) {
				return nil
			}
			errs = append(errs, parseErr)
			return nil
		}

		results = append(results, file)
		return nil
	}); walkErr != nil {
		errs = append(errs, fmt.Errorf("skills: walk %s: %w", root, walkErr))
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })
	return results, errs
}

func loadEmbeddedSkillDir(projectRoot string, embed fs.FS) ([]SkillFile, []error) {
	var (
		results []SkillFile
		errs    []error
	)
	if embed == nil {
		return nil, nil
	}
	const embedRoot = ".agents/skills"
	if _, err := fs.Stat(embed, embedRoot); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("skills: stat %s: %w", embedRoot, err)}
	}

	fsLayer := config.NewFS(projectRoot, embed)
	if err := fs.WalkDir(embed, embedRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return fs.SkipDir
			}
			errs = append(errs, fmt.Errorf("skills: walk %s: %w", path, walkErr))
			return nil
		}
		if d == nil || d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		fullPath := filepath.Join(projectRoot, filepath.FromSlash(path))
		dirName := filepath.Base(filepath.Dir(fullPath))
		file, parseErr := parseSkillFile(fullPath, dirName, fsLayer)
		if parseErr != nil {
			if errors.Is(parseErr, fs.ErrNotExist) {
				return nil
			}
			errs = append(errs, parseErr)
			return nil
		}
		results = append(results, file)
		return nil
	}); err != nil {
		errs = append(errs, fmt.Errorf("skills: walk %s: %w", embedRoot, err))
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })
	return results, errs
}

func parseSkillFile(path, dirName string, fsLayer *config.FS) (SkillFile, error) {
	meta, err := readFrontMatter(path, fsLayer)
	if err != nil {
		return SkillFile{}, fmt.Errorf("skills: read %s: %w", path, err)
	}
	if meta.Name != "" && dirName != "" && meta.Name != dirName {
		return SkillFile{}, fmt.Errorf("skills: name %q does not match directory %q in %s", meta.Name, dirName, path)
	}
	if err := validateMetadata(meta); err != nil {
		return SkillFile{}, fmt.Errorf("skills: validate %s: %w", path, err)
	}

	return SkillFile{
		Name:     meta.Name,
		Path:     path,
		Metadata: meta,
		fs:       fsLayer,
	}, nil
}

func readFrontMatter(path string, fsLayer *config.FS) (SkillMetadata, error) {
	var (
		file fs.File
		err  error
	)
	if fsLayer != nil {
		file, err = fsLayer.Open(path)
	} else {
		file, err = os.Open(path)
	}
	if err != nil {
		return SkillMetadata{}, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	first, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return SkillMetadata{}, err
	}

	first = strings.TrimPrefix(first, "\uFEFF")
	if strings.TrimSpace(first) != "---" {
		return SkillMetadata{}, errors.New("missing YAML frontmatter")
	}

	var lines []string
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return SkillMetadata{}, readErr
		}
		if strings.TrimSpace(line) == "---" {
			metaText := strings.Join(lines, "")
			var meta SkillMetadata
			if err := yaml.Unmarshal([]byte(metaText), &meta); err != nil {
				return SkillMetadata{}, fmt.Errorf("decode YAML: %w", err)
			}
			return meta, nil
		}

		if line != "" {
			lines = append(lines, line)
		}

		if errors.Is(readErr, io.EOF) {
			return SkillMetadata{}, errors.New("missing closing frontmatter separator")
		}
	}
}

func parseFrontMatter(content string) (SkillMetadata, string, error) {
	trimmed := strings.TrimPrefix(content, "\uFEFF") // drop BOM if present
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return SkillMetadata{}, "", errors.New("missing YAML frontmatter")
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return SkillMetadata{}, "", errors.New("missing closing frontmatter separator")
	}

	metaText := strings.Join(lines[1:end], "\n")
	var meta SkillMetadata
	if err := yaml.Unmarshal([]byte(metaText), &meta); err != nil {
		return SkillMetadata{}, "", fmt.Errorf("decode YAML: %w", err)
	}

	body := strings.Join(lines[end+1:], "\n")
	body = strings.TrimPrefix(body, "\n")

	return meta, body, nil
}

func validateMetadata(meta SkillMetadata) error {
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		return errors.New("name is required")
	}
	if !skillNameRegexp.MatchString(name) {
		return fmt.Errorf("invalid name %q", meta.Name)
	}
	desc := strings.TrimSpace(meta.Description)
	if desc == "" {
		return errors.New("description is required")
	}
	if len(desc) > 1024 {
		return errors.New("description exceeds 1024 characters")
	}
	compat := strings.TrimSpace(meta.Compatibility)
	if len(compat) > 500 {
		return errors.New("compatibility exceeds 500 characters")
	}
	if when := strings.TrimSpace(meta.WhenToUse); len(when) > 512 {
		return errors.New("when_to_use exceeds 512 characters")
	}
	return nil
}

func loadSupportFiles(dir string) (map[string][]string, []error) {
	return loadSupportFilesWithFS(dir, nil)
}

func loadSupportFilesWithFS(dir string, fsLayer *config.FS) (map[string][]string, []error) {
	out := map[string][]string{}
	var errs []error

	if fsLayer == nil {
		fsLayer = config.NewFS("", nil)
	}

	for _, sub := range []string{"scripts", "references", "assets"} {
		root := filepath.Join(dir, sub)
		info, err := fsLayer.Stat(root)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("skills: stat %s: %w", root, err))
			}
			continue
		}
		if !info.IsDir() {
			errs = append(errs, fmt.Errorf("skills: %s is not a directory", root))
			continue
		}

		var files []string
		if walkErr := fsLayer.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				errs = append(errs, fmt.Errorf("skills: walk %s: %w", path, walkErr))
				return nil
			}
			if d.IsDir() {
				return nil
			}

			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = d.Name()
			}
			files = append(files, filepath.ToSlash(rel))
			return nil
		}); walkErr != nil {
			errs = append(errs, fmt.Errorf("skills: walk %s: %w", root, walkErr))
			continue
		}

		sort.Strings(files)
		if len(files) > 0 {
			out[sub] = files
		}
	}

	if len(out) == 0 {
		return nil, errs
	}
	return out, errs
}

func buildDefinitionMetadata(file SkillFile) map[string]string {
	var meta map[string]string
	if len(file.Metadata.Metadata) > 0 {
		meta = make(map[string]string, len(file.Metadata.Metadata)+4)
		for k, v := range file.Metadata.Metadata {
			meta[k] = v
		}
	}

	if tools := file.Metadata.AllowedTools; len(tools) > 0 {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["allowed-tools"] = strings.Join(tools, ",")
	}

	if license := strings.TrimSpace(file.Metadata.License); license != "" {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["license"] = license
	}

	if compat := strings.TrimSpace(file.Metadata.Compatibility); compat != "" {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["compatibility"] = compat
	}
	if when := strings.TrimSpace(file.Metadata.WhenToUse); when != "" {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["when_to_use"] = when
	}
	if len(file.Metadata.Paths) > 0 {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["paths"] = strings.Join(file.Metadata.Paths, ",")
	}

	if file.Path != "" {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["source"] = file.Path
	}

	return meta
}

func skillDisplayDescription(meta SkillMetadata) string {
	if when := strings.TrimSpace(meta.WhenToUse); when != "" {
		return when
	}
	return strings.TrimSpace(meta.Description)
}

func resolveFileOps(fsLayer *config.FS) fileOps {
	if fsLayer != nil {
		return fileOps{
			readFile: fsLayer.ReadFile,
			openFile: fsLayer.Open,
			statFile: fsLayer.Stat,
		}
	}
	return fileOps{
		readFile: readFileOverrideOrOS,
		openFile: func(path string) (fs.File, error) { return os.Open(path) },
		statFile: statFileOverrideOrOS,
	}
}

func buildHandler(file SkillFile, ops fileOps) Handler {
	return &lazySkillHandler{
		path: file.Path,
		file: file,
		ops:  ops,
	}
}

func loadSkillContent(file SkillFile) (Result, error) {
	body, err := loadSkillBodyFromFS(file.Path, file.fs)
	if err != nil {
		return Result{}, err
	}

	support, supportErrs := loadSupportFilesWithFS(filepath.Dir(file.Path), file.fs)
	if err := errors.Join(supportErrs...); err != nil {
		return Result{}, err
	}

	output := map[string]any{"body": body}
	meta := map[string]any{}

	if len(file.Metadata.AllowedTools) > 0 {
		meta["allowed-tools"] = []string(file.Metadata.AllowedTools)
	}
	meta["source"] = file.Path

	if len(support) > 0 {
		output["support_files"] = support
		count := 0
		for _, files := range support {
			count += len(files)
		}
		meta["support-file-count"] = count
	}

	return Result{
		Skill:    file.Metadata.Name,
		Output:   output,
		Metadata: meta,
	}, nil
}

func loadSkillBody(path string) (string, error) {
	return loadSkillBodyFromFS(path, nil)
}

func loadSkillBodyFromFS(path string, fsLayer *config.FS) (string, error) {
	var (
		data []byte
		err  error
	)
	if fsLayer != nil {
		data, err = fsLayer.ReadFile(path)
	} else {
		data, err = readFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("skills: read %s: %w", path, err)
	}
	_, body, err := parseFrontMatter(string(data))
	if err != nil {
		return "", fmt.Errorf("skills: parse %s: %w", path, err)
	}
	return body, nil
}

// SetReadFileForTest swaps the file reader; intended for white-box tests only.
func SetReadFileForTest(fn func(string) ([]byte, error)) (restore func()) {
	prev := readFile
	readFile = fn
	return func() {
		readFile = prev
	}
}

// SetSkillFileOpsForTest swaps filesystem helpers; intended for white-box tests only.
func SetSkillFileOpsForTest(
	read func(string) ([]byte, error),
	stat func(string) (fs.FileInfo, error),
) (restore func()) {
	fileOpOverridesMu.Lock()
	prev := fileOpOverrides
	if read != nil {
		fileOpOverrides.read = read
	}
	if stat != nil {
		fileOpOverrides.stat = stat
	}
	fileOpOverridesMu.Unlock()
	return func() {
		fileOpOverridesMu.Lock()
		fileOpOverrides = prev
		fileOpOverridesMu.Unlock()
	}
}

// lazySkillHandler defers loading the skill body until first execution and
// supports hot-reload by checking file modification time on each access.
type lazySkillHandler struct {
	path string
	file SkillFile
	ops  fileOps

	mu      sync.Mutex
	cached  Result
	loadErr error
	loaded  bool
	modTime time.Time
}

func (h *lazySkillHandler) Execute(_ context.Context, _ ActivationContext) (Result, error) {
	if h == nil {
		return Result{}, errors.New("skills: handler is nil")
	}

	info, err := h.ops.statFile(h.path)
	if err != nil {
		return Result{}, fmt.Errorf("skills: stat %s: %w", h.path, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.loaded && !info.ModTime().After(h.modTime) {
		if h.loadErr != nil {
			return Result{}, h.loadErr
		}
		return h.cached, nil
	}

	h.cached, h.loadErr = loadSkillContent(h.file)
	h.loaded = true
	h.modTime = info.ModTime()

	if h.loadErr != nil {
		return Result{}, h.loadErr
	}
	return h.cached, nil
}

// BodyLength reports the cached body length without triggering a load. The
// second return value indicates whether a body has been loaded.
func (h *lazySkillHandler) BodyLength() (int, bool) {
	if h == nil {
		return 0, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.loaded {
		return 0, false
	}
	return skillBodyLength(h.cached), true
}

func skillBodyLength(res Result) int {
	if res.Output == nil {
		return 0
	}
	if output, ok := res.Output.(map[string]any); ok {
		if body, ok := output["body"].(string); ok {
			return len(body)
		}
		if raw, ok := output["body"].([]byte); ok {
			return len(raw)
		}
	}
	return 0
}
