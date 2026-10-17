package main

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"mcp-go/internal/config"
	"mcp-go/internal/gateway"
	"mcp-go/internal/registry"
)

// serverForm is the add/edit dialog's state.
type serverForm struct {
	editing bool
	isNew   bool

	id        string
	name      string
	transport int // index into transportOptions
	command   string
	args      string
	env       string
	url       string
	headers   string
	oauth     bool
	enabled   bool

	// A static credential, for the servers that take a token instead of
	// signing in. The token is write-only: an existing one is never read back
	// into the form.
	tokenHeader string
	tokenScheme string
	token       string
	hasToken    bool

	clientID     string
	clientSecret string

	err string
}

// transportOptions are the transports, in the order the segmented control
// shows them.
var transportOptions = []string{"stdio", "SSE", "HTTP"}

// pageDef is one page of the app: where it lives, what it is called, and the
// icon the sidebar shows for it. The sidebar, the router's titles, the
// Cmd+digit shortcuts and the screen-reader page names all read this list, so
// a page is described once.
type pageDef struct {
	path  string
	title string
	icon  *ui.SVG
}

// pages are the app's pages, in the order the sidebar shows them and the
// order Cmd+1 … Cmd+5 reaches them.
var pages = []pageDef{
	{"/servers", "Servers", iconServers},
	{"/market", "Market", iconStore},
	{"/routes", "Routes", iconRoutes},
	{"/tools", "Tools", iconTools},
	{"/settings", "Settings", iconSliders},
}

// pageKeys go to the pages above, in order: Cmd+1 is Servers, and so on.
var pageKeys = []ui.Key{ui.Key1, ui.Key2, ui.Key3, ui.Key4, ui.Key5}

// pageShortcutLabel spells a page shortcut the way the platform writes it, so
// the hint beside a page's name matches the key the user would press.
func pageShortcutLabel(i int) string {
	modifier := "Ctrl+"
	if runtime.GOOS == "darwin" {
		modifier = "⌘"
	}
	return modifier + strconv.Itoa(i+1)
}

// inboundModes are the ways the gateway can guard itself, in the order the
// interface shows them.
var inboundModes = []config.InboundMode{config.InboundOff, config.InboundToken, config.InboundOAuth}

func inboundIndex(mode config.InboundMode) int {
	for i, candidate := range inboundModes {
		if candidate == mode {
			return i
		}
	}
	return 0
}

func transportAt(i int) config.Transport {
	switch i {
	case 0:
		return config.TransportStdio
	case 1:
		return config.TransportSSE
	}
	return config.TransportHTTP
}

func transportIndex(t config.Transport) int {
	switch t {
	case config.TransportStdio:
		return 0
	case config.TransportSSE:
		return 1
	}
	return 2
}

// importForm is the paste-a-configuration dialog's state.
type importForm struct {
	open bool
	text string
	err  string
}

// app is the state the window shows.
type app struct {
	cfg   *config.Config
	creds *config.CredentialStore
	gw    *gateway.Gateway

	win      *mygo.Window
	router   *ui.Router
	panel    *mygo.Window // the menu bar's open flyout, or nil
	onReload func()       // runs on the main thread after every reload, for surfaces outside the window
	openMain func(string) // brings the main window up on a page; the menu bar sets it

	serversList ui.ListState
	toolsList   ui.ListState

	// Copied out of the gateway when it changes, on the main thread.
	status map[string]gateway.ServerView
	tools  []gateway.ToolView
	events []gateway.Event

	// flags holds the enabled switch of each server. The pointers are stable
	// for the process, so an edit is never lost to a rebuilt slice.
	flags map[string]*bool

	sel         int
	toolSel     int
	panelTab    int // which tab the menu bar's panel shows
	port        float64
	busy        string
	pending     string // a toast waiting for the next frame
	filter      string // narrows the servers list
	serverView  int    // 0 lists the servers, 1 shows them as cards
	toolFilter  string // narrows the tools table
	form        serverForm
	imp         importForm
	confirm     bool
	removeID    string
	serversGrid ui.GridState

	// reg searches the official MCP registry; a field so a test can point it
	// at a stub. market is the Market page's state.
	reg    *registry.Client
	market marketState

	// openExternal opens a URL in the user's browser. It is a field so a test
	// can observe what would be opened.
	openExternal func(string) error

	// How the gateway guards itself, copied out of it on reload.
	auth      gateway.AuthState
	authToken string
	authMode  int

	// Tools page.
	callArgs   string
	callResult string
	callErr    bool
	calling    bool
}

