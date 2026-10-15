package gateway

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"

	"mcp-go/internal/config"
)

// pendingAuth is a sign-in that is waiting for the browser to come back.
type pendingAuth struct {
	serverID string
	verifier string
	handler  *transport.OAuthHandler
	created  time.Time
}

// pendingTTL is how long a started sign-in is remembered.
const pendingTTL = 5 * time.Minute

// beginAuthTimeout bounds the discovery and registration requests a sign-in
// makes before the user is sent to the browser.
const beginAuthTimeout = 60 * time.Second

// BeginAuth starts the sign-in for a server and returns the URL the user has
// to open. The gateway's own /oauth/callback completes the flow, after which
// the server is connected again.
//
// It returns an empty URL when the server turns out to need no sign-in.
func (g *Gateway) BeginAuth(ctx context.Context, serverID string) (string, error) {
	sc, ok := g.cfg.Server(serverID)
	if !ok {
		return "", fmt.Errorf("no such server %q", serverID)
	}
	if sc.Transport == config.TransportStdio || sc.TokenHeader != "" {
		// A stdio server runs locally, and one given a token of its own is not
		// signed in to; neither has an authorization server to visit.
		return "", fmt.Errorf("%s does not sign in with OAuth", sc.Name)
	}
	if !g.Serving() {
		// The browser is sent back to the gateway's own callback, so it has to
		// be there to receive it.
		return "", errors.New("start the gateway first, so that your browser can come back to it")
	}

	ctx, cancel := context.WithTimeout(ctx, beginAuthTimeout)
	defer cancel()

	// A connection attempt is what makes the server state its authorization
	// requirements, and hands back the handler already configured for them.
	// OAuth is forced here, since the discovery that follows is what produces
	// the handler even for a server the configuration never marked as OAuth.
	up, err := connect(ctx, sc, connectOptions{
		creds:       g.creds,
		redirectURI: g.redirectURI(),
		version:     g.version,
		forceOAuth:  true,
	})
	if err == nil {
		// It connected on its own, so there is nothing to sign in to.
		g.mu.Lock()
		g.upstreams[sc.ID] = up
		g.status[sc.ID] = StatusReady
		g.registerLocked(up)
		g.mu.Unlock()
		g.note(sc.ID, sc.Name+": connected without sign-in", false)
		g.changed()
		return "", nil
	}

	var needs *NeedsAuthError
	if !errors.As(err, &needs) {
		return "", err
	}
	handler := client.GetOAuthHandler(needs.Err)
	if handler == nil {
		return "", errors.New("the server requires authorization but sent no OAuth details")
	}

	// Register the gateway with the authorization server when it asks clients
	// to. A pre-registered client ID, which the user can supply, skips this.
	if handler.GetClientID() == "" {
		if err := handler.RegisterClient(ctx, clientName); err != nil {
			return "", fmt.Errorf(
				"%s needs a client registration its authorization server does not offer; "+
					"enter a client ID and secret for it and try again: %w", sc.Name, err)
		}
	}
	if err := g.saveClient(sc.ID, handler); err != nil {
		return "", err
	}

	state, err := transport.GenerateState()
	if err != nil {
		return "", err
	}
	verifier, err := transport.GenerateCodeVerifier()
	if err != nil {
		return "", err
	}

	authURL, err := handler.GetAuthorizationURL(ctx, state, transport.GenerateCodeChallenge(verifier))
	if err != nil {
		return "", fmt.Errorf("build the authorization URL: %w", err)
	}

	g.mu.Lock()
	g.prunePendingLocked()
	g.pending[state] = &pendingAuth{
		serverID: sc.ID,
		verifier: verifier,
		handler:  handler,
		created:  time.Now(),
	}
	g.mu.Unlock()

	g.note(sc.ID, sc.Name+": waiting for sign-in in the browser", false)
	g.changed()
	return authURL, nil
}

// saveClient keeps what the authorization server handed the gateway, so a
// later sign-in reuses it instead of registering again.
func (g *Gateway) saveClient(serverID string, h *transport.OAuthHandler) error {
	// Keep an issuer the user's own credentials were not bound to.
	saved := g.creds.Client(serverID)
	issuer := h.GetCredentialsIssuer()
	if issuer == "" {
		issuer = saved.Issuer
	}
	return g.creds.SaveClient(serverID, config.ClientCredentials{
		ClientID:   h.GetClientID(),
		Secret:     h.GetClientSecret(),
		Issuer:     issuer,
		AuthMethod: h.GetTokenEndpointAuthMethod(),
	})
}

