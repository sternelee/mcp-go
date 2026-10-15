// Package config holds the gateway's persisted settings and the credential
// store its OAuth flows write to.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo"
)

// Transport is how the gateway reaches an upstream MCP server.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportSSE   Transport = "sse"
	TransportHTTP  Transport = "http"
)

// Valid reports whether t is a transport the gateway can connect with.
func (t Transport) Valid() bool {
	switch t {
	case TransportStdio, TransportSSE, TransportHTTP:
		return true
	}
	return false
}

// Label is the transport's name for the interface.
func (t Transport) Label() string {
	switch t {
	case TransportStdio:
		return "stdio"
	case TransportSSE:
		return "SSE"
	case TransportHTTP:
		return "HTTP"
	}
	return string(t)
}

// InboundMode is how callers of the gateway prove who they are.
type InboundMode string

const (
	// InboundOff leaves the gateway open to anything that can reach its port.
	InboundOff InboundMode = "off"
	// InboundToken requires the bearer token the user set.
	InboundToken InboundMode = "token"
	// InboundOAuth requires a bearer token, and lets MCP clients obtain one by
	// signing in, the gateway acting as its own authorization server.
	InboundOAuth InboundMode = "oauth"
)

// Valid reports whether m is a mode the gateway can serve in.
func (m InboundMode) Valid() bool {
	switch m {
	case InboundOff, InboundToken, InboundOAuth:
		return true
	}
	return false
}

// Label is the mode's name for the interface.
func (m InboundMode) Label() string {
	switch m {
	case InboundToken:
		return "Token"
	case InboundOAuth:
		return "Token + OAuth"
	}
	return "Off"
}

// RequiresToken reports whether callers must present a credential.
func (m InboundMode) RequiresToken() bool { return m == InboundToken || m == InboundOAuth }

// DefaultPort is where the gateway serves when the user has not chosen another.
const DefaultPort = 7331

// ServerConfig describes one upstream MCP server.
type ServerConfig struct {
	ID        string    `json:"id"`    // stable identity, survives renames
	Name      string    `json:"name"`  // what the interface shows
	Alias     string    `json:"alias"` // namespaces this server's tools
	Transport Transport `json:"transport"`
	Enabled   bool      `json:"enabled"` // connect on launch

	// stdio upstreams.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"` // "KEY=VALUE"

	// SSE and streamable HTTP upstreams.
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"` // static, non-secret
	OAuth   bool              `json:"oauth,omitempty"`   // sign in with OAuth 2.1

	// A static credential, for the servers that take a token rather than
	// running an OAuth flow. The token itself is kept in the credential store;
	// only the header it goes in is written down here.
	TokenHeader string `json:"token_header,omitempty"` // e.g. "Authorization"
	TokenScheme string `json:"token_scheme,omitempty"` // e.g. "Bearer"
}

// Validate reports why the server cannot be connected, or nil.
func (s ServerConfig) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("name is required")
	}
	if !s.Transport.Valid() {
		return fmt.Errorf("unknown transport %q", s.Transport)
	}
	if s.Transport == TransportStdio {
		if strings.TrimSpace(s.Command) == "" {
			return errors.New("command is required for stdio servers")
		}
		if s.OAuth || s.TokenHeader != "" {
			return errors.New("signing in is only for SSE and HTTP servers")
		}
		return nil
	}
	if strings.TrimSpace(s.URL) == "" {
		return fmt.Errorf("URL is required for %s servers", s.Transport.Label())
	}
	if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
		return errors.New("URL must start with http:// or https://")
	}
	if s.OAuth && s.TokenHeader != "" {
		return errors.New("a server signs in with OAuth or with a token, not both")
	}
	if s.TokenHeader != "" && !validHeaderName(s.TokenHeader) {
		return fmt.Errorf("%q is not a valid header name", s.TokenHeader)
	}
	return nil
}

// validHeaderName reports whether s can be sent as an HTTP header name.
func validHeaderName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return s != ""
}

// Config is everything the app persists outside the credential store.
type Config struct {
	Port    int            `json:"port"`
	Servers []ServerConfig `json:"servers"`
	// Inbound is how callers of the gateway prove who they are.
	Inbound InboundMode `json:"inbound,omitempty"`
}

// Default is the configuration a fresh install starts from.
func Default() *Config {
	return &Config{Port: DefaultPort, Inbound: InboundOff}
}

// InboundMode returns the configured mode, defaulting to open.
func (c *Config) InboundMode() InboundMode {
	if !c.Inbound.Valid() {
		return InboundOff
	}
	return c.Inbound
}

// Server returns the server with id, and whether it was found.
func (c *Config) Server(id string) (ServerConfig, bool) {
	for _, s := range c.Servers {
		if s.ID == id {
			return s, true
		}
	}
	return ServerConfig{}, false
}

// Upsert adds sc, or replaces the server that shares its ID.
func (c *Config) Upsert(sc ServerConfig) {
	for i := range c.Servers {
		if c.Servers[i].ID == sc.ID {
			c.Servers[i] = sc
			return
		}
	}
	c.Servers = append(c.Servers, sc)
}

// Remove deletes the server with id and reports whether it was there.
func (c *Config) Remove(id string) bool {
	for i := range c.Servers {
		if c.Servers[i].ID == id {
			c.Servers = append(c.Servers[:i], c.Servers[i+1:]...)
			return true
		}
	}
	return false
}

// EnsureAliases gives every server a unique, sanitized alias and returns true
// when it changed one. Aliases name the tools a server contributes, so two
// servers sharing one would collide downstream.
func (c *Config) EnsureAliases() bool {
	changed := false
	taken := map[string]bool{}
	for i := range c.Servers {
		s := &c.Servers[i]
		alias := s.Alias
		if alias == "" {
			alias = s.Name
		}
		alias = aliasFor(alias)
		if taken[alias] {
			alias = uniqueAlias(alias, taken)
		}
		taken[alias] = true
		if alias != s.Alias {
			s.Alias = alias
			changed = true
		}
	}
	return changed
}

// aliasFor turns a display name into the alias tools are prefixed with.
func aliasFor(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '.':
			b.WriteByte('_')
		}
	}
	alias := strings.Trim(b.String(), "_")
	if alias == "" {
		return "server"
	}
	return alias
}

func uniqueAlias(base string, taken map[string]bool) string {
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s_%d", base, n)
		if !taken[candidate] {
			taken[candidate] = true
			return candidate
		}
	}
}

// Path is where the configuration is stored.
func Path() (string, error) {
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "servers.json"), nil
}

// NewID returns an identifier for a server that does not have one yet.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("server-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Load reads the saved configuration, or the default one when none is saved.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	cfg.EnsureAliases()
	return cfg, nil
}

// Save writes the configuration, replacing the previous one atomically.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "servers-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