func newApp(cfg *config.Config, creds *config.CredentialStore, gw *gateway.Gateway) *app {
	router := ui.NewRouter("/servers")
	// Pages move with a spring, so a page keeps its velocity when the user
	// goes on to another mid-flight, rather than stopping dead at the edge.
	router.Animation = &ui.Snappy

	a := &app{
		cfg:          cfg,
		creds:        creds,
		gw:           gw,
		router:       router,
		port:         float64(cfg.Port),
		status:       map[string]gateway.ServerView{},
		flags:        map[string]*bool{},
		openExternal: mygo.Shell.OpenExternal,
		reg:          registry.New(),
	}
	a.serversList.Selected = &a.sel
	a.toolsList.Selected = &a.toolSel
	a.reload()
	return a
}

// flag returns the stable enabled binding of a server, seeding it from the
// configuration the first time it is asked for.
func (a *app) flag(id string, enabled bool) *bool {
	p, ok := a.flags[id]
	if !ok {
		p = new(bool)
		*p = enabled
		a.flags[id] = p
	}
	return p
}

// setEnabled records a server's enabled switch and reconnects accordingly.
func (a *app) setEnabled(id string, enabled bool) {
	sc, ok := a.cfg.Server(id)
	if !ok || sc.Enabled == enabled {
		return
	}
	sc.Enabled = enabled
	a.cfg.Upsert(sc)
	if err := a.cfg.Save(); err != nil {
		a.toast("Could not save settings: " + err.Error())
	}
	a.sync()
}

// reload copies what the window shows out of the gateway. It runs on the main
// thread only.
func (a *app) reload() {
	views := a.gw.Servers()
	status := make(map[string]gateway.ServerView, len(views))
	for _, v := range views {
		status[v.Config.ID] = v
	}
	a.status = status
	a.tools = a.gw.Tools()
	a.events = a.gw.Events()
	a.auth = a.gw.AuthState()
	a.authToken = a.gw.AuthToken()
	a.authMode = inboundIndex(a.auth.Mode)
	a.clampToolSelection()
	if a.onReload != nil {
		a.onReload()
	}
}

// clampToolSelection keeps the tools selection on a row while there is one, so
// the detail pane is never an empty column when there is something to show.
func (a *app) clampToolSelection() {
	switch {
	case len(a.tools) == 0:
		a.toolSel = -1
	case a.toolSel < 0 || a.toolSel >= len(a.tools):
		a.toolSel = 0
	}
}

// onChange is what the gateway calls when it has news. Window.Update lands it
// on the main thread, where the view reads the new state.
func (a *app) onChange() {
	a.post(a.reload)
}

// post runs fn on the main thread, if there is a window to run it on. Views
// run without a window in tests, so the work has nowhere to report to there.
func (a *app) post(fn func()) {
	if a.win == nil {
		return
	}
	a.win.Update(fn)
	if a.panel != nil {
		a.panel.Update(func() {})
	}
}

// live reports whether there is a window for background work to report to.
func (a *app) live() bool { return a.win != nil }

// showMain raises the main window without moving it to another page, the
// way the Dock icon and the tray's Open entry bring the app back after
// its window was hidden.
func (a *app) showMain() {
	if a.win == nil {
		return
	}
	a.win.Show()
	a.win.Focus()
}

// sync connects the gateway to the configured servers in the background.
func (a *app) sync() {
	if !a.live() {
		return
	}
	a.busy = "Connecting servers…"
	go func() {
		err := a.gw.Sync(context.Background())
		a.post(func() {
			a.busy = ""
			a.reload()
			if err != nil {
				// Each failure is already on its own row; this is the summary.
				a.toast("Some servers could not be connected")
			}
		})
	}()
}

// toast shows a short message on the next frame. Call it on the main thread.
func (a *app) toast(text string) {
	a.pending = text
	if a.win != nil {
		a.win.Invalidate()
	}
}

// view builds the window's interface, for every frame.
func (a *app) view(c *ui.Context) {
	if a.pending != "" {
		c.Toast(a.pending)
		a.pending = ""
	}
	a.pageShortcuts(c)
	ui.Row(c).Fill().AlignItems(ui.Stretch).Children(func() {
		a.sidebar(c)
		ui.Column(c).Grow(1).MinWidth(0).Children(func() {
			a.router.View(c, func(r *ui.Route) {
				switch {
				case r.Match("/market"):
					r.Title("Market")
					a.marketPage(c)
				case r.Match("/routes"):
					r.Title("Routes")
					a.routesPage(c)
				case r.Match("/tools"):
					r.Title("Tools")
					a.toolsPage(c)
				case r.Match("/settings"):
					r.Title("Settings")
					a.settingsPage(c)
				default:
					r.Title("Servers")
					a.serversPage(c)
				}
			})
		})
	})

	a.serverForm(c)
	a.importDialog(c)
	a.removeDialog(c)
}

