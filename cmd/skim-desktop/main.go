// Command skim-desktop is the Wails desktop shell. It runs the same loopback
// service as cmd/skim in-process and shows it in a native web view window.
//
// Build with the Wails CLI from this directory (see README "Desktop app").
package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	goruntime "runtime"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"skim/internal/host"
	"skim/internal/httpapi"
)

//go:embed build/appicon.png
var icon []byte

func main() {
	if len(os.Args) == 2 && os.Args[1] == envProbeArg {
		printEnvironment()
		return
	}
	importLoginShellEnvironment()
	if err := run(); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

type desktop struct {
	ctx    atomic.Pointer[context.Context]
	server *host.Server
}

func run() error {
	o, err := host.ParseFlags("skim-desktop", os.Args[1:], false)
	if err != nil {
		return err
	}
	d := &desktop{}
	token := rand.Text()
	o.Desktop = &httpapi.Desktop{LaunchToken: token, OpenExternal: d.openExternal}
	// A start failure (port in use, data directory owned by another Skim) is
	// shown in the window rather than lost on a GUI launch with no console.
	server, startErr := host.Start(o)
	d.server = server
	app := &options.App{
		Title:                    "Skim",
		Width:                    1440,
		Height:                   900,
		MinWidth:                 640,
		MinHeight:                480,
		EnableDefaultContextMenu: true,
		AssetServer:              &assetserver.Options{Handler: bootstrap(server, token, startErr)},
		OnStartup:                d.startup,
		OnShutdown:               d.shutdown,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "sh.mackie.skim",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { d.focus() },
		},
		Mac:   &mac.Options{About: &mac.AboutInfo{Title: "Skim", Message: "Local-first pull-request review", Icon: icon}},
		Linux: &linux.Options{Icon: icon, ProgramName: "skim", WebviewGpuPolicy: linux.WebviewGpuPolicyNever},
	}
	if goruntime.GOOS == "darwin" {
		app.Menu = macMenu(d)
	}
	return wails.Run(app)
}

func (d *desktop) startup(ctx context.Context) {
	d.ctx.Store(&ctx)
	if d.server == nil {
		return
	}
	go func() {
		// Done also fires with nil after a normal Shutdown; only report failures.
		if err := <-d.server.Done(); err != nil {
			_, _ = runtime.MessageDialog(ctx, runtime.MessageDialogOptions{Type: runtime.ErrorDialog, Title: "Skim stopped", Message: err.Error()})
			runtime.Quit(ctx)
		}
	}()
}

// shutdown cancels and drains background jobs (ACP agents, analysis) before exit.
func (d *desktop) shutdown(context.Context) {
	if d.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = d.server.Shutdown(ctx)
}

func (d *desktop) focus() {
	if ctx := d.ctx.Load(); ctx != nil {
		runtime.WindowUnminimise(*ctx)
		runtime.Show(*ctx)
	}
}

// openExternal backs httpapi.Config.OpenExternal: web views do not open
// target=_blank links, so the UI asks the host to use the system browser.
func (d *desktop) openExternal(url string) error {
	ctx := d.ctx.Load()
	if ctx == nil {
		return errors.New("the desktop window is not ready")
	}
	runtime.BrowserOpenURL(*ctx, url)
	return nil
}

// macMenu supplies the standard menus; without an Edit menu macOS web views
// ignore Cmd+C/V/X/A/Z in text fields.
func macMenu(d *desktop) *menu.Menu {
	m := menu.NewMenu()
	m.Append(menu.AppMenu())
	m.Append(menu.EditMenu())
	view := m.AddSubmenu("View")
	view.AddText("Reload", keys.CmdOrCtrl("r"), func(*menu.CallbackData) {
		if ctx := d.ctx.Load(); ctx != nil {
			runtime.WindowExecJS(*ctx, "location.reload()")
		}
	})
	m.Append(menu.WindowMenu())
	return m
}

// bootstrap is the web view's start page. The window then navigates to the
// loopback service, so the UI keeps exact browser semantics and the server's
// Host/Origin protections, as when Skim runs in a normal browser. The token
// admits this one cross-site navigation (see httpapi.Desktop).
func bootstrap(server *host.Server, token string, startErr error) http.Handler {
	var page string
	if startErr != nil {
		page = fmt.Sprintf(`<!doctype html><meta charset="utf-8"><title>Skim</title>
<style>body{font:15px/1.5 -apple-system,system-ui,"Segoe UI",sans-serif;margin:3rem;max-width:42rem;color-scheme:light dark}code{font-size:13px}</style>
<h1>Skim could not start</h1><p><code>%s</code></p>
<p>If another Skim (desktop or <code>skim</code> command) is running with the same data directory or port, quit it and reopen Skim. To run both, give one a different <code>SKIM_LISTEN</code> and <code>SKIM_DATA_DIR</code>. Skim reads <code>SKIM_*</code> variables from your login shell.</p>`, html.EscapeString(startErr.Error()))
	} else {
		target := html.EscapeString(server.URL + "/desktop-launch?token=" + url.QueryEscape(token))
		page = fmt.Sprintf(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%s"><title>Skim</title>`, target)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(page))
	})
}
