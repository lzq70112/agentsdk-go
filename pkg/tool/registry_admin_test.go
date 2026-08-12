package tool

import (
	"testing"

	"github.com/lzq70112/agentsdk-go/pkg/mcp"
)

func TestListMCPServersEmpty(t *testing.T) {
	reg := NewRegistry()
	servers := reg.ListMCPServers()
	if len(servers) != 0 {
		t.Fatalf("expected 0 servers, got %d", len(servers))
	}
}

// TestMcpServerTypeFromSpec verifies the Type field reflects how the SDK's
// transport layer (mcp.go buildSessionTransport) actually classifies a spec.
// This matters because admin users reading the Type must not be misled:
// e.g. an "https://..." URL is wired as SSE by the SDK, not "http".
func TestMcpServerTypeFromSpec(t *testing.T) {
	cases := []struct {
		spec string
		want string
	}{
		{"stdio://echo --foo", "stdio"},
		{"echo --foo", "stdio"},          // bare command falls back to stdio
		{"  echo arg ", "stdio"},         // whitespace + bare command
		{"sse://example.com/events", "sse"},
		{"http://example.com/mcp", "sse"},   // plain http(s) URL → SSE in SDK
		{"https://example.com/mcp", "sse"},  // plain https URL → SSE in SDK
		{"http+stream://example.com", "http"},
		{"https+streamable://example.com", "http"},
		{"http+sse://example.com", "sse"},
		{"http+ws://example.com", "unknown"}, // unsupported hint rejected by mcp.go
		{"HTTP+STREAM://example.com", "http"},
		{"SSE://example.com/events", "sse"},
		{"", "unknown"},
		{"   ", "unknown"},
	}
	for _, c := range cases {
		got := mcpServerTypeFromSpec(c.spec)
		if got != c.want {
			t.Errorf("mcpServerTypeFromSpec(%q) = %q, want %q", c.spec, got, c.want)
		}
	}
}

func TestRemoveMCPServerNotFound(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RemoveMCPServer("nonexistent"); err == nil {
		t.Fatal("expected error for non-existent server")
	}
}

func TestRemoveMCPServerEmptyID(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RemoveMCPServer("   "); err == nil {
		t.Fatal("expected error for empty server id")
	}
}

// TestRemoveMCPServerHappyPath verifies removal semantics that matter for a
// running admin UI: after RemoveMCPServer the server is gone from ListMCPServers,
// its tools are removed from the registry (so the bot stops offering them), and
// the underlying MCP session is closed.
func TestRemoveMCPServerHappyPath(t *testing.T) {
	reg := NewRegistry()

	server := &stubMCPServer{tools: []*mcp.Tool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}}
	session, err := server.newSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// Seed the registry with a registered MCP server + its tool.
	if err := reg.registerMCPSession("stdio://echo", "echo-server", session, []Tool{&spyTool{name: "echo-server__echo"}}, []string{"echo-server__echo"}, MCPServerOptions{}); err != nil {
		t.Fatalf("register session: %v", err)
	}

	// Sanity: server + tool present.
	servers := reg.ListMCPServers()
	if len(servers) != 1 || servers[0].ServerID != "stdio://echo" {
		t.Fatalf("expected 1 server stdio://echo, got %+v", servers)
	}
	if _, gerr := reg.Get("echo-server__echo"); gerr != nil {
		t.Fatalf("expected tool present before removal, got %v", gerr)
	}

	// Remove it.
	if err := reg.RemoveMCPServer("stdio://echo"); err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}

	// Server no longer listed.
	if got := reg.ListMCPServers(); len(got) != 0 {
		t.Fatalf("expected 0 servers after removal, got %+v", got)
	}
	// Tool removed from registry.
	if _, gerr := reg.Get("echo-server__echo"); gerr == nil {
		t.Fatal("expected tool to be removed after server removal")
	}
	// Underlying MCP session was closed — admin UI must actually disconnect,
	// not just hide the entry while the connection leaks.
	if !server.Closed() {
		t.Fatal("expected MCP session to be closed after removal")
	}
	// Removing again fails as not-found.
	if err := reg.RemoveMCPServer("stdio://echo"); err == nil {
		t.Fatal("expected error on second removal")
	}
}

