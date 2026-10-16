package main

import (
	"context"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"mcp-go/internal/config"
	"mcp-go/internal/gateway"
	"mcp-go/internal/registry"
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

func TestASignedInOAuthServerMenuOffersDisconnect(t *testing.T) {
	sc := config.ServerConfig{
		ID: "s1", Name: "Remote", Alias: "remote",
		Transport: config.TransportHTTP, URL: "https://example.com/mcp",
		OAuth: true, Enabled: true,
	}
	a := newTestApp(t, sc)
	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusReady, SignedIn: true, Tools: 3}

	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.RightClick(sc.Name); err != nil {
		t.Fatalf("right-click the row: %v", err)
	}
	menu := strings.Join(tt.Menu(), " ")
	if !strings.Contains(menu, "Disconnect") {
		t.Errorf("a signed-in server's menu has no Disconnect: %q", menu)
	}
	if strings.Contains(menu, "Connect") {
		t.Errorf("a signed-in server's menu still offers Connect: %q", menu)
	}
}

func TestSignInOfferedDecision(t *testing.T) {
	remote := func(id string) config.ServerConfig {
		return config.ServerConfig{ID: id, Name: id, Alias: id, Transport: config.TransportHTTP, URL: "https://example.com/mcp"}
	}
	oauth := remote("oauth")
	oauth.OAuth = true
	token := remote("token")
	token.TokenHeader = "Authorization"
	plain := remote("plain")

	cases := []struct {
		what string
		sc   config.ServerConfig
		view gateway.ServerView
		want bool
	}{
		{"a stdio server", stdioServer(), gateway.ServerView{Status: gateway.StatusNeedsAuth}, false},
		{"a token server", token, gateway.ServerView{Status: gateway.StatusNeedsAuth}, false},
		{"a configured OAuth server", oauth, gateway.ServerView{Status: gateway.StatusDisabled}, true},
		{"a server found to need auth", plain, gateway.ServerView{Status: gateway.StatusNeedsAuth}, true},
		{"a signed-in server", plain, gateway.ServerView{Status: gateway.StatusReady, SignedIn: true}, true},
		{"a plain connected server", plain, gateway.ServerView{Status: gateway.StatusReady}, false},
	}
	for _, tc := range cases {
		if got := signInOffered(tc.sc, tc.view); got != tc.want {
			t.Errorf("%s: signInOffered = %v, want %v", tc.what, got, tc.want)
		}
	}
}

func TestASignedInOAuthServerOffersDisconnect(t *testing.T) {
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
	if !tt.HasText("Disconnect") {
		t.Errorf("a signed-in server offers no way to sign out; texts are %q", tt.Texts())
	}
}

func TestConnectDoesNotChangeTheSwitch(t *testing.T) {
	// The two controls are separate: Connect is for authorizing, the switch is
	// for on and off. Clicking one must not move the other.
	sc := config.ServerConfig{
		ID: "s1", Name: "Remote", Alias: "remote",
		Transport: config.TransportHTTP, URL: "https://example.com/mcp",
		OAuth: true, Enabled: false,
	}
	a := newTestApp(t, sc)
	tt := ui.NewTester(a.view, 1080, 740)

	if err := tt.Click("Connect"); err != nil {
		t.Fatalf("click Connect: %v", err)
	}
	tt.Frame()
	if *a.flag(sc.ID, true) {
		t.Error("clicking Connect flipped the switch; on and off should be the switch's alone")
	}
	if stored, _ := a.cfg.Server(sc.ID); stored.Enabled {
		t.Error("clicking Connect changed the stored setting")
	}
}

