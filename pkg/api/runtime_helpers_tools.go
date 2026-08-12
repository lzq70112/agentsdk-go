package api

import "github.com/lzq70112/agentsdk-go/pkg/model"

// EnabledBuiltinToolKeys returns the built-in registration keys selected by
// Options.EnabledBuiltinTools for the effective entrypoint.
func EnabledBuiltinToolKeys(opts Options) []string {
	resolved := opts.withDefaults()
	entry := effectiveEntryPoint(resolved)
	return filterBuiltinNames(resolved.EnabledBuiltinTools, builtinOrder(entry))
}

// AvailableTools returns model-facing tool definitions from the runtime registry.
func (rt *Runtime) AvailableTools() []model.ToolDefinition {
	if rt == nil {
		return nil
	}
	return availableToolsForSession(rt.registry, nil, rt.deferred, "")
}

// AvailableToolsForWhitelist returns model-facing tool definitions constrained by whitelist.
func (rt *Runtime) AvailableToolsForWhitelist(toolWhitelist []string) []model.ToolDefinition {
	if rt == nil {
		return nil
	}
	return availableToolsForSession(rt.registry, toLowerSet(toolWhitelist), rt.deferred, "")
}

func (rt *Runtime) systemPromptForSession(sessionID string, whitelist map[string]struct{}) string {
	if rt == nil {
		return ""
	}
	rt.mu.RLock()
	base := rt.opts.SystemPrompt
	rt.mu.RUnlock()
	if rt.deferred == nil {
		return base
	}
	section := deferredToolSection(rt.deferred.inactiveNames(sessionID, whitelist))
	if section == "" {
		return base
	}
	if base == "" {
		return section
	}
	return base + "\n\n" + section
}
