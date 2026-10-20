package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"mcp-go/internal/config"
	"mcp-go/internal/registry"
)

// marketPageSize is how many servers one search asks the registry for.
const marketPageSize = 30

// marketState is the Market page's state.
type marketState struct {
	query    string
	results  []registry.Server
	cursor   string // where the next page starts, empty at the last
	loading  bool
	searched bool // a search has run, so an empty result is an answer
	err      string
	seq      int // debounces the search as the query is typed
	sel      int
	list     ui.ListState
}

// marketPage searches the official MCP registry, and adds what it finds.
func (a *app) marketPage(c *ui.Context) {
	t := c.Theme()
	m := &a.market

	// The first visit shows the registry's latest before any searching.
	if !m.searched && !m.loading && m.err == "" && strings.TrimSpace(m.query) == "" {
		a.marketSearch(false)
	}

	ui.Column(c).Fill().Padding(14, 20, 20).Gap(14).Children(func() {
		pageHeader(c, "Market", "Public servers from the official MCP registry, added in one step.", func() {
			if m.loading {
				ui.Spinner(c).Label("Searching…")
			}
		})

		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			if ui.SearchField(c.Key("market-search"), &m.query).
				Placeholder("Search public MCP servers…").Label("Search the registry").Width(320).Changed() {
				a.marketSearchSoon()
			}
			if ui.PrimaryButton(c, "Search").Clicked() {
				a.marketSearch(false)
			}
			ui.Spacer(c)
			if m.searched && !m.loading {
				ui.Text(c, a.marketSummary()).FontSize(11).TextColor(t.TextMuted)
			}
		})

		if m.err != "" {
			ui.Text(c, "The registry could not be reached: "+m.err).FontSize(12).TextColor(t.Danger)
		}

		if !m.searched {
			emptyState(c, "Find a server",
				"Search the official registry — filesystems, browsers, databases and more.",
				nil)
			return
		}
		if m.searched && !m.loading && len(m.results) == 0 {
			emptyState(c, "Nothing found",
				fmt.Sprintf("No servers on the registry match %q.", strings.TrimSpace(m.query)),
				func() {
					if ui.Button(c, "Show the latest").Clicked() {
						m.query = ""
						a.marketSearch(false)
					}
				})
			return
		}

		m.list.Key = func(i int) any { return m.results[i].Name }
		m.list.Label = func(i int) string { return m.results[i].ShortName() }
		m.list.Selected = &m.sel
		surfaceShadow(t, ui.List(c.Key("market"), &m.list, len(m.results), func(i int) {
			a.marketRow(c, i)
		}).Grow(1).Border(1, t.Border).Radius(10).Padding(4)).Label("Registry results")

		if m.cursor != "" {
			ui.Row(c).Justify(ui.Center).Children(func() {
				if ui.Button(c, "Show more").Disabled(m.loading).Clicked() {
					a.marketSearch(true)
				}
			})
		}
	})
}

// marketRow draws one registry server.
func (a *app) marketRow(c *ui.Context, i int) {
	s := a.market.results[i]
	t := c.Theme()
	install, installable := s.Install()

	// A search's results arrive one after another, by the spring above and a
	// small growing delay, so a new page of them reads as a cascade rather
	// than appearing all at once. The delay is capped so a long page is not
	// still arriving when the user has read past it.
	anim := ui.Snappy
	anim.Delay = time.Duration(min(i, 8)) * 20 * time.Millisecond

	row := ui.Row(c).Height(56).PaddingX(12).Gap(12).AlignItems(ui.Center)
	row.Transition(ui.ElementTransition{
		Animation: &anim,
		Enter:     &ui.Motion{Y: 6},
		Exit:      &ui.Motion{Collapse: true},
	})
	row.Children(func() {
		ui.Avatar(c, s.ShortName(), nil).FontSize(15)
		ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
			ui.Text(c, s.ShortName()).SingleLine()
			ui.Text(c, s.Description).FontSize(11).TextColor(t.TextMuted).SingleLine()
		})
		ui.Badge(c, marketKind(install, installable))
		if s.Version != "" {
			ui.Text(c, "v"+s.Version).FontSize(11).TextColor(t.TextMuted)
		}
		switch {
		case a.installed(s):
			ui.Badge(c, "Added").Background(t.Success).TextColor(t.InverseText)
		case !installable:
			ui.Text(c, "Manual").FontSize(11).TextColor(t.TextMuted)
		default:
			if ui.PrimaryButton(c, "Add…").Label("Add " + s.ShortName()).Clicked() {
				a.addFromMarket(s)
			}
		}
	})
}

