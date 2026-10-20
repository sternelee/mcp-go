package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"

	"mcp-go/internal/gateway"
)

// routesPage shows the agents connected to the gateway: who they are, when
// they joined, and what they have asked the gateway to do.
func (a *app) routesPage(c *ui.Context) {
	t := c.Theme()
	sessions := a.gw.Sessions()

	ui.Column(c).Fill().Padding(20).Gap(14).Children(func() {
		pageHeader(c, "Routes", "Agents connected to the gateway, and how they use it.", func() {
			ui.Text(c, fmt.Sprintf("%d connected", len(sessions))).FontSize(12).TextColor(t.TextMuted)
		})

		if len(sessions) == 0 {
			emptyState(c, "No agents connected",
				"Point an MCP client at the gateway's endpoints on Settings. Its session shows up here as soon as it connects.",
				func() {
					if ui.Button(c, "Open Settings").Clicked() {
						a.router.Push("/settings")
					}
				})
			return
		}

		ui.Column(c).Gap(6).Children(func() {
			for _, s := range sessions {
				routeRow(c, s)
			}
		})
	})
}

// routeRow draws one connected agent.
func routeRow(c *ui.Context, s gateway.SessionView) {
	t := c.Theme()
	label := s.Client
	if label == "" {
		// The agent has not initialized yet; the session ID is all it has
		// said.
		label = s.ID
	}

	ui.Row(c).Height(56).PaddingX(12).Gap(12).AlignItems(ui.Center).
		Background(t.Surface).Radius(10).Border(1, t.Border).Children(func() {
		ui.Avatar(c, label, nil).FontSize(15)
		ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, label).SingleLine()
				if s.Version != "" {
					ui.Text(c, "v"+s.Version).FontSize(11).TextColor(t.TextMuted)
				}
			})
			ui.Text(c, s.ID).Font("monospace").FontSize(10).TextColor(t.TextMuted).SingleLine()
		})
		if s.Protocol != "" {
			ui.Badge(c, s.Protocol)
		}
		call := "no calls yet"
		if s.Calls > 0 {
			if s.Calls == 1 {
				call = "1 call"
			} else {
				call = fmt.Sprintf("%d calls", s.Calls)
			}
		}
		ui.Column(c).Width(150).Gap(1).AlignItems(ui.End).Children(func() {
			ui.Text(c, call).FontSize(12)
			ui.Text(c, "connected "+relativeTime(s.Connected)).FontSize(10).TextColor(t.TextMuted)
		})
	})
}

// relativeTime says how long ago something happened: "just now", "4m ago" and
// so on.
func relativeTime(at time.Time) string {
	d := time.Since(at)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