func TestOpenAuthURLOpensTheBrowser(t *testing.T) {
	a := newTestApp(t)
	var opened []string
	a.openExternal = func(url string) error {
		opened = append(opened, url)
		return nil
	}

	// A server that needed no sign-in produces no URL, so nothing opens.
	if err := a.openAuthURL(""); err != nil {
		t.Fatalf("open an empty URL: %v", err)
	}
	if len(opened) != 0 {
		t.Errorf("an empty URL opened %q", opened)
	}

	const want = "https://auth.example/authorize?state=abc"
	if err := a.openAuthURL(want); err != nil {
		t.Fatalf("open the authorization URL: %v", err)
	}
	if len(opened) != 1 || opened[0] != want {
		t.Errorf("opened %q, want [%q]", opened, want)
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

func TestFilterNarrowsTheServersList(t *testing.T) {
	a := newTestApp(t, stdioServer(), remoteServer())
	a.filter = "remote"
	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("Remote") {
		t.Errorf("the matching server is gone; texts are %q", tt.Texts())
	}
	if tt.HasText("Filesystem") {
		t.Errorf("a server that does not match is still listed; texts are %q", tt.Texts())
	}
}

func TestFilterMatchesTheCommandToo(t *testing.T) {
	a := newTestApp(t, stdioServer(), remoteServer())
	a.filter = "npx"
	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("Filesystem") {
		t.Errorf("filtering by the command lost the stdio server; texts are %q", tt.Texts())
	}
	if tt.HasText("Remote") {
		t.Errorf("a server that does not match the command is listed; texts are %q", tt.Texts())
	}
}

func TestFilterWithNoMatchOffersToClear(t *testing.T) {
	a := newTestApp(t, stdioServer())
	a.filter = "nothing matches this"
	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("No servers match") {
		t.Fatalf("the empty filter state is missing; texts are %q", tt.Texts())
	}
	if err := tt.Click("Clear the filter"); err != nil {
		t.Fatalf("click Clear: %v", err)
	}
	tt.Frame()
	if a.filter != "" {
		t.Errorf("the filter is still %q", a.filter)
	}
}

func TestFilterKeepsTheSelectionInRange(t *testing.T) {
	a := newTestApp(t, stdioServer(), remoteServer())
	a.sel = 1
	a.filter = "filesystem" // only the first server survives
	_ = ui.NewTester(a.view, 1080, 740)
	if a.sel != 0 {
		t.Errorf("the selection is %d, want it pulled back to the only row", a.sel)
	}
	if got := a.selectedIndex(a.visibleServers()); got != 0 {
		t.Errorf("the selection names server %d, want 0", got)
	}
}

func TestToolsSelectionFollowsTheList(t *testing.T) {
	a := newTestApp(t)

	a.tools = nil
	a.clampToolSelection()
	if a.toolSel != -1 {
		t.Errorf("with no tools the selection is %d, want -1", a.toolSel)
	}

	a.tools = []gateway.ToolView{{Name: "a__x"}}
	a.clampToolSelection()
	if a.toolSel != 0 {
		t.Errorf("the first tool should be selected, got %d", a.toolSel)
	}

	a.toolSel = 9
	a.clampToolSelection()
	if a.toolSel != 0 {
		t.Errorf("an out-of-range selection should come back to 0, got %d", a.toolSel)
	}
}

func TestFilterIgnoresCase(t *testing.T) {
	a := newTestApp(t, stdioServer(), secondRemoteServer())
	a.filter = "FILESYSTEM"
	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("Filesystem") {
		t.Errorf("the filter is case-sensitive; texts are %q", tt.Texts())
	}
	if tt.HasText("Remote") {
		t.Errorf("a server that does not match is listed; texts are %q", tt.Texts())
	}
}

func TestEditingUnderAFilterEditsTheShownServer(t *testing.T) {
	a := newTestApp(t, stdioServer(), secondRemoteServer())
	a.filter = "remote"
	tt := ui.NewTester(a.view, 1080, 740)
	if err := tt.Click("Edit…"); err != nil {
		t.Fatalf("click Edit…: %v", err)
	}
	if !a.form.editing {
		t.Fatal("the form did not open")
	}
	// The selection is the row index in the filtered list, which is not the
	// index in the configuration; the form must still edit the shown server.
	if a.form.name != "Remote" {
		t.Errorf("the form edits %q, want the filtered server", a.form.name)
	}
}

func TestSavingAServerRevealsItUnderTheFilter(t *testing.T) {
	a := newTestApp(t, stdioServer(), secondRemoteServer())
	a.filter = "remote"
	a.addServer()
	a.form.name = "Filesystem Two"
	a.form.command = "npx"
	a.saveForm()
	if a.form.err != "" {
		t.Fatalf("save: %s", a.form.err)
	}
	sc, ok := a.serverNamed("Filesystem Two")
	if !ok {
		t.Fatal("the server was not stored")
	}
	if a.filter != "" {
		t.Errorf("the filter %q would hide the server just saved", a.filter)
	}
	if a.sel < 0 || a.cfg.Servers[a.sel].ID != sc.ID {
		t.Errorf("the selection %d does not name the saved server", a.sel)
	}
}

// secondRemoteServer is remoteServer with an ID of its own, so a test can list
// it beside the stdio server without two rows sharing a key.
func secondRemoteServer() config.ServerConfig {
	sc := remoteServer()
	sc.ID = "s2"
	return sc
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

func TestADiscoveredNeedsAuthServerOffersConnect(t *testing.T) {
	// A server configured without the OAuth flag and without a token of its
	// own: the gateway learns it needs signing in to from a 401, and the
	// interface still has to offer the button.
	sc := config.ServerConfig{
		ID: "s1", Name: "Linear", Alias: "linear",
		Transport: config.TransportHTTP, URL: "https://mcp.linear.app/mcp",
		Enabled: true,
	}
	a := newTestApp(t, sc)
	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusNeedsAuth}

	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("Connect") {
		t.Errorf("a server the gateway found needs auth offers no Connect; texts are %q", tt.Texts())
	}
	if err := tt.RightClick(sc.Name); err != nil {
		t.Fatalf("right-click the row: %v", err)
	}
	if menu := strings.Join(tt.Menu(), " "); !strings.Contains(menu, "Connect") {
		t.Errorf("the menu has no Connect: %q", menu)
	}
}

func TestATokenServerIsNotOfferedSignIn(t *testing.T) {
	// A server given a token of its own is not signed in to; a 401 from it is a
	// problem with that token, so it must not be offered a sign-in.
	sc := remoteServer()
	a := newTestApp(t, sc)
	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusNeedsAuth, Err: "authorization required"}

	tt := ui.NewTester(a.view, 1080, 740)
	if tt.HasText("Connect") {
		t.Errorf("a token server is offered a sign-in; texts are %q", tt.Texts())
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

// registryStub answers searches with one installable server.
func registryStub(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "servers": [
    {"server": {
      "name": "io.github.example/filesystem",
      "description": "Read and write files.",
      "version": "0.1.5",
      "packages": [{
        "registryType": "npm",
        "identifier": "@example/fs-mcp",
        "runtimeHint": "npx",
        "runtimeArguments": [{"value": "-y", "type": "positional"}],
        "environmentVariables": [{"name": "ROOT", "isRequired": true}]
      }]
    }, "_meta": {"io.modelcontextprotocol.registry/official": {"isLatest": true}}}
  ],
  "metadata": {"count": 1}
}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// openMarket points the app at a registry stub and shows the Market page. The
// first showing searches it, synchronously, there being no window.
func openMarket(t *testing.T, a *app, ts *httptest.Server) *ui.Tester {
	t.Helper()
	a.reg = &registry.Client{BaseURL: ts.URL}
	a.router.Push("/market")
	return ui.NewTester(a.view, 1080, 740)
}

func TestMarketPageListsWhatTheRegistryFound(t *testing.T) {
	a := newTestApp(t)
	tt := openMarket(t, a, registryStub(t))

	for _, want := range []string{"filesystem", "Read and write files.", "npx", "v0.1.5"} {
		if !tt.HasText(want) {
			t.Errorf("%q is missing; texts are %q", want, tt.Texts())
		}
	}
	if tt.HasText("The registry could not be reached") {
		t.Errorf("the search failed; texts are %q", tt.Texts())
	}
}

func TestMarketAddOpensAFilledInForm(t *testing.T) {
	a := newTestApp(t)
	tt := openMarket(t, a, registryStub(t))

	tt.Click("Add filesystem")

	if !a.form.editing {
		t.Fatal("no form opened")
	}
	if a.form.name != "filesystem" {
		t.Errorf("the name is %q", a.form.name)
	}
	if a.form.command != "npx" {
		t.Errorf("the command is %q", a.form.command)
	}
	if a.form.args != "-y\n@example/fs-mcp" {
		t.Errorf("the arguments are %q", a.form.args)
	}
	if a.form.env != "ROOT=" {
		t.Errorf("the environment is %q", a.form.env)
	}
	// The form is there to be reviewed before anything is saved.
	tt = ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("Add a server") {
		t.Errorf("the form is not shown; texts are %q", tt.Texts())
	}
	if len(a.cfg.Servers) != 0 {
		t.Error("nothing should be saved before the form is")
	}
}

func TestMarketMarksAnInstalledServer(t *testing.T) {
	sc := stdioServer()
	sc.Args = []string{"-y", "@example/fs-mcp"}
	a := newTestApp(t, sc)
	tt := openMarket(t, a, registryStub(t))

	if !tt.HasText("Added") {
		t.Errorf("an installed server is not marked; texts are %q", tt.Texts())
	}
}

func TestMarketReportsARegistryFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	a := newTestApp(t)
	tt := openMarket(t, a, ts)

	if !tt.HasText("The registry could not be reached") {
		t.Errorf("the failure is not shown; texts are %q", tt.Texts())
	}
}

// openRoutes shows the Routes page directly.
func openRoutes(t *testing.T, a *app) *ui.Tester {
	t.Helper()
	a.router.Push("/routes")
	return ui.NewTester(a.view, 1080, 740)
}

func TestRoutesShowsAnEmptyStateWithoutAgents(t *testing.T) {
	a := newTestApp(t)
	tt := openRoutes(t, a)

	if !tt.HasText("No agents connected") {
		t.Errorf("the empty state is missing; texts are %q", tt.Texts())
	}
}

func TestRoutesListsAConnectedAgent(t *testing.T) {
	a := newTestApp(t)
	c, err := client.NewInProcessClient(a.gw.Downstream())
	if err != nil {
		t.Fatalf("in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	req := mcp.InitializeRequest{
		Request: mcp.Request{Method: string(mcp.MethodInitialize)},
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "mcp-gateway", Version: "test"},
		},
	}
	if _, err := c.Initialize(context.Background(), req); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	tt := openRoutes(t, a)
	if !tt.HasText("mcp-gateway") {
		t.Fatalf("the connected agent is not listed; texts are %q", tt.Texts())
	}
	if !tt.HasText("1 connected") {
		t.Errorf("the count is not shown; texts are %q", tt.Texts())
	}
}

// pageKeysForTest mirrors the order the sidebar and the shortcuts use, so a
// test names the pages it expects.
var pageKeysForTest = []ui.Key{ui.Key1, ui.Key2, ui.Key3, ui.Key4, ui.Key5}

func TestCmdDigitShortcutsGoToEachPage(t *testing.T) {
	a := newTestApp(t)
	// The Market page searches the registry as it opens, so point it at a
	// stub before any page is built.
	ts := registryStub(t)
	a.reg = &registry.Client{BaseURL: ts.URL}

	tt := ui.NewTester(a.view, 1080, 740)
	want := []string{"/servers", "/market", "/routes", "/tools", "/settings"}
	for i, path := range want {
		tt.Key(ui.Cmd, pageKeysForTest[i])
		tt.Frame()
		if a.router.Path() != path {
			t.Errorf("Cmd+%d went to %q, want %q", i+1, a.router.Path(), path)
		}
	}
}

func TestEachPageNamesItself(t *testing.T) {
	a := newTestApp(t)
	// The Market page searches the registry as it opens; keep that off the
	// network.
	ts := registryStub(t)
	a.reg = &registry.Client{BaseURL: ts.URL}

	tt := ui.NewTester(a.view, 1080, 740)
	// The first page is titled as the sidebar shows it.
	if got := a.router.Title(); got != "Servers" {
		t.Errorf("the first page's title is %q, want %q", got, "Servers")
	}
	// A page pushed directly is titled too, so back and forward menus and
	// screen readers name it.
	for _, p := range pages[1:] {
		a.router.Push(p.path)
		tt.Frame()
		if got := a.router.Title(); got != p.title {
			t.Errorf("%s is titled %q, want %q", p.path, got, p.title)
		}
	}
}

func TestTheStatusWordSurvivesItsColorChange(t *testing.T) {
	sc := stdioServer()
	a := newTestApp(t, sc)
	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusReady, Tools: 1}
	tt := ui.NewTester(a.view, 1080, 740)
	if !tt.HasText("ready") {
		t.Fatalf("a ready server is not labelled; texts are %q", tt.Texts())
	}
	// An animated color must not stop the row from saying what changed.
	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusError, Err: "boom"}
	tt.Frame()
	if !tt.HasText("error") {
		t.Errorf("the row does not say it failed; texts are %q", tt.Texts())
	}
}

