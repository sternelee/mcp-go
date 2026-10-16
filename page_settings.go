package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"mcp-go/internal/config"
	"mcp-go/internal/gateway"
)

// settingsPage shows where the gateway listens and what it has been doing.
func (a *app) settingsPage(c *ui.Context) {
	ui.Column(c).Fill().Padding(20).Gap(14).Children(func() {
		pageHeader(c, "Settings", "Where the gateway listens, and what it has been doing.", func() {
			if a.busy != "" {
				ui.Spinner(c).Label(a.busy)
			}
		})
		ui.Scroll(c).Grow(1).Children(func() {
			ui.Column(c).Gap(16).Children(func() {
				a.serveCard(c)
				a.callersCard(c)
				a.endpointsCard(c)
				a.activityCard(c)
			})
		})
	})
}

// callersCard decides how callers of the gateway prove who they are.
func (a *app) callersCard(c *ui.Context) {
	t := c.Theme()
	card(c, iconShield, "Callers", func() {
		ui.Segmented(c, &a.authMode, "Off", "Token", "Token + OAuth").OnChange(func() {
			a.setAuthMode(a.authMode)
		})
		ui.Text(c, authExplanation(a.auth.Mode)).FontSize(11).TextColor(t.TextMuted)

		if !a.auth.Mode.RequiresToken() {
			return
		}

		ui.Divider(c)
		ui.Row(c).Gap(8).AlignItems(ui.End).Children(func() {
			ui.Field(c, "Token callers present", func() {
				ui.TextInput(c, &a.authToken).ReadOnly(true).Width(320)
			}).Description("Send it as: Authorization: Bearer <token>")
			ui.Spacer(c)
			if ui.Button(c, "Copy").Clicked() {
				mygo.Clipboard.WriteText(a.authToken)
				a.toast("Token copied")
			}
			if ui.Button(c, "New token").Clicked() {
				a.rotateAuthToken()
			}
		})

		if a.auth.AuthorizationServer {
			ui.Divider(c)
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Column(c).Gap(2).Grow(1).Children(func() {
					ui.Text(c, "MCP clients can also sign in").FontSize(12).Bold()
					ui.Text(c, "They discover it at "+a.auth.MetadataURL+" and are issued a token of their own.").
						FontSize(11).TextColor(t.TextMuted)
				})
				if ui.Button(c, "Copy discovery URL").Clicked() {
					mygo.Clipboard.WriteText(a.auth.MetadataURL)
					a.toast("Discovery URL copied")
				}
			})
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Textf(c, "%d issued token(s) are live.", a.auth.IssuedTokens).FontSize(11).TextColor(t.TextMuted).Grow(1)
				if ui.Button(c, "Revoke them all").Disabled(a.auth.IssuedTokens == 0).Clicked() {
					a.gw.RevokeIssuedTokens()
					a.reload()
					a.toast("Every issued token was revoked")
				}
			})
		}
	})
}

// authExplanation says what the chosen mode means.
func authExplanation(mode config.InboundMode) string {
	switch mode {
	case config.InboundToken:
		return "Callers must send the token below. Nothing else can reach the tools."
	case config.InboundOAuth:
		return "Callers must send the token below, and MCP clients that support OAuth can sign in for a token of their own. " +
			"Because this gateway is the sign-in server, anything running on this computer can also ask to be approved, so the token mode is the tighter of the two."
	}
	return "Anything that can reach the port may call the tools. The port is loopback only."
}

// setAuthMode applies the chosen way of guarding the gateway.
func (a *app) setAuthMode(index int) {
	if index < 0 || index >= len(inboundModes) {
		index = 0
	}
	mode := inboundModes[index]

	token := a.gw.AuthToken()
	if mode.RequiresToken() && token == "" {
		generated, err := a.gw.GenerateAuthToken()
		if err != nil {
			a.toast("Could not make a token: " + err.Error())
			a.reload()
			return
		}
		token = generated
	}
	if err := a.gw.SetInbound(mode, token); err != nil {
		a.toast(err.Error())
		a.reload()
		return
	}
	a.cfg.Inbound = mode
	if err := a.cfg.Save(); err != nil {
		a.toast("Could not save: " + err.Error())
	}
	a.reload()
	a.toast("Callers now need: " + mode.Label())
}

// rotateAuthToken replaces the token callers must present.
func (a *app) rotateAuthToken() {
	token, err := a.gw.GenerateAuthToken()
	if err != nil {
		a.toast("Could not make a token: " + err.Error())
		return
	}
	if err := a.gw.SetInbound(a.auth.Mode, token); err != nil {
		a.toast(err.Error())
		return
	}
	a.reload()
	a.toast("A new token was issued; callers must use it now")
}

