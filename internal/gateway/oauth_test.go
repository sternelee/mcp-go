package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"mcp-go/internal/config"
)

// authServer is a minimal OAuth 2.1 authorization server and protected
// resource, enough for the gateway's flow: discovery, dynamic registration, a
// token endpoint, and an MCP endpoint that answers only with the token.
type authServer struct {
	*httptest.Server

	mu        sync.Mutex
	clientID  string
	lastToken string
	challenge string
}

const authToken = "test-access-token"

func newAuthServer(t *testing.T) *authServer {
	t.Helper()

	mcpSrv := server.NewMCPServer("protected", "test", server.WithToolCapabilities(true))
	mcpSrv.AddTool(mcp.NewTool("echo", mcp.WithDescription("Echo the text back.")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(req.GetString("text", "")), nil
		})
	stream := server.NewStreamableHTTPServer(mcpSrv)

	as := &authServer{}

	mux := http.NewServeMux()
	base := func() string { return as.URL }

	// RFC 9728 protected resource metadata, naming itself as the resource.
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              base() + "/mcp",
			"authorization_servers": []string{base()},
		})
	})

	// RFC 8414 authorization server metadata.
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                base(),
			"authorization_endpoint":                base() + "/authorize",
			"token_endpoint":                        base() + "/token",
			"registration_endpoint":                 base() + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})

	// Dynamic client registration.
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		as.mu.Lock()
		as.clientID = "registered-client"
		as.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{
			"client_id":                  "registered-client",
			"token_endpoint_auth_method": "none",
		})
	})

	// The authorization endpoint is never fetched; the test plays the browser.
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the test drives this step itself", http.StatusTeapot)
	})

	// Token exchange.
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		as.mu.Lock()
		as.challenge = r.Form.Get("code_challenge")
		as.lastToken = "code=" + r.Form.Get("code")
		as.mu.Unlock()
		writeJSON(w, map[string]any{
			"access_token": authToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})

	// The MCP endpoint, which insists on the token.
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+authToken {
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+base()+`/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		stream.ServeHTTP(w, r)
	}))

	as.Server = httptest.NewServer(mux)
	t.Cleanup(as.Close)
	return as
}

func TestOAuthFlowSignsInAndConnects(t *testing.T) {
	as := newAuthServer(t)
	sc := config.ServerConfig{
		ID: "s1", Name: "Protected", Alias: "protected",
		Transport: config.TransportHTTP,
		URL:       as.URL + "/mcp",
		OAuth:     true,
		Enabled:   true,
	}
	g := newTestGateway(t, sc)
	ctx := context.Background()
	// The browser is sent back to the gateway's own callback, so it has to be
	// serving before a sign-in can start.
	startServing(t, g)

	authURL, err := g.BeginAuth(ctx, sc.ID)
	if err != nil {
		t.Fatalf("begin auth: %v", err)
	}
	if authURL == "" {
		t.Fatal("no authorization URL was built")
	}

	// The gateway should have registered itself before sending the user away.
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse the authorization URL: %v", err)
	}
	if got := parsed.Query().Get("client_id"); got != "registered-client" {
		t.Errorf("client_id is %q, want %q", got, "registered-client")
	}
	if parsed.Query().Get("code_challenge") == "" {
		t.Error("the authorization URL carries no PKCE challenge")
	}
	if got := g.ClientCredentials(sc.ID).ClientID; got != "registered-client" {
		t.Errorf("the registered client was not kept: %q", got)
	}

	// Play the browser coming back to the gateway's callback.
	state := parsed.Query().Get("state")
	recorder := httptest.NewRecorder()
	g.handleOAuthCallback(recorder, httptest.NewRequest(http.MethodGet,
		"/oauth/callback?code=the-code&state="+url.QueryEscape(state), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the callback answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Signed in") {
		t.Errorf("the callback page says %q", recorder.Body.String())
	}
	if !g.creds.SignedIn(sc.ID) {
		t.Error("the token was not stored")
	}

	// The callback connects the server in the background, so wait for it
	// rather than racing it.
	client := downstream(t, g)
	waitFor(t, "the signed-in server to connect", func() bool {
		view, ok := g.Server(sc.ID)
		return ok && view.Status == StatusReady
	})
	waitFor(t, "the protected server's tools to be served", func() bool {
		return listNames(t, client)["protected__echo"]
	})
}

// waitFor polls until check is satisfied, or fails the test.
func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAServerThatAsksForAuthIsRecognizedWithoutOAuthConfigured(t *testing.T) {
	as := newAuthServer(t)
	sc := config.ServerConfig{
		ID: "s1", Name: "Protected", Alias: "protected",
		Transport: config.TransportHTTP,
		URL:       as.URL + "/mcp",
		Enabled:   true,
		// The configuration never says the server uses OAuth; the server says so
		// itself, by answering 401.
	}
	g := newTestGateway(t, sc)
	startServing(t, g)

	view, ok := g.Server(sc.ID)
	if !ok {
		t.Fatal("the server is missing from the view")
	}
	if view.Status != StatusNeedsAuth {
		t.Fatalf("status is %q, want %q (error %q)", view.Status, StatusNeedsAuth, view.Err)
	}

	// The sign-in can still be started, so the interface has something to offer
	// even though the configuration never marked the server as OAuth.
	authURL, err := g.BeginAuth(context.Background(), sc.ID)
	if err != nil {
		t.Fatalf("begin auth: %v", err)
	}
	if authURL == "" {
		t.Fatal("no authorization URL was built")
	}
}

func TestSigningOutForgetsTheTokenAndAsksAgain(t *testing.T) {
	as := newAuthServer(t)
	sc := config.ServerConfig{
		ID: "s1", Name: "Protected", Alias: "protected",
		Transport: config.TransportHTTP,
		URL:       as.URL + "/mcp",
		OAuth:     true,
		Enabled:   true,
	}
	g := newTestGateway(t, sc)
	startServing(t, g)

	// Sign in the way the browser would.
	authURL, err := g.BeginAuth(context.Background(), sc.ID)
	if err != nil {
		t.Fatalf("begin auth: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse the authorization URL: %v", err)
	}
	recorder := httptest.NewRecorder()
	g.handleOAuthCallback(recorder, httptest.NewRequest(http.MethodGet,
		"/oauth/callback?code=the-code&state="+url.QueryEscape(parsed.Query().Get("state")), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the callback answered %d: %s", recorder.Code, recorder.Body.String())
	}
	waitFor(t, "the signed-in server to connect", func() bool {
		view, ok := g.Server(sc.ID)
		return ok && view.Status == StatusReady
	})

	// Disconnect, which is what the row's button offers: the saved token goes
	// and the server asks to be authorized again.
	if err := g.SignOut(context.Background(), sc.ID); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	if g.creds.SignedIn(sc.ID) {
		t.Error("the token is still saved after signing out")
	}
	waitFor(t, "the server to ask for authorization again", func() bool {
		view, ok := g.Server(sc.ID)
		return ok && view.Status == StatusNeedsAuth
	})
}

func TestBeginAuthRejectsAServerWithoutOAuth(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	if _, err := g.BeginAuth(context.Background(), sc.ID); err == nil {
		t.Error("a stdio server should not be able to begin an OAuth flow")
	}
}

func TestBeginAuthUsesAdvertisedResourceMetadata(t *testing.T) {
	for _, kind := range []config.Transport{config.TransportHTTP, config.TransportSSE} {
		for _, configured := range []bool{false, true} {
			name := string(kind) + "/detected"
			if configured {
				name = string(kind) + "/configured"
			}
			t.Run(name, func(t *testing.T) {
				var base string
				var challenges, registrations, guessedRegistrations atomic.Int32
				as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/mcp":
						if r.Header.Get("X-Tenant") != "example" {
							http.Error(w, "missing tenant header", http.StatusBadRequest)
							return
						}
						challenges.Add(1)
						w.Header().Set("WWW-Authenticate",
							`Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
						w.WriteHeader(http.StatusUnauthorized)
					case "/.well-known/oauth-protected-resource":
						writeJSON(w, map[string]any{
							"resource":              base + "/api/mcp",
							"authorization_servers": []string{base},
						})
					case "/.well-known/oauth-authorization-server":
						writeJSON(w, map[string]any{
							"issuer":                                base,
							"authorization_endpoint":                base + "/oauth/authorize",
							"token_endpoint":                        base + "/oauth/token",
							"registration_endpoint":                 base + "/oauth/register",
							"token_endpoint_auth_methods_supported": []string{"none"},
						})
					case "/oauth/register":
						registrations.Add(1)
						w.WriteHeader(http.StatusCreated)
						writeJSON(w, map[string]any{
							"client_id":                  "public-client",
							"token_endpoint_auth_method": "none",
						})
					case "/register":
						guessedRegistrations.Add(1)
						w.Header().Set("Content-Type", "text/html")
						w.Write([]byte("<!doctype html><title>Not an OAuth endpoint</title>"))
					default:
						http.NotFound(w, r)
					}
				}))
				defer as.Close()
				base = as.URL
				sc := config.ServerConfig{
					ID: "s1", Name: "Nested", Alias: "nested",
					Transport: kind, URL: base + "/api/mcp",
					OAuth: configured, Enabled: true,
					Headers: map[string]string{"X-Tenant": "example"},
				}
				g := newTestGateway(t, sc)
				startServing(t, g)

				authURL, err := g.BeginAuth(context.Background(), sc.ID)
				if err != nil {
					t.Fatalf("begin auth: %v", err)
				}
				parsed, err := url.Parse(authURL)
				if err != nil {
					t.Fatal(err)
				}
				if parsed.Path != "/oauth/authorize" || parsed.Query().Get("client_id") != "public-client" {
					t.Errorf("authorization did not use discovered endpoints: %s", authURL)
				}
				if got := parsed.Query().Get("resource"); got != sc.URL {
					t.Errorf("resource = %q, want %q", got, sc.URL)
				}
				if challenges.Load() == 0 || registrations.Load() != 1 || guessedRegistrations.Load() != 0 {
					t.Errorf("challenges=%d, registrations=%d, guessed registrations=%d",
						challenges.Load(), registrations.Load(), guessedRegistrations.Load())
				}
			})
		}
	}
}

