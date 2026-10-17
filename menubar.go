package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"runtime"
	"sort"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"mcp-go/internal/gateway"
)

// The menu bar is the quick surface: an icon beside the clock whose panel
// shows what the gateway serves and who is using it, one click away from
// the full window.

const (
	panelWidth  = 360
	panelHeight = 480
)

// menubar owns the tray icon and the flyout it opens.
type menubar struct {
	a    *app
	tray *mygo.Tray
}

// runMenubar puts the icon in the system menu bar. Call it once the app is
// ready; the icon stays for the life of the process.
func runMenubar(a *app) (*menubar, error) {
	tray, err := mygo.NewTray(mygo.TrayOptions{
		Icon:           trayIcon(runtime.GOOS == "darwin"),
		IconIsTemplate: runtime.GOOS == "darwin",
		ToolTip:        "MCP Gateway",
	})
	if err != nil {
		return nil, err
	}
	m := &menubar{a: a, tray: tray}

	menu := mygo.NewMenu([]*mygo.MenuItem{
		{Label: "Open MCP Gateway", Click: func(*mygo.MenuItem, *mygo.Window) { m.open("/servers") }},
		{Label: "Show Routes", Click: func(*mygo.MenuItem, *mygo.Window) { m.open("/routes") }},
		mygo.Separator(),
		{Role: mygo.RoleQuit},
	})

	if runtime.GOOS == "linux" {
		// AppIndicator menus cannot report clicks, so the menu is the only
		// way in; the panel opens from it.
		tray.SetMenu(menu)
	} else {
		tray.OnClick(func() { m.toggle() })
		tray.OnRightClick(func() {
			// The panel and a menu at once fight over the icon: close the
			// panel, show the menu, and take it back afterwards so a left
			// click keeps toggling the panel.
			if a.panel != nil {
				a.panel.Close()
			}
			tray.SetMenu(menu)
			defer tray.SetMenu(nil)
			tray.PopUpMenu()
		})
	}
	return m, nil
}

// toggle opens the panel under the icon, or closes the open one.
func (m *menubar) toggle() {
	if m.a.panel != nil {
		m.a.panel.Close()
		return
	}
	panel := mygo.NewFlyout(mygo.FlyoutOptions{
		Tray:      m.tray,
		Placement: mygo.PlacementBottom,
		Gap:       4,
		Width:     panelWidth,
		Height:    panelHeight,
		Focusable: true,
		Shadow:    true,
		Popover:   true,
		Content:   ui.View(m.a.panelView),
	})
	panel.OnClosed(func() { m.a.panel = nil })
	m.a.panel = panel
}

// open brings the main window up on a page, from the tray.
func (m *menubar) open(page string) {
	if m.a.panel != nil {
		m.a.panel.Close()
	}
	m.a.post(func() { m.a.router.Push(page) })
	m.a.showMain()
}

// refresh updates the tray title and tooltip from the state the app has just
// reloaded. It runs on the main thread.
func (m *menubar) refresh() {
	title, tip := "", "MCP Gateway · "+m.a.summary()
	if agents := len(m.a.gw.Sessions()); agents > 0 {
		title = fmt.Sprint(agents)
		tip += fmt.Sprintf(" · %d agent%s connected", agents, plural(agents))
	}
	m.tray.SetTitle(title)
	m.tray.SetToolTip(tip)
}

// plural says "agent" or "agents" for n.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// panelView draws the tray panel: a tab for the servers behind the gateway
// and one for the agents using it. Each row is a shortcut into the main
// window.
func (a *app) panelView(c *ui.Context) {
	t := c.Theme()
	c.Root().Background(ui.Transparent)

	box := ui.Column(c).Fill().FontSize(13)
	switch {
	case c.Vibrancy():
		// The popover's material shows through; drawing a background over
		// it would waste the effect.
	case runtime.GOOS == "windows":
		box.Background(t.Background)
	default:
		box.Background(t.Background).Radius(12).Border(1, t.Border)
	}

	box.Children(func() {
		ui.Row(c).Padding(14, 16, 10).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, iconZap).FontSize(15).TextColor(t.Accent)
			ui.Text(c, "MCP Gateway").Bold().Grow(1)
			livePill(c, a.gw.Serving(), "serving", "stopped")
			page := "/servers"
			if a.panelTab == 1 {
				page = "/routes"
			}
			if ui.Button(c, "Open").Clicked() && a.openMain != nil {
				a.openMain(page)
			}
		})
		ui.Row(c).Padding(0, 16, 10).Children(func() {
			ui.Segmented(c, &a.panelTab, "Servers", "Routes").Label("Panel section")
		})
		ui.Divider(c)

		ui.Scroll(c).Fill().Children(func() {
			ui.Column(c).Fill().Padding(10, 16, 16).Gap(2).Children(func() {
				if a.panelTab == 0 {
					a.panelServers(c)
				} else {
					a.panelRoutes(c)
				}
			})
		})
	})
}

