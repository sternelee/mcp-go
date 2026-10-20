package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"

	"mcp-go/internal/config"
	"mcp-go/internal/gateway"
)

// serversPage lists the configured servers and what to do with them.
func (a *app) serversPage(c *ui.Context) {
	t := c.Theme()
	visible := a.visibleServers()
	a.clampSelection(len(visible))
	selected := a.selectedIndex(visible)
	ui.Column(c).Fill().Padding(20).Gap(14).Children(func() {
		pageHeader(c, "Servers", "Upstream MCP servers whose tools this gateway serves as one.", func() {
			if a.busy != "" {
				ui.Spinner(c).Label(a.busy)
			}
			if ui.Button(c, "Edit…").Disabled(selected < 0).Clicked() {
				a.editServer(selected)
			}
			if ui.Button(c, "Reconnect").Disabled(selected < 0).Clicked() {
				a.reconnect(a.cfg.Servers[selected].ID)
			}
			if ui.Button(c, "Remove").Disabled(selected < 0).Clicked() {
				a.askRemove(a.cfg.Servers[selected].ID)
			}
			if ui.Button(c, "Paste JSON…").Clicked() {
				a.openImport()
			}
			if ui.PrimaryButton(c, "Add server…").Clicked() {
				a.addServer()
			}
		})

		if len(a.cfg.Servers) == 0 {
			emptyState(c, "No servers yet",
				"Add a stdio command or a remote URL by hand, or paste a configuration from Claude Desktop, Cursor or VS Code.",
				func() {
					ui.Row(c).Gap(8).Children(func() {
						if ui.Button(c, "Paste a configuration…").Clicked() {
							a.openImport()
						}
						if ui.PrimaryButton(c, "Add your first server…").Clicked() {
							a.addServer()
						}
					})
				})
			return
		}

		a.toolbarRow(c)

		if len(visible) == 0 {
			emptyState(c, "No servers match",
				fmt.Sprintf("Nothing matches %q.", strings.TrimSpace(a.filter)),
				func() {
					if ui.Button(c, "Clear the filter").Clicked() {
						a.filter = ""
					}
				})
			return
		}

		if a.serverView == 1 {
			a.serversGridView(c, visible)
			return
		}

		a.serversList.Key = func(i int) any { return a.cfg.Servers[visible[i]].ID }
		a.serversList.Label = func(i int) string { return a.cfg.Servers[visible[i]].Name }
		a.serversList.Selected = &a.sel
		surfaceShadow(t, ui.List(c.Key("servers"), &a.serversList, len(visible), func(i int) {
			a.serverRow(c, visible[i], i)
		}).Grow(1).Border(1, t.Border).Radius(10).Padding(4))
		// Enter opens the chosen server as well, taken in the page's scope
		// because the list's own submitted notice does not reach the page under
		// a Router. It fires only while the keyboard is in the servers list, so
		// Enter in the filter field is left alone.
		c.OnShortcut(0, ui.KeyEnter, func() {
			if selected >= 0 && a.serversList.FocusWithin(c) {
				a.editServer(selected)
			}
		})

		if selected >= 0 {
			ui.Text(c, "Double-click a server to edit it. Its tools are named "+a.cfg.Servers[selected].Alias+"__…").
				FontSize(11).TextColor(t.TextMuted)
		}
	})
}

// serversGridView shows the servers as cards, the same content as the rows
// laid out for scanning rather than reading.
func (a *app) serversGridView(c *ui.Context, visible []int) {
	a.serversGrid.Key = func(i int) any { return a.cfg.Servers[visible[i]].ID }
	a.serversGrid.Label = func(i int) string { return a.cfg.Servers[visible[i]].Name }
	a.serversGrid.Selected = &a.sel
	grid := ui.GridView(c.Key("servers-grid"), &a.serversGrid, len(visible), 280, 128, func(i int) {
		a.serverCard(c, visible[i], i)
	}).Grow(1)
	if selected := a.selectedIndex(visible); grid.Submitted() && selected >= 0 {
		a.editServer(selected)
	}
}