// topAccentY finds the topmost strongly blue pixel in a band: for a selected
// grid card, its accent border. A pose moves the border, which the layout does
// not, so the border's top is where a lifted card can be seen moving.
func topAccentY(img *image.RGBA, x0, x1, y0, y1 int) int {
	if img == nil {
		return -1
	}
	b := img.Bounds()
	x0, y0 = max(x0, 0), max(y0, 0)
	x1, y1 = min(x1, b.Dx()), min(y1, b.Dy())
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			i := y*img.Stride + x*4
			r, g, bl := int(img.Pix[i]), int(img.Pix[i+1]), int(img.Pix[i+2])
			if bl > r+50 && bl > g+30 {
				return y
			}
		}
	}
	return -1
}

// textHue counts how red or green the pixels of a box are, to check that a
// state's color is painted. It is positive when redder, negative when greener,
// zero when neither.
func textHue(img *image.RGBA, r ui.Rect) int {
	if img == nil {
		return 0
	}
	b := img.Bounds()
	red, green := 0, 0
	for y := max(int(r.Y)-2, 0); y <= min(int(r.Y+r.H)+2, b.Dy()-1); y++ {
		for x := max(int(r.X)-2, 0); x <= min(int(r.X+r.W)+2, b.Dx()-1); x++ {
			i := y*img.Stride + x*4
			rr, gg, bb := int(img.Pix[i]), int(img.Pix[i+1]), int(img.Pix[i+2])
			if rr > gg+30 && rr > bb+30 {
				red++
			}
			if gg > rr+30 && gg > bb+30 {
				green++
			}
		}
	}
	switch {
	case red == 0 && green == 0:
		return 0
	case red >= green:
		return red
	default:
		return -green
	}
}

