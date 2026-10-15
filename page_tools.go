package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/mark3labs/mcp-go/mcp"
)

// toolCallTimeout bounds a call made from the Tools page.
const toolCallTimeout = 60 * time.Second

// toolsPage shows every aggregated tool, its schema and a way to try it.
func (a *app) toolsPage(c *ui.Context) {
	ui.Column(c).Fill().Padding(20).Gap(14).Children(func() {
		pageHeader(c, "Tools", "What clients of the gateway see, named after the server each tool comes from.",
			func() {
				if a.busy != "" {
					ui.Spinner(c).Label(a.busy)
				}
			})

		if len(a.tools) == 0 {
			emptyState(c, "No tools yet",
				"Connect a server on the Servers page and its tools show up here. Enable the gateway on Settings to serve them.",
				func() {
					if ui.PrimaryButton(c, "Go to Servers").Clicked() {
						a.router.Push("/servers")
					}
				})
			return
		}

		ui.Row(c).Grow(1).Gap(14).AlignItems(ui.Stretch).Children(func() {
			ui.Column(c).Grow(3).MinWidth(0).Children(func() {
				a.toolTable(c)
			})
			ui.Column(c).Width(420).Shrink(0).Children(func() {
				a.toolDetail(c)
			})
		})
	})
}

// toolTable is the list of aggregated tools.
func (a *app) toolTable(c *ui.Context) {
	t := c.Theme()
	columns := []ui.TableColumn{
		{Title: "Tool"},
		{Title: "Server", Width: 150},
		{Title: "Description"},
	}
	a.toolsList.Key = func(i int) any { return a.tools[i].Name }
	a.toolsList.Label = func(i int) string { return a.tools[i].Name }
	a.toolsList.Selected = &a.toolSel

	ui.Table(c.Key("tools"), &a.toolsList, columns, len(a.tools), func(row, col int) {
		tool := a.tools[row]
		switch col {
		case 0:
			ui.Text(c, tool.Name).SingleLine()
		case 1:
			ui.Text(c, tool.ServerName).SingleLine().TextColor(t.TextMuted)
		default:
			ui.Text(c, tool.Description).SingleLine().TextColor(t.TextMuted)
		}
	}).Grow(1).Border(1, t.Border).Radius(8)
}

// toolDetail shows the selected tool's schema and lets the user call it.
func (a *app) toolDetail(c *ui.Context) {
	t := c.Theme()
	if a.toolSel < 0 || a.toolSel >= len(a.tools) {
		ui.Column(c).Fill().Center().Children(func() {
			ui.Text(c, "Select a tool").TextColor(t.TextMuted)
		})
		return
	}
	tool := a.tools[a.toolSel]

	ui.Column(c).Fill().Gap(8).Children(func() {
		ui.Text(c, tool.Name).FontSize(14).Bold().SingleLine()
		ui.Text(c, fmt.Sprintf("%s · %s at %s", tool.ServerName, tool.Upstream, a.transportWord(tool.ServerID))).
			FontSize(11).TextColor(t.TextMuted).SingleLine()
		ui.Row(c).Gap(8).Children(func() {
			if ui.Button(c, "Copy name").Clicked() {
				mygo.Clipboard.WriteText(tool.Name)
				a.toast("Tool name copied")
			}
			if ui.Button(c, "Copy schema").Clicked() {
				mygo.Clipboard.WriteText(tool.Schema)
				a.toast("Schema copied")
			}
		})

		ui.Divider(c)

		ui.Text(c, "Input schema").FontSize(12).Bold()
		ui.Scroll(c).Grow(1).MinHeight(120).Children(func() {
			ui.Column(c).Padding(8).Children(func() {
				ui.Text(c, orPlaceholder(tool.Schema, "No parameters.")).
					Font("monospace").FontSize(11).TextColor(t.TextMuted)
			})
		})

		ui.Divider(c)

		ui.Text(c, "Test call").FontSize(12).Bold()
		ui.TextArea(c, &a.callArgs).Height(72).Placeholder(`{"path": "/tmp"}`)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if a.calling {
				ui.Spinner(c).Label("Calling…")
			} else if ui.PrimaryButton(c, "Call tool").Clicked() {
				a.callTool(tool.Name)
			}
			if a.callResult != "" && ui.Button(c, "Clear").Clicked() {
				a.callResult, a.callErr = "", false
			}
		})
		if a.callResult != "" {
			color := t.TextMuted
			if a.callErr {
				color = t.Danger
			}
			ui.Scroll(c).Height(130).Border(1, t.Border).Radius(6).Children(func() {
				ui.Column(c).Padding(8).Children(func() {
					ui.Text(c, a.callResult).Font("monospace").FontSize(11).TextColor(color)
				})
			})
		}
	})
}

// transportWord names the transport of the server a tool came from.
func (a *app) transportWord(serverID string) string {
	if sc, ok := a.cfg.Server(serverID); ok {
		return sc.Transport.Label()
	}
	return "unknown"
}

// orPlaceholder returns s, or fallback when s is empty.
func orPlaceholder(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// callTool runs the selected tool with the arguments in the box.
func (a *app) callTool(name string) {
	var args any
	if text := strings.TrimSpace(a.callArgs); text != "" {
		if err := json.Unmarshal([]byte(text), &args); err != nil {
			a.callErr = true
			a.callResult = "Arguments are not valid JSON: " + err.Error()
			return
		}
	}

	a.calling = true
	a.callResult = ""
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), toolCallTimeout)
		defer cancel()
		result, err := a.gw.CallTool(ctx, name, args)
		a.win.Update(func() {
			a.calling = false
			switch {
			case err != nil:
				a.callErr = true
				a.callResult = err.Error()
			default:
				a.callErr = result.IsError
				a.callResult = formatToolResult(result)
			}
		})
	}()
}

// formatToolResult renders a tool result for the pane: its text when that is
// all there is, and JSON otherwise.
func formatToolResult(result *mcp.CallToolResult) string {
	texts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		text, ok := content.(mcp.TextContent)
		if !ok {
			texts = nil
			break
		}
		texts = append(texts, text.Text)
	}
	if len(texts) > 0 {
		out := strings.Join(texts, "\n")
		if result.IsError {
			return "error: " + out
		}
		return out
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Sprintf("%+v", result)
	}
	return string(out)
}
