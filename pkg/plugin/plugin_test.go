package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePlugin creates a plugin directory under t.TempDir with the given
// manifest fields and optional resource files (paths relative to plugin root).
func writePlugin(t *testing.T, name, description string, files map[string]string, skillsDecl []string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	manifestJSON := `{"name": "` + name + `", "description": "` + description + `"`
	if skillsDecl != nil {
		quoted := make([]string, 0, len(skillsDecl))
		for _, s := range skillsDecl {
			quoted = append(quoted, `"`+s+`"`)
		}
		manifestJSON += `, "skills": [` + strings.Join(quoted, ",") + `]`
	}
	manifestJSON += `}`
	all := make(map[string]string, len(files)+1)
	for rel, content := range files {
		all[rel] = content
	}
	all[".claude-plugin/plugin.json"] = manifestJSON
	for rel, content := range all {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return dir
}

func TestLoadManifestRequiredFields(t *testing.T) {
	dir := writePlugin(t, "good", "desc", nil, nil)
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if m.Name != "good" || m.Description != "desc" || m.SkillsDeclared() {
		t.Fatalf("unexpected manifest: %+v declared=%v", m, m.SkillsDeclared())
	}

	bad := writePlugin(t, "nodesc", "", nil, nil)
	if _, err := loadManifest(bad); err == nil || !strings.Contains(err.Error(), "description is required") {
		t.Fatalf("want description required error, got %v", err)
	}

	invalid := writePlugin(t, "Bad Name!", "d", nil, nil)
	if _, err := loadManifest(invalid); err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("want invalid name error, got %v", err)
	}
}

func TestLoadManifestAuthorAndExtraFields(t *testing.T) {
	// 生态真实插件带 homepage/license/keywords/mcpServers 等附加字段与对象 author，
	// 必须宽松接受且 mcpServers 不参与解析（存档字段）。
	dir := t.TempDir()
	manifest := `{
		"name": "rich", "description": "d", "version": "1.2.3",
		"author": {"name": "Anthropic", "url": "https://example.com"},
		"homepage": "https://github.com/x", "license": "MIT",
		"keywords": ["a"], "mcpServers": {}
	}`
	if err := os.MkdirAll(filepath.Join(dir, ManifestDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestDir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if m.Version != "1.2.3" || m.Author != "Anthropic (https://example.com)" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}

func TestLoadSkillsDefaultDirVsDeclared(t *testing.T) {
	files := map[string]string{
		"skills/alpha/SKILL.md":  "---\nname: alpha\ndescription: alpha skill\n---\nbody",
		"skills/beta/SKILL.md":   "---\nname: beta\ndescription: beta skill\n---\nbody",
		"skills/gamma/README.md": "not a skill",
	}
	dir := writePlugin(t, "skills-default", "d", files, nil)
	p, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Skills) != 2 {
		t.Fatalf("want 2 skills (gamma has no SKILL.md), got %d", len(p.Skills))
	}

	declared := writePlugin(t, "skills-declared", "d", files, []string{"skills/alpha"})
	pd, err := Load(declared)
	if err != nil {
		t.Fatalf("Load declared: %v", err)
	}
	if len(pd.Skills) != 1 || pd.Skills[0].Definition.Name != "alpha" {
		t.Fatalf("declared skills should load only alpha, got %+v", pd.Skills)
	}

	// 空数组也是声明：默认目录不再扫描，技能全空。
	empty := writePlugin(t, "skills-empty", "d", files, []string{})
	pe, err := Load(empty)
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if len(pe.Skills) != 0 {
		t.Fatalf("declared empty skills must load none, got %d", len(pe.Skills))
	}
}

func TestLoadAgentsAndCommandsAndMCP(t *testing.T) {
	files := map[string]string{
		"agents/reviewer.md": "---\nname: reviewer\ndescription: reviews code\n---\nYou review code.",
		"commands/deploy.md": "---\ndescription: deploy service\nargument-hint: <env>\n---\nDeploy $1 to $ARGUMENTS",
		"commands/broken.md": "---\ndescription: broken\n---\nbroken body",
		".mcp.json":          `{"fetcher": {"type": "stdio", "command": "fetch-tool"}, "web": {"type": "http", "url": "http://x"}}`,
	}
	dir := writePlugin(t, "full", "d", files, nil)
	p, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Agents) != 1 || p.Agents[0].Definition.Name != "reviewer" {
		t.Fatalf("agents mismatch: %+v", p.Agents)
	}
	if len(p.Commands) != 2 || p.Commands[1].Name != "deploy" {
		t.Fatalf("commands mismatch: %+v", p.Commands)
	}
	if got := p.Commands[1].Render("prod a b"); got != "Deploy prod to prod a b" {
		t.Fatalf("render mismatch: %q", got)
	}
	if got := p.Commands[1].Render("prod"); got != "Deploy prod to prod" {
		t.Fatalf("positional out-of-range should be empty: %q", got)
	}
	if len(p.MCPServers) != 2 || p.MCPServers["fetcher"].Command != "fetch-tool" {
		t.Fatalf("mcp mismatch: %+v", p.MCPServers)
	}
}

func TestLoadCommandFrontmatterOptional(t *testing.T) {
	cmd, err := parseCommandFile("plain body without frontmatter")
	if err != nil {
		t.Fatalf("parse plain command: %v", err)
	}
	if cmd.Template != "plain body without frontmatter" || cmd.Description != "" {
		t.Fatalf("unexpected command: %+v", cmd)
	}

	// 未闭合的 frontmatter 是格式错误，必须报错而不是当作正文。
	if _, err := parseCommandFile("---\ndescription: no close\n"); err == nil || !strings.Contains(err.Error(), "not closed") {
		t.Fatalf("unclosed frontmatter should error, got %v", err)
	}
}

func TestLoadMCPServerValidation(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"bad type", `{"x": {"type": "tcp"}}`, "not supported"},
		{"stdio no command", `{"x": {"type": "stdio"}}`, "command is required"},
		{"http no url", `{"x": {"type": "http"}}`, "url is required"},
		{"empty name", `{"  ": {"type": "http", "url": "http://x"}}`, "empty server name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writePlugin(t, "mcp-bad", "d", map[string]string{".mcp.json": tc.content}, nil)
			if _, err := loadPluginMCP(dir); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDiscoverEnabledAndConflicts(t *testing.T) {
	root := t.TempDir()
	one := writePlugin(t, "one", "first", nil, nil)
	two := writePlugin(t, "one", "second", nil, nil) // 同名插件：后者丢弃
	_ = two
	entries := []InstalledEntry{
		{ID: "one@local", Name: "one", InstallPath: one, Enabled: true},
		{Name: "one", ID: "one@other", InstallPath: two, Enabled: true},
		{Name: "off", ID: "off@local", InstallPath: writePlugin(t, "off", "d", nil, nil), Enabled: false},
	}
	if err := saveRegistry(root, entries); err != nil {
		t.Fatalf("saveRegistry: %v", err)
	}
	plugins, errs := Discover(root)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "namespace conflict") {
		t.Fatalf("want one namespace conflict error, got %v", errs)
	}
	if len(plugins) != 1 || plugins[0].Manifest.Name != "one" {
		t.Fatalf("want only plugin one, got %+v", plugins)
	}
}

func TestLoadMissingResourcesIsNotError(t *testing.T) {
	dir := writePlugin(t, "manifest-only", "d", nil, nil)
	p, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Skills)+len(p.Agents)+len(p.Commands)+len(p.MCPServers) != 0 {
		t.Fatalf("expected empty resources, got %+v", p)
	}
}
