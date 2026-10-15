package main

import (
	"os"
	"strings"
	"testing"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"mcp-go/internal/config"
	"mcp-go/internal/gateway"
)

// TestMain keeps the tests' configuration out of the real user directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mcp-go-test")
	if err != nil {
		panic(err)
	}
	mygo.App.SetPath(mygo.PathUserData, dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newTestApp returns an app with no window, which is how a view test runs it.
func newTestApp(t *testing.T, servers ...config.ServerConfig) *app {
	t.Helper()
	cfg := config.Default()
	cfg.Servers = servers
	cfg.EnsureAliases()
	creds := config.NewMemoryCredentialStore()
	gw := gateway.New(cfg, creds, "test")
	return newApp(cfg, creds, gw)
}

func stdioServer() config.ServerConfig {
	return config.ServerConfig{
		ID: "s1", Name: "Filesystem", Alias: "filesystem",
		Transport: config.TransportStdio, Command: "npx", Args: []string{"-y", "server"},
		Enabled: true,
	}
}

func TestServersPageShowsEmptyState(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("No servers yet") {
		t.Errorf("the empty state is missing; texts are %q", tt.Texts())
	}
}

func TestServersPageListsAServer(t *testing.T) {
	a := newTestApp(t, stdioServer())
	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("Filesystem") {
		t.Fatalf("the server is not listed; texts are %q", tt.Texts())
	}
	if !tt.HasText("stdio") {
		t.Errorf("the transport is not shown; texts are %q", tt.Texts())
	}
}

func TestAServerThatIsOffShowsAnOffSwitch(t *testing.T) {
	sc := stdioServer()
	sc.Enabled = false
	a := newTestApp(t, sc)
	tt := ui.NewTester(a.view, 1080, 740)
	if _, ok := tt.Find(sc.Name + " enabled"); !ok {
		t.Errorf("a server that is off has no switch; texts are %q", tt.Texts())
	}
	// Connecting is the switch's job, so a plain server offers no button for it.
	if tt.HasText("Connect") || tt.HasText("Disconnect") {
		t.Errorf("a plain server is offered a Connect/Disconnect button as well as a switch; texts are %q", tt.Texts())
	}
	// The row says why the switch is off, rather than leaving it to a colour.
	for _, text := range tt.Texts() {
		if strings.HasPrefix(text, "Off · npx -y server") {
			return
		}
	}
	t.Errorf("the row does not say the server is off; texts are %q", tt.Texts())
}

func TestTheSwitchIsDrawnInsideTheWindow(t *testing.T) {
	sc := stdioServer()
	sc.Enabled = false
	a := newTestApp(t, sc)

	const width, height = 1080, 740
	tt := ui.NewTester(a.view, width, height)
	rect, ok := tt.Find(sc.Name + " enabled")
	if !ok {
		t.Fatal("no switch is drawn")
	}
	if rect.W <= 0 || rect.H <= 0 {
		t.Errorf("the switch has no size: %+v", rect)
	}
	if rect.X < 0 || rect.Y < 0 || rect.X+rect.W > width || rect.Y+rect.H > height {
		t.Errorf("the switch falls outside the window %dx%d: %+v", width, height, rect)
	}
}

func TestAConnectedServerHasNoConnectButton(t *testing.T) {
	sc := stdioServer()
	a := newTestApp(t, sc)
	// Stand in for a connection the gateway has made.
	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusReady, Tools: 2}

	tt := ui.NewTester(a.view, 1080, 740)
	if tt.HasText("Connect") || tt.HasText("Disconnect") {
		t.Errorf("a connected plain server is offered a Connect/Disconnect button; texts are %q", tt.Texts())
	}
	if _, ok := tt.Find(sc.Name + " enabled"); !ok {
		t.Errorf("the switch is missing; texts are %q", tt.Texts())
	}
}

func TestConnectingAndDisconnectingChangeTheSetting(t *testing.T) {
	sc := stdioServer()
	sc.Enabled = false
	a := newTestApp(t, sc)

	a.connect(sc.ID)
	stored, _ := a.cfg.Server(sc.ID)
	if !stored.Enabled {
		t.Error("Connect left the server switched off")
	}
	if !*a.flag(sc.ID, false) {
		t.Error("Connect left the switch behind")
	}

	a.disconnect(sc.ID)
	stored, _ = a.cfg.Server(sc.ID)
	if stored.Enabled {
		t.Error("Disconnect left the server switched on, so it would come back at launch")
	}
	if *a.flag(sc.ID, true) {
		t.Error("Disconnect left the switch on")
	}
}