// toolbarRow narrows a long list of servers, says how they are doing, and
// switches between the list and the cards.
func (a *app) toolbarRow(c *ui.Context) {
	t := c.Theme()
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.SearchField(c.Key("server-filter"), &a.filter).
			Placeholder("Filter by name, command or URL…").Label("Filter servers").Width(280)
		ui.Spacer(c)
		ui.Text(c, a.serversSummary()).FontSize(11).TextColor(t.TextMuted)
		ui.Segmented(c, &a.serverView, "List", "Grid").Label("Servers view")
	})
}

// serversSummary counts the servers by what they are doing, for the row above
// the list.
func (a *app) serversSummary() string {
	var ready, auth, failed, off int
	for _, sc := range a.cfg.Servers {
		switch a.status[sc.ID].Status {
		case gateway.StatusReady:
			ready++
		case gateway.StatusNeedsAuth:
			auth++
		case gateway.StatusError:
			failed++
		default:
			off++
		}
	}
	parts := []string{fmt.Sprintf("%d ready", ready)}
	if auth > 0 {
		parts = append(parts, fmt.Sprintf("%d need sign-in", auth))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if off > 0 {
		parts = append(parts, fmt.Sprintf("%d off", off))
	}
	return strings.Join(parts, " · ")
}

// visibleServers returns the indices of the configured servers the filter
// keeps, in order. With an empty filter it is every server.
func (a *app) visibleServers() []int {
	query := strings.ToLower(strings.TrimSpace(a.filter))
	idx := make([]int, 0, len(a.cfg.Servers))
	for i, sc := range a.cfg.Servers {
		if query == "" || serverMatches(sc, query) {
			idx = append(idx, i)
		}
	}
	return idx
}

// serverMatches reports whether a server matches a lower-cased query, by its
// name, alias, transport, command or URL.
func serverMatches(sc config.ServerConfig, query string) bool {
	for _, field := range []string{sc.Name, sc.Alias, sc.Transport.Label(), sc.Command, sc.URL} {
		if strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	return false
}

// clampSelection keeps the servers selection inside the visible list, so a
// filter or a removal never leaves it pointing at a row that is gone.
func (a *app) clampSelection(n int) {
	switch {
	case n == 0:
		a.sel = -1
	case a.sel < 0:
		a.sel = 0
	case a.sel >= n:
		a.sel = n - 1
	}
}

// selectedIndex returns the configured-server index the selection names, or -1.
func (a *app) selectedIndex(visible []int) int {
	if a.sel < 0 || a.sel >= len(visible) {
		return -1
	}
	return visible[a.sel]
}

// revealServer clears the filter and selects a server, so one that was just
// added or edited is the row in view.
func (a *app) revealServer(id string) {
	a.filter = ""
	for i, sc := range a.cfg.Servers {
		if sc.ID == id {
			a.sel = i
			return
		}
	}
	a.sel = -1
}

// rowTransition brings a server's row or card in as it appears and collapses
// it as it goes, so adding, removing or filtering reads as a change rather
// than the list jumping. The spring keeps the rise smooth when several rows
// move at once.
var rowTransition = ui.ElementTransition{
	Animation: &ui.Snappy,
	Enter:     &ui.Motion{Y: 6},
	Exit:      &ui.Motion{Collapse: true},
}

// cardTransition is rowTransition for a grid card, which leaves by scaling
// down rather than collapsing: a card has no height of its own in the wrap
// layout to take away.
var cardTransition = ui.ElementTransition{
	Animation: &ui.Snappy,
	Enter:     &ui.Motion{Y: 8, Scale: 0.98},
	Exit:      &ui.Motion{Scale: 0.96},
}

// serverRow draws one server. i is its index in the configuration, row its
// index in the list the filter leaves.
func (a *app) serverRow(c *ui.Context, i, row int) {
	sc := a.cfg.Servers[i]
	view := a.status[sc.ID]
	t := c.Theme()

	line := ui.Row(c).Height(56).PaddingX(12).Gap(12).AlignItems(ui.Center)
	line.ContextMenu(func(m *ui.Menu) {
		a.serverMenu(m, sc, i, view)
	})
	line.Transition(rowTransition)
	// A click chooses the row, and a double click opens it. Both are taken here
	// rather than left to the list's own submitted notice, which does not reach
	// the page when the list is under a Router.
	if line.DoubleClicked() {
		a.editServer(i)
	} else if line.Clicked() {
		a.sel = row
		a.serversList.Handle.Focus(c)
	}

	line.Children(func() {
		// The list paints a chosen row in the accent color, so its secondary
		// text and badge have to be drawn for a dark background rather than the
		// theme's ordinary muted gray.
		selected := row == a.sel
		muted := t.TextMuted
		if selected {
			muted = t.AccentText.Alpha(0.82)
		}
		ui.Avatar(c, sc.Name, nil).FontSize(15)
		ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
			ui.Text(c, sc.Name).SingleLine()
			ui.Text(c, serverDetail(sc, view)).FontSize(11).TextColor(muted).SingleLine()
		})
		statusPill(c, view.Status, selected)
		badge := ui.Badge(c, sc.Transport.Label())
		if selected {
			badge.Background(t.AccentText.Alpha(0.22)).TextColor(t.AccentText)
		}
		ui.Textf(c, "%d tools", view.Tools).FontSize(11).TextColor(muted).Width(54).TextAlign(ui.End).
			ContentTransition(ui.ContentTransition{Numeric: true})
		if signInOffered(sc, view) {
			// Authorizing is the one step a switch cannot express, so it keeps a
			// button. It is offered whatever the connection state, so a server
			// can be authorized before it is switched on.
			if view.SignedIn {
				if ui.Button(c, "Disconnect").Clicked() {
					a.signOut(sc.ID)
				}
			} else if ui.PrimaryButton(c, "Connect").Clicked() {
				a.signIn(sc.ID)
			}
		}
		// The switch is the server's on/off state: turning it on connects the
		// server, and turning it off disconnects it and keeps it off at launch.
		ui.Row(c).Width(64).Justify(ui.End).Children(func() {
			a.serverSwitch(c, sc)
		})
	})
}

