package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/config"
	"github.com/lzq70112/agentsdk-go/pkg/plugin"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/skills"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/subagents"
)

func noopSkillHandler(_ context.Context, _ skills.ActivationContext) (skills.Result, error) {
	return skills.Result{}, nil
}

func noopSubagentHandler(_ context.Context, _ subagents.Context, _ subagents.Request) (subagents.Result, error) {
	return subagents.Result{}, nil
}

// writeTestPlugin creates a minimal .claude-plugin directory so real plugin
// fixtures can be registered through plugin.InstallLocal.
func writeTestPlugin(t *testing.T, parent, name, description string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	manifestDir := filepath.Join(dir, ".claude-plugin")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name": "` + name + `", "description": "` + description + `"}`
	if err := os.WriteFile(filepath.Join(manifestDir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolvePluginsManualWinsAndDisable(t *testing.T) {
	root := t.TempDir()
	diskAlpha := writeTestPlugin(t, t.TempDir(), "alpha", "from-disk")
	if _, err := plugin.InstallLocal(root, diskAlpha); err != nil {
		t.Fatalf("InstallLocal alpha: %v", err)
	}
	beta := writeTestPlugin(t, t.TempDir(), "beta", "beta-desc")
	if _, err := plugin.InstallLocal(root, beta); err != nil {
		t.Fatalf("InstallLocal beta: %v", err)
	}
	gamma := writeTestPlugin(t, t.TempDir(), "gamma", "gamma-desc")
	if _, err := plugin.InstallLocal(root, gamma); err != nil {
		t.Fatalf("InstallLocal gamma: %v", err)
	}

	manual := plugin.Plugin{Manifest: plugin.Manifest{Name: "alpha", Description: "manual"}, Dir: "manual-dir"}
	opts := Options{
		Plugins:         []plugin.Plugin{manual},
		PluginsDir:      root,
		DisabledPlugins: []string{"BETA"},
	}
	plugins, errs := resolvePlugins(opts)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(plugins) != 2 {
		t.Fatalf("want manual alpha + gamma, got %+v", plugins)
	}
	if plugins[0].Manifest.Name != "alpha" || plugins[0].Manifest.Description != "manual" {
		t.Fatalf("manual injection must win the name conflict, got %+v", plugins[0].Manifest)
	}
	if plugins[1].Manifest.Name != "gamma" {
		t.Fatalf("expected gamma second, got %+v", plugins[1].Manifest)
	}
}

func TestResolvePluginsRejectsEmptyName(t *testing.T) {
	opts := Options{Plugins: []plugin.Plugin{{Dir: "anon"}}}
	plugins, errs := resolvePlugins(opts)
	if len(plugins) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "empty name") {
		t.Fatalf("nameless plugin must be rejected, got %+v %v", plugins, errs)
	}
}

func TestBuildSkillsRegistryPluginHostPriority(t *testing.T) {
	project := t.TempDir()
	skillDir := filepath.Join(project, ".agents", "skills", "shared")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: shared\ndescription: project skill\n---\nbody"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		ProjectRoot: project,
		Skills: []SkillRegistration{{
			Definition: skills.Definition{Name: "shared", Metadata: map[string]string{"source": "host"}},
			Handler:    skills.HandlerFunc(noopSkillHandler),
		}},
	}
	opts.plugins = []plugin.Plugin{{
		Skills: []skills.SkillRegistration{{
			Definition: skills.Definition{Name: "shared", Metadata: map[string]string{"source": "plugin"}},
			Handler:    skills.HandlerFunc(noopSkillHandler),
		}},
	}}
	reg, errs := buildSkillsRegistry(opts)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if skill, ok := reg.Get("shared"); !ok {
		t.Fatal("shared skill must be registered")
	} else if got := skill.Definition().Metadata["source"]; got != "host" {
		t.Fatalf("host explicit definition must win, got source=%q", got)
	}

	// 无宿主定义时插件压过项目文件。
	opts.Skills = nil
	reg, errs = buildSkillsRegistry(opts)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if skill, ok := reg.Get("shared"); !ok {
		t.Fatal("shared skill must be registered")
	} else if got := skill.Definition().Metadata["source"]; got != "plugin" {
		t.Fatalf("plugin must beat project filesystem skill, got source=%q", got)
	}
}

func TestBuildSubagentsManagerPluginHostPriority(t *testing.T) {
	opts := Options{
		Subagents: []SubagentRegistration{{
			Definition: subagents.Definition{Name: "reviewer", Description: "host"},
			Handler:    subagents.HandlerFunc(noopSubagentHandler),
		}},
	}
	opts.plugins = []plugin.Plugin{{
		Agents: []subagents.SubagentRegistration{{
			Definition: subagents.Definition{Name: "reviewer", Description: "plugin"},
			Handler:    subagents.HandlerFunc(noopSubagentHandler),
		}},
	}}
	mgr, errs := buildSubagentsManager(opts)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	for _, def := range mgr.List() {
		if def.Name == "reviewer" && def.Description != "host" {
			t.Fatalf("host explicit subagent must win, got description=%q", def.Description)
		}
	}

	opts.Subagents = nil
	mgr, errs = buildSubagentsManager(opts)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	found := false
	for _, def := range mgr.List() {
		if def.Name == "reviewer" {
			found = true
			if def.Description != "plugin" {
				t.Fatalf("plugin subagent must register, got description=%q", def.Description)
			}
		}
	}
	if !found {
		t.Fatal("plugin subagent missing from manager")
	}
}

func TestCollectMCPServersPluginSource(t *testing.T) {
	settings := &config.Settings{
		MCP: &config.MCPConfig{
			Servers: map[string]config.MCPServerConfig{
				"fetcher": {Type: "http", URL: "http://settings.example"},
			},
		},
	}
	pluginServers := map[string]config.MCPServerConfig{
		"fetcher": {Type: "http", URL: "http://plugin.example"},
		"extra":   {Type: "stdio", Command: "extra-tool"},
	}
	servers := collectMCPServers(settings, nil, nil, pluginServers)
	if len(servers) != 2 {
		t.Fatalf("want settings fetcher + plugin extra, got %+v", servers)
	}
	for _, srv := range servers {
		if srv.Name == "fetcher" && srv.URL != "http://settings.example" {
			t.Fatalf("settings must win on name conflict, got url=%q", srv.URL)
		}
		if srv.Name == "extra" && srv.Spec != "stdio://extra-tool" {
			t.Fatalf("plugin stdio spec mismatch: %q", srv.Spec)
		}
	}

	// 插件来源同样受 DisabledMCP 约束。
	servers = collectMCPServers(settings, nil, []string{"extra"}, pluginServers)
	if len(servers) != 1 || servers[0].Name != "fetcher" {
		t.Fatalf("disabled plugin server must be skipped, got %+v", servers)
	}
}

func TestRuntimeCommandsAndRender(t *testing.T) {
	// 输入镜像 resolvePlugins 的产物（按插件名排序）：aaa 先到先得。
	rt := &Runtime{commands: pluginCommands([]plugin.Plugin{
		{Manifest: plugin.Manifest{Name: "aaa"}, Commands: []plugin.Command{{Name: "deploy", Template: "A $1"}, {Name: "greet", Template: "hi $ARGUMENTS"}}},
		{Manifest: plugin.Manifest{Name: "zzz"}, Commands: []plugin.Command{{Name: "deploy", Template: "Z $1"}}},
	})}
	cmds := rt.Commands()
	if len(cmds) != 2 || cmds[0].Name != "deploy" || cmds[1].Name != "greet" {
		t.Fatalf("commands must be name-sorted, got %+v", cmds)
	}
	// 跨插件同名命令：插件名字典序在前的 aaa 先到先得。
	if out, err := rt.RenderCommand("Deploy", "prod"); err != nil || out != "A prod" {
		t.Fatalf("render mismatch: %q %v", out, err)
	}
	if out, err := rt.RenderCommand("greet", "a b"); err != nil || out != "hi a b" {
		t.Fatalf("render mismatch: %q %v", out, err)
	}
	if _, err := rt.RenderCommand("missing", ""); !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("missing command must map to ErrCommandNotFound, got %v", err)
	}
	var nilRt *Runtime
	if got := nilRt.Commands(); got != nil {
		t.Fatalf("nil runtime must return nil commands, got %+v", got)
	}
	if _, err := nilRt.RenderCommand("x", ""); err == nil {
		t.Fatal("nil runtime RenderCommand must fail")
	}
}