func TestTheSwitchConnectsAndDisconnectsAServer(t *testing.T) {
	sc := stdioServer()
	sc.Enabled = false
	a := newTestApp(t, sc)
	tt := ui.NewTester(a.view, 1080, 740)

	if err := tt.Click(sc.Name + " enabled"); err != nil {
		t.Fatalf("click the switch: %v", err)
	}
	tt.Frame()
	if stored, _ := a.cfg.Server(sc.ID); !stored.Enabled {
		t.Error("the switch did not connect the server")
	}
	if !*a.flag(sc.ID, false) {
		t.Error("the switch left its own state behind")
	}

	if err := tt.Click(sc.Name + " enabled"); err != nil {
		t.Fatalf("click the switch: %v", err)
	}
	tt.Frame()
	if stored, _ := a.cfg.Server(sc.ID); stored.Enabled {
		t.Error("the switch did not disconnect the server")
	}
}

func TestTheSwitchConnectsAnOAuthServer(t *testing.T) {
	// The switch is the same control whatever the transport, so a server that
	// also authorizes is switched on the same way.
	sc := config.ServerConfig{
		ID: "s1", Name: "Remote", Alias: "remote",
		Transport: config.TransportHTTP, URL: "https://example.com/mcp",
		OAuth: true, Enabled: false,
	}
	a := newTestApp(t, sc)
	tt := ui.NewTester(a.view, 1080, 740)

	if err := tt.Click(sc.Name + " enabled"); err != nil {
		t.Fatalf("click the switch: %v", err)
	}
	tt.Frame()
	if stored, _ := a.cfg.Server(sc.ID); !stored.Enabled {
		t.Error("the switch did not connect the OAuth server")
	}
}

func TestAPlainServerMenuOffersNoConnect(t *testing.T) {
	sc := stdioServer()
	a := newTestApp(t, sc)
	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.RightClick(sc.Name); err != nil {
		t.Fatalf("right-click the row: %v", err)
	}
	menu := strings.Join(tt.Menu(), " ")
	if strings.Contains(menu, "Connect") || strings.Contains(menu, "Disconnect") {
		t.Errorf("a plain server's menu still offers Connect/Disconnect: %q", menu)
	}
	if !strings.Contains(menu, "Reconnect") {
		t.Errorf("the menu has no Reconnect: %q", menu)
	}
}

func TestAnOAuthServerMenuOffersConnect(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Remote", Alias: "remote",
		Transport: config.TransportHTTP, URL: "https://example.com/mcp",
		OAuth: true, Enabled: true,
	}
	a := newTestApp(t, sc)
	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.RightClick(sc.Name); err != nil {
		t.Fatalf("right-click the row: %v", err)
	}
	if menu := strings.Join(tt.Menu(), " "); !strings.Contains(menu, "Connect") {
		t.Errorf("an un-authorized OAuth server's menu has no Connect: %q", menu)
	}
}

func TestASignedInOAuthServerOffersSignOut(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Remote", Alias: "remote",
		Transport: config.TransportHTTP, URL: "https://example.com/mcp",
		OAuth: true, Enabled: true,
	}
	a := newTestApp(t, sc)
	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusReady, SignedIn: true, Tools: 3}

	tt := ui.NewTester(a.view, 1080, 740)
	if tt.HasText("Connect") {
		t.Errorf("a signed-in server still offers Connect; texts are %q", tt.Texts())
	}
	if !tt.HasText("Sign out") {
		t.Errorf("a signed-in server offers no way to sign out; texts are %q", tt.Texts())
	}
}

func TestANeedsAuthServerOffersConnectAndSaysWhy(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Remote", Alias: "remote",
		Transport: config.TransportHTTP, URL: "https://example.com/mcp",
		OAuth: true, Enabled: true,
	}
	a := newTestApp(t, sc)
	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusNeedsAuth}

	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("Connect") {
		t.Errorf("a server that needs authorization offers no way to do it; texts are %q", tt.Texts())
	}
	if !tt.HasText("Sign in required · " + sc.URL) {
		t.Errorf("the row does not say why Connect is needed; texts are %q", tt.Texts())
	}
}

func TestAddingAServerOpensTheForm(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.Click("Add server…"); err != nil {
		t.Fatalf("click Add: %v", err)
	}
	if !a.form.editing {
		t.Fatal("the form did not open")
	}
	if !tt.HasText("Command") {
		t.Errorf("the form does not ask for a command; texts are %q", tt.Texts())
	}
}

