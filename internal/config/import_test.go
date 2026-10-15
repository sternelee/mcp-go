package config

import (
	"strings"
	"testing"
)

func TestParseClaudeDesktopConfiguration(t *testing.T) {
	const config = `{
	  "mcpServers": {
	    "github": {
	      "type": "http",
	      "url": "https://api.githubcopilot.com/mcp/",
	      "headers": {"Authorization": "Bearer ghp_x"}
	    },
	    "filesystem": {
	      "command": "npx",
	      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/Users/me"],
	      "env": {"HOME": "/Users/me", "DEBUG": true, "PORT": 8080}
	    }
	  }
	}`

	servers, err := ParseMCPConfig([]byte(config))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("parsed %d servers, want 2: %+v", len(servers), servers)
	}
	// The order is stable, so the interface does not shuffle on import.
	if servers[0].Name != "filesystem" || servers[1].Name != "github" {
		t.Errorf("servers are ordered %q, %q", servers[0].Name, servers[1].Name)
	}

	fs := servers[0]
	if fs.Transport != TransportStdio {
		t.Errorf("filesystem is %q, want stdio", fs.Transport)
	}
	if fs.Command != "npx" {
		t.Errorf("command is %q", fs.Command)
	}
	if len(fs.Args) != 3 || fs.Args[2] != "/Users/me" {
		t.Errorf("arguments are %q", fs.Args)
	}
	if !fs.Enabled {
		t.Error("an imported server should be switched on")
	}
	for _, want := range []string{"HOME=/Users/me", "DEBUG=true", "PORT=8080"} {
		if !contains(fs.Env, want) {
			t.Errorf("environment %q does not hold %q", fs.Env, want)
		}
	}

	gh := servers[1]
	if gh.Transport != TransportHTTP {
		t.Errorf("github is %q, want http", gh.Transport)
	}
	if gh.URL != "https://api.githubcopilot.com/mcp/" {
		t.Errorf("url is %q", gh.URL)
	}
	if gh.Headers["Authorization"] != "Bearer ghp_x" {
		t.Errorf("headers are %q", gh.Headers)
	}
}

func TestParseAcceptsTheOtherShapes(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		wantName string
		wantType Transport
	}{
		{
			name:     "a bare map of servers",
			config:   `{"local": {"command": "uvx", "args": "mcp-server-time"}}`,
			wantName: "local",
			wantType: TransportStdio,
		},
		{
			name:     "the servers key",
			config:   `{"servers": {"legacy": {"type": "sse", "url": "https://example.com/sse"}}}`,
			wantName: "legacy",
			wantType: TransportSSE,
		},
		{
			name:     "a remote server with no kind",
			config:   `{"remote": {"url": "https://example.com/mcp"}}`,
			wantName: "remote",
			wantType: TransportHTTP,
		},
		{
			name:     "one server on its own",
			config:   `{"name": "Solo", "command": "npx", "args": ["-y", "solo"]}`,
			wantName: "Solo",
			wantType: TransportStdio,
		},
		{
			name:     "streamable http spelled out",
			config:   `{"modern": {"transport": "streamable-http", "url": "https://example.com/mcp"}}`,
			wantName: "modern",
			wantType: TransportHTTP,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			servers, err := ParseMCPConfig([]byte(test.config))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(servers) != 1 {
				t.Fatalf("parsed %d servers, want 1", len(servers))
			}
			if servers[0].Name != test.wantName {
				t.Errorf("name is %q, want %q", servers[0].Name, test.wantName)
			}
			if servers[0].Transport != test.wantType {
				t.Errorf("transport is %q, want %q", servers[0].Transport, test.wantType)
			}
		})
	}
}

