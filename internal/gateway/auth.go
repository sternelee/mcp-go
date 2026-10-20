package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"mcp-go/internal/config"
)

// authScope is the scope the gateway advertises and issues.
const authScope = "mcp"

// authCodeTTL is how long an authorization code stays exchangeable.
const authCodeTTL = 5 * time.Minute

// authCode is an issued authorization code, waiting to be exchanged.
type authCode struct {
	clientID    string
	redirectURI string
	challenge   string
	scope       string
	expiresAt   time.Time
}

// AuthState describes how the gateway is guarding itself, for the interface.
type AuthState struct {
	Mode         config.InboundMode
	HasToken     bool
	IssuedTokens int
	// MetadataURL is where an MCP client discovers how to sign in.
	MetadataURL string
	// AuthorizationServer is true when the gateway can issue tokens itself.
	AuthorizationServer bool
}

// resourcePaths are the endpoints that are protected resources in their own
// right. A client names the one it addressed in the metadata path, because
// RFC 9728 requires the resource identifier it reads back to match the URL it
// used, and the gateway serves tools at more than one URL.
var resourcePaths = []string{"mcp", "sse"}

// defaultResourcePath is the endpoint a bare well-known request describes.
const defaultResourcePath = "mcp"

// protectedResourceMetadataPath is where the metadata for one endpoint is
// served: the well-known path with the endpoint appended, per RFC 9728.
func protectedResourceMetadataPath(resourcePath string) string {
	if resourcePath == "" {
		resourcePath = defaultResourcePath
	}
	return server.WellKnownProtectedResourcePath + "/" + resourcePath
}

// resourceURL is the identifier of the resource served at an endpoint.
func (g *Gateway) resourceURL(resourcePath string) string {
	if resourcePath == "" {
		resourcePath = defaultResourcePath
	}
	return g.BaseURL() + "/" + resourcePath
}

// guard wraps a handler so that callers must identify themselves when the
// configuration asks them to. resourcePath names the endpoint being guarded, so
// the challenge can point at the right metadata. The signature is the shape
// net/http expects, so it can wrap any handler.
func (g *Gateway) guard(resourcePath string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode, want := g.inbound()
		if !mode.RequiresToken() {
			next.ServeHTTP(w, r)
			return
		}
		presented := bearerToken(r)
		if presented != "" && (sameToken(presented, want) || g.tokens.accepts(presented)) {
			next.ServeHTTP(w, r)
			return
		}
		g.rejectUnauthorized(w, resourcePath)
	})
}