// serverCard draws one server for the grid: the row's content reflowed into a
// card. i is its index in the configuration, item its place in the grid.
func (a *app) serverCard(c *ui.Context, i, item int) {
	sc := a.cfg.Servers[i]
	view := a.status[sc.ID]
	t := c.Theme()

	border := t.Border
	if item == a.sel {
		border = t.Accent
	}
	box := surfaceShadow(t, ui.Column(c).Fill().Padding(14).Gap(8).Radius(12).Background(t.Background).Border(1, border))
	box.ContextMenu(func(m *ui.Menu) {
		a.serverMenu(m, sc, i, view)
	})
	// The card rises a little under the pointer, and its edge settles as the
	// selection moves, so a wall of servers is easy to scan and to target. The
	// lift only paints, so a large grid stays smooth.
	lift := ui.Pose{}
	if box.Hovered() {
		lift = ui.Pose{Y: -3}
	}
	box.AnimatePose(lift, ui.Snappy)
	box.Transition(cardTransition)

	box.Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Avatar(c, sc.Name, nil).FontSize(15)
			ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
				ui.Text(c, sc.Name).SingleLine().Bold()
				statusPill(c, view.Status, false)
			})
			ui.Badge(c, sc.Transport.Label())
		})
		ui.Text(c, serverDetail(sc, view)).FontSize(11).TextColor(t.TextMuted).SingleLine()
		ui.Spacer(c)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Textf(c, "%d tools", view.Tools).FontSize(11).TextColor(t.TextMuted).
				ContentTransition(ui.ContentTransition{Numeric: true})
			ui.Spacer(c)
			if signInOffered(sc, view) {
				if view.SignedIn {
					if ui.Button(c, "Disconnect").Clicked() {
						a.signOut(sc.ID)
					}
				} else if ui.PrimaryButton(c, "Connect").Clicked() {
					a.signIn(sc.ID)
				}
			}
			a.serverSwitch(c, sc)
		})
	})
}

