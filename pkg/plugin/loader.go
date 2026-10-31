package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lzq70112/agentsdk-go/pkg/config"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/skills"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/subagents"
)

// resource directory names inside a plugin root.
const (
	skillsDir   = "skills"
	agentsDir   = "agents"
	commandsDir = "commands"
)

// Plugin is a fully loaded plugin: manifest plus converted resources ready to
// be merged into the runtime by pkg/api.
type Plugin struct {
	Manifest   Manifest
	Dir        string
	Skills     []skills.SkillRegistration
	Agents     []subagents.SubagentRegistration
	MCPServers map[string]config.MCPServerConfig
	Commands   []Command
}

// Load reads one plugin from disk: manifest, skills, agents, commands and MCP
// config. A missing resources directory is not an error (a plugin may ship
// only some resource kinds); a malformed resource is.
func Load(dir string) (Plugin, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Plugin{}, fmt.Errorf("plugin: resolve %s: %w", dir, err)
	}
	manifest, err := loadManifest(abs)
	if err != nil {
		return Plugin{}, err
	}
	p := Plugin{Manifest: manifest, Dir: abs}

	p.Skills, err = loadPluginSkills(abs, manifest)
	if err != nil {
		return Plugin{}, err
	}
	p.Agents, err = loadPluginAgents(abs)
	if err != nil {
		return Plugin{}, err
	}
	p.Commands, err = loadPluginCommands(abs)
	if err != nil {
		return Plugin{}, err
	}
	p.MCPServers, err = loadPluginMCP(abs)
	if err != nil {
		return Plugin{}, err
	}
	return p, nil
}

// loadPluginSkills collects skill registrations. When plugin.json declares the
// "skills" key, only the declared relative paths load (an empty declaration
// yields no skills); otherwise every subdirectory of skills/ with a SKILL.md
// loads in sorted order for determinism.
func loadPluginSkills(pluginDir string, manifest Manifest) ([]skills.SkillRegistration, error) {
	if manifest.SkillsDeclared() {
		var out []skills.SkillRegistration
		for _, rel := range manifest.Skills {
			rel = strings.TrimSpace(rel)
			if rel == "" {
				continue
			}
			reg, err := skills.LoadSingleSkill(filepath.Join(pluginDir, rel), nil)
			if err != nil {
				return nil, err
			}
			out = append(out, reg)
		}
		return out, nil
	}
	root := filepath.Join(pluginDir, skillsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugin: scan %s: %w", root, err)
	}
	var out []skills.SkillRegistration
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
			continue // subdirectory without SKILL.md is not a skill
		}
		reg, err := skills.LoadSingleSkill(skillDir, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, reg)
	}
	return out, nil
}

// loadPluginAgents loads every agents/*.md subagent definition.
func loadPluginAgents(pluginDir string) ([]subagents.SubagentRegistration, error) {
	root := filepath.Join(pluginDir, agentsDir)
	files, err := listFilesByExt(root, ".md")
	if err != nil {
		return nil, err
	}
	var out []subagents.SubagentRegistration
	for _, path := range files {
		reg, err := subagents.LoadSingleSubagent(path)
		if err != nil {
			return nil, err
		}
		out = append(out, reg)
	}
	return out, nil
}

// loadPluginCommands loads every commands/*.md definition.
func loadPluginCommands(pluginDir string) ([]Command, error) {
	root := filepath.Join(pluginDir, commandsDir)
	files, err := listFilesByExt(root, ".md")
	if err != nil {
		return nil, err
	}
	var out []Command
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("plugin: read %s: %w", path, err)
		}
		cmd, err := parseCommandFile(string(data))
		if err != nil {
			return nil, fmt.Errorf("plugin: %s: %w", path, err)
		}
		cmd.Name = strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
		out = append(out, cmd)
	}
	sortCommands(out)
	return out, nil
}

func sortCommands(cmds []Command) {
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
}

// listFilesByExt returns sorted *.ext file paths directly under root.
func listFilesByExt(root, ext string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugin: scan %s: %w", root, err)
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ext) {
			continue
		}
		out = append(out, filepath.Join(root, entry.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// Discover loads every enabled plugin registered under pluginsRoot. A plugin
// that fails to load is skipped and reported in the returned errors so one bad
// plugin never blocks the others. Two plugins claiming the same name is a
// conflict: the later one is dropped entirely (comate semantics).
func Discover(root string) ([]Plugin, []error) {
	entries, err := loadRegistry(root)
	if err != nil {
		return nil, []error{err}
	}
	var (
		plugins []Plugin
		errs    []error
		seen    = map[string]struct{}{}
	)
	for _, entry := range entries {
		if !entry.Enabled {
			continue
		}
		if _, dup := seen[entry.Name]; dup {
			errs = append(errs, fmt.Errorf("plugin: namespace conflict on %q from %s, later plugin dropped", entry.Name, entry.InstallPath))
			continue
		}
		p, err := Load(entry.InstallPath)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		seen[p.Manifest.Name] = struct{}{}
		plugins = append(plugins, p)
	}
	return plugins, errs
}
