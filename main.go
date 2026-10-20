package main

import (
	"context"
	"log"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"mcp-go/internal/config"
	"mcp-go/internal/gateway"
)

// version is what the gateway reports to the servers it connects to, and in
// its own identity.
const version = "0.1.0"

func main() {
	cfg, err := config.Load()
	if err != nil {
		// A configuration we cannot read should not stop the app: the user
		// needs the interface to fix it.
		log.Printf("mcp-go: %v; starting from defaults", err)
		cfg = config.Default()
	}

	creds := config.NewCredentialStore()
	gw := gateway.New(cfg, creds, version)
	a := newApp(cfg, creds, gw)

	mygo.App.WhenReady(func() {
		a.win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "MCP Go",
			Width:     1080,
			Height:    740,
			MinWidth:  820,
			MinHeight: 560,
			StateKey:  "main",
			// The sidebar is glass over the desktop: the material sits
			// behind everything the view leaves clear, and the sidebar is
			// what leaves it clear. Where no material shows, the sidebar
			// draws a surface of its own.
			Vibrancy: mygo.VibrancySidebar,
			// The window shows the interface MyGo draws, not a web page.
			Content: ui.View(a.view),
		})
		var quitting bool
		// Quitting must still be able to close the hidden window; OnBeforeQuit
		// runs before the windows close.
		mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { quitting = true })
		// Closing hides the window; the menu bar icon brings it back, so the
		// gateway stays reachable while its window is away.
		a.win.OnClose(func(e *mygo.CloseEvent) {
			if quitting {
				return
			}
			e.PreventDefault()
			a.win.Hide()
		})
		// Clicking the Dock icon brings the window back too: without this,
		// a hidden window leaves nothing to click on.
		mygo.App.OnActivate(func(visible bool) {
			if !visible {
				a.showMain()
			}
		})
		if mb, err := runMenubar(a); err != nil {
			log.Printf("mcp-go: menu bar: %v", err)
		} else {
			a.onReload = mb.refresh
			a.openMain = mb.open
		}
		// The gateway redraws the window from its own goroutines.
		gw.SetOnChange(a.onChange)
		go a.boot()
	})

	// Quitting must take the listener and any stdio child processes with it.
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) {
		_ = gw.Stop(context.Background())
	})

	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// boot opens the configured port, which connects the enabled servers with it.
func (a *app) boot() {
	a.busy = "Starting the gateway…"
	err := a.gw.Start(context.Background(), a.cfg.Port)
	a.post(func() {
		a.busy = ""
		a.reload()
		if err != nil {
			a.toast(err.Error())
		}
	})
}
