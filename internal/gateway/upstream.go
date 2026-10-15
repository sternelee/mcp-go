package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"mcp-go/internal/config"
)

// NeedsAuthError reports that an upstream answered with "unauthorized", so the
// user has to sign in before the gateway can use it.
type NeedsAuthError struct {
	ServerID string
	Err      error
}

func (e *NeedsAuthError) Error() string {
	return "authorization required: " + e.Err.Error()
}

func (e *NeedsAuthError) Unwrap() error { return e.Err }

// clientName is what the gateway calls itself when registering with an
// authorization server.
const clientName = "MCP Gateway"

// Upstream is one connected MCP server the gateway proxies.
type Upstream struct {
	Config config.ServerConfig

	client *client.Client
	tools  []mcp.Tool
}

// Tools returns the tools as the upstream reported them, before namespacing.
func (u *Upstream) Tools() []mcp.Tool { return u.tools }

// CallTool invokes a tool by its upstream name.
func (u *Upstream) CallTool(ctx context.Context, name string, args any) (*mcp.CallToolResult, error) {
	return u.client.CallTool(ctx, mcp.CallToolRequest{
		Request: mcp.Request{Method: string(mcp.MethodToolsCall)},
		Params:  mcp.CallToolParams{Name: name, Arguments: args},
	})
}

// Close shuts the connection down, ending a stdio child process with it.
func (u *Upstream) Close() error { return u.client.Close() }

// connectOptions is what connecting needs from the gateway that owns it.
type connectOptions struct {
	creds       *config.CredentialStore
	redirectURI string
	version     string
}

// connect starts an upstream and lists its tools.
func connect(ctx context.Context, sc config.ServerConfig, opts connectOptions) (*Upstream, error) {
	// A stdio server explains itself on stderr, so keep what it says in case
	// connecting fails. Draining it also keeps the child from blocking on a
	// full pipe.
	logs := NewLogBuffer(50)
	c, err := newClient(sc, opts, logs)
	if err != nil {
		return nil, err
	}
	// Start is idempotent; a stdio transport has already started by now.
	if err := c.Start(ctx); err != nil {
		c.Close()
		return nil, withLogs(err, logs)
	}
	if _, err := c.Initialize(ctx, initializeRequest(opts.version)); err != nil {
		c.Close()
		if client.IsOAuthAuthorizationRequiredError(err) {
			return nil, &NeedsAuthError{ServerID: sc.ID, Err: err}
		}
		return nil, withLogs(err, logs)
	}
	result, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		return nil, withLogs(err, logs)
	}
	return &Upstream{Config: sc, client: c, tools: result.Tools}, nil
}

// withLogs adds what the server wrote to stderr, which is where a command says
// why it would not run.
func withLogs(err error, logs *LogBuffer) error {
	text := strings.TrimSpace(logs.String())
	if text == "" {
		return err
	}
	return fmt.Errorf("%w\n%s", err, text)
}

// newClient builds the transport an upstream's configuration calls for.
func newClient(sc config.ServerConfig, opts connectOptions, logs *LogBuffer) (*client.Client, error) {
	switch sc.Transport {
	case config.TransportStdio:
		// Capture the child's whole stderr rather than the transport's bounded
		// ring buffer, so a server that fails to start can say why.
		return client.NewStdioMCPClientWithOptions(sc.Command, sc.Env, sc.Args,
			transport.WithCommandStderrWriter(logs))
	case config.TransportSSE:
		if sc.OAuth {
			return client.NewOAuthSSEClient(sc.URL, oauthConfig(sc, opts))
		}
		return client.NewSSEMCPClient(sc.URL, transport.WithHeaders(requestHeaders(sc, opts)))
	case config.TransportHTTP:
		if sc.OAuth {
			return client.NewOAuthStreamableHttpClient(sc.URL, oauthConfig(sc, opts))
		}
		return client.NewStreamableHttpClient(sc.URL, transport.WithHTTPHeaders(requestHeaders(sc, opts)))
	}
	return nil, fmt.Errorf("unknown transport %q", sc.Transport)
}