// pageShortcuts binds Cmd+1 … Cmd+5 to the pages, the way a browser's tabs
// are reached. They are registered in the window's scope, so they are live
// wherever the focus is.
func (a *app) pageShortcuts(c *ui.Context) {
	for i, p := range pages {
		if i >= len(pageKeys) {
			break
		}
		path := p.path
		c.OnShortcut(ui.Cmd, pageKeys[i], func() { a.router.Push(path) })
	}
}

// sidebar lists the pages, and stays as the page changes.
func (a *app) sidebar(c *ui.Context) {
	t := c.Theme()
	page := a.router.Path()
	ui.Column(c).Width(216).Background(t.Surface).Shrink(0).Children(func() {
		ui.Row(c).Padding(18, 16, 14).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Width(30).Height(30).Center().Radius(8).Background(t.Accent).Children(func() {
				ui.Icon(c, iconZap).FontSize(15).TextColor(t.AccentText)
			})
			ui.Column(c).Gap(1).Children(func() {
				ui.Text(c, "MCP Gateway").FontSize(13).Bold()
				ui.Text(c, "v"+version).FontSize(10).TextColor(t.TextMuted)
			})
		})
		if ui.Sidebar(c, &page, func() {
			for i, p := range pages {
				// The shortcut is what the visible name does not say, so it goes
				// in the tooltip, which also describes the item to screen readers.
				item := ui.SidebarItem(c, p.path, p.icon, p.title)
				if i < len(pageKeys) {
					item.Tooltip(p.title + " (" + pageShortcutLabel(i) + ")")
				}
			}
		}).Grow(1).Label("Pages").Changed() {
			a.router.Push(page)
		}
		ui.Divider(c)
		ui.Row(c).Padding(10, 20, 12).Gap(8).AlignItems(ui.Center).Children(func() {
			livePill(c, a.gw.Serving(), "serving", "stopped")
			ui.Text(c, a.summary()).FontSize(11).TextColor(t.TextMuted).Grow(1)
		})
	})
}

// summary describes how many servers are ready, for the sidebar's foot.
func (a *app) summary() string {
	ready := 0
	for _, v := range a.status {
		if v.Status == gateway.StatusReady {
			ready++
		}
	}
	total := len(a.cfg.Servers)
	if total == 0 {
		return "No servers configured"
	}
	return fmt.Sprintf("%d of %d servers ready", ready, total)
}

// pageHeader draws a page's title, subtitle and trailing controls.
func pageHeader(c *ui.Context, title, subtitle string, actions func()) {
	t := c.Theme()
	ui.Row(c).AlignItems(ui.Center).Gap(12).Children(func() {
		ui.Column(c).Gap(2).Children(func() {
			ui.Text(c, title).FontSize(20).Bold()
			if subtitle != "" {
				ui.Text(c, subtitle).FontSize(12).TextColor(t.TextMuted)
			}
		})
		ui.Spacer(c)
		if actions != nil {
			actions()
		}
	})
}

// emptyState is shown where a page has nothing to list yet.
func emptyState(c *ui.Context, title, body string, action func()) {
	t := c.Theme()
	ui.Column(c).Fill().Center().Gap(10).Padding(40).Children(func() {
		ui.Text(c, title).FontSize(15).Bold()
		ui.Text(c, body).FontSize(12).TextColor(t.TextMuted).TextAlign(ui.Center)
		if action != nil {
			action()
		}
	})
}

// statusColor maps a server's state to the color of its dot.
func statusColor(t *ui.Theme, s gateway.Status) ui.Color {
	switch s {
	case gateway.StatusReady:
		return t.Success
	case gateway.StatusConnecting:
		return t.Warning
	case gateway.StatusNeedsAuth:
		return t.Accent
	case gateway.StatusError:
		return t.Danger
	}
	return t.TextMuted
}

// statusLabel is the word shown for a server's state.
func statusLabel(s gateway.Status) string {
	switch s {
	case gateway.StatusReady:
		return "ready"
	case gateway.StatusConnecting:
		return "connecting"
	case gateway.StatusNeedsAuth:
		return "sign in"
	case gateway.StatusError:
		return "error"
	}
	return "off"
}

// splitLines turns a text area's contents into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// parsePairs reads "Key: value" lines into a map.
func parsePairs(s string) map[string]string {
	out := map[string]string{}
	for _, line := range splitLines(s) {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key != "" {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// formatPairs writes a map as "Key: value" lines, in a stable order.
func formatPairs(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(m[k])
		b.WriteByte('\n')
	}
	return b.String()
}
