package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"mcp-go/internal/config"
)

// noRedirect stops at the first response, so a test can read the redirect a
// handler sent rather than following it.
var noRedirect = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// requestAt makes a GET, with an optional bearer token, and returns the body.
func requestAt(t *testing.T, url, token string) (int, http.Header, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return response.StatusCode, response.Header, string(body)
}

// postForm posts a form and decodes a JSON response.
func postForm(t *testing.T, url string, form url.Values) (int, map[string]any) {
	t.Helper()
	response, err := noRedirect.PostForm(url, form)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &decoded)
	}
	return response.StatusCode, decoded
}

// postJSON posts a JSON body and decodes a JSON response.
func postJSON(t *testing.T, url string, payload any) (int, map[string]any, http.Header) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	response, err := http.Post(url, "application/json", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &decoded)
	}
	return response.StatusCode, decoded, response.Header
}

func TestGatewayRefusesCallersWithoutTheToken(t *testing.T) {
	g := newTestGateway(t)
	if err := g.SetInbound(config.InboundToken, "s3cret"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	base := startServing(t, g)

	for _, path := range []string{"/mcp", "/sse", "/healthz"} {
		status, headers, _ := requestAt(t, base+path, "")
		if status != http.StatusUnauthorized {
			t.Errorf("GET %s answered %d without a token, want 401", path, status)
		}
		// The challenge is what makes an MCP client start a sign-in.
		if challenge := headers.Get("WWW-Authenticate"); !strings.Contains(challenge, "/.well-known/oauth-protected-resource") {
			t.Errorf("GET %s challenged with %q", path, challenge)
		}
	}

	if status, _, _ := requestAt(t, base+"/healthz", "wrong"); status != http.StatusUnauthorized {
		t.Errorf("a wrong token answered %d, want 401", status)
	}
	if status, _, _ := requestAt(t, base+"/healthz", "s3cret"); status != http.StatusOK {
		t.Errorf("the right token answered %d, want 200", status)
	}

	// Discovery has to stay open, or a client could never obtain the token.
	if status, _, _ := requestAt(t, base+"/.well-known/oauth-protected-resource", ""); status != http.StatusOK {
		t.Errorf("the metadata endpoint answered %d, want it open", status)
	}
}

func TestGatewayIsOpenWhenAuthIsOff(t *testing.T) {
	g := newTestGateway(t)
	base := startServing(t, g)
	if status, _, _ := requestAt(t, base+"/healthz", ""); status != http.StatusOK {
		t.Errorf("healthz answered %d with auth off, want 200", status)
	}
}

func TestProtectedResourceMetadataNamesTheAuthorizationServerOnlyWithOAuth(t *testing.T) {
	g := newTestGateway(t)
	base := startServing(t, g)

	if err := g.SetInbound(config.InboundToken, "s3cret"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	status, _, body := requestAt(t, base+"/.well-known/oauth-protected-resource", "")
	if status != http.StatusOK {
		t.Fatalf("metadata answered %d", status)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(body), &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if _, ok := metadata["authorization_servers"]; ok {
		t.Error("a token-only gateway points callers at an authorization server it does not run")
	}

	// RFC 9728 wants the resource identifier to match the URL the client
	// addressed, so each endpoint's metadata declares its own.
	_, _, body = requestAt(t, base+"/.well-known/oauth-protected-resource/sse", "")
	if err := json.Unmarshal([]byte(body), &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if metadata["resource"] != base+"/sse" {
		t.Errorf("the sse metadata declares resource %v, want %q", metadata["resource"], base+"/sse")
	}

	if err := g.SetInbound(config.InboundOAuth, "s3cret"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	_, _, body = requestAt(t, base+"/.well-known/oauth-protected-resource", "")
	if err := json.Unmarshal([]byte(body), &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if metadata["resource"] != base+"/mcp" {
		t.Errorf("the mcp metadata declares resource %v, want %q", metadata["resource"], base+"/mcp")
	}
	servers, _ := metadata["authorization_servers"].([]any)
	if len(servers) != 1 || servers[0] != base {
		t.Errorf("authorization_servers is %v, want [%q]", metadata["authorization_servers"], base)
	}
}

func TestTheChallengePointsAtMetadataForTheEndpointAddressed(t *testing.T) {
	g := newTestGateway(t)
	if err := g.SetInbound(config.InboundToken, "s3cret"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	base := startServing(t, g)

	tests := map[string]string{
		"/mcp": "/.well-known/oauth-protected-resource/mcp",
		"/sse": "/.well-known/oauth-protected-resource/sse",
	}
	for path, want := range tests {
		_, headers, _ := requestAt(t, base+path, "")
		challenge := headers.Get("WWW-Authenticate")
		if !strings.Contains(challenge, base+want) {
			t.Errorf("GET %s challenged with %q, want it to name %q", path, challenge, base+want)
		}
	}
}

func TestAuthorizationServerIsOffUntilTheModeAsksForIt(t *testing.T) {
	g := newTestGateway(t)
	base := startServing(t, g)

	if status, _, _ := requestAt(t, base+"/.well-known/oauth-authorization-server", ""); status != http.StatusNotFound {
		t.Errorf("the authorization server answered %d with auth off, want 404", status)
	}

	if err := g.SetInbound(config.InboundOAuth, "s3cret"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	status, _, body := requestAt(t, base+"/.well-known/oauth-authorization-server", "")
	if status != http.StatusOK {
		t.Fatalf("the metadata answered %d, want 200", status)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(body), &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if metadata["issuer"] != base {
		t.Errorf("issuer is %v, want %q", metadata["issuer"], base)
	}
	if metadata["token_endpoint"] != base+"/oauth/token" {
		t.Errorf("token_endpoint is %v", metadata["token_endpoint"])
	}
	// The mode switch took effect without restarting, since these routes are
	// mounted whatever the mode.
	if !g.Serving() {
		t.Error("the gateway stopped serving when the mode changed")
	}
}

func TestGatewayIssuesTokensToSigningInClients(t *testing.T) {
	g := newTestGateway(t)
	if err := g.SetInbound(config.InboundOAuth, "s3cret"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	base := startServing(t, g)
	redirect := "http://127.0.0.1:9999/callback"

	// The client registers itself.
	status, registration, _ := postJSON(t, base+"/oauth/register", map[string]any{
		"client_name":   "Test Client",
		"redirect_uris": []string{redirect},
	})
	if status != http.StatusCreated {
		t.Fatalf("registration answered %d: %v", status, registration)
	}
	clientID, _ := registration["client_id"].(string)
	if clientID == "" {
		t.Fatal("registration returned no client_id")
	}

	verifier, err := transport.GenerateCodeVerifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	challenge := transport.GenerateCodeChallenge(verifier)
	authorize := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"the-state"},
	}

	// The user is asked to approve it first.
	status, _, page := requestAt(t, base+"/oauth/authorize?"+authorize.Encode(), "")
	if status != http.StatusOK {
		t.Fatalf("the consent page answered %d", status)
	}
	if !strings.Contains(page, "Allow this client") || !strings.Contains(page, clientID) {
		t.Errorf("the consent page does not describe the client: %s", page)
	}

	// Approving sends the client back with a code.
	response, err := noRedirect.PostForm(base+"/oauth/authorize", authorize)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	defer response.Body.Close()
	location := response.Header.Get("Location")
	if response.StatusCode != http.StatusFound || location == "" {
		t.Fatalf("approving answered %d with location %q", response.StatusCode, location)
	}
	target, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse the redirect: %v", err)
	}
	if target.Host != "127.0.0.1:9999" {
		t.Errorf("the redirect went to %q", target.Host)
	}
	if got := target.Query().Get("state"); got != "the-state" {
		t.Errorf("state came back as %q", got)
	}
	code := target.Query().Get("code")
	if code == "" {
		t.Fatal("no authorization code came back")
	}

	// Exchanging the code yields tokens.
	status, tokens := postForm(t, base+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
	if status != http.StatusOK {
		t.Fatalf("the token request answered %d: %v", status, tokens)
	}
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("the token response is %v", tokens)
	}

	// The issued token opens the gateway.
	if status, _, _ := requestAt(t, base+"/healthz", access); status != http.StatusOK {
		t.Errorf("the issued token answered %d, want 200", status)
	}

	// A code is good exactly once.
	status, again := postForm(t, base+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
	if status != http.StatusBadRequest || again["error"] != "invalid_grant" {
		t.Errorf("reusing the code answered %d with %v", status, again)
	}

	// And a refresh token renews it.
	status, renewed := postForm(t, base+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
	})
	if status != http.StatusOK {
		t.Fatalf("the refresh answered %d: %v", status, renewed)
	}
	fresh, _ := renewed["access_token"].(string)
	if fresh == "" || fresh == access {
		t.Errorf("the refresh returned %q", fresh)
	}
	if status, _, _ := requestAt(t, base+"/healthz", fresh); status != http.StatusOK {
		t.Errorf("the renewed token answered %d, want 200", status)
	}

	// Revoking closes the door on both.
	g.RevokeIssuedTokens()
	if status, _, _ := requestAt(t, base+"/healthz", fresh); status != http.StatusUnauthorized {
		t.Errorf("a revoked token answered %d, want 401", status)
	}
	if status, _, _ := requestAt(t, base+"/healthz", "s3cret"); status != http.StatusOK {
		t.Error("revoking the issued tokens also locked out the fixed token")
	}
}

func TestAuthorizeRefusesAnUnregisteredRedirect(t *testing.T) {
	g := newTestGateway(t)
	if err := g.SetInbound(config.InboundOAuth, "s3cret"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	base := startServing(t, g)

	_, registration, _ := postJSON(t, base+"/oauth/register", map[string]any{
		"redirect_uris": []string{"http://127.0.0.1:9999/callback"},
	})
	clientID, _ := registration["client_id"].(string)

	// The client asks for its code to be delivered somewhere else.
	response, err := noRedirect.Get(base + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {"http://evil.example/callback"},
		"code_challenge":        {"whatever"},
		"code_challenge_method": {"S256"},
	}.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("an unregistered redirect answered %d, want 400", response.StatusCode)
	}
	if location := response.Header.Get("Location"); location != "" {
		t.Errorf("the gateway redirected to %q, which it must never do", location)
	}
}

func TestTokenExchangeChecksTheProof(t *testing.T) {
	g := newTestGateway(t)
	if err := g.SetInbound(config.InboundOAuth, "s3cret"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	base := startServing(t, g)
	redirect := "http://127.0.0.1:9999/callback"

	_, registration, _ := postJSON(t, base+"/oauth/register", map[string]any{
		"redirect_uris": []string{redirect},
	})
	clientID, _ := registration["client_id"].(string)

	verifier, _ := transport.GenerateCodeVerifier()
	authorize := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {transport.GenerateCodeChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	response, err := noRedirect.PostForm(base+"/oauth/authorize", authorize)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	response.Body.Close()
	target, _ := url.Parse(response.Header.Get("Location"))
	code := target.Query().Get("code")

	// A verifier that does not hash to the challenge must be refused.
	status, body := postForm(t, base+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"code_verifier": {"not-the-verifier"},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("a wrong verifier answered %d with %v", status, body)
	}

	// That attempt burned the code, so a verifier cannot be guessed at. The
	// client has to start the sign-in again.
	status, body = postForm(t, base+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("the burnt code answered %d with %v", status, body)
	}
}

func TestRefreshTokenBelongsToOneClient(t *testing.T) {
	g := newTestGateway(t)
	if err := g.SetInbound(config.InboundOAuth, "s3cret"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	access, refresh, _, err := g.tokens.issue("client-a", authScope)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if access == "" {
		t.Fatal("no access token")
	}
	if _, _, err := g.tokens.renew(refresh, "client-b"); err == nil {
		t.Error("another client renewed a token that was not its own")
	}
}

func TestUpstreamTokenIsSentAsAHeader(t *testing.T) {
	var sent string
	mcpSrv := server.NewMCPServer("remote", "test", server.WithToolCapabilities(true))
	mcpSrv.AddTool(mcp.NewTool("echo", mcp.WithDescription("Echo.")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(req.GetString("text", "")), nil
		})
	stream := server.NewStreamableHTTPServer(mcpSrv)

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Get("Authorization")
		if sent != "Bearer upstream-secret" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="remote"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		stream.ServeHTTP(w, r)
	}))
	defer remote.Close()

	creds := config.NewMemoryCredentialStore()
	sc := config.ServerConfig{
		ID: "s1", Name: "Remote", Alias: "remote",
		Transport:   config.TransportHTTP,
		URL:         remote.URL,
		TokenHeader: "Authorization",
		TokenScheme: "Bearer",
		Enabled:     true,
	}

	// Without the token the upstream refuses, and the gateway says so.
	g := newTestGatewayWith(t, creds, sc)
	if err := g.Sync(context.Background()); err != nil {
		t.Logf("sync without a token: %v", err)
	}
	if view, _ := g.Server("s1"); view.Status == StatusReady {
		t.Fatal("the upstream accepted a call it should have refused")
	}

	// With the token saved, it connects.
	if err := creds.SetUpstreamToken("s1", "upstream-secret"); err != nil {
		t.Fatalf("save the token: %v", err)
	}
	if err := g.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if view, _ := g.Server("s1"); view.Status != StatusReady {
		t.Fatalf("the server is %q, want ready", view.Status)
	}
	if sent != "Bearer upstream-secret" {
		t.Errorf("the upstream saw %q", sent)
	}
	if !listNames(t, downstream(t, g))["remote__echo"] {
		t.Error("the token-authorized server's tools are not being served")
	}
}

func TestRequestHeadersPrefersTheStoredToken(t *testing.T) {
	creds := config.NewMemoryCredentialStore()
	if err := creds.SetUpstreamToken("s1", "from-keychain"); err != nil {
		t.Fatalf("save: %v", err)
	}
	sc := config.ServerConfig{
		ID: "s1", Transport: config.TransportHTTP, URL: "https://example.com/mcp",
		TokenHeader: "Authorization", TokenScheme: "Bearer",
		Headers: map[string]string{"Authorization": "Bearer typed-by-hand", "X-Trace": "1"},
	}
	headers := requestHeaders(sc, connectOptions{creds: creds})
	if headers["Authorization"] != "Bearer from-keychain" {
		t.Errorf("Authorization is %q, want the stored token", headers["Authorization"])
	}
	if headers["X-Trace"] != "1" {
		t.Errorf("the other headers were dropped: %q", headers)
	}
}

func TestRequestHeadersWithoutATokenOrHeaders(t *testing.T) {
	creds := config.NewMemoryCredentialStore()
	sc := config.ServerConfig{ID: "s1", Transport: config.TransportHTTP, URL: "https://example.com/mcp"}
	if headers := requestHeaders(sc, connectOptions{creds: creds}); headers != nil {
		t.Errorf("headers are %q, want none", headers)
	}
}

// TestAnMCPClientCanSignInWithTheGateway drives the whole sign-in the way a
// real MCP client does: discover, register, be authorized, exchange the code,
// and then reach the tools. It is the check that the gateway speaks enough
// OAuth for a client library to work against it.
func TestAnMCPClientCanSignInWithTheGateway(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	attach(t, g, sc, fakeUpstream(t))
	if err := g.SetInbound(config.InboundOAuth, "the-fixed-token"); err != nil {
		t.Fatalf("set inbound: %v", err)
	}
	base := startServing(t, g)
	ctx := context.Background()

	mcpClient, err := client.NewOAuthStreamableHttpClient(base+"/mcp", transport.OAuthConfig{
		RedirectURI: "http://127.0.0.1:9999/callback",
		TokenStore:  transport.NewMemoryTokenStore(),
		PKCEEnabled: true,
	})
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	defer mcpClient.Close()

	if err := mcpClient.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The gateway turns the client away and says how to sign in.
	_, err = mcpClient.Initialize(ctx, initializeRequest("test"))
	if !client.IsOAuthAuthorizationRequiredError(err) {
		t.Fatalf("connecting answered %v, want an authorization error", err)
	}
	handler := client.GetOAuthHandler(err)
	if handler == nil {
		t.Fatal("the gateway offered no OAuth details")
	}

	// The client registers itself, as a public client.
	if err := handler.RegisterClient(ctx, "Test Client"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if handler.GetClientID() == "" {
		t.Fatal("registration returned no client id")
	}

	state, err := transport.GenerateState()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	verifier, err := transport.GenerateCodeVerifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	authURL, err := handler.GetAuthorizationURL(ctx, state, transport.GenerateCodeChallenge(verifier))
	if err != nil {
		t.Fatalf("authorization URL: %v", err)
	}

	// The user approves, which is the browser step.
	target, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse the authorization URL: %v", err)
	}
	if target.Host != strings.TrimPrefix(base, "http://") {
		t.Errorf("the client was sent to %q, not the gateway", target.Host)
	}
	response, err := noRedirect.PostForm(base+"/oauth/authorize", target.Query())
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	response.Body.Close()
	redirected, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse the redirect: %v", err)
	}
	code := redirected.Query().Get("code")
	if code == "" {
		t.Fatalf("approving sent the client to %q", response.Header.Get("Location"))
	}

	// The client exchanges the code for a token of its own.
	if err := handler.ProcessAuthorizationResponse(ctx, code, state, verifier); err != nil {
		t.Fatalf("exchange the code: %v", err)
	}

	// And now it is through.
	if _, err := mcpClient.Initialize(ctx, initializeRequest("test")); err != nil {
		t.Fatalf("initialize after signing in: %v", err)
	}
	result, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var found bool
	for _, tool := range result.Tools {
		if tool.Name == "filesystem__echo" {
			found = true
		}
	}
	if !found {
		t.Errorf("the signed-in client sees %d tools, without the expected one", len(result.Tools))
	}
}
