package config

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/egoist/mygo"
	"github.com/mark3labs/mcp-go/client/transport"
)

// TestMain keeps the tests' configuration out of the real user directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mcp-go-config-test")
	if err != nil {
		panic(err)
	}
	mygo.App.SetPath(mygo.PathUserData, dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestEnsureAliasesAreSafeAndUnique(t *testing.T) {
	cfg := &Config{Servers: []ServerConfig{
		{ID: "a", Name: "My Server!"},
		{ID: "b", Name: "My Server!"},
		{ID: "c", Name: "  "},
		{ID: "d", Name: "Filesystem", Alias: "filesystem"},
	}}
	if !cfg.EnsureAliases() {
		t.Error("aliases were filled in but EnsureAliases reported no change")
	}

	want := map[string]string{"a": "my_server", "b": "my_server_2", "c": "server", "d": "filesystem"}
	for _, sc := range cfg.Servers {
		if sc.Alias != want[sc.ID] {
			t.Errorf("server %s has alias %q, want %q", sc.ID, sc.Alias, want[sc.ID])
		}
	}

	// Running it again changes nothing.
	if cfg.EnsureAliases() {
		t.Error("EnsureAliases is not idempotent")
	}
}

func TestEnsureAliasesKeepsADistinctExistingAlias(t *testing.T) {
	cfg := &Config{Servers: []ServerConfig{
		{ID: "a", Name: "GitHub", Alias: "gh"},
		{ID: "b", Name: "GitLab"},
	}}
	cfg.EnsureAliases()
	if cfg.Servers[0].Alias != "gh" {
		t.Errorf("the chosen alias became %q", cfg.Servers[0].Alias)
	}
	if cfg.Servers[1].Alias != "gitlab" {
		t.Errorf("gitlab's alias is %q", cfg.Servers[1].Alias)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	cfg := &Config{Port: 7444, Servers: []ServerConfig{{
		ID: "s1", Name: "Filesystem", Transport: TransportStdio,
		Command: "npx", Args: []string{"-y", "server"},
		Env:     []string{"A=B"},
		Enabled: true,
	}}}
	cfg.EnsureAliases()
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Port != 7444 {
		t.Errorf("port is %d, want 7444", loaded.Port)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("loaded %d servers, want 1", len(loaded.Servers))
	}
	got := loaded.Servers[0]
	if got.Name != "Filesystem" || got.Command != "npx" || len(got.Args) != 2 || !got.Enabled {
		t.Errorf("loaded %+v", got)
	}
	if got.Alias != "filesystem" {
		t.Errorf("alias is %q", got.Alias)
	}
}

func TestLoadFallsBackToDefaults(t *testing.T) {
	path, err := Path()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	_ = os.Remove(path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("port is %d, want %d", cfg.Port, DefaultPort)
	}
	if len(cfg.Servers) != 0 {
		t.Errorf("a fresh configuration has %d servers", len(cfg.Servers))
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		server  ServerConfig
		wantErr bool
	}{
		{"stdio is complete", ServerConfig{Name: "a", Transport: TransportStdio, Command: "npx"}, false},
		{"stdio needs a command", ServerConfig{Name: "a", Transport: TransportStdio}, true},
		{"a name is required", ServerConfig{Transport: TransportStdio, Command: "npx"}, true},
		{"the transport must be known", ServerConfig{Name: "a", Transport: "carrier-pigeon"}, true},
		{"http is complete", ServerConfig{Name: "a", Transport: TransportHTTP, URL: "https://example.com/mcp"}, false},
		{"http needs a URL", ServerConfig{Name: "a", Transport: TransportHTTP}, true},
		{"http needs a real scheme", ServerConfig{Name: "a", Transport: TransportHTTP, URL: "example.com"}, true},
		{"sse is complete", ServerConfig{Name: "a", Transport: TransportSSE, URL: "http://example.com/sse", OAuth: true}, false},
		{
			"a token is fine on a remote server",
			ServerConfig{Name: "a", Transport: TransportHTTP, URL: "https://example.com/mcp", TokenHeader: "X-API-Key"},
			false,
		},
		{
			"signing in and a token conflict",
			ServerConfig{Name: "a", Transport: TransportHTTP, URL: "https://example.com/mcp", OAuth: true, TokenHeader: "Authorization"},
			true,
		},
		{
			"a header name must be sendable",
			ServerConfig{Name: "a", Transport: TransportHTTP, URL: "https://example.com/mcp", TokenHeader: "Not A Header"},
			true,
		},
		{
			"a stdio server takes its credentials in the environment",
			ServerConfig{Name: "a", Transport: TransportStdio, Command: "npx", TokenHeader: "Authorization"},
			true,
		},
		{
			"a stdio server has nothing to sign in to",
			ServerConfig{Name: "a", Transport: TransportStdio, Command: "npx", OAuth: true},
			true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.server.Validate()
			if test.wantErr && err == nil {
				t.Error("want an error, got none")
			}
			if !test.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestServerLookupAndRemove(t *testing.T) {
	cfg := &Config{Servers: []ServerConfig{{ID: "a"}, {ID: "b"}}}

	if _, ok := cfg.Server("a"); !ok {
		t.Error("server a is missing")
	}
	if _, ok := cfg.Server("zzz"); ok {
		t.Error("an unknown id was found")
	}

	cfg.Upsert(ServerConfig{ID: "a", Name: "renamed"})
	if len(cfg.Servers) != 2 {
		t.Errorf("upsert added a server: %d", len(cfg.Servers))
	}
	if sc, _ := cfg.Server("a"); sc.Name != "renamed" {
		t.Errorf("upsert did not replace: %+v", sc)
	}

	if !cfg.Remove("a") {
		t.Error("removing a server failed")
	}
	if cfg.Remove("a") {
		t.Error("removing a server twice should report it was not there")
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].ID != "b" {
		t.Errorf("after removal the servers are %+v", cfg.Servers)
	}
}

func TestClientCredentialsRoundTrip(t *testing.T) {
	store := NewMemoryCredentialStore()
	want := ClientCredentials{ClientID: "cid", Secret: "sec", Issuer: "https://as.example", AuthMethod: "none"}
	if err := store.SaveClient("s1", want); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := store.Client("s1"); got != want {
		t.Errorf("client credentials are %+v, want %+v", got, want)
	}
	if got := store.Client("s2"); got != (ClientCredentials{}) {
		t.Errorf("an unknown server has credentials %+v", got)
	}
}

func TestTokenStoreStartsEmptyAndForgets(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryCredentialStore()
	tokens := store.TokenStore("s1")

	if _, err := tokens.GetToken(ctx); !errors.Is(err, transport.ErrNoToken) {
		t.Errorf("an empty store returned %v, want transport.ErrNoToken", err)
	}
	if store.SignedIn("s1") {
		t.Error("a server with no token reports being signed in")
	}

	if err := tokens.SaveToken(ctx, &transport.Token{
		AccessToken: "access", TokenType: "Bearer", RefreshToken: "refresh", Issuer: "https://as.example",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := tokens.GetToken(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AccessToken != "access" || got.RefreshToken != "refresh" {
		t.Errorf("token is %+v", got)
	}
	if got.Issuer != "https://as.example" {
		t.Errorf("the issuer was not preserved: %q", got.Issuer)
	}
	if !store.SignedIn("s1") {
		t.Error("a server with a token does not report being signed in")
	}

	if err := store.SignOut("s1"); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	if _, err := tokens.GetToken(ctx); !errors.Is(err, transport.ErrNoToken) {
		t.Errorf("after signing out GetToken returned %v, want transport.ErrNoToken", err)
	}
}

func TestTokenStoreHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewMemoryCredentialStore()
	if _, err := store.TokenStore("s1").GetToken(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("GetToken returned %v, want context.Canceled", err)
	}
	if err := store.TokenStore("s1").SaveToken(ctx, &transport.Token{}); !errors.Is(err, context.Canceled) {
		t.Errorf("SaveToken returned %v, want context.Canceled", err)
	}
}

func TestTransportLabels(t *testing.T) {
	if TransportSSE.Label() != "SSE" || TransportHTTP.Label() != "HTTP" || TransportStdio.Label() != "stdio" {
		t.Errorf("labels are %q, %q, %q",
			TransportStdio.Label(), TransportSSE.Label(), TransportHTTP.Label())
	}
	if Transport("carrier-pigeon").Valid() {
		t.Error("an unknown transport is reported as valid")
	}
}

func TestUpstreamTokenIsKeptAndForgotten(t *testing.T) {
	store := NewMemoryCredentialStore()

	if store.HasUpstreamToken("s1") {
		t.Error("a fresh store reports a token it does not have")
	}
	if got := store.UpstreamToken("s1"); got != "" {
		t.Errorf("a fresh store returned %q", got)
	}

	if err := store.SetUpstreamToken("s1", "ghp_secret"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := store.UpstreamToken("s1"); got != "ghp_secret" {
		t.Errorf("the token came back as %q", got)
	}
	if !store.HasUpstreamToken("s1") {
		t.Error("a saved token is not reported")
	}
	if store.UpstreamToken("s2") != "" {
		t.Error("one server's token turned up under another's id")
	}

	// An empty value means forget, so a form can clear it.
	if err := store.SetUpstreamToken("s1", "   "); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if store.HasUpstreamToken("s1") {
		t.Error("a blank token did not clear the stored one")
	}
}

func TestInboundTokenIsKept(t *testing.T) {
	store := NewMemoryCredentialStore()
	if got := store.InboundToken(); got != "" {
		t.Errorf("a fresh store has inbound token %q", got)
	}
	if err := store.SetInboundToken("callers-must-present-this"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := store.InboundToken(); got != "callers-must-present-this" {
		t.Errorf("the token came back as %q", got)
	}
	if err := store.SetInboundToken(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := store.InboundToken(); got != "" {
		t.Errorf("clearing left %q", got)
	}
}

func TestInboundModeDefaultsToOpen(t *testing.T) {
	cfg := Default()
	if cfg.InboundMode() != InboundOff {
		t.Errorf("a fresh configuration is in mode %q", cfg.InboundMode())
	}
	cfg.Inbound = InboundMode("something-else")
	if cfg.InboundMode() != InboundOff {
		t.Errorf("an unknown mode was accepted as %q", cfg.InboundMode())
	}
	cfg.Inbound = InboundOAuth
	if cfg.InboundMode() != InboundOAuth {
		t.Errorf("a set mode came back as %q", cfg.InboundMode())
	}
}

func TestInboundModeLabelsAndTokenRequirement(t *testing.T) {
	if InboundOff.RequiresToken() || InboundOAuth.RequiresToken() == false || InboundToken.RequiresToken() == false {
		t.Error("modes disagree about whether they need a token")
	}
	if InboundOff.Label() != "Off" || InboundOAuth.Label() != "Token + OAuth" {
		t.Errorf("labels are %q and %q", InboundOff.Label(), InboundOAuth.Label())
	}
	if InboundMode("nonsense").Valid() {
		t.Error("an unknown mode is reported as valid")
	}
}

func TestInboundModeSurvivesARoundTrip(t *testing.T) {
	cfg := &Config{Port: 7331, Inbound: InboundOAuth}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.InboundMode() != InboundOAuth {
		t.Errorf("the mode came back as %q", loaded.InboundMode())
	}
}
