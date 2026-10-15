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
	ui.Column(c).Fill().Padding(20).Gap(14).Children(func() {
		pageHeader(c, "Servers", "Upstream MCP servers whose tools this gateway serves as one.", func() {
			if a.busy != "" {
				ui.Spinner(c).Label(a.busy)
			}
			selected := a.selectedServerID()
			if ui.Button(c, "Edit…").Disabled(selected == "").Clicked() {
				a.editServer(a.sel)
			}
			if ui.Button(c, "Reconnect").Disabled(selected == "").Clicked() {
				a.reconnect(selected)
			}
			if ui.Button(c, "Remove").Disabled(selected == "").Clicked() {
				a.askRemove(selected)
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

		a.serversList.Key = func(i int) any { return a.cfg.Servers[i].ID }
		a.serversList.Label = func(i int) string { return a.cfg.Servers[i].Name }
		a.serversList.Selected = &a.sel
		list := ui.List(c.Key("servers"), &a.serversList, len(a.cfg.Servers), func(i int) {
			a.serverRow(c, i)
		}).Grow(1).Border(1, t.Border).Radius(8).Padding(4)
		// Opening a server is Enter or a double click on its row.
		if list.Submitted() {
			a.editServer(a.sel)
		}

		if n := len(a.cfg.Servers); a.sel >= 0 && a.sel < n {
			ui.Text(c, "Double-click a server to edit it. Its tools are named "+a.cfg.Servers[a.sel].Alias+"__…").
				FontSize(11).TextColor(t.TextMuted)
		}
	})
}

// serverRow draws one server.
func (a *app) serverRow(c *ui.Context, i int) {
	sc := a.cfg.Servers[i]
	view := a.status[sc.ID]
	t := c.Theme()

	row := ui.Row(c).Height(46).PaddingX(12).Gap(12).AlignItems(ui.Center)
	row.ContextMenu(func(m *ui.Menu) {
		if m.Item("Edit…").Chosen() {
			a.editServer(i)
		}
		m.Separator()
		if m.Item("Reconnect").Chosen() {
			a.reconnect(sc.ID)
		}
		if sc.OAuth {
			if view.SignedIn {
				if m.Item("Sign out").Chosen() {
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
	})

	row.Children(func() {
		ui.Text(c, "●").FontSize(13).TextColor(statusColor(t, view.Status))
		ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
			ui.Text(c, sc.Name).SingleLine()
			ui.Text(c, serverDetail(sc, view)).FontSize(11).TextColor(t.TextMuted).SingleLine()
		})
		ui.Badge(c, sc.Transport.Label())
		ui.Textf(c, "%d tools", view.Tools).FontSize(11).TextColor(t.TextMuted).Width(54).TextAlign(ui.End)
		if sc.OAuth {
			// Authorizing is the one step a switch cannot express, so it keeps a
			// button. It is offered whatever the connection state, so a server
			// can be authorized before it is switched on.
			if view.SignedIn {
				if ui.Button(c, "Sign out").Clicked() {
					a.signOut(sc.ID)
				}
			} else if ui.PrimaryButton(c, "Connect").Clicked() {
				a.signIn(sc.ID)
			}
		}
		// The switch is the server's on/off state: turning it on connects the
		// server, and turning it off disconnects it and keeps it off at launch.
		ui.Row(c).Width(64).Justify(ui.End).Children(func() {
			on := a.flag(sc.ID, sc.Enabled)
			if ui.Switch(c, on).Label(sc.Name + " enabled").Changed() {
				if *on {
					a.connect(sc.ID)
				} else {
					a.disconnect(sc.ID)
				}
			}
		})
	})
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

// selectedServerID is the ID of the highlighted server, or "".
func (a *app) selectedServerID() string {
	if a.sel < 0 || a.sel >= len(a.cfg.Servers) {
		return ""
	}
	return a.cfg.Servers[a.sel].ID
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
	if f.isNew {
		a.sel = len(a.cfg.Servers) - 1
	}
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
	if a.sel >= len(a.cfg.Servers) {
		a.sel = len(a.cfg.Servers) - 1
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