// requestHeaders are the headers every request to a remote upstream carries:
// the ones written in the configuration, plus the stored token when the server
// takes one.
//
// The token is read here rather than kept in the configuration so that it lives
// in the system's credential store, not in a file beside the other settings.
func requestHeaders(sc config.ServerConfig, opts connectOptions) map[string]string {
	headers := make(map[string]string, len(sc.Headers)+1)
	for name, value := range sc.Headers {
		headers[name] = value
	}
	if sc.TokenHeader != "" {
		if token := opts.creds.UpstreamToken(sc.ID); token != "" {
			// The token wins over a header written by hand under the same name.
			headers[sc.TokenHeader] = tokenValue(sc.TokenScheme, token)
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// tokenValue renders a token as the header value, prefixing the scheme the
// server expects.
func tokenValue(scheme, token string) string {
	if scheme = strings.TrimSpace(scheme); scheme != "" {
		return scheme + " " + token
	}
	return token
}

// oauthConfig describes how to authorize against a remote upstream. The
// redirect URI is the gateway's own callback, so it stays valid across runs.
func oauthConfig(sc config.ServerConfig, opts connectOptions) transport.OAuthConfig {
	saved := opts.creds.Client(sc.ID)
	return transport.OAuthConfig{
		ClientID:                saved.ClientID,
		ClientSecret:            saved.Secret,
		CredentialsIssuer:       saved.Issuer,
		TokenEndpointAuthMethod: saved.AuthMethod,
		RedirectURI:             opts.redirectURI,
		TokenStore:              opts.creds.TokenStore(sc.ID),
		PKCEEnabled:             true,
		HTTPClient:              &http.Client{Timeout: 30 * time.Second},
	}
}

// initializeRequest is the handshake the gateway offers every upstream.
func initializeRequest(version string) mcp.InitializeRequest {
	return mcp.InitializeRequest{
		Request: mcp.Request{Method: string(mcp.MethodInitialize)},
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "mcp-gateway", Version: version},
		},
	}
}

// maxLogLine caps a single line, so a stream that never writes a newline stays
// bounded and the interface keeps a readable width.
const maxLogLine = 512

// LogBuffer keeps the most recent lines a process wrote, for error messages to
// quote.
type LogBuffer struct {
	mu    sync.Mutex
	limit int
	lines []string
	part  string
}

// NewLogBuffer returns a buffer that keeps at most limit lines.
func NewLogBuffer(limit int) *LogBuffer {
	return &LogBuffer{limit: limit}
}

// Write implements io.Writer. Writes arrive as the process flushes, which is
// usually per line but not guaranteed.
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.part += string(p)
	for {
		i := strings.IndexByte(b.part, '\n')
		if i < 0 {
			break
		}
		b.append(b.part[:i])
		b.part = b.part[i+1:]
	}
	// A process that writes without newlines gets a line of its own once it is
	// long enough, rather than growing the buffer.
	if len(b.part) > maxLogLine {
		b.append(b.part)
		b.part = ""
	}
	return len(p), nil
}

// append adds a line, dropping the oldest once the limit is reached. The
// caller holds the lock.
func (b *LogBuffer) append(line string) {
	line = strings.TrimRight(line, "\r")
	if len(line) > maxLogLine {
		line = line[:maxLogLine] + "…"
	}
	b.lines = append(b.lines, line)
	if len(b.lines) > b.limit {
		b.lines = b.lines[len(b.lines)-b.limit:]
	}
}

// Lines returns the lines written so far, oldest first.
func (b *LogBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := make([]string, len(b.lines))
	copy(lines, b.lines)
	if b.part != "" {
		lines = append(lines, strings.TrimRight(b.part, "\r"))
	}
	return lines
}

// String returns the lines as one block of text.
func (b *LogBuffer) String() string {
	return strings.Join(b.Lines(), "\n")
}