func TestTypingANameIntoTheForm(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.Click("Add server…"); err != nil {
		t.Fatalf("click Add: %v", err)
	}
	// The name field asks for focus when the form opens.
	tt.Type("Filesystem")
	if a.form.name != "Filesystem" {
		t.Errorf("the typed name is %q, want %q", a.form.name, "Filesystem")
	}
}

func TestSidebarNavigatesToTools(t *testing.T) {
	a := newTestApp(t, stdioServer())
	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.Click("Tools"); err != nil {
		t.Fatalf("click Tools: %v", err)
	}
	tt.Frame()
	if a.router.Path() != "/tools" {
		t.Fatalf("the page is %q, want %q", a.router.Path(), "/tools")
	}
	if !tt.HasText("What clients of the gateway see, named after the server each tool comes from.") {
		t.Errorf("the tools page is not shown; texts are %q", tt.Texts())
	}
}

func TestSaveFormStoresAServer(t *testing.T) {
	a := newTestApp(t)
	a.addServer()
	a.form.name = "Filesystem"
	a.form.command = "npx"
	a.form.args = "-y\nserver\n/tmp"
	a.saveForm()

	if a.form.editing {
		t.Error("the form should close once saved")
	}
	sc, ok := a.cfg.Server(a.cfg.Servers[0].ID)
	if !ok {
		t.Fatal("the server was not stored")
	}
	if sc.Name != "Filesystem" || sc.Command != "npx" {
		t.Errorf("stored %+v", sc)
	}
	if len(sc.Args) != 3 || sc.Args[2] != "/tmp" {
		t.Errorf("arguments are %q, want three lines", sc.Args)
	}
	if sc.Alias != "filesystem" {
		t.Errorf("alias is %q, want %q", sc.Alias, "filesystem")
	}
}

func TestSaveFormRejectsAnIncompleteServer(t *testing.T) {
	a := newTestApp(t)
	a.addServer()
	a.form.name = "Filesystem"
	a.form.command = ""
	a.saveForm()

	if !a.form.editing {
		t.Error("the form should stay open when the server is not valid")
	}
	if !strings.Contains(a.form.err, "command") {
		t.Errorf("the error %q does not mention the command", a.form.err)
	}
	if len(a.cfg.Servers) != 0 {
		t.Error("an invalid server was stored anyway")
	}
}

func TestRemoveServerForgetsIt(t *testing.T) {
	a := newTestApp(t, stdioServer())
	id := a.cfg.Servers[0].ID
	a.removeServer(id)

	if len(a.cfg.Servers) != 0 {
		t.Errorf("the server is still configured: %+v", a.cfg.Servers)
	}
}

func TestOAuthFieldsFollowTheTransport(t *testing.T) {
	a := newTestApp(t)
	a.addServer()
	a.form.transport = transportIndex(config.TransportHTTP)
	a.form.oauth = true
	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("Client ID") {
		t.Errorf("an OAuth server is not asked for a client ID; texts are %q", tt.Texts())
	}
}

func TestImportDialogIsReachableFromTheServersPage(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.Click("Paste JSON…"); err != nil {
		t.Fatalf("click Paste JSON: %v", err)
	}
	if !a.imp.open {
		t.Fatal("the import dialog did not open")
	}
	if !tt.HasText("Paste a configuration") {
		t.Errorf("the dialog is not shown; texts are %q", tt.Texts())
	}
}

func TestImportFromPastedJSON(t *testing.T) {
	a := newTestApp(t)
	a.openImport()
	a.imp.text = `{"mcpServers": {"github": {"type": "http", "url": "https://example.com/mcp"},
	                              "filesystem": {"command": "npx", "args": ["-y", "server"]}}}`
	a.importServers()

	if a.imp.err != "" {
		t.Fatalf("import failed: %s", a.imp.err)
	}
	if a.imp.open {
		t.Error("the dialog should close after a successful import")
	}
	if len(a.cfg.Servers) != 2 {
		t.Fatalf("imported %d servers, want 2", len(a.cfg.Servers))
	}

	filesystem, ok := a.serverNamed("filesystem")
	if !ok {
		t.Fatal("the stdio server was not imported")
	}
	if filesystem.Command != "npx" || len(filesystem.Args) != 2 {
		t.Errorf("imported %+v", filesystem)
	}
	if !*a.flag(filesystem.ID, false) {
		t.Error("an imported server should be switched on")
	}
}

// serverNamed finds a configured server by its display name. It must be called
// on the main thread.
func (a *app) serverNamed(name string) (config.ServerConfig, bool) {
	for _, sc := range a.cfg.Servers {
		if sc.Name == name {
			return sc, true
		}
	}
	return config.ServerConfig{}, false
}