// serveAsAuthServer mounts a handler that only answers when the gateway is in
// the mode where it issues tokens itself.
func (g *Gateway) serveAsAuthServer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mode, _ := g.inbound(); mode != config.InboundOAuth {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// rejectUnauthorized tells the caller what it needs, and where to get it. The
// 401 and the resource metadata URL are what make an MCP client start a sign-in
// of its own.
func (g *Gateway) rejectUnauthorized(w http.ResponseWriter, resourcePath string) {
	w.Header().Set("WWW-Authenticate",
		`Bearer resource_metadata="`+g.BaseURL()+protectedResourceMetadataPath(resourcePath)+`"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":             "invalid_token",
		"error_description": "this gateway requires a bearer token",
	})
}

// inbound returns the configured mode and the token callers must present. Both
// are cached, since the credential store is far too slow to read per request.
func (g *Gateway) inbound() (config.InboundMode, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cfg.InboundMode(), g.authToken
}

// AuthState describes how the gateway is currently guarding itself.
func (g *Gateway) AuthState() AuthState {
	g.mu.Lock()
	mode, token := g.cfg.InboundMode(), g.authToken
	g.mu.Unlock()
	return AuthState{
		Mode:                mode,
		HasToken:            token != "",
		IssuedTokens:        g.tokens.count(),
		MetadataURL:         g.protectedResourceMetadataURL(),
		AuthorizationServer: mode == config.InboundOAuth,
	}
}

// AuthToken returns the token callers must present, so the interface can show
// it and copy it.
func (g *Gateway) AuthToken() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authToken
}

// GenerateAuthToken returns a fresh token for the user to keep or hand out.
func (g *Gateway) GenerateAuthToken() (string, error) { return randomToken() }

// SetInbound records how callers must identify themselves, and the token they
// present. It takes effect on the next request.
func (g *Gateway) SetInbound(mode config.InboundMode, token string) error {
	if !mode.Valid() {
		return fmt.Errorf("unknown auth mode %q", mode)
	}
	token = strings.TrimSpace(token)
	if mode.RequiresToken() {
		g.mu.Lock()
		known := g.authToken
		g.mu.Unlock()
		if token == "" && known == "" {
			return fmt.Errorf("%s mode needs a token", mode.Label())
		}
	}
	// A token that is already set is kept when switching modes, so turning
	// authentication off and on again does not lose it.
	if token != "" {
		if err := g.creds.SetInboundToken(token); err != nil {
			return err
		}
	}
	g.mu.Lock()
	g.cfg.Inbound = mode
	if token != "" {
		g.authToken = token
	}
	g.mu.Unlock()
	return nil
}

// RevokeIssuedTokens forgets every token the gateway handed out.
func (g *Gateway) RevokeIssuedTokens() {
	g.tokens.revokeAll()
	g.note("", "every issued token was revoked", false)
	g.changed()
}

// protectedResourceMetadataURL is where clients are sent to discover the
// authorization server.
func (g *Gateway) protectedResourceMetadataURL() string {
	return g.BaseURL() + protectedResourceMetadataPath("")
}

// handleProtectedResource serves RFC 9728 metadata for the gateway itself. The
// path it was asked for names the endpoint being described, so the resource
// identifier matches the URL the client used.
func (g *Gateway) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	resourcePath := strings.Trim(strings.TrimPrefix(r.URL.Path, server.WellKnownProtectedResourcePath), "/")
	if !containsString(resourcePaths, resourcePath) {
		// A bare well-known request, or one naming something we do not serve.
		resourcePath = ""
	}

	metadata := server.ProtectedResourceMetadataConfig{
		Resource:               g.resourceURL(resourcePath),
		ResourceName:           "MCP Go",
		BearerMethodsSupported: []string{"header"},
		ScopesSupported:        []string{authScope},
	}
	// The gateway can only point clients at an authorization server when it is
	// one, which is the mode that lets them sign in.
	if mode, _ := g.inbound(); mode == config.InboundOAuth {
		metadata.AuthorizationServers = []string{g.BaseURL()}
	}
	server.NewProtectedResourceMetadataHandler(metadata).ServeHTTP(w, r)
}

// handleAuthServerMetadata serves RFC 8414 metadata for the gateway acting as
// its own authorization server.
func (g *Gateway) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	base := g.BaseURL()
	writeJSON(w, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{authScope},
	})
}

// handleRegister is the dynamic client registration endpoint. Clients are
// public: they get an identifier and nothing else.
func (g *Gateway) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the registration is not valid JSON")
		return
	}
	if len(request.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, uri := range request.RedirectURIs {
		if !validRedirectURI(uri) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("%q is not an http or https URL", uri))
			return
		}
	}

	clientID, err := g.tokens.registerClient(request.RedirectURIs)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	g.note("", "registered OAuth client "+describeClient(request.ClientName), false)
	g.changed()

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{
		"client_id":                  clientID,
		"client_name":                request.ClientName,
		"redirect_uris":              request.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// handleAuthorize asks the user to approve a client, then hands out a code.
// Nothing is ever redirected to before its redirect URI has been checked.
func (g *Gateway) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	if err := r.ParseForm(); err != nil {
		g.authorizeError(w, http.StatusBadRequest, "invalid_request", "the request could not be read")
		return
	}
	clientID := r.Form.Get("client_id")
	redirectURI := r.Form.Get("redirect_uri")

	// A redirect URI is only ever trusted once it has been matched against the
	// one the client registered.
	registered, known := g.tokens.clientRedirects(clientID)
	if !known || !containsString(registered, redirectURI) {
		g.authorizeError(w, http.StatusBadRequest, "invalid_request",
			"the client is not registered, or the redirect_uri was not one of its own")
		return
	}

	if responseType := r.Form.Get("response_type"); responseType != "code" {
		g.authorizeRedirectError(w, r, redirectURI, "unsupported_response_type",
			"only response_type=code is supported")
		return
	}
	challenge := r.Form.Get("code_challenge")
	if challenge == "" || r.Form.Get("code_challenge_method") != "S256" {
		g.authorizeRedirectError(w, r, redirectURI, "invalid_request",
			"PKCE with code_challenge_method=S256 is required")
		return
	}

	if r.Method == http.MethodGet {
		g.consentPage(w, r, clientID, challenge)
		return
	}

	code, err := g.tokens.newCode(clientID, redirectURI, challenge, authScope)
	if err != nil {
		g.authorizeRedirectError(w, r, redirectURI, "server_error", err.Error())
		return
	}

	target, err := url.Parse(redirectURI)
	if err != nil {
		g.authorizeError(w, http.StatusBadRequest, "invalid_request", "the redirect_uri is not a URL")
		return
	}
	query := target.Query()
	query.Set("code", code)
	if state := r.Form.Get("state"); state != "" {
		query.Set("state", state)
	}
	target.RawQuery = query.Encode()

	g.note("", "authorized an OAuth client", false)
	g.changed()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// consentPage asks the user to approve a client signing in.
func (g *Gateway) consentPage(w http.ResponseWriter, r *http.Request, clientID, challenge string) {
	fields := []string{"client_id", "redirect_uri", "response_type", "code_challenge", "code_challenge_method", "scope", "state"}
	var hidden strings.Builder
	for _, name := range fields {
		if value := r.Form.Get(name); value != "" {
			fmt.Fprintf(&hidden, `<input type="hidden" name="%s" value="%s">`+"\n",
				html.EscapeString(name), html.EscapeString(value))
		}
	}
	target, _ := url.Parse(r.Form.Get("redirect_uri"))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Authorize</title></head>
<body style="font:16px -apple-system,system-ui,sans-serif;margin:4rem auto;max-width:34rem;color:#111">
<h1 style="font-size:1.25rem">Allow this client to use MCP Go?</h1>
<p>"<strong>%s</strong>" wants to call the tools this gateway serves, and will be sent back to
<code>%s</code>.</p>
<form method="post" action="/oauth/authorize">
%s
<button type="submit" style="font:inherit;padding:.5rem 1rem">Authorize</button>
</form>
<p style="color:#555;font-size:.875rem">Only authorize a client you started yourself.</p>
</body>
</html>
`, html.EscapeString(clientID), html.EscapeString(target.Host), hidden.String())
}

// handleToken exchanges a code, or a refresh token, for an access token.
func (g *Gateway) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the request could not be read")
		return
	}
	clientID := r.Form.Get("client_id")

	switch r.Form.Get("grant_type") {
	case "authorization_code":
		g.exchangeCode(w, r, clientID)
	case "refresh_token":
		access, expiresAt, err := g.tokens.renew(r.Form.Get("refresh_token"), clientID)
		if err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
			return
		}
		g.tokenResponse(w, access, r.Form.Get("refresh_token"), expiresAt)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"only authorization_code and refresh_token are supported")
	}
}

