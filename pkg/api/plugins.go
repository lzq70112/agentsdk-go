package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lzq70112/agentsdk-go/pkg/config"
	"github.com/lzq70112/agentsdk-go/pkg/plugin"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/skills"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/subagents"
)

// ErrCommandNotFound 由 RenderCommand 在指令名未命中时返回（errors.Is 可判），
// 宿主据此把消息回落为普通对话。
var ErrCommandNotFound = errors.New("api: command not found")

// resolvePlugins merges manually injected plugins (Options.Plugins) with the
// ones auto-discovered from Options.PluginsDir, applying the DisabledPlugins
// list (plugin name, case-insensitive). Manual injection wins on plugin-name
// conflicts; the output is sorted by plugin name so downstream merges are
// deterministic.
func resolvePlugins(opts Options) ([]plugin.Plugin, []error) {
	disabled := disabledNameSet(opts.DisabledPlugins)
	var (
		out  []plugin.Plugin
		errs []error
		seen = map[string]struct{}{}
		add  = func(p plugin.Plugin, source string) {
			key := strings.ToLower(strings.TrimSpace(p.Manifest.Name))
			if key == "" {
				errs = append(errs, fmt.Errorf("api: plugin from %s has empty name", source))
				return
			}
			if _, off := disabled[key]; off {
				return
			}
			if _, dup := seen[key]; dup {
				return // 手动注入优先：同名发现插件丢弃
			}
			seen[key] = struct{}{}
			out = append(out, p)
		}
	)
	for _, p := range opts.Plugins {
		add(p, "options")
	}
	if dir := strings.TrimSpace(opts.PluginsDir); dir != "" {
		found, discErrs := plugin.Discover(dir)
		errs = append(errs, discErrs...)
		for _, p := range found {
			add(p, dir)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out, errs
}

// pluginSkillRegistrations gathers skill registrations from resolved plugins,
// in plugin-name order for deterministic conflict resolution.
func pluginSkillRegistrations(plugins []plugin.Plugin) []skills.SkillRegistration {
	var out []skills.SkillRegistration
	for _, p := range plugins {
		out = append(out, p.Skills...)
	}
	return out
}

// pluginAgentRegistrations gathers subagent registrations from resolved plugins.
func pluginAgentRegistrations(plugins []plugin.Plugin) []subagents.SubagentRegistration {
	var out []subagents.SubagentRegistration
	for _, p := range plugins {
		out = append(out, p.Agents...)
	}
	return out
}

// pluginMCPServers merges plugin MCP configs by server name; on cross-plugin
// name conflicts the first plugin (name-sorted) wins.
func pluginMCPServers(plugins []plugin.Plugin) map[string]config.MCPServerConfig {
	if len(plugins) == 0 {
		return nil
	}
	out := make(map[string]config.MCPServerConfig)
	for _, p := range plugins {
		for name, cfg := range p.MCPServers {
			if _, ok := out[name]; !ok {
				out[name] = cfg
			}
		}
	}
	return out
}

// pluginCommands collects slash commands from resolved plugins sorted by
// command name; cross-plugin name conflicts resolve first-wins (plugin-name
// order), so Runtime.RenderCommand stays deterministic.
func pluginCommands(plugins []plugin.Plugin) []plugin.Command {
	var out []plugin.Command
	seen := map[string]struct{}{}
	for _, p := range plugins {
		for _, cmd := range p.Commands {
			key := strings.ToLower(cmd.Name)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, cmd)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Commands returns the slash commands contributed by all loaded plugins,
// sorted by command name. Empty when no plugin ships commands.
func (rt *Runtime) Commands() []plugin.Command {
	if rt == nil {
		return nil
	}
	return rt.commands
}

// RenderCommand renders a plugin command template with user-supplied
// arguments ($ARGUMENTS / $1..$9 substitution). ErrCommandNotFound when the
// name is not registered; errors.Is lets the host fall back to a normal turn.
func (rt *Runtime) RenderCommand(name, args string) (string, error) {
	if rt == nil {
		return "", fmt.Errorf("api: runtime is nil")
	}
	key := strings.ToLower(strings.TrimSpace(name))
	for _, cmd := range rt.commands {
		if cmd.Name == key {
			return cmd.Render(args), nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrCommandNotFound, name)
}