// serverMenu is the right-click menu of a server's row or card.
func (a *app) serverMenu(m *ui.Menu, sc config.ServerConfig, i int, view gateway.ServerView) {
	if m.Item("Edit…").Chosen() {
		a.editServer(i)
	}
	m.Separator()
	if m.Item("Reconnect").Chosen() {
		a.reconnect(sc.ID)
	}
	if signInOffered(sc, view) {
		if view.SignedIn {
			if m.Item("Disconnect").Chosen() {
				a.signOut(sc.ID)
			}
		} else if m.Item("Connect").Chosen() {
			a.signIn(sc.ID)
		}
	}
	m.Separator()
	if m.Item("Remove").Chosen() {
		a.askRemove(sc.ID)
	}
}

// serverSwitch is a server's on/off switch: turning it on connects the server,
// and turning it off disconnects it and keeps it off at launch.
func (a *app) serverSwitch(c *ui.Context, sc config.ServerConfig) {
	on := a.flag(sc.ID, sc.Enabled)
	if ui.Switch(c, on).Label(sc.Name + " enabled").Changed() {
		if *on {
			a.connect(sc.ID)
		} else {
			a.disconnect(sc.ID)
		}
	}
}

// serverDetail is the line under a server's name: what it is, and how it is
// doing when that is not simply "connected".
func serverDetail(sc config.ServerConfig, view gateway.ServerView) string {
	if view.Err != "" {
		return view.Err
	}
	target := sc.URL
	if sc.Transport == config.TransportStdio {
		target = strings.Join(append([]string{sc.Command}, sc.Args...), " ")
	}
	switch view.Status {
	case gateway.StatusDisabled:
		return "Off · " + target
	case gateway.StatusConnecting:
		return "Connecting · " + target
	case gateway.StatusNeedsAuth:
		return "Sign in required · " + target
	}
	return target
}

// signInOffered reports whether a server can be signed in to: a remote server
// that carries no token of its own. It is offered once the server has asked for
// authorization, and whenever the configuration already expects OAuth, so a
// server the gateway discovers needs signing in to still gets a button.
func signInOffered(sc config.ServerConfig, view gateway.ServerView) bool {
	if sc.Transport == config.TransportStdio || sc.TokenHeader != "" {
		return false
	}
	return sc.OAuth || view.SignedIn || view.Status == gateway.StatusNeedsAuth
}

// addServer opens the form for a new server.
func (a *app) addServer() {
	a.form = serverForm{
		editing: true,
		isNew:   true,
		enabled: true,
		// The common case, ready to paste a token into.
		tokenHeader: "Authorization",
		tokenScheme: "Bearer",
	}
}

// tokenPlaceholder is what the token box shows when it is empty.
func tokenPlaceholder(f *serverForm) string {
	if f.hasToken {
		return "•••••••• saved"
	}
	return "Paste the token"
}

// tokenHint explains where the token is kept.
func tokenHint(f *serverForm) string {
	if f.hasToken {
		return "A token is saved in the system keychain. Type a new one to replace it."
	}
	return "Kept in the system keychain, never in the settings file."
}

// forgetToken removes a server's saved token.
func (a *app) forgetToken(id string) {
	if id == "" {
		return
	}
	if err := a.gw.SetUpstreamToken(id, ""); err != nil {
		a.toast("Could not forget the token: " + err.Error())
		return
	}
	a.form.hasToken = false
	a.form.token = ""
	a.toast("The saved token was forgotten")
	a.reconnect(id)
}