func TestImportKeepsTheDialogOpenOnBadJSON(t *testing.T) {
	a := newTestApp(t)
	a.openImport()
	a.imp.text = "this is not json"
	a.importServers()

	if !a.imp.open {
		t.Error("the dialog should stay open so the text can be fixed")
	}
	if a.imp.err == "" {
		t.Error("no error was shown")
	}
	if len(a.cfg.Servers) != 0 {
		t.Error("a broken configuration was imported anyway")
	}
}

func TestImportReplacesAServerOfTheSameName(t *testing.T) {
	a := newTestApp(t, stdioServer())
	original := a.cfg.Servers[0].ID

	a.openImport()
	a.imp.text = `{"mcpServers": {"Filesystem": {"command": "uvx", "args": ["server"]}}}`
	a.importServers()

	if len(a.cfg.Servers) != 1 {
		t.Fatalf("there are %d servers, want 1", len(a.cfg.Servers))
	}
	replaced := a.cfg.Servers[0]
	if replaced.ID != original {
		t.Errorf("the server was replaced rather than updated: id %q, want %q", replaced.ID, original)
	}
	if replaced.Command != "uvx" {
		t.Errorf("the command is %q, want the imported one", replaced.Command)
	}
}

func TestTypingAConfigurationIntoTheImportDialog(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.Click("Paste JSON…"); err != nil {
		t.Fatalf("click Paste JSON: %v", err)
	}
	tt.Type(`{"mcpServers":{"solo":{"command":"npx"}}}`)
	tt.Frame()
	if !strings.Contains(a.imp.text, "solo") {
		t.Fatalf("the box holds %q", a.imp.text)
	}
	if err := tt.Click("Import"); err != nil {
		t.Fatalf("click Import: %v", err)
	}
	if len(a.cfg.Servers) != 1 {
		t.Fatalf("imported %d servers, want 1", len(a.cfg.Servers))
	}
	if name := a.cfg.Servers[0].Name; name != "solo" {
		t.Errorf("the imported server is named %q", name)
	}
}

func TestRemoteServerFormAsksForAToken(t *testing.T) {
	a := newTestApp(t)
	a.addServer()
	a.form.transport = transportIndex(config.TransportHTTP)
	tt := ui.NewTester(a.view, 1080, 740)
	for _, want := range []string{
		"Token header",
		"Token scheme",
		"Kept in the system keychain, never in the settings file.",
		"Other headers",
	} {
		if !tt.HasText(want) {
			t.Errorf("the form does not offer %q; texts are %q", want, tt.Texts())
		}
	}
}

func TestAnOAuthServerIsNotAskedForAToken(t *testing.T) {
	a := newTestApp(t)
	a.addServer()
	a.form.transport = transportIndex(config.TransportHTTP)
	a.form.oauth = true
	tt := ui.NewTester(a.view, 1080, 740)
	if tt.HasText("Token header") {
		t.Errorf("an OAuth server is also asked for a token; texts are %q", tt.Texts())
	}
}

func remoteServer() config.ServerConfig {
	return config.ServerConfig{
		ID: "s1", Name: "Remote", Alias: "remote",
		Transport: config.TransportHTTP, URL: "https://example.com/mcp",
		TokenHeader: "Authorization", TokenScheme: "Bearer",
	}
}

func TestASavedTokenStaysOutOfTheSettingsFile(t *testing.T) {
	const secret = "super-secret-token"
	a := newTestApp(t)
	a.addServer()
	a.form.name = "Remote"
	a.form.transport = transportIndex(config.TransportHTTP)
	a.form.url = "https://example.com/mcp"
	a.form.tokenHeader = "Authorization"
	a.form.tokenScheme = "Bearer"
	a.form.token = secret
	a.saveForm()

	if a.form.err != "" {
		t.Fatalf("save: %s", a.form.err)
	}
	sc := a.cfg.Servers[0]
	if sc.TokenHeader != "Authorization" || sc.TokenScheme != "Bearer" {
		t.Errorf("the header is %+v", sc)
	}

	// The token goes to the credential store...
	if got := a.creds.UpstreamToken(sc.ID); got != secret {
		t.Errorf("the stored token is %q", got)
	}
	// ...and never into the file beside the other settings.
	path, err := config.Path()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the settings: %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Errorf("the settings file holds the token:\n%s", data)
	}
}

func TestForgetTokenClearsTheSavedOne(t *testing.T) {
	a := newTestApp(t, remoteServer())
	if err := a.gw.SetUpstreamToken("s1", "tok"); err != nil {
		t.Fatalf("save: %v", err)
	}
	a.form.hasToken = true

	a.forgetToken("s1")

	if a.gw.HasUpstreamToken("s1") {
		t.Error("the token is still saved")
	}
	if a.form.hasToken {
		t.Error("the form still believes a token is saved")
	}
}