// panelServers lists the configured servers.
func (a *app) panelServers(c *ui.Context) {
	t := c.Theme()
	shown := false
	for _, sc := range a.cfg.Servers {
		v, ok := a.status[sc.ID]
		if !ok {
			continue
		}
		shown = true
		panelServerRow(c, a, v)
	}
	if !shown {
		ui.Row(c).Padding(6, 8, 10).Children(func() {
			ui.Text(c, "No servers configured").FontSize(12).TextColor(t.TextMuted)
		})
	}
}

// panelRoutes lists the agents connected to the gateway.
func (a *app) panelRoutes(c *ui.Context) {
	t := c.Theme()
	sessions := a.gw.Sessions()
	if len(sessions) == 0 {
		ui.Row(c).Padding(6, 8, 10).Children(func() {
			ui.Text(c, "No agents connected").FontSize(12).TextColor(t.TextMuted)
		})
		return
	}
	for _, s := range sessions {
		panelAgentRow(c, a, s)
	}
}

// panelServerRow draws one configured server. Clicking it opens the servers
// page.
func panelServerRow(c *ui.Context, a *app, v gateway.ServerView) {
	t := c.Theme()
	row := ui.Row(c).Height(40).PaddingX(8).Gap(10).AlignItems(ui.Center).Radius(8)
	if row.Clicked() && a.openMain != nil {
		a.openMain("/servers")
	}
	row.Children(func() {
		ui.Avatar(c, v.Config.Name, nil).FontSize(12)
		ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
			ui.Text(c, v.Config.Name).SingleLine()
			tools := "no tools yet"
			if v.Tools == 1 {
				tools = "1 tool"
			} else if v.Tools > 1 {
				tools = fmt.Sprintf("%d tools", v.Tools)
			}
			ui.Text(c, tools).FontSize(10).TextColor(t.TextMuted).SingleLine()
		})
		statusPill(c, v.Status, false)
	})
}

// panelAgentRow draws one connected agent. Clicking it opens the routes
// page.
func panelAgentRow(c *ui.Context, a *app, s gateway.SessionView) {
	t := c.Theme()
	label := s.Client
	if label == "" {
		label = s.ID
	}
	row := ui.Row(c).Height(40).PaddingX(8).Gap(10).AlignItems(ui.Center).Radius(8)
	if row.Clicked() && a.openMain != nil {
		a.openMain("/routes")
	}
	row.Children(func() {
		ui.Avatar(c, label, nil).FontSize(12)
		ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, label).SingleLine()
				if s.Version != "" {
					ui.Text(c, "v"+s.Version).FontSize(10).TextColor(t.TextMuted)
				}
			})
			ui.Text(c, "connected "+relativeTime(s.Connected)).FontSize(10).TextColor(t.TextMuted).SingleLine()
		})
		if s.Protocol != "" {
			ui.Badge(c, s.Protocol)
		}
	})
}

// trayIcon draws the bolt of the app icon as a small PNG for the menu bar:
// black when macOS tints it as a template icon, mid gray elsewhere, so it
// reads on light and dark taskbars alike. It is drawn at 2× so it stays
// sharp on retina screens.
func trayIcon(template bool) []byte {
	ink := color.NRGBA{R: 128, G: 128, B: 128, A: 255}
	if template {
		ink = color.NRGBA{A: 255}
	}
	const size = 36
	// The bolt, in a 36 px box, straight from the 24 px icon scaled by 1.5.
	bolt := [][2]float64{
		{19.5, 3}, {4.5, 21}, {18, 21}, {16.5, 33}, {31.5, 15}, {18, 15},
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		cy := float64(y) + 0.5
		var xs []float64
		for i, a := range bolt {
			b := bolt[(i+1)%len(bolt)]
			if (cy >= a[1]) != (cy >= b[1]) {
				xs = append(xs, a[0]+(cy-a[1])/(b[1]-a[1])*(b[0]-a[0]))
			}
		}
		sort.Float64s(xs)
		for i := 0; i+1 < len(xs); i += 2 {
			for x := 0; x < size; x++ {
				if cx := float64(x) + 0.5; xs[i] <= cx && cx < xs[i+1] {
					img.Set(x, y, ink)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