// editServer opens the form for an existing server.
func (a *app) editServer(i int) {
	if i < 0 || i >= len(a.cfg.Servers) {
		return
	}
	sc := a.cfg.Servers[i]
	creds := a.gw.ClientCredentials(sc.ID)
	a.form = serverForm{
		editing:      true,
		id:           sc.ID,
		name:         sc.Name,
		transport:    transportIndex(sc.Transport),
		command:      sc.Command,
		args:         strings.Join(sc.Args, "\n"),
		env:          strings.Join(sc.Env, "\n"),
		url:          sc.URL,
		headers:      formatPairs(sc.Headers),
		oauth:        sc.OAuth,
		enabled:      sc.Enabled,
		tokenHeader:  sc.TokenHeader,
		tokenScheme:  sc.TokenScheme,
		hasToken:     a.gw.HasUpstreamToken(sc.ID),
		clientID:     creds.ClientID,
		clientSecret: creds.Secret,
	}
}

// serverForm is the add/edit dialog.
func (a *app) serverForm(c *ui.Context) {
	f := &a.form
	if !f.editing {
		return
	}
	t := c.Theme()

	title := "Add a server"
	if !f.isNew {
		title = "Edit server"
	}

	ui.Modal(c, &f.editing, func() {
		ui.Text(c, title).FontSize(18).Bold()
		ui.Form(c, func() {
			ui.Field(c, "Name", func() {
				ui.TextInput(c, &f.name).Placeholder("Filesystem").AutoFocus()
			})
			ui.Field(c, "Transport", func() {
				ui.Segmented(c, &f.transport, transportOptions...)
			})

			if transportAt(f.transport) == config.TransportStdio {
				ui.Field(c, "Command", func() {
					ui.TextInput(c, &f.command).Placeholder("npx")
				})
				ui.Field(c, "Arguments", func() {
					ui.TextArea(c, &f.args).Height(80).
						Placeholder("-y\n@modelcontextprotocol/server-filesystem\n/tmp")
				}).Description("One argument per line.")
				ui.Field(c, "Environment", func() {
					ui.TextArea(c, &f.env).Height(60).Placeholder("API_KEY=…")
				}).Description("KEY=VALUE, one per line.")
			} else {
				ui.Field(c, "URL", func() {
					ui.TextInput(c, &f.url).Placeholder("https://example.com/mcp")
				})
				ui.Field(c, "Sign in with OAuth", func() {
					ui.Switch(c, &f.oauth)
				}).Description("The gateway opens your browser and stores the token in the system keychain.")
				if f.oauth {
					ui.Field(c, "Client ID", func() {
						ui.TextInput(c, &f.clientID)
					}).Description("Leave empty when the server registers clients itself.")
					ui.Field(c, "Client secret", func() {
						ui.TextInput(c, &f.clientSecret).Password()
					})
					if !f.isNew {
						ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
							if ui.Button(c, "Sign in now").Clicked() {
								a.signIn(f.id)
							}
							ui.Text(c, "Your browser opens to approve it. The gateway must be serving.").
								FontSize(11).TextColor(t.TextMuted)
						})
					}
				} else {
					ui.Field(c, "Token header", func() {
						ui.TextInput(c, &f.tokenHeader).Placeholder("Authorization")
					}).Description("The header the token is sent in. Leave blank for a server that needs none.")
					ui.Field(c, "Token scheme", func() {
						ui.TextInput(c, &f.tokenScheme).Placeholder("Bearer")
					}).Description("Prefixed to the token, as in \"Bearer\". Leave blank to send it alone.")
					ui.Field(c, "Token", func() {
						ui.TextInput(c, &f.token).Password().Placeholder(tokenPlaceholder(f))
					}).Description(tokenHint(f))
					if f.hasToken {
						if ui.Button(c, "Forget the saved token").Clicked() {
							a.forgetToken(f.id)
						}
					}
					ui.Field(c, "Other headers", func() {
						ui.TextArea(c, &f.headers).Height(50).Placeholder("X-Trace: 1")
					}).Description("Key: value, one per line.")
				}
			}

			ui.Field(c, "Enabled", func() {
				ui.Switch(c, &f.enabled)
			}).Description("Connect this server when the gateway starts.")
		})

		if f.err != "" {
			ui.Text(c, f.err).FontSize(12).TextColor(t.Danger)
		}

		ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
			if ui.Button(c, "Cancel").Clicked() {
				f.editing = false
			}
			if ui.PrimaryButton(c, "Save").Clicked() {
				a.saveForm()
			}
		})
	})
}

