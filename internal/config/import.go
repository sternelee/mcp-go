package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
)

// containerKeys are the wrappers MCP clients put their servers under.
var containerKeys = []string{"mcpServers", "servers", "mcp"}

// serverKeys are the fields that mark an object as one server rather than a map
// of them keyed by name.
var serverKeys = []string{"command", "url", "type", "transport", "args", "env", "headers", "oauth"}

// ParseMCPConfig reads the configuration MCP clients write, so a block can be
// pasted in as it stands. It accepts a {"mcpServers": {…}} wrapper, a
// {"servers": {…}} wrapper, a bare map of name to server, and a single server
// object on its own.
func ParseMCPConfig(data []byte) ([]ServerConfig, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("there is nothing to import")
	}
	if trimmed[0] != '{' {
		return nil, errors.New("the configuration should be a JSON object")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, fmt.Errorf("that is not valid JSON: %w", err)
	}
	if len(fields) == 0 {
		return nil, errors.New("no servers were found in that configuration")
	}

	for _, key := range containerKeys {
		if inner, ok := fields[key]; ok {
			return parseServerMap(inner)
		}
	}
	if hasAnyKey(fields, serverKeys) {
		// One server, pasted without the map around it.
		server, err := parseServer("", trimmed)
		if err != nil {
			return nil, err
		}
		return []ServerConfig{server}, nil
	}
	return parseServerMap(trimmed)
}

// ImportResult names what an import changed.
type ImportResult struct {
	Added   []string
	Updated []string
}

// Summary describes an import in one line.
func (r ImportResult) Summary() string {
	switch {
	case len(r.Added) == 0 && len(r.Updated) == 0:
		return "Nothing was imported"
	case len(r.Updated) == 0:
		return "Imported " + count(len(r.Added), "server")
	case len(r.Added) == 0:
		return "Updated " + count(len(r.Updated), "server")
	default:
		return fmt.Sprintf("Imported %s and updated %s", count(len(r.Added), "server"), count(len(r.Updated), "server"))
	}
}

// Import adds servers, replacing any that share a name with one already
// configured. A replaced server keeps its identity and its alias, so the tool
// names clients already hold stay the same.
func (c *Config) Import(servers []ServerConfig) ImportResult {
	var result ImportResult
	for _, sc := range servers {
		if existing, found := c.serverByName(sc.Name); found {
			sc.ID = existing.ID
			sc.Alias = existing.Alias
			result.Updated = append(result.Updated, sc.Name)
		} else {
			sc.ID = NewID()
			result.Added = append(result.Added, sc.Name)
		}
		c.Upsert(sc)
	}
	c.EnsureAliases()
	return result
}

// serverByName finds a server by its display name, ignoring case.
func (c *Config) serverByName(name string) (ServerConfig, bool) {
	for _, sc := range c.Servers {
		if strings.EqualFold(sc.Name, name) {
			return sc, true
		}
	}
	return ServerConfig{}, false
}

// parseServerMap reads an object keyed by server name.
func parseServerMap(data []byte) ([]ServerConfig, error) {
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, errors.New("the servers should be a JSON object keyed by name")
	}
	if len(entries) == 0 {
		return nil, errors.New("no servers were found in that configuration")
	}

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	servers := make([]ServerConfig, 0, len(entries))
	for _, name := range names {
		server, err := parseServer(name, entries[name])
		if err != nil {
			return nil, fmt.Errorf("%q: %w", name, err)
		}
		servers = append(servers, server)
	}
	return servers, nil
}

// parseServer reads one server, taking its name from the key it was under.
func parseServer(name string, data []byte) (ServerConfig, error) {
	var raw struct {
		Name      string                     `json:"name"`
		Command   string                     `json:"command"`
		Args      stringList                 `json:"args"`
		Env       map[string]json.RawMessage `json:"env"`
		URL       string                     `json:"url"`
		Type      string                     `json:"type"`
		Transport string                     `json:"transport"`
		Headers   map[string]json.RawMessage `json:"headers"`
		OAuth     bool                       `json:"oauth"`
		Enabled   *bool                      `json:"enabled"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ServerConfig{}, errors.New("that is not a server object")
	}

	sc := ServerConfig{
		Name:    strings.TrimSpace(firstNonEmpty(name, raw.Name)),
		Command: strings.TrimSpace(raw.Command),
		Args:    raw.Args,
		Env:     keyValues(raw.Env, "="),
		URL:     strings.TrimSpace(raw.URL),
		Headers: textMap(raw.Headers),
		OAuth:   raw.OAuth,
		// A configuration the user pastes is one they want to use.
		Enabled: true,
	}
	if raw.Enabled != nil {
		sc.Enabled = *raw.Enabled
	}

	// The transport may be given, or inferred from what the server has.
	switch kind := strings.ToLower(firstNonEmpty(raw.Type, raw.Transport)); kind {
	case "", "stdio", "command":
		switch {
		case sc.Command != "":
			sc.Transport = TransportStdio
		case kind == "" && sc.URL != "":
			// A remote server with no kind is streamable HTTP, the transport
			// current clients default to.
			sc.Transport = TransportHTTP
		default:
			return ServerConfig{}, errors.New("a stdio server needs a command")
		}
	case "sse", "event-stream":
		sc.Transport = TransportSSE
	case "http", "streamable-http", "streamablehttp", "streamable":
		sc.Transport = TransportHTTP
	default:
		return ServerConfig{}, fmt.Errorf("unknown transport %q", kind)
	}

	if sc.Name == "" {
		sc.Name = defaultName(sc)
	}
	if err := sc.Validate(); err != nil {
		return ServerConfig{}, err
	}
	return sc, nil
}

// defaultName names a server that arrived without one.
func defaultName(sc ServerConfig) string {
	if sc.Command != "" {
		switch base := filepath.Base(sc.Command); base {
		case "", ".", "/", "\\":
		default:
			return base
		}
	}
	if parsed, err := url.Parse(sc.URL); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return "Imported server"
}

// stringList accepts either a list of strings or a single string, since
// configurations do both.
type stringList []string

func (l *stringList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var one string
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return err
		}
		if one != "" {
			*l = []string{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return errors.New("should be a list of strings")
	}
	*l = many
	return nil
}

// textMap reads an object whose values are strings, numbers or booleans.
func textMap(raw map[string]json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		if key = strings.TrimSpace(key); key != "" {
			out[key] = text(value)
		}
	}
	return out
}

// keyValues writes an object's entries as "KEY=VALUE", in a stable order.
func keyValues(raw map[string]json.RawMessage, separator string) []string {
	if len(raw) == 0 {
		return nil
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(raw))
	for _, key := range keys {
		out = append(out, key+separator+text(raw[key]))
	}
	return out
}

// text renders a JSON value as the text it stands for, without inventing
// quotes around numbers and booleans.
func text(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) >= 2 && trimmed[0] == '"' {
		var unquoted string
		if err := json.Unmarshal(raw, &unquoted); err == nil {
			return unquoted
		}
	}
	if trimmed == "null" {
		return ""
	}
	return trimmed
}

// hasAnyKey reports whether m has any of the keys.
func hasAnyKey(m map[string]json.RawMessage, keys []string) bool {
	for _, key := range keys {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

// firstNonEmpty returns the first value that is not blank.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// count writes "1 server" or "3 servers".
func count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