// waitForAnimation waits out an animation's target. The tester draws frames as fast as it
// can, so without real time passing a value stays near where it started; the
// sleep lets the frame that follows land on the target.
func waitForAnimation() { time.Sleep(320 * time.Millisecond) }

func TestAStatusPillPaintsTheStatesColor(t *testing.T) {
	sc := stdioServer()
	a := newTestApp(t, sc)
	// A grid card's status is never accent-painted, so its color is the state's.
	a.serverView = 1
	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusReady, Tools: 1}
	tt := ui.NewTester(a.view, 1080, 740)
	tt.Frame()
	ready, ok := tt.Find("ready")
	if !ok {
		t.Fatalf("no ready status; texts are %q", tt.Texts())
	}
	if hue := textHue(tt.Image(), ready); hue >= 0 {
		t.Errorf("the ready status is not green (hue %d)", hue)
	}

	a.status[sc.ID] = gateway.ServerView{Config: sc, Status: gateway.StatusError, Err: "boom"}
	tt.Frame()
	waitForAnimation()
	tt.Frame()
	errRect, ok := tt.Find("error")
	if !ok {
		t.Fatalf("no error status; texts are %q", tt.Texts())
	}
	if hue := textHue(tt.Image(), errRect); hue <= 0 {
		t.Errorf("the error status is not red (hue %d)", hue)
	}
}