func TestParseSingleStringArguments(t *testing.T) {
	servers, err := ParseMCPConfig([]byte(`{"solo": {"command": "uvx", "args": "mcp-server-time"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(servers[0].Args) != 1 || servers[0].Args[0] != "mcp-server-time" {
		t.Errorf("arguments are %q, want one entry", servers[0].Args)
	}
}

func TestParseNamesAServerThatHasNone(t *testing.T) {
	servers, err := ParseMCPConfig([]byte(`{"url": "https://example.com/mcp"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if servers[0].Name != "example.com" {
		t.Errorf("the server is named %q, want the host", servers[0].Name)
	}
}

func TestParseReadsAnExplicitEnabledFlag(t *testing.T) {
	servers, err := ParseMCPConfig([]byte(`{"off": {"command": "npx", "enabled": false}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if servers[0].Enabled {
		t.Error("the server was switched on despite enabled:false")
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		mustSay string
	}{
		{"nothing at all", "", "nothing to import"},
		{"not an object", "[1, 2]", "should be a JSON object"},
		{"not JSON", "{oops", "not valid JSON"},
		{"an empty map", `{}`, "no servers"},
		{"no servers in the wrapper", `{"mcpServers": {}}`, "no servers"},
		{"a server that is not an object", `{"mcpServers": {"x": 5}}`, "x"},
		{"a stdio server with no command", `{"mcpServers": {"x": {"type": "stdio"}}}`, "command"},
		{"a remote server with no url", `{"mcpServers": {"x": {"type": "sse"}}}`, "URL"},
		{"an unknown transport", `{"mcpServers": {"x": {"type": "carrier-pigeon", "url": "https://e.com"}}}`, "carrier-pigeon"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseMCPConfig([]byte(test.config))
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if !strings.Contains(err.Error(), test.mustSay) {
				t.Errorf("the error is %q, which does not mention %q", err, test.mustSay)
			}
		})
	}
}

func TestImportAddsAndUpdates(t *testing.T) {
	cfg := &Config{Servers: []ServerConfig{{
		ID: "keep", Name: "Filesystem", Alias: "filesystem",
		Transport: TransportStdio, Command: "old-command", Enabled: false,
	}}}

	result := cfg.Import([]ServerConfig{
		{Name: "filesystem", Transport: TransportStdio, Command: "npx", Enabled: true},
		{Name: "GitHub", Transport: TransportHTTP, URL: "https://example.com/mcp", Enabled: true},
	})

	if len(result.Added) != 1 || result.Added[0] != "GitHub" {
		t.Errorf("added is %q, want [GitHub]", result.Added)
	}
	if len(result.Updated) != 1 || result.Updated[0] != "filesystem" {
		t.Errorf("updated is %q, want [filesystem]", result.Updated)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("the configuration holds %d servers, want 2", len(cfg.Servers))
	}

	replaced, _ := cfg.Server("keep")
	if replaced.Command != "npx" || !replaced.Enabled {
		t.Errorf("the server was not replaced: %+v", replaced)
	}
	if replaced.Alias != "filesystem" {
		t.Errorf("the alias changed to %q, which would rename the tools clients hold", replaced.Alias)
	}

	added, ok := cfg.serverByName("GitHub")
	if !ok {
		t.Fatal("the new server was not added")
	}
	if added.ID == "" || added.ID == "keep" {
		t.Errorf("the new server got id %q", added.ID)
	}
	if added.Alias != "github" {
		t.Errorf("the new server's alias is %q", added.Alias)
	}
}

func TestImportKeepsAliasesDistinct(t *testing.T) {
	cfg := Default()
	cfg.Import([]ServerConfig{
		{Name: "A B", Transport: TransportStdio, Command: "npx"},
		{Name: "A-B", Transport: TransportStdio, Command: "npx"},
	})
	if cfg.Servers[0].Alias == cfg.Servers[1].Alias {
		t.Errorf("both servers are aliased %q", cfg.Servers[0].Alias)
	}
	if cfg.Servers[0].Alias != "a_b" || cfg.Servers[1].Alias != "a_b_2" {
		t.Errorf("aliases are %q and %q", cfg.Servers[0].Alias, cfg.Servers[1].Alias)
	}
}

func TestImportResultSummary(t *testing.T) {
	tests := []struct {
		result ImportResult
		want   string
	}{
		{ImportResult{}, "Nothing was imported"},
		{ImportResult{Added: []string{"a"}}, "Imported 1 server"},
		{ImportResult{Added: []string{"a", "b", "c"}}, "Imported 3 servers"},
		{ImportResult{Updated: []string{"a", "b"}}, "Updated 2 servers"},
		{ImportResult{Added: []string{"a"}, Updated: []string{"b", "c"}}, "Imported 1 server and updated 2 servers"},
	}
	for _, test := range tests {
		if got := test.result.Summary(); got != test.want {
			t.Errorf("Summary() is %q, want %q", got, test.want)
		}
	}
}

// contains reports whether a slice holds a value.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
