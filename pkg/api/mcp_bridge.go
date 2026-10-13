package api

import (
	"strings"

	"github.com/lzq70112/agentsdk-go/pkg/config"
)

type mcpServer struct {
	Name               string
	Spec               string
	URL                string
	Headers            map[string]string
	Env                map[string]string
	TimeoutSeconds     int
	EnabledTools       []string
	DisabledTools      []string
	ToolTimeoutSeconds int
}

// collectMCPServers merges explicit API inputs, settings.json entries, and
// managed allow/deny policies. disabled lists MCP server config names
// (case-insensitive) that must not be connected; the list is inherited by
// sub-runtimes so admin disable decisions stay effective there.
func collectMCPServers(settings *config.Settings, explicit, disabled []string) []mcpServer {
	seen := map[string]struct{}{}
	var servers []mcpServer
	allowRules := managedAllowRules(settings)
	denyRules := managedDenyRules(settings)

	add := func(server mcpServer) {
		server.Spec = strings.TrimSpace(server.Spec)
		if server.Spec == "" {
			return
		}
		if isDisabledServer(server.Name, disabled) {
			return
		}
		if !allowedByManagedPolicies(server.Name, server.URL, allowRules, denyRules) {
			return
		}
		// 去重键做 stream 提示归一化：显式传入的 "http://x" 与 settings 生成的
		// "http+stream://x" 是同一个 server，不能因传输提示不同而重复注册。
		dedupKey := addStreamHint(server.Spec)
		if _, ok := seen[dedupKey]; ok {
			return
		}
		seen[dedupKey] = struct{}{}
		server.Name = strings.TrimSpace(server.Name)
		server.URL = strings.TrimSpace(server.URL)
		servers = append(servers, server)
	}

	for _, spec := range explicit {
		add(mcpServer{Spec: spec, URL: spec})
	}

	if settings != nil && settings.MCP != nil {
		for name, cfg := range settings.MCP.Servers {
			// Convert MCPServerConfig to spec string
			spec := mcpConfigToSpec(cfg)
			add(mcpServer{
				Name:               name,
				Spec:               spec,
				URL:                cfg.URL,
				Headers:            cfg.Headers,
				Env:                cfg.Env,
				TimeoutSeconds:     cfg.TimeoutSeconds,
				EnabledTools:       cfg.EnabledTools,
				DisabledTools:      cfg.DisabledTools,
				ToolTimeoutSeconds: cfg.ToolTimeoutSeconds,
			})
		}
	}
	return servers
}

func managedAllowRules(s *config.Settings) []config.MCPServerRule {
	if s == nil {
		return nil
	}
	return s.AllowedMcpServers
}

func managedDenyRules(s *config.Settings) []config.MCPServerRule {
	if s == nil {
		return nil
	}
	return s.DeniedMcpServers
}

// isDisabledServer 判断 server 是否在禁用名单中；大小写不敏感，空名不匹配。
func isDisabledServer(name string, disabled []string) bool {
	if len(disabled) == 0 {
		return false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, d := range disabled {
		if strings.EqualFold(strings.TrimSpace(d), name) {
			return true
		}
	}
	return false
}

func allowedByManagedPolicies(name, target string, allow, deny []config.MCPServerRule) bool {
	if matchesRule(name, target, deny) {
		return false
	}
	if len(allow) == 0 {
		return true
	}
	return matchesRule(name, target, allow)
}

func matchesRule(name, target string, rules []config.MCPServerRule) bool {
	for _, rule := range rules {
		if rule.ServerName != "" && !strings.EqualFold(rule.ServerName, name) {
			continue
		}
		if rule.URL != "" && !strings.EqualFold(rule.URL, target) {
			continue
		}
		if strings.TrimSpace(rule.ServerName) == "" && strings.TrimSpace(rule.URL) == "" {
			continue
		}
		return true
	}
	return false
}