func TestDiscoverAuthChallengeDoesNotTrustAnotherOrigin(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		name := "metadata"
		if redirect {
			name = "redirect"
		}
		t.Run(name, func(t *testing.T) {
			var foreignRequests atomic.Int32
			foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				foreignRequests.Add(1)
				http.Error(w, "must not be reached", http.StatusInternalServerError)
			}))
			defer foreign.Close()
			trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/mcp" {
					http.NotFound(w, r)
					return
				}
				if redirect {
					http.Redirect(w, r, foreign.URL, http.StatusFound)
					return
				}
				w.Header().Set("WWW-Authenticate",
					`Bearer resource_metadata="`+foreign.URL+`/metadata"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer trusted.Close()
			sc := config.ServerConfig{URL: trusted.URL + "/api/mcp"}
			handler := transport.NewOAuthHandler(transport.OAuthConfig{})
			handler.SetBaseURL(sc.URL)
			if err := discoverAuthChallenge(context.Background(), sc, handler); err != nil {
				t.Fatal(err)
			}
			metadata, err := handler.GetServerMetadata(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Issuer != trusted.URL {
				t.Errorf("discovery left the trusted origin: %q", metadata.Issuer)
			}
			if foreignRequests.Load() != 0 {
				t.Errorf("sent %d requests to another origin", foreignRequests.Load())
			}
		})
	}
}

func TestCallbackRejectsAnUnknownSignIn(t *testing.T) {
	g := newTestGateway(t)
	recorder := httptest.NewRecorder()
	g.handleOAuthCallback(recorder, httptest.NewRequest(http.MethodGet,
		"/oauth/callback?code=x&state=never-started", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("the callback answered %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestCallbackReportsADeniedSignIn(t *testing.T) {
	g := newTestGateway(t)
	recorder := httptest.NewRecorder()
	g.handleOAuthCallback(recorder, httptest.NewRequest(http.MethodGet,
		"/oauth/callback?error=access_denied&error_description=The+user+said+no", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("the callback answered %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if !strings.Contains(recorder.Body.String(), "access_denied") {
		t.Errorf("the callback page does not say what happened: %q", recorder.Body.String())
	}
}
