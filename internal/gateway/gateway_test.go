package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"mcp-go/internal/config"
)

// stdioHelperEnv makes the test binary serve MCP over stdio instead of running
// tests, so the stdio transport can be exercised against a real subprocess.
const stdioHelperEnv = "MCP_GO_STDIO_HELPER"

// TestMain keeps the tests' configuration and credentials out of the real user
// directories, and doubles as the stdio server the stdio test spawns.
func TestMain(m *testing.M) {
	if os.Getenv(stdioHelperEnv) == "1" {
		serveStdio()
		return
	}
	dir, err := os.MkdirTemp("", "mcp-go-gateway-test")
	if err != nil {
		panic(err)
	}
	mygo.App.SetPath(mygo.PathUserData, dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// serveStdio answers MCP over the process's standard streams.
func serveStdio() {
	srv := server.NewMCPServer("helper", "test", server.WithToolCapabilities(true))
	srv.AddTool(mcp.NewTool("echo", mcp.WithDescription("Echo the text back.")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("helper: " + req.GetString("text", "")), nil
		})
	if err := server.ServeStdio(srv); err != nil {
		fmt.Fprintln(os.Stderr, "stdio helper:", err)
		os.Exit(1)
	}
}

// fakeUpstream is an MCP server with a tool that answers, and one that fails.
func fakeUpstream(t *testing.T) *server.MCPServer {
	t.Helper()
	srv := server.NewMCPServer("fake", "test", server.WithToolCapabilities(true))
	srv.AddTool(mcp.NewTool("echo", mcp.WithDescription("Echo the text back.")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(req.GetString("text", "")), nil
		})
	srv.AddTool(mcp.NewTool("boom", mcp.WithDescription("Always fails.")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("upstream exploded")
		})
	return srv
}

// newTestGateway returns a gateway whose configuration holds the given servers.
func newTestGateway(t *testing.T, servers ...config.ServerConfig) *Gateway {
	t.Helper()
	return newTestGatewayWith(t, config.NewMemoryCredentialStore(), servers...)
}

// newTestGatewayWith is newTestGateway with a credential store of the caller's
// choosing, for the tests that need one already holding a token.
func newTestGatewayWith(t *testing.T, creds *config.CredentialStore, servers ...config.ServerConfig) *Gateway {
	t.Helper()
	cfg := config.Default()
	cfg.Servers = servers
	cfg.EnsureAliases()
	g := New(cfg, creds, "test")
	// Each test gets its own store, so tokens and clients cannot leak between
	// them through the shared data directory.
	g.tokens = newIsolatedTokens(t)
	return g
}

// newIsolatedTokens is an empty issued-token store in a directory of its own.
func newIsolatedTokens(t *testing.T) *issuedTokens {
	t.Helper()
	return &issuedTokens{
		path:    filepath.Join(t.TempDir(), "gateway-tokens.json"),
		access:  map[string]tokenRecord{},
		refresh: map[string]tokenRecord{},
		clients: map[string]clientRecord{},
		codes:   map[string]authCode{},
	}
}

// attach connects srv in place of the transport the configuration names, which
// is how the gateway would hold it after a successful connection.
func attach(t *testing.T, g *Gateway, sc config.ServerConfig, srv *server.MCPServer) {
	t.Helper()
	ctx := context.Background()

	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("in-process client: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := c.Initialize(ctx, initializeRequest("test")); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	result, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	up := &Upstream{Config: sc, client: c, tools: result.Tools}
	g.mu.Lock()
	g.upstreams[sc.ID] = up
	g.status[sc.ID] = StatusReady
	g.registerLocked(up)
	g.mu.Unlock()
}

// downstream connects a client to the gateway itself.
func downstream(t *testing.T, g *Gateway) *client.Client {
	t.Helper()
	ctx := context.Background()

	c, err := client.NewInProcessClient(g.Downstream())
	if err != nil {
		t.Fatalf("in-process client: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := c.Initialize(ctx, initializeRequest("test")); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

// listNames is the set of tool names a client of the gateway sees.
func listNames(t *testing.T, c *client.Client) map[string]bool {
	t.Helper()
	result, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make(map[string]bool, len(result.Tools)+1)
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	return names
}

// textOf joins the text a tool result carries.
func textOf(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func call(t *testing.T, c *client.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := c.CallTool(context.Background(), mcp.CallToolRequest{
		Request: mcp.Request{Method: string(mcp.MethodToolsCall)},
		Params:  mcp.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

func TestGatewayNamespacesUpstreamTools(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	attach(t, g, sc, fakeUpstream(t))

	names := listNames(t, downstream(t, g))
	for _, want := range []string{"filesystem__echo", "filesystem__boom", metaToolName} {
		if !names[want] {
			t.Errorf("tool %q is missing from %v", want, names)
		}
	}
}

func TestGatewayRoutesCallAndNamesTheOrigin(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	attach(t, g, sc, fakeUpstream(t))

	result := call(t, downstream(t, g), "filesystem__echo", map[string]any{"text": "hello"})
	if result.IsError {
		t.Fatalf("echo reported an error: %s", textOf(result))
	}
	if got := textOf(result); got != "hello" {
		t.Errorf("echo returned %q, want %q", got, "hello")
	}
}

func TestFailingUpstreamIsAToolErrorNotAProtocolError(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	attach(t, g, sc, fakeUpstream(t))

	result := call(t, downstream(t, g), "filesystem__boom", nil)
	if !result.IsError {
		t.Fatal("a failing tool should answer with an error result")
	}
	if !strings.Contains(textOf(result), "exploded") {
		t.Errorf("the error text %q does not say what happened", textOf(result))
	}
}

func TestDisconnectingWithdrawsTools(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	attach(t, g, sc, fakeUpstream(t))
	c := downstream(t, g)

	g.mu.Lock()
	g.unregisterLocked(sc.ID)
	delete(g.upstreams, sc.ID)
	g.mu.Unlock()

	names := listNames(t, c)
	if names["filesystem__echo"] {
		t.Error("echo is still listed after its server was disconnected")
	}
	if !names[metaToolName] {
		t.Error("the gateway's own tool disappeared with the server")
	}
}

func TestToolsReportTheirServerAndSchema(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	attach(t, g, sc, fakeUpstream(t))

	var found bool
	for _, tool := range g.Tools() {
		if tool.Name != "filesystem__echo" {
			continue
		}
		found = true
		if tool.ServerName != "Filesystem" {
			t.Errorf("ServerName is %q, want %q", tool.ServerName, "Filesystem")
		}
		if tool.Upstream != "echo" {
			t.Errorf("Upstream is %q, want %q", tool.Upstream, "echo")
		}
		if !strings.Contains(tool.Description, "Filesystem") {
			t.Errorf("the description %q does not name the server", tool.Description)
		}
		if !strings.Contains(tool.Schema, "properties") {
			t.Errorf("the schema %q does not look like a JSON schema", tool.Schema)
		}
	}
	if !found {
		t.Error("the echo tool is missing from the tool list")
	}
}

func TestServerViewsReportStatus(t *testing.T) {
	ready := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	off := config.ServerConfig{
		ID: "s2", Name: "Search", Alias: "search",
		Transport: config.TransportHTTP, URL: "https://example.com/mcp", Enabled: false,
	}
	g := newTestGateway(t, ready, off)
	attach(t, g, ready, fakeUpstream(t))

	views := g.Servers()
	if len(views) != 2 {
		t.Fatalf("got %d servers, want 2", len(views))
	}
	byID := map[string]ServerView{}
	for _, v := range views {
		byID[v.Config.ID] = v
	}
	if got := byID["s1"]; got.Status != StatusReady || got.Tools != 2 {
		t.Errorf("s1 is %q with %d tools, want %q with 2", got.Status, got.Tools, StatusReady)
	}
	if got := byID["s2"]; got.Status != StatusDisabled || got.Tools != 0 {
		t.Errorf("s2 is %q with %d tools, want %q with 0", got.Status, got.Tools, StatusDisabled)
	}
}

func TestSyncDisconnectsServersThatAreGone(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	attach(t, g, sc, fakeUpstream(t))

	if removed := g.cfg.Remove("s1"); !removed {
		t.Fatal("removing the server from the configuration failed")
	}
	if err := g.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if names := listNames(t, downstream(t, g)); names["filesystem__echo"] {
		t.Error("a removed server's tools are still being served")
	}
}

func TestConnectOverARealSubprocess(t *testing.T) {
	// The test binary serves MCP over stdio when it is given the helper's
	// environment variable, so this spawns a real child process.
	sc := config.ServerConfig{
		ID: "s1", Name: "Helper", Alias: "helper",
		Transport: config.TransportStdio,
		Command:   os.Args[0],
		Env:       []string{stdioHelperEnv + "=1"},
		Enabled:   true,
	}

	up, err := connect(context.Background(), sc, connectOptions{
		creds:   config.NewMemoryCredentialStore(),
		version: "test",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = up.Close() })

	var found bool
	for _, tool := range up.Tools() {
		if tool.Name == "echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the subprocess's tools are %v", up.Tools())
	}

	result, err := up.CallTool(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := textOf(result); got != "helper: hi" {
		t.Errorf("echo returned %q, want %q", got, "helper: hi")
	}
}

func TestConnectingAMissingCommandFails(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Missing", Alias: "missing",
		Transport: config.TransportStdio,
		Command:   "mcp-go-no-such-command-here",
		Enabled:   true,
	}
	if _, err := connect(context.Background(), sc, connectOptions{
		creds:   config.NewMemoryCredentialStore(),
		version: "test",
	}); err == nil {
		t.Fatal("connecting a command that does not exist should fail")
	}
}

func TestSyncReportsAFailedServerWithoutStopping(t *testing.T) {
	broken := config.ServerConfig{
		ID: "s1", Name: "Broken", Alias: "broken",
		Transport: config.TransportStdio,
		Command:   "mcp-go-no-such-command-here",
		Enabled:   true,
	}
	good := config.ServerConfig{
		ID: "s2", Name: "Helper", Alias: "helper",
		Transport: config.TransportStdio,
		Command:   os.Args[0],
		Env:       []string{stdioHelperEnv + "=1"},
		Enabled:   true,
	}
	g := newTestGateway(t, broken, good)

	err := g.Sync(context.Background())
	if err == nil {
		t.Error("sync should report the server it could not connect")
	}
	t.Cleanup(func() { _ = g.Stop(context.Background()) })

	view, ok := g.Server("s1")
	if !ok {
		t.Fatal("the broken server is missing from the views")
	}
	if view.Status != StatusError {
		t.Errorf("the broken server is %q, want %q", view.Status, StatusError)
	}
	if view.Err == "" {
		t.Error("the failure has no explanation to show")
	}

	// The server after it must still have connected.
	ready, _ := g.Server("s2")
	if ready.Status != StatusReady {
		t.Errorf("the working server is %q, want %q", ready.Status, StatusReady)
	}
	if !listNames(t, downstream(t, g))["helper__echo"] {
		t.Error("the working server's tools are not being served")
	}
}

func TestLogBufferKeepsTheMostRecentLines(t *testing.T) {
	buffer := NewLogBuffer(3)
	buffer.Write([]byte("one\ntwo\n"))
	buffer.Write([]byte("three\nfour\n"))

	lines := buffer.Lines()
	if len(lines) != 3 {
		t.Fatalf("kept %d lines, want 3: %q", len(lines), lines)
	}
	if lines[0] != "two" || lines[2] != "four" {
		t.Errorf("lines are %q, want the last three", lines)
	}
}

func TestLogBufferKeepsALineWithoutItsNewline(t *testing.T) {
	buffer := NewLogBuffer(5)
	buffer.Write([]byte("still writing"))

	if got := buffer.String(); got != "still writing" {
		t.Errorf("String is %q", got)
	}
}

func TestWithLogsQuotesWhatTheCommandSaid(t *testing.T) {
	logs := NewLogBuffer(5)
	logs.Write([]byte("Error: no API key is set\n"))

	err := withLogs(errors.New("initialize: EOF"), logs)
	if !strings.Contains(err.Error(), "no API key is set") {
		t.Errorf("the error does not quote the command: %v", err)
	}
	if !strings.Contains(err.Error(), "initialize: EOF") {
		t.Errorf("the original error is missing: %v", err)
	}
}

func TestWithLogsLeavesASilentServerAlone(t *testing.T) {
	original := errors.New("boom")
	if got := withLogs(original, NewLogBuffer(5)); got != original {
		t.Errorf("withLogs changed an error with no logs: %v", got)
	}
}

func TestWritingWithoutNewlinesStaysBounded(t *testing.T) {
	const limit = 5
	buffer := NewLogBuffer(limit)
	// A megabyte of one "line" should not be kept whole.
	for range 1024 {
		buffer.Write([]byte(strings.Repeat("x", 1024)))
	}
	bound := (maxLogLine + 2) * (limit + 1)
	if total := len(buffer.String()); total > bound {
		t.Errorf("the buffer kept %d bytes of a newline-free stream, over the %d it should", total, bound)
	}
	if len(buffer.Lines()) > limit+1 {
		t.Errorf("the buffer kept %d lines, over the %d it should", len(buffer.Lines()), limit+1)
	}
}

func TestEndpointsAreLoopback(t *testing.T) {
	g := newTestGateway(t)
	for _, endpoint := range g.Endpoints() {
		if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
			t.Errorf("endpoint %q does not stay on the loopback interface", endpoint)
		}
	}
	g.cfg.Port = 7100
	for _, endpoint := range g.Endpoints() {
		if !strings.HasPrefix(endpoint, "http://127.0.0.1:7100/") {
			t.Errorf("endpoint %q does not use the configured port", endpoint)
		}
	}
}

// freePort asks the system for a port nothing is using.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// startServing opens a free port and returns its base URL.
func startServing(t *testing.T, g *Gateway) string {
	t.Helper()
	port := freePort(t)
	if err := g.Start(context.Background(), port); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = g.Stop(context.Background()) })
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// rpcPosts sends one JSON-RPC message to the gateway's streamable HTTP
// endpoint and returns the response.
func rpcPosts(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	return response
}

func TestGatewayAnswersMCPOverStreamableHTTP(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	attach(t, g, sc, fakeUpstream(t))
	base := startServing(t, g)

	// A client that speaks the older handshake gets the classic response.
	response := rpcPosts(t, base, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, nil)
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initialize answered %d: %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "mcp-gateway") {
		t.Errorf("the server does not introduce itself: %s", body)
	}

	sessionID := response.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("the server did not hand out a session id")
	}

	// Listing tools is what proves the aggregated tools are reachable over HTTP.
	listed := rpcPosts(t, base, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		map[string]string{"Mcp-Session-Id": sessionID})
	defer listed.Body.Close()
	listedBody, err := io.ReadAll(listed.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	for _, want := range []string{"filesystem__echo", "filesystem__boom", metaToolName} {
		if !strings.Contains(string(listedBody), want) {
			t.Errorf("tools/list does not mention %q: %s", want, listedBody)
		}
	}
}

func TestGatewayAnswersLegacySSE(t *testing.T) {
	g := newTestGateway(t)
	base := startServing(t, g)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sse", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("connect to /sse: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("/sse answered %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Errorf("/sse answered with %q, want an event stream", contentType)
	}
	// The server opens with the endpoint the client must post to.
	buffer := make([]byte, 512)
	n, _ := response.Body.Read(buffer)
	if !strings.Contains(string(buffer[:n]), "event: endpoint") {
		t.Errorf("the stream did not begin with an endpoint event: %q", buffer[:n])
	}
}

func TestGatewayServesOverHTTP(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "true", Enabled: true,
	}
	g := newTestGateway(t, sc)
	attach(t, g, sc, fakeUpstream(t))

	base := startServing(t, g)
	if !g.Serving() {
		t.Fatal("the gateway does not report itself as serving")
	}

	response, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("healthz answered %s", response.Status)
	}
	var health struct {
		Status string `json:"status"`
		Tools  int    `json:"tools"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.Status != "ok" || health.Tools != 2 {
		t.Errorf("health is %+v, want ok with 2 tools", health)
	}
}