// exchangeCode turns an authorization code into tokens, checking that the code
// went to the client that asked for it and that PKCE matches.
//
// The code is taken before anything is checked, so that a failed attempt burns
// it: an attacker who intercepted a code but not the verifier gets one guess,
// not unlimited ones.
func (g *Gateway) exchangeCode(w http.ResponseWriter, r *http.Request, clientID string) {
	code := r.Form.Get("code")
	record, ok := g.tokens.takeCode(code)
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, expired or already used")
		return
	}
	if record.clientID != clientID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
		return
	}
	if record.redirectURI != r.Form.Get("redirect_uri") {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the redirect_uri does not match the one the code was issued for")
		return
	}
	if !pkceMatches(r.Form.Get("code_verifier"), record.challenge) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code_verifier does not match the code_challenge")
		return
	}

	access, refresh, expiresAt, err := g.tokens.issue(clientID, record.scope)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	g.note("", "issued an access token to an OAuth client", false)
	g.changed()
	g.tokenResponse(w, access, refresh, expiresAt)
}

// tokenResponse is the RFC 6749 token response.
func (g *Gateway) tokenResponse(w http.ResponseWriter, access, refresh string, expiresAt time.Time) {
	body := map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(time.Until(expiresAt).Seconds()),
		"scope":        authScope,
	}
	if refresh != "" {
		body["refresh_token"] = refresh
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, body)
}

// authorizeError writes a page for a request that can never be redirected,
// because its redirect URI was not trusted.
func (g *Gateway) authorizeError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Sign-in failed</title></head>
<body style="font:16px -apple-system,system-ui,sans-serif;margin:4rem auto;max-width:34rem;color:#b00020">
<h1 style="font-size:1.25rem">Sign-in failed</h1><p>%s: %s</p></body></html>`,
		html.EscapeString(code), html.EscapeString(description))
}

// authorizeRedirectError sends the failure back to the client, now that its
// redirect URI is known to be trustworthy.
func (g *Gateway) authorizeRedirectError(w http.ResponseWriter, r *http.Request, redirectURI, code, description string) {
	target, err := url.Parse(redirectURI)
	if err != nil {
		g.authorizeError(w, http.StatusBadRequest, code, description)
		return
	}
	query := target.Query()
	query.Set("error", code)
	query.Set("error_description", description)
	if state := r.Form.Get("state"); state != "" {
		query.Set("state", state)
	}
	target.RawQuery = query.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// bearerToken reads the token a caller presented.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// sameToken compares two tokens without giving away their contents by how long
// the comparison takes.
func sameToken(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// pkceMatches checks a PKCE verifier against the challenge the client sent.
func pkceMatches(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) == 1
}

// validRedirectURI reports whether a redirect URI is one we will send a browser
// to.
func validRedirectURI(uri string) bool {
	parsed, err := url.Parse(uri)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// containsString reports whether a slice holds a value.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// describeClient names a client in the activity list.
func describeClient(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(unnamed)"
	}
	return name
}

// methodNotAllowed answers a request made with the wrong method.
func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// oauthError is the RFC 6749 error response.
func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"error": code, "error_description": description})
}

// writeJSON writes a JSON body.
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