func TestAHoveredCardLifts(t *testing.T) {
	sc := stdioServer()
	a := newTestApp(t, sc)
	a.serverView = 1
	tt := ui.NewTester(a.view, 1080, 740)
	tt.Frame()

	rect, ok := tt.Find(sc.Name)
	if !ok {
		t.Fatalf("the card is not drawn; texts are %q", tt.Texts())
	}
	x0, x1 := int(rect.X)-30, int(rect.X+rect.W)+30
	y0, y1 := int(rect.Y)-40, int(rect.Y+rect.H)+40
	before := topAccentY(tt.Image(), x0, x1, y0, y1)
	if before < 0 {
		t.Fatal("the card's border is not where it was expected")
	}

	tt.Move(rect.X+rect.W/2, rect.Y+rect.H/2)
	waitForAnimation()
	tt.Frame()
	after := topAccentY(tt.Image(), x0, x1, y0, y1)
	if after >= before {
		t.Errorf("the hovered card did not lift: its border was at %d, now %d", before, after)
	}
}

func TestALiftedCardStaysWithinTheWindow(t *testing.T) {
	sc := stdioServer()
	a := newTestApp(t, sc)
	a.serverView = 1
	const width, height = 1080, 740
	tt := ui.NewTester(a.view, width, height)

	rect, ok := tt.Find(sc.Name)
	if !ok {
		t.Fatalf("the card is not drawn; texts are %q", tt.Texts())
	}
	// Hover the card and let its lift settle.
	tt.Move(rect.X+rect.W/2, rect.Y+rect.H/2)
	tt.Frame()

	after, ok := tt.Find(sc.Name)
	if !ok {
		t.Fatal("the card vanished under the pointer")
	}
	if after.X < 0 || after.Y < 0 || after.X+after.W > width || after.Y+after.H > height {
		t.Errorf("the lifted card falls outside the window %dx%d: %+v", width, height, after)
	}
}

