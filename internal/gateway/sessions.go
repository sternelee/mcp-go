package gateway

import (
	"context"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// SessionView is one connected agent, as the interface shows it.
type SessionView struct {
	ID       string // the session identifier clients send with every request
	Client   string // the agent as it introduced itself, e.g. "Cline"
	Version  string // that agent's reported version
	Protocol string // negotiated MCP protocol version
	// Connected is when the gateway first saw the agent; LastActive is its
	// most recent tool call.
	Connected  time.Time
	LastActive time.Time
	Calls      int
}

// sessionRecord is what the gateway remembers behind one SessionView. The
// ClientSession itself is not kept: it belongs to the MCP server.
type sessionRecord struct {
	view SessionView
}

// introduce records the identity a client gave on its request, whether that
// was the legacy initialize handshake or a modern request's _meta. The first
// time an agent names itself is worth noting in the activity, since at
// register time the gateway only knows the session's ID.
func (g *Gateway) introduce(session server.ClientSession, client mcp.Implementation, protocol string) {
	named := ""
	g.mu.Lock()
	if record, ok := g.sessions[session.SessionID()]; ok {
		if client.Name != "" && record.view.Client != client.Name {
			named = client.Name
		}
		record.view.Client = client.Name
		record.view.Version = client.Version
		record.view.Protocol = protocol
	}
	g.mu.Unlock()
	if named != "" {
		g.note("", "agent connected: "+named, false)
	}
}

// newHooks returns the hook registry the gateway reports agent activity
// through. It is its own function so tests can build servers without it.
func newHooks() *server.Hooks {
	return &server.Hooks{}
}

// trackSessions installs the register, initialize and call hooks that follow
// which agents are connected and what they ask the gateway to do.
func (g *Gateway) trackSessions() {
	hooks := g.srv.GetHooks()
	if hooks == nil {
		return
	}
	hooks.AddOnRegisterSession(func(_ context.Context, session server.ClientSession) {
		now := time.Now()
		g.mu.Lock()
		g.sessions[session.SessionID()] = &sessionRecord{view: SessionView{
			ID:        session.SessionID(),
			Connected: now,
		}}
		g.mu.Unlock()
	})
	hooks.AddOnUnregisterSession(func(_ context.Context, session server.ClientSession) {
		g.mu.Lock()
		delete(g.sessions, session.SessionID())
		g.mu.Unlock()
		g.note("", "agent disconnected: "+describeSession(session), false)
	})
	// Clients reach the gateway two ways: the legacy initialize handshake
	// fires AddAfterInitialize, while protocol 2026-07-28 clients skip the
	// handshake and send server/discover, which fires AddAfterDiscover instead.
	hooks.AddAfterInitialize(func(ctx context.Context, _ any, message *mcp.InitializeRequest, result *mcp.InitializeResult) {
		if session := server.ClientSessionFromContext(ctx); session != nil {
			g.introduce(session, message.Params.ClientInfo, result.ProtocolVersion)
		}
	})
	hooks.AddAfterDiscover(func(ctx context.Context, _ any, message *mcp.DiscoverRequest, _ *mcp.DiscoverResult) {
		session := server.ClientSessionFromContext(ctx)
		if session == nil {
			return
		}
		// The identity travels in the request's _meta; the protocol version is
		// a property of the request, not the session, so take both from here.
		if info := server.RequestProtocolInfoFromContext(ctx); info != nil {
			client := mcp.Implementation{}
			if info.ClientInfo != nil {
				client = *info.ClientInfo
			}
			g.introduce(session, client, info.ProtocolVersion)
		} else if withInfo, ok := session.(server.SessionWithClientInfo); ok {
			// Without protocol metadata fall back to whatever the session
			// carries and the request itself declares.
			g.introduce(session, withInfo.GetClientInfo(), message.Params.Meta.ProtocolVersion())
		}
	})
	hooks.AddBeforeCallTool(func(ctx context.Context, _ any, _ *mcp.CallToolRequest) {
		session := server.ClientSessionFromContext(ctx)
		if session == nil {
			return
		}
		now := time.Now()
		g.mu.Lock()
		if record, ok := g.sessions[session.SessionID()]; ok {
			record.view.Calls++
			record.view.LastActive = now
		}
		g.mu.Unlock()
	})
}

// describeSession names an agent by its client info, or by its session ID
// before it has initialized.
func describeSession(session server.ClientSession) string {
	if info, ok := session.(server.SessionWithClientInfo); ok {
		if client := info.GetClientInfo(); client.Name != "" {
			return client.Name
		}
	}
	return session.SessionID()
}

// Sessions lists the connected agents, longest-connected first.
func (g *Gateway) Sessions() []SessionView {
	g.mu.Lock()
	views := make([]SessionView, 0, len(g.sessions))
	for _, record := range g.sessions {
		views = append(views, record.view)
	}
	g.mu.Unlock()
	sort.Slice(views, func(i, j int) bool {
		return views[i].Connected.Before(views[j].Connected)
	})
	return views
}