// prunePendingLocked forgets sign-ins that were started too long ago. The
// caller holds the lock.
func (g *Gateway) prunePendingLocked() {
	for state, p := range g.pending {
		if time.Since(p.created) > pendingTTL {
			delete(g.pending, state)
		}
	}
}

// handleOAuthCallback finishes a sign-in when the browser comes back to the
// gateway's own port.
func (g *Gateway) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if errCode := query.Get("error"); errCode != "" {
		detail := errCode
		if desc := query.Get("error_description"); desc != "" {
			detail += ": " + desc
		}
		g.writeCallbackPage(w, false, "Sign-in failed", detail)
		return
	}

	state, code := query.Get("state"), query.Get("code")
	if state == "" || code == "" {
		g.writeCallbackPage(w, false, "Sign-in failed",
			"The authorization server did not send an authorization code.")
		return
	}

	g.mu.Lock()
	g.prunePendingLocked()
	p := g.pending[state]
	delete(g.pending, state)
	g.mu.Unlock()

	if p == nil {
		g.writeCallbackPage(w, false, "Sign-in failed",
			"This sign-in has expired, or was already completed. Start it again from MCP Gateway.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), beginAuthTimeout)
	defer cancel()

	if err := p.handler.ProcessAuthorizationResponse(ctx, code, state, p.verifier); err != nil {
		g.note(p.serverID, g.serverName(p.serverID)+": sign-in failed: "+err.Error(), true)
		g.changed()
		g.writeCallbackPage(w, false, "Sign-in failed", err.Error())
		return
	}
	if err := g.saveClient(p.serverID, p.handler); err != nil {
		// The token is what matters; the credentials we already had still work.
		g.note(p.serverID, "could not save client credentials: "+err.Error(), true)
	}

	g.note(p.serverID, g.serverName(p.serverID)+": signed in", false)
	g.markSignedIn(p.serverID, true)
	g.changed()
	g.writeCallbackPage(w, true, "Signed in",
		"You can close this tab and return to MCP Gateway.")

	// Connect the server now that it has a token.
	serverID := p.serverID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), beginAuthTimeout)
		defer cancel()
		if err := g.Reconnect(ctx, serverID); err != nil {
			g.changed()
		}
	}()
}

// SignOut forgets a server's token and tries it again, which usually puts it
// back into the "sign in" state.
func (g *Gateway) SignOut(ctx context.Context, serverID string) error {
	if err := g.creds.SignOut(serverID); err != nil {
		return err
	}
	g.markSignedIn(serverID, false)
	g.note(serverID, g.serverName(serverID)+": signed out", false)
	g.changed()
	// The server usually answers "authorization required" now, which is what
	// signing out asks for rather than a failure to report.
	err := g.Reconnect(ctx, serverID)
	var needs *NeedsAuthError
	if errors.As(err, &needs) {
		return nil
	}
	return err
}

// SetClientCredentials stores a client ID and secret the user entered for a
// server whose authorization server does not register clients on demand.
func (g *Gateway) SetClientCredentials(serverID, clientID, secret string) error {
	if clientID == "" {
		// Clearing them also clears the binding learned for the old ones.
		return g.creds.SaveClient(serverID, config.ClientCredentials{})
	}
	return g.creds.SaveClient(serverID, config.ClientCredentials{
		ClientID: clientID,
		Secret:   secret,
	})
}

// ClientCredentials returns what the user entered for a server.
func (g *Gateway) ClientCredentials(serverID string) config.ClientCredentials {
	return g.creds.Client(serverID)
}

// serverName returns a server's display name.
func (g *Gateway) serverName(id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.serverNameLocked(id)
}

// writeCallbackPage answers the browser with something short and readable.
func (g *Gateway) writeCallbackPage(w http.ResponseWriter, ok bool, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	}
	fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>%s</title></head>
<body style="font:16px -apple-system,system-ui,sans-serif;margin:4rem auto;max-width:32rem;color:%s">
<h1 style="font-size:1.25rem">%s</h1>
<p style="color:%s">%s</p>
</body>
</html>
`, html.EscapeString(title), textColor(ok), html.EscapeString(title), mutedColor(ok), html.EscapeString(detail))
}

func textColor(ok bool) string {
	if ok {
		return "#111"
	}
	return "#b00020"
}

func mutedColor(bool) string { return "#555" }
