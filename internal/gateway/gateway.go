package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"mcp-go/internal/config"
)

// separator joins a server alias and an upstream tool name into the name the
// gateway publishes.
const separator = "__"

// metaToolName reports the configured servers to a calling model.
const metaToolName = "gateway" + separator + "list_servers"

// Status is how one configured server is doing.
type Status string

const (
	StatusDisabled   Status = "disabled"
	StatusConnecting Status = "connecting"
	StatusReady      Status = "ready"
	StatusNeedsAuth  Status = "needs-auth"
	StatusError      Status = "error"
)

// route remembers where an aggregated tool comes from.
type route struct {
	serverID string
	upstream string // the tool's name at the upstream
	def      mcp.Tool
}

// ToolView is one aggregated tool, as the interface shows it.
type ToolView struct {
	Name        string // the namespaced name clients call
	Upstream    string // the name at its own server
	ServerID    string
	ServerName  string
	Description string
	Schema      string // the input schema as indented JSON
}

// ServerView is one configured server with its live state.
type ServerView struct {
	Config   config.ServerConfig
	Status   Status
	Err      string
	Tools    int
	SignedIn bool
}

// Event is something the gateway did, for the activity list.
type Event struct {
	Time     time.Time
	ServerID string
	Text     string
	Err      bool
}

// Gateway aggregates the configured MCP servers and serves them as one.
type Gateway struct {
	cfg   *config.Config
	creds *config.CredentialStore

	// srv is the downstream MCP server every client of the gateway reaches.
	srv *server.MCPServer

	// syncMu serializes reconciliation; mu guards the fields below.
	syncMu    sync.Mutex
	mu        sync.Mutex
	upstreams map[string]*Upstream
	routes    map[string]route
	status    map[string]Status
	errs      map[string]string
	// signedIn caches the credential store, which reads the system keychain and
	// is far too slow to consult while drawing a frame. RefreshCredentials
	// fills it, along with authToken.
	signedIn map[string]bool
	// authToken is the bearer token callers must present, cached for the same
	// reason.
	authToken string
	// tokens are the credentials the gateway issues to its callers.
	tokens  *issuedTokens
	events  []Event
	pending map[string]*pendingAuth

	stream *server.StreamableHTTPServer
	sse    *server.SSEServer
	http   *http.Server
	port   int

	onChange func()
	version  string
}

// maxEvents is how many activity entries are kept.
const maxEvents = 500

// New returns a gateway for cfg that reads and writes credentials through creds.
func New(cfg *config.Config, creds *config.CredentialStore, version string) *Gateway {
	g := &Gateway{
		cfg:       cfg,
		creds:     creds,
		version:   version,
		upstreams: map[string]*Upstream{},
		routes:    map[string]route{},
		status:    map[string]Status{},
		errs:      map[string]string{},
		signedIn:  map[string]bool{},
		pending:   map[string]*pendingAuth{},
		tokens:    newIssuedTokens(),
		srv: server.NewMCPServer("mcp-gateway", version,
			server.WithInstructions("A gateway that aggregates the tools of several MCP servers. Tool names are prefixed with the server they come from."),
			server.WithToolCapabilities(true), // publish tools/list_changed as servers come and go
			server.WithRecovery(),
		),
	}
	g.registerMetaTool()
	return g
}

// Downstream is the MCP server the gateway exposes.
func (g *Gateway) Downstream() *server.MCPServer { return g.srv }

// SetOnChange sets the function called whenever something the window shows
// has changed. It may run on any goroutine.
func (g *Gateway) SetOnChange(fn func()) {
	g.mu.Lock()
	g.onChange = fn
	g.mu.Unlock()
}

// SetUpstreamToken saves the static credential a server takes, or forgets it
// when the token is empty.
func (g *Gateway) SetUpstreamToken(serverID, token string) error {
	return g.creds.SetUpstreamToken(serverID, token)
}

// HasUpstreamToken reports whether a server has a static credential, without
// revealing it. It reads the credential store.
func (g *Gateway) HasUpstreamToken(serverID string) bool {
	return g.creds.HasUpstreamToken(serverID)
}