// pixAt reads a pixel's red, green and blue, or -1 when out of the frame.
func pixAt(img *image.RGBA, x, y int) [3]int {
	if img == nil || x < 0 || y < 0 || x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
		return [3]int{-1, -1, -1}
	}
	i := y*img.Stride + x*4
	return [3]int{int(img.Pix[i]), int(img.Pix[i+1]), int(img.Pix[i+2])}
}

func TestAShadowedPanelCastsAShadow(t *testing.T) {
	view := func(c *ui.Context) {
		th := c.Theme()
		ui.Column(c).Fill().Background(th.Background).Padding(40).Children(func() {
			surfaceShadow(ui.Column(c).Width(200).Height(80).Radius(12).
				Background(th.Background).Border(1, th.Border))
		})
	}
	tt := ui.NewTester(view, 400, 300)
	tt.Frame()

	// Just under the panel the shadow darkens the page; well below it the page
	// is plain, so the near pixel must be darker than the far one.
	boxBottom := 40 + 80
	near := pixAt(tt.Image(), 100, boxBottom+1)
	far := pixAt(tt.Image(), 100, boxBottom+60)
	if near[0] < 0 || far[0] < 0 {
		t.Fatalf("the probes fell outside the frame: near %v far %v", near, far)
	}
	if near[0] >= far[0] {
		t.Errorf("the panel casts no shadow: near %v, far %v", near, far)
	}
}

func registryEntriesStub(t *testing.T, bodies ...string) *httptest.Server {
	t.Helper()
	var n int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := bodies[len(bodies)-1]
		if n < len(bodies) {
			body = bodies[n]
		}
		n++
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// entryJSON is one registry entry the parser accepts: every version is listed,
// and only _meta's isLatest is of interest.
func entryJSON(name, version string) string {
	return fmt.Sprintf(`{"server":{"name":%q,"description":"D %s","version":%q},`+
		`"_meta":{"io.modelcontextprotocol.registry/official":{"isLatest":true}}}`, name, name, version)
}

func TestMarketReplacesThePreviousResults(t *testing.T) {
	ts := registryEntriesStub(t,
		`{"servers":[`+entryJSON("a/alpha", "1")+`]}`,
		`{"servers":[`+entryJSON("b/beta", "2")+`]}`,
	)
	a := newTestApp(t)
	tt := openMarket(t, a, ts)
	if !tt.HasText("alpha") {
		t.Fatalf("the first search is not shown; texts are %q", tt.Texts())
	}

	a.market.query = "beta"
	a.marketSearch(false)
	tt.Frame()
	if tt.HasText("alpha") {
		t.Errorf("the previous search is still shown; texts are %q", tt.Texts())
	}
	if !tt.HasText("beta") {
		t.Errorf("the new search is not shown; texts are %q", tt.Texts())
	}
}

func TestARemovedServerLeavesTheList(t *testing.T) {
	one := stdioServer()
	two := one
	two.ID, two.Name, two.Alias = "s2", "Second", "second"
	a := newTestApp(t, one, two)
	tt := ui.NewTester(a.view, 1080, 740)
	// Find matches a whole text, so the row's own name is what is looked for.
	if _, ok := tt.Find("Second"); !ok {
		t.Fatalf("the second server is not listed; texts are %q", tt.Texts())
	}

	// Removing it takes its row out of the list as well as the configuration.
	a.removeServer(two.ID)
	tt.Frame()
	if _, ok := tt.Find("Second"); ok {
		t.Errorf("the removed server is still listed; texts are %q", tt.Texts())
	}
}

func TestTheLiveIndicatorTakesItsColor(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		word string
		want string // "green" or "gray"
	}{
		{true, "serving", "green"},
		{false, "stopped", "gray"},
	} {
		tc := tc
		view := func(c *ui.Context) {
			ui.Column(c).Fill().Padding(10).Children(func() {
				livePill(c, tc.on, "serving", "stopped")
			})
		}
		tt := ui.NewTester(view, 240, 60)
		tt.Frame()
		rect, ok := tt.Find(tc.word)
		if !ok {
			t.Fatalf("%q is not shown", tc.word)
		}
		hue := textHue(tt.Image(), rect)
		switch tc.want {
		case "green":
			if hue >= 0 {
				t.Errorf("the serving indicator is not green (hue %d)", hue)
			}
		case "gray":
			if hue != 0 {
				t.Errorf("the stopped indicator is not plain (hue %d)", hue)
			}
		}
	}
}