// marketKind names the way a registry server runs: over the network, or as a
// local command.
func marketKind(install registry.Install, ok bool) string {
	if !ok {
		return "manual"
	}
	if install.Remote {
		return "remote"
	}
	return install.Command
}

// marketSummary counts what a search found, for the row above the list.
func (a *app) marketSummary() string {
	m := &a.market
	n := len(m.results)
	if m.cursor != "" {
		return fmt.Sprintf("%d shown, more on the registry", n)
	}
	return fmt.Sprintf("%d found", n)
}

// marketSearch searches the registry. more asks for the page after the one
// already shown. Without a window — in a test — it runs at once.
func (a *app) marketSearch(more bool) {
	m := &a.market
	query := strings.TrimSpace(m.query)
	cursor := ""
	if more {
		cursor = m.cursor
	}
	m.loading = true
	m.err = ""

	fetch := func() (registry.Page, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return a.reg.Search(ctx, query, cursor, marketPageSize)
	}
	apply := func(page registry.Page, err error) {
		m.loading = false
		m.searched = true
		if err != nil {
			m.err = err.Error()
			if !more {
				m.results = nil
				m.cursor = ""
			}
			return
		}
		if more {
			m.results = append(m.results, page.Servers...)
		} else {
			m.results = page.Servers
			m.sel = 0
		}
		m.cursor = page.NextCursor
	}

	if !a.live() {
		page, err := fetch()
		apply(page, err)
		return
	}
	go func() {
		page, err := fetch()
		a.post(func() { apply(page, err) })
	}()
}

// marketSearchSoon searches shortly after the last keystroke, so typing does
// not send a request per letter.
func (a *app) marketSearchSoon() {
	a.market.seq++
	seq := a.market.seq
	time.AfterFunc(400*time.Millisecond, func() {
		a.post(func() {
			if a.market.seq == seq {
				a.marketSearch(false)
			}
		})
	})
}

// installed reports whether a registry server is already among the configured
// ones: its remote URL, or its package among a command's arguments.
func (a *app) installed(s registry.Server) bool {
	for _, sc := range a.cfg.Servers {
		for _, remote := range s.Remotes {
			if sc.URL != "" && sc.URL == remote.URL {
				return true
			}
		}
		for _, pkg := range s.Packages {
			if pkg.Identifier != "" && slices.Contains(sc.Args, pkg.Identifier) {
				return true
			}
		}
	}
	return false
}

// addFromMarket opens the add-a-server form filled in from a registry entry,
// so the environment variables and the name can be reviewed before saving.
func (a *app) addFromMarket(s registry.Server) {
	install, ok := s.Install()
	if !ok {
		a.toast(s.ShortName() + " has no install the gateway understands")
		return
	}
	a.form = serverForm{
		editing:     true,
		isNew:       true,
		enabled:     true,
		name:        s.ShortName(),
		tokenHeader: "Authorization",
		tokenScheme: "Bearer",
	}
	if install.Remote {
		a.form.url = install.URL
		if install.SSE {
			a.form.transport = transportIndex(config.TransportSSE)
		} else {
			a.form.transport = transportIndex(config.TransportHTTP)
		}
		return
	}
	a.form.transport = transportIndex(config.TransportStdio)
	a.form.command = install.Command
	a.form.args = strings.Join(install.Args, "\n")
	a.form.env = registry.EnvLines(install.Env)
}