// saveForm validates the form and stores the server.
func (a *app) saveForm() {
	f := &a.form

	sc := config.ServerConfig{
		ID:        f.id,
		Name:      strings.TrimSpace(f.name),
		Transport: transportAt(f.transport),
		Enabled:   f.enabled,
	}
	if sc.ID == "" {
		sc.ID = newID()
	}
	switch sc.Transport {
	case config.TransportStdio:
		sc.Command = strings.TrimSpace(f.command)
		sc.Args = splitLines(f.args)
		sc.Env = splitLines(f.env)
	default:
		sc.URL = strings.TrimSpace(f.url)
		sc.OAuth = f.oauth
		// A server signs in with OAuth or with a token, never both.
		if !f.oauth {
			sc.TokenHeader = strings.TrimSpace(f.tokenHeader)
			sc.TokenScheme = strings.TrimSpace(f.tokenScheme)
			sc.Headers = parsePairs(f.headers)
		}
	}
	if err := sc.Validate(); err != nil {
		f.err = err.Error()
		return
	}

	a.cfg.Upsert(sc)
	a.cfg.EnsureAliases()
	if err := a.cfg.Save(); err != nil {
		f.err = "Could not save: " + err.Error()
		return
	}
	if sc.OAuth {
		if err := a.gw.SetClientCredentials(sc.ID, strings.TrimSpace(f.clientID), f.clientSecret); err != nil {
			f.err = "Could not save credentials: " + err.Error()
			return
		}
	} else if token := strings.TrimSpace(f.token); token != "" {
		// The token goes to the keychain, and is never read back into the form.
		if err := a.gw.SetUpstreamToken(sc.ID, token); err != nil {
			f.err = "Could not save the token: " + err.Error()
			return
		}
	}

	*a.flag(sc.ID, sc.Enabled) = sc.Enabled
	f.editing = false
	a.revealServer(sc.ID)
	a.toast(sc.Name + " saved")
	a.sync()
}

// askRemove asks before removing a server.
func (a *app) askRemove(id string) {
	if id == "" {
		return
	}
	a.removeID = id
	a.confirm = true
}

// removeDialog confirms and performs a removal.
func (a *app) removeDialog(c *ui.Context) {
	if !a.confirm {
		return
	}
	sc, ok := a.cfg.Server(a.removeID)
	if !ok {
		a.confirm = false
		return
	}
	switch ui.AlertDialog(c, &a.confirm,
		fmt.Sprintf("Remove “%s”?", sc.Name),
		"Its tools are withdrawn from the gateway, and its saved sign-in is forgotten.",
		"Cancel", "Remove") {
	case 1:
		a.removeServer(a.removeID)
	}
}

// removeServer forgets a server and everything stored for it.
func (a *app) removeServer(id string) {
	sc, ok := a.cfg.Server(id)
	if !ok {
		return
	}
	enabled := sc.Enabled
	a.cfg.Remove(id)
	if err := a.cfg.Save(); err != nil {
		a.toast("Could not save: " + err.Error())
	}
	delete(a.flags, id)
	if err := a.creds.SignOut(id); err != nil {
		a.toast("Could not forget the sign-in: " + err.Error())
	}
	if err := a.gw.SetUpstreamToken(id, ""); err != nil {
		a.toast("Could not forget the token: " + err.Error())
	}
	if enabled {
		a.sync()
	}
	a.reload()
	a.toast(sc.Name + " removed")
}

// connect switches a server on and connects it, or tries again when it is
// already switched on but not connected.
func (a *app) connect(id string) {
	sc, ok := a.cfg.Server(id)
	if !ok {
		return
	}
	if sc.Enabled {
		// On, but not connected: the connection is what needs retrying.
		a.reconnect(id)
		return
	}
	*a.flag(id, true) = true
	a.setEnabled(id, true)
}