// serveCard starts and stops the gateway, and chooses its port.
func (a *app) serveCard(c *ui.Context) {
	serving := a.gw.Serving()
	card(c, iconZap, "Gateway", func() {
		ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
			ui.Field(c, "Port", func() {
				ui.NumberInput(c, &a.port, 1, 65535, 1).Width(120)
			}).Description("Loopback only, so only this computer can reach it.")

			changed := int(a.port) != a.cfg.Port
			if ui.Button(c, "Save port").Disabled(!changed).Clicked() {
				a.setPort()
			}

			ui.Spacer(c)

			livePill(c, serving, "serving", "stopped")
			if serving {
				if ui.Button(c, "Stop").Clicked() {
					a.stop()
				}
			} else if ui.PrimaryButton(c, "Start").Clicked() {
				a.start()
			}
		})
	})
}

// endpointsCard lists what the gateway serves and how to paste it elsewhere.
func (a *app) endpointsCard(c *ui.Context) {
	t := c.Theme()
	card(c, iconGlobe, "Endpoints", func() {
		for _, endpoint := range a.gw.Endpoints() {
			endpoint := endpoint
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, endpoint).Font("monospace").FontSize(12).Grow(1).SingleLine()
				if ui.Button(c, "Copy").Clicked() {
					mygo.Clipboard.WriteText(endpoint)
					a.toast("Copied " + endpoint)
				}
			})
		}
		ui.Divider(c)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "Paste the streamable HTTP endpoint into a client that speaks MCP.").
				FontSize(11).TextColor(t.TextMuted).Grow(1)
			if ui.Button(c, "Copy client config").Clicked() {
				mygo.Clipboard.WriteText(a.clientConfig())
				a.toast("Client configuration copied")
			}
		})
		if a.serverLine() != "" {
			ui.Text(c, a.serverLine()).FontSize(11).TextColor(t.TextMuted)
		}
	})
}

// clientConfig is a ready-to-paste MCP client configuration.
func (a *app) clientConfig() string {
	config := map[string]any{
		"mcpServers": map[string]any{
			"gateway": map[string]any{"url": a.gw.BaseURL() + "/mcp"},
		},
	}
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}

// serverLine describes which clients the gateway currently answers.
func (a *app) serverLine() string {
	ready := 0
	for _, v := range a.status {
		if v.Status == gateway.StatusReady {
			ready++
		}
	}
	if ready == 0 {
		return "No server is connected yet, so clients would see only the gateway's own tool."
	}
	return fmt.Sprintf("Clients now see the tools of %d connected server(s).", ready)
}

// activityCard shows what the gateway has done recently, newest first.
func (a *app) activityCard(c *ui.Context) {
	t := c.Theme()
	card(c, iconActivity, "Activity", func() {
		if len(a.events) == 0 {
			ui.Text(c, "Nothing yet.").FontSize(12).TextColor(t.TextMuted)
			return
		}
		events := a.events
		start := 0
		if len(events) > 100 {
			start = len(events) - 100
		}
		for i := len(events) - 1; i >= start; i-- {
			event := events[i]
			color := t.TextMuted
			if event.Err {
				color = t.Danger
			}
			ui.Row(c).Gap(10).AlignItems(ui.Start).Children(func() {
				ui.Text(c, event.Time.Format("15:04:05")).Font("monospace").FontSize(11).TextColor(t.TextMuted)
				ui.Text(c, event.Text).FontSize(12).TextColor(color).Grow(1)
			})
		}
	})
}

// setPort records the chosen port and restarts the gateway when it is serving.
func (a *app) setPort() {
	port := int(a.port)
	if port < 1 || port > 65535 {
		a.toast("Choose a port between 1 and 65535")
		return
	}
	a.cfg.Port = port
	if err := a.cfg.Save(); err != nil {
		a.toast("Could not save the port: " + err.Error())
		return
	}
	if a.gw.Serving() {
		a.restart(port)
		return
	}
	a.toast(fmt.Sprintf("Port set to %d", port))
}

// start begins serving in the background.
func (a *app) start() {
	if !a.live() {
		return
	}
	port := int(a.port)
	a.cfg.Port = port
	if err := a.cfg.Save(); err != nil {
		a.toast("Could not save the port: " + err.Error())
	}
	a.busy = "Starting the gateway…"
	go func() {
		err := a.gw.Start(context.Background(), port)
		a.post(func() {
			a.busy = ""
			a.reload()
			if err != nil {
				a.toast(err.Error())
				return
			}
			a.toast("Serving on " + a.gw.BaseURL())
		})
	}()
}

// stop stops serving in the background.
func (a *app) stop() {
	if !a.live() {
		return
	}
	a.busy = "Stopping the gateway…"
	go func() {
		err := a.gw.Stop(context.Background())
		a.post(func() {
			a.busy = ""
			a.reload()
			if err != nil {
				a.toast(err.Error())
			}
		})
	}()
}

// restart moves the gateway to another port.
func (a *app) restart(port int) {
	if !a.live() {
		return
	}
	a.busy = "Moving the gateway…"
	go func() {
		err := a.gw.Stop(context.Background())
		if err == nil {
			err = a.gw.Start(context.Background(), port)
		}
		a.post(func() {
			a.busy = ""
			a.reload()
			if err != nil {
				a.toast(err.Error())
				return
			}
			a.toast("Serving on " + a.gw.BaseURL())
		})
	}()
}
