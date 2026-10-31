package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lzq70112/agentsdk-go/pkg/config"
)

// loadPluginMCP reads <pluginDir>/.mcp.json in the flat form
// {"server-name": {type, command/url, ...}} — deliberately NOT wrapped in
// {"mcpServers": {...}}} like project-level settings. Unknown transports are
// rejected so a bad entry fails loudly instead of registering a broken server.
func loadPluginMCP(pluginDir string) (map[string]config.MCPServerConfig, error) {
	path := filepath.Join(pluginDir, ".mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugin: read %s: %w", path, err)
	}
	var flat map[string]config.MCPServerConfig
	if err := json.Unmarshal(data, &flat); err != nil {
		return nil, fmt.Errorf("plugin: decode %s: %w", path, err)
	}
	out := make(map[string]config.MCPServerConfig, len(flat))
	for name, srv := range flat {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("plugin: %s: empty server name", path)
		}
		switch srv.Type {
		case "stdio":
			if srv.Command == "" {
				return nil, fmt.Errorf("plugin: %s: mcp server %q: command is required for type stdio", path, name)
			}
		case "http", "sse":
			if srv.URL == "" {
				return nil, fmt.Errorf("plugin: %s: mcp server %q: url is required for type %s", path, name, srv.Type)
			}
		default:
			return nil, fmt.Errorf("plugin: %s: mcp server %q: type %q is not supported", path, name, srv.Type)
		}
		out[name] = srv
	}
	return out, nil
}