// disconnect drops a server's connection and leaves it off, so it is not
// connected again at the next launch either.
func (a *app) disconnect(id string) {
	*a.flag(id, false) = false
	a.setEnabled(id, false)
}

// reconnect drops a server's connection and connects it again.
func (a *app) reconnect(id string) {
	if id == "" || !a.live() {
		return
	}
	a.busy = "Reconnecting…"
	go func() {
		err := a.gw.Reconnect(context.Background(), id)
		a.post(func() {
			a.busy = ""
			a.reload()
			if err != nil {
				a.toast(err.Error())
			}
		})
	}()
}

// signIn begins the OAuth flow and opens the browser.
func (a *app) signIn(id string) {
	if !a.live() {
		return
	}
	a.busy = "Starting sign-in…"
	go func() {
		url, err := a.gw.BeginAuth(context.Background(), id)
		if err == nil {
			err = a.openAuthURL(url)
		}
		a.post(func() {
			a.busy = ""
			a.reload()
			switch {
			case err != nil:
				a.toast(err.Error())
			case url == "":
				a.toast("Already signed in")
			default:
				a.toast("Finish signing in in your browser")
			}
		})
	}()
}

// openAuthURL sends the user to the authorization URL a sign-in produced. An
// empty URL means the server turned out to need no sign-in, so there is nothing
// to open.
func (a *app) openAuthURL(url string) error {
	if url == "" {
		return nil
	}
	return a.openExternal(url)
}

// signOut forgets a server's token.
func (a *app) signOut(id string) {
	if !a.live() {
		return
	}
	a.busy = "Signing out…"
	go func() {
		err := a.gw.SignOut(context.Background(), id)
		a.post(func() {
			a.busy = ""
			a.reload()
			if err != nil {
				a.toast(err.Error())
			}
		})
	}()
}

// openImport opens the paste-a-configuration dialog.
func (a *app) openImport() {
	a.imp = importForm{open: true}
}

// importPlaceholder is the example an empty box shows.
const importPlaceholder = `{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    },
    "remote": {
      "type": "http",
      "url": "https://example.com/mcp"
    }
  }
}`

// importDialog takes a configuration copied from another MCP client.
func (a *app) importDialog(c *ui.Context) {
	imp := &a.imp
	if !imp.open {
		return
	}
	t := c.Theme()

	ui.Modal(c, &imp.open, func() {
		ui.Text(c, "Paste a configuration").FontSize(18).Bold()
		ui.Text(c, "The mcpServers block from Claude Desktop, Cursor or VS Code works as it stands. "+
			"A server whose name is already here is replaced.").
			FontSize(12).TextColor(t.TextMuted)

		ui.TextArea(c, &imp.text).Height(220).AutoFocus().
			Placeholder(importPlaceholder).Label("Configuration")

		if imp.err != "" {
			ui.Text(c, imp.err).FontSize(12).TextColor(t.Danger)
		}

		ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
			if ui.Button(c, "Cancel").Clicked() {
				imp.open = false
			}
			if ui.PrimaryButton(c, "Import").Disabled(strings.TrimSpace(imp.text) == "").Clicked() {
				a.importServers()
			}
		})
	})
}

// importServers parses the pasted configuration and adds what it found.
func (a *app) importServers() {
	servers, err := config.ParseMCPConfig([]byte(a.imp.text))
	if err != nil {
		a.imp.err = err.Error()
		return
	}

	result := a.cfg.Import(servers)
	if err := a.cfg.Save(); err != nil {
		a.imp.err = "Could not save: " + err.Error()
		return
	}
	// The switches follow the stored configuration.
	for _, sc := range a.cfg.Servers {
		*a.flag(sc.ID, sc.Enabled) = sc.Enabled
	}

	a.imp = importForm{}
	a.reload()
	a.toast(result.Summary())
	a.sync()
}

// newID makes an identifier for a new server.
func newID() string {
	return config.NewID()
}