func TestConnectIsOfferedForAnOAuthServer(t *testing.T) {
	// A disabled OAuth server has never been connected, so its status is not
	// "sign in" — the browser authorization has to be reachable anyway.
	sc := config.ServerConfig{
		ID: "s1", Name: "Remote", Alias: "remote",
		Transport: config.TransportHTTP, URL: "https://example.com/mcp",
		OAuth: true, Enabled: false,
	}
	a := newTestApp(t, sc)
	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("Connect") {
		t.Errorf("an OAuth server offers no way to authorize; texts are %q", tt.Texts())
	}
	// The switch is there too, for turning the server on and off.
	if _, ok := tt.Find(sc.Name + " enabled"); !ok {
		t.Errorf("an OAuth server has no switch; texts are %q", tt.Texts())
	}
}

func TestCallersModeMakesAToken(t *testing.T) {
	a := newTestApp(t)
	if a.cfg.InboundMode() != config.InboundOff {
		t.Fatalf("a fresh app is in mode %q", a.cfg.InboundMode())
	}

	a.setAuthMode(inboundIndex(config.InboundToken))

	if a.cfg.InboundMode() != config.InboundToken {
		t.Errorf("the configuration says %q", a.cfg.InboundMode())
	}
	if a.auth.Mode != config.InboundToken {
		t.Errorf("the gateway says %q", a.auth.Mode)
	}
	if a.authToken == "" {
		t.Error("no token was made for the callers to present")
	}
	if a.auth.HasToken != true {
		t.Error("the gateway does not report having a token")
	}
}

func TestTurningAuthOffKeepsTheTokenForLater(t *testing.T) {
	a := newTestApp(t)
	a.setAuthMode(inboundIndex(config.InboundToken))
	token := a.authToken

	a.setAuthMode(inboundIndex(config.InboundOff))
	if a.cfg.InboundMode() != config.InboundOff {
		t.Errorf("the mode is %q", a.cfg.InboundMode())
	}
	if a.auth.Mode.RequiresToken() {
		t.Error("the gateway still demands a token")
	}

	a.setAuthMode(inboundIndex(config.InboundOAuth))
	if a.authToken != token {
		t.Errorf("the token became %q, want the one from before", a.authToken)
	}
	if !a.auth.AuthorizationServer {
		t.Error("the gateway does not report itself as an authorization server")
	}
}

func TestRotatingTheTokenChangesIt(t *testing.T) {
	a := newTestApp(t)
	a.setAuthMode(inboundIndex(config.InboundToken))
	before := a.authToken

	a.rotateAuthToken()

	if a.authToken == before {
		t.Error("rotating the token left it unchanged")
	}
	if a.authToken == "" {
		t.Error("rotating the token left none")
	}
}

func TestCallersCardShowsTheTokenAndItsDiscoveryURL(t *testing.T) {
	a := newTestApp(t)
	a.setAuthMode(inboundIndex(config.InboundOAuth))

	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.Click("Settings"); err != nil {
		t.Fatalf("click Settings: %v", err)
	}
	tt.Frame()

	for _, want := range []string{"Callers", "Token callers present", "MCP clients can also sign in"} {
		if !tt.HasText(want) {
			t.Errorf("the callers card does not show %q; texts are %q", want, tt.Texts())
		}
	}
}

func TestSplitLinesDropsBlankLines(t *testing.T) {
	got := splitLines("  -y \n\n server \r\n  ")
	if len(got) != 2 || got[0] != "-y" || got[1] != "server" {
		t.Errorf("splitLines returned %q", got)
	}
}

func TestParsePairsReadsHeaders(t *testing.T) {
	got := parsePairs("Authorization: Bearer abc\n\nX-Trace: 1\nnonsense\n")
	if got["Authorization"] != "Bearer abc" {
		t.Errorf("Authorization is %q", got["Authorization"])
	}
	if got["X-Trace"] != "1" {
		t.Errorf("X-Trace is %q", got["X-Trace"])
	}
	if len(got) != 2 {
		t.Errorf("parsed %d entries, want 2: %q", len(got), got)
	}
}

func TestFormatPairsRoundTrips(t *testing.T) {
	headers := map[string]string{"X-Trace": "1", "Authorization": "Bearer abc"}
	if got := parsePairs(formatPairs(headers)); len(got) != 2 {
		t.Errorf("round trip lost an entry: %q", got)
	}
}