// changed tells the window to redraw.
func (g *Gateway) changed() {
	g.mu.Lock()
	fn := g.onChange
	g.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// note records an activity entry.
func (g *Gateway) note(serverID, text string, isErr bool) {
	g.mu.Lock()
	g.events = append(g.events, Event{Time: time.Now(), ServerID: serverID, Text: text, Err: isErr})
	if len(g.events) > maxEvents {
		g.events = g.events[len(g.events)-maxEvents:]
	}
	g.mu.Unlock()
}

// Events returns the activity entries, oldest first.
func (g *Gateway) Events() []Event {
	g.mu.Lock()
	defer g.mu.Unlock()
	events := make([]Event, len(g.events))
	copy(events, g.events)
	return events
}

// Servers returns every configured server with its live state.
func (g *Gateway) Servers() []ServerView {
	g.mu.Lock()
	defer g.mu.Unlock()
	counts := map[string]int{}
	for _, r := range g.routes {
		counts[r.serverID]++
	}
	views := make([]ServerView, 0, len(g.cfg.Servers))
	for _, sc := range g.cfg.Servers {
		status := g.status[sc.ID]
		if status == "" {
			status = StatusDisabled
		}
		if !sc.Enabled {
			status = StatusDisabled
		}
		views = append(views, ServerView{
			Config:   sc,
			Status:   status,
			Err:      g.errs[sc.ID],
			Tools:    counts[sc.ID],
			SignedIn: g.signedIn[sc.ID],
		})
	}
	return views
}

// Server returns one server's live state.
func (g *Gateway) Server(id string) (ServerView, bool) {
	for _, v := range g.Servers() {
		if v.Config.ID == id {
			return v, true
		}
	}
	return ServerView{}, false
}

// Tools returns the aggregated tools, sorted by name.
func (g *Gateway) Tools() []ToolView {
	g.mu.Lock()
	defer g.mu.Unlock()
	views := make([]ToolView, 0, len(g.routes))
	for name, r := range g.routes {
		views = append(views, ToolView{
			Name:        name,
			Upstream:    r.upstream,
			ServerID:    r.serverID,
			ServerName:  g.serverNameLocked(r.serverID),
			Description: r.def.Description,
			Schema:      schemaJSON(r.def),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

// CallTool invokes an aggregated tool by its namespaced name.
func (g *Gateway) CallTool(ctx context.Context, name string, args any) (*mcp.CallToolResult, error) {
	g.mu.Lock()
	r, ok := g.routes[name]
	up := g.upstreams[r.serverID]
	g.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if up == nil {
		return nil, fmt.Errorf("server of tool %q is not connected", name)
	}
	return up.CallTool(ctx, r.upstream, args)
}

// serverNameLocked returns a server's display name. The caller holds the lock.
func (g *Gateway) serverNameLocked(id string) string {
	if sc, ok := g.cfg.Server(id); ok {
		return sc.Name
	}
	return id
}

// schemaJSON renders a tool's input schema for display.
func schemaJSON(tool mcp.Tool) string {
	if len(tool.RawInputSchema) > 0 {
		var buf any
		if err := json.Unmarshal(tool.RawInputSchema, &buf); err == nil {
			if out, err := json.MarshalIndent(buf, "", "  "); err == nil {
				return string(out)
			}
		}
		return string(tool.RawInputSchema)
	}
	out, err := json.MarshalIndent(tool.InputSchema, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}

// registerMetaTool publishes the tool that describes the gateway's topology.
func (g *Gateway) registerMetaTool() {
	g.srv.AddTool(
		mcp.NewTool(metaToolName, mcp.WithDescription(
			"List the MCP servers this gateway aggregates, with their transport, status and tool counts.")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			type serverInfo struct {
				Name      string `json:"name"`
				Transport string `json:"transport"`
				Status    string `json:"status"`
				Tools     int    `json:"tools"`
				Error     string `json:"error,omitempty"`
			}
			views := g.Servers()
			servers := make([]serverInfo, 0, len(views))
			for _, v := range views {
				servers = append(servers, serverInfo{
					Name:      v.Config.Name,
					Transport: string(v.Config.Transport),
					Status:    string(v.Status),
					Tools:     v.Tools,
					Error:     v.Err,
				})
			}
			out, err := json.MarshalIndent(map[string]any{"servers": servers, "tools": len(g.Tools())}, "", "  ")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(string(out)), nil
		},
	)
}

// RefreshCredentials reads the credential store: which servers can sign in, and
// the token callers of the gateway must present. It reads the system's
// credential store, so call it off the interface thread.
func (g *Gateway) RefreshCredentials() {
	signedIn := map[string]bool{}
	for _, sc := range g.cfg.Servers {
		if sc.OAuth {
			signedIn[sc.ID] = g.creds.SignedIn(sc.ID)
		}
	}
	token := ""
	if g.cfg.InboundMode().RequiresToken() {
		token = g.creds.InboundToken()
	}

	g.mu.Lock()
	g.signedIn = signedIn
	if token != "" || !g.cfg.InboundMode().RequiresToken() {
		g.authToken = token
	}
	g.mu.Unlock()
}

// markSignedIn records a server's sign-in state without reading the credential
// store, for changes the gateway itself just made.
func (g *Gateway) markSignedIn(serverID string, signedIn bool) {
	g.mu.Lock()
	g.signedIn[serverID] = signedIn
	g.mu.Unlock()
}

// Sync reconciles the connected upstreams with the configuration. Servers that
// are gone or disabled are disconnected; enabled ones that are not connected
// are connected and their tools published. Failures are recorded per server and
// joined into the returned error.
func (g *Gateway) Sync(ctx context.Context) error {
	g.syncMu.Lock()
	defer g.syncMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	g.RefreshCredentials()

	g.mu.Lock()
	var stale []*Upstream
	for id, up := range g.upstreams {
		sc, ok := g.cfg.Server(id)
		if !ok || !sc.Enabled {
			stale = append(stale, up)
			delete(g.upstreams, id)
			delete(g.status, id)
			delete(g.errs, id)
			g.unregisterLocked(id)
		}
	}
	var todo []config.ServerConfig
	for _, sc := range g.cfg.Servers {
		if !sc.Enabled {
			g.status[sc.ID] = StatusDisabled
			delete(g.errs, sc.ID)
			continue
		}
		if _, ok := g.upstreams[sc.ID]; ok {
			continue
		}
		g.status[sc.ID] = StatusConnecting
		todo = append(todo, sc)
	}
	g.mu.Unlock()
	g.changed()

	for _, up := range stale {
		g.note(up.Config.ID, "disconnected "+up.Config.Name, false)
		go up.Close()
	}

	var failed []error
	for _, sc := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := g.connectOne(ctx, sc); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// Reconnect drops a server's connection and connects it again.
func (g *Gateway) Reconnect(ctx context.Context, id string) error {
	g.syncMu.Lock()
	defer g.syncMu.Unlock()
	g.RefreshCredentials()

	g.mu.Lock()
	up := g.upstreams[id]
	delete(g.upstreams, id)
	delete(g.status, id)
	delete(g.errs, id)
	g.unregisterLocked(id)
	sc, ok := g.cfg.Server(id)
	g.mu.Unlock()

	if up != nil {
		_ = up.Close()
	}
	if !ok {
		return fmt.Errorf("no such server %q", id)
	}
	if !sc.Enabled {
		g.setStatus(id, StatusDisabled, "")
		g.changed()
		return nil
	}
	defer g.changed()
	return g.connectOne(ctx, sc)
}

// connectOne connects one server and publishes its tools.
func (g *Gateway) connectOne(ctx context.Context, sc config.ServerConfig) error {
	up, err := connect(ctx, sc, connectOptions{
		creds:       g.creds,
		redirectURI: g.redirectURI(),
		version:     g.version,
	})
	if err != nil {
		var needs *NeedsAuthError
		if errors.As(err, &needs) {
			g.setStatus(sc.ID, StatusNeedsAuth, "")
			g.note(sc.ID, sc.Name+": sign in required", false)
			return err
		}
		g.setStatus(sc.ID, StatusError, err.Error())
		g.note(sc.ID, sc.Name+": "+err.Error(), true)
		return err
	}

	g.mu.Lock()
	g.upstreams[sc.ID] = up
	g.status[sc.ID] = StatusReady
	delete(g.errs, sc.ID)
	g.registerLocked(up)
	g.mu.Unlock()

	g.note(sc.ID, fmt.Sprintf("%s: %d tools", sc.Name, len(up.Tools())), false)
	g.changed()
	return nil
}

// registerLocked publishes an upstream's tools under its alias. The caller
// holds the lock.
func (g *Gateway) registerLocked(up *Upstream) {
	batch := make([]server.ServerTool, 0, len(up.tools))
	for _, t := range up.tools {
		tool := t
		name := up.Config.Alias + separator + t.Name
		tool.Name = name
		// Say where the tool comes from, so a model choosing between several
		// servers can tell them apart.
		if tool.Description == "" {
			tool.Description = "From " + up.Config.Name + "."
		} else {
			tool.Description = "[" + up.Config.Name + "] " + tool.Description
		}
		upstreamName := t.Name
		owner := up
		batch = append(batch, server.ServerTool{
			Tool: tool,
			Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				result, err := owner.CallTool(ctx, upstreamName, req.Params.Arguments)
				if err != nil {
					// Report it as a tool error so the calling model sees the
					// failure and can react, rather than as a protocol error.
					return mcp.NewToolResultError(fmt.Sprintf("%s: %v", owner.Config.Name, err)), nil
				}
				return result, nil
			},
		})
		g.routes[name] = route{serverID: up.Config.ID, upstream: upstreamName, def: tool}
	}
	if len(batch) > 0 {
		g.srv.AddTools(batch...)
	}
}

// unregisterLocked withdraws a server's tools. The caller holds the lock.
func (g *Gateway) unregisterLocked(serverID string) {
	var names []string
	for name, r := range g.routes {
		if r.serverID == serverID {
			names = append(names, name)
			delete(g.routes, name)
		}
	}
	if len(names) > 0 {
		g.srv.DeleteTools(names...)
	}
}

// setStatus records a server's state, and the error to explain it.
func (g *Gateway) setStatus(serverID string, status Status, errText string) {
	g.mu.Lock()
	g.status[serverID] = status
	if errText == "" {
		delete(g.errs, serverID)
	} else {
		g.errs[serverID] = errText
	}
	g.mu.Unlock()
	g.changed()
}

// Start begins serving the gateway on port and connects the configured servers.
func (g *Gateway) Start(ctx context.Context, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("port %d is out of range", port)
	}

	g.mu.Lock()
	if g.http != nil {
		g.mu.Unlock()
		return errors.New("the gateway is already serving")
	}
	g.port = port
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	g.mu.Unlock()

	stream := server.NewStreamableHTTPServer(g.srv)
	sse := server.NewSSEServer(g.srv,
		server.WithStaticBasePath("/"),
		server.WithSSEEndpoint("/sse"),
		server.WithMessageEndpoint("/message"),
		server.WithBaseURL(base),
	)

	mux := http.NewServeMux()
	// Everything that serves the tool surface is behind the caller check. The
	// discovery endpoints below stay open, since a client has to reach them in
	// order to obtain the credential in the first place. Each guarded endpoint
	// names itself, so its challenge points at metadata that describes it.
	mux.Handle("/mcp", g.guard("mcp", stream))
	mux.Handle("/sse", g.guard("sse", sse.SSEHandler()))
	mux.Handle("/message", g.guard("sse", sse.MessageHandler()))
	mux.Handle("/healthz", g.guard("mcp", http.HandlerFunc(g.handleHealth)))

	// Our own sign-in with an upstream, and the endpoints that let callers
	// sign in with us. These stay mounted whatever the mode, so that turning
	// the authorization server on takes effect without restarting.
	mux.HandleFunc("/oauth/callback", g.handleOAuthCallback)
	mux.HandleFunc(server.WellKnownProtectedResourcePath, g.handleProtectedResource)
	for _, resourcePath := range resourcePaths {
		mux.HandleFunc(protectedResourceMetadataPath(resourcePath), g.handleProtectedResource)
	}
	mux.HandleFunc("/.well-known/oauth-authorization-server", g.serveAsAuthServer(g.handleAuthServerMetadata))
	mux.HandleFunc("/oauth/register", g.serveAsAuthServer(g.handleRegister))
	mux.HandleFunc("/oauth/authorize", g.serveAsAuthServer(g.handleAuthorize))
	mux.HandleFunc("/oauth/token", g.serveAsAuthServer(g.handleToken))

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}
	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	g.mu.Lock()
	g.stream, g.sse, g.http = stream, sse, httpSrv
	g.mu.Unlock()

	go func() {
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			g.note("", "serving stopped: "+err.Error(), true)
			g.changed()
		}
	}()

	g.note("", "serving on "+base, false)
	g.changed()
	// Connecting the configured servers is reported per server, on its row and
	// in the activity list. A server that asks to be signed in to, or that is
	// simply down, is not a failure to serve.
	_ = g.Sync(ctx)
	return nil
}

// Stop stops serving and disconnects every upstream.
func (g *Gateway) Stop(ctx context.Context) error {
	g.syncMu.Lock()
	defer g.syncMu.Unlock()

	g.mu.Lock()
	httpSrv := g.http
	g.http, g.stream, g.sse = nil, nil, nil
	ups := make([]*Upstream, 0, len(g.upstreams))
	for id, up := range g.upstreams {
		ups = append(ups, up)
		delete(g.upstreams, id)
		delete(g.status, id)
		delete(g.errs, id)
		g.unregisterLocked(id)
	}
	g.pending = map[string]*pendingAuth{}
	g.mu.Unlock()

	for _, up := range ups {
		_ = up.Close()
	}
	if httpSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}

	g.note("", "gateway stopped", false)
	g.changed()
	return nil
}

// Serving reports whether the gateway is listening.
func (g *Gateway) Serving() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.http != nil
}

// Port returns the port the gateway serves on, or 0 when it is not serving.
func (g *Gateway) Port() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.http == nil {
		return 0
	}
	return g.port
}

// BaseURL is where the gateway serves, or the configured port's URL when it is
// not serving.
func (g *Gateway) BaseURL() string {
	port := g.Port()
	if port == 0 {
		port = g.cfg.Port
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// redirectURI is where an authorization server sends the browser back.
func (g *Gateway) redirectURI() string {
	return g.BaseURL() + "/oauth/callback"
}

// Endpoints lists the URLs the gateway serves, for the interface to show.
func (g *Gateway) Endpoints() []string {
	base := g.BaseURL()
	return []string{
		base + "/mcp",
		base + "/sse",
		base + "/message",
		base + "/healthz",
	}
}

// handleHealth answers the readiness probe.
func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	tools := len(g.routes)
	g.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"tools":  tools,
	})
}
