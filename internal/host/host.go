// Package host starts the Skim service (state store, application services and
// loopback HTTP server) for both the command-line and desktop front ends.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"skim/internal/app"
	"skim/internal/httpapi"
	"skim/internal/model"
	"skim/internal/store"
	"skim/web"
)

// DefaultAddress is the loopback address used when neither --listen nor
// SKIM_LISTEN is given. Browser preferences are keyed by this origin.
const DefaultAddress = "127.0.0.1:7437"

type Options struct {
	Address string
	DataDir string
	// WebDir serves built assets from disk; empty uses the embedded web/dist.
	WebDir    string
	DevOrigin string
	// Desktop is set by the desktop shell; see httpapi.Config.
	Desktop *httpapi.Desktop
}

// ParseFlags reads the shared flags, falling back to SKIM_* environment
// defaults. webDir enables --web-dir/SKIM_WEB_DIR; the desktop app always
// serves its embedded assets (a shell-exported, usually relative, development
// SKIM_WEB_DIR is meaningless to a GUI launch).
func ParseFlags(name string, args []string, webDir bool) (Options, error) {
	config, err := os.UserConfigDir()
	if err != nil {
		return Options{}, err
	}
	fallback := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	var o Options
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.StringVar(&o.Address, "listen", fallback("SKIM_LISTEN", DefaultAddress), "loopback host:port")
	flags.StringVar(&o.DataDir, "data-dir", fallback("SKIM_DATA_DIR", filepath.Join(config, "skim")), "private persistent state directory")
	if webDir {
		flags.StringVar(&o.WebDir, "web-dir", os.Getenv("SKIM_WEB_DIR"), "built Svelte assets directory (default: assets embedded at build time)")
	}
	flags.StringVar(&o.DevOrigin, "dev-origin", os.Getenv("SKIM_DEV_ORIGIN"), "explicit trusted Vite origin, e.g. http://localhost:5173")
	if err := flags.Parse(args); err != nil {
		return Options{}, err
	}
	if flags.NArg() != 0 {
		return Options{}, fmt.Errorf("unexpected arguments: %q", flags.Args())
	}
	return o, nil
}

// Server is a running Skim service. Call Shutdown exactly once.
type Server struct {
	// URL is the loopback origin serving the web app, e.g. http://127.0.0.1:7437.
	URL    string
	http   *http.Server
	cancel context.CancelFunc
	app    *app.App
	store  *store.Store
	root   *os.Root
	done   chan error
}

// Start opens the private data directory (one process owns it), then serves.
func Start(o Options) (*Server, error) {
	if _, err := httpapi.New(nil, httpapi.Config{Address: o.Address, DevOrigin: o.DevOrigin}); err != nil {
		return nil, err
	}
	settings := model.Defaults()
	if raw := os.Getenv("SKIM_ACP_ARGS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &settings.ACP.Args); err != nil {
			return nil, fmt.Errorf("SKIM_ACP_ARGS must be a JSON array of strings: %w", err)
		}
	}
	if err := model.Validate(settings); err != nil {
		return nil, err
	}
	var assets fs.FS
	var root *os.Root
	if o.WebDir != "" {
		var err error
		if root, err = os.OpenRoot(o.WebDir); err != nil {
			return nil, fmt.Errorf("web directory: %w", err)
		}
		// os.Root refuses symbolic links that escape the build directory.
		assets = root.FS()
	} else {
		assets = web.Dist()
	}
	s := &Server{root: root, done: make(chan error, 1)}
	fail := func(err error) (*Server, error) {
		s.close()
		return nil, err
	}
	var err error
	if s.store, err = store.Open(o.DataDir, settings); err != nil {
		return fail(err)
	}
	if err = model.Validate(s.store.Settings()); err != nil {
		return fail(fmt.Errorf("saved settings: %w", err))
	}
	s.app = app.New(s.store)
	listener, err := net.Listen("tcp", o.Address)
	if err != nil {
		return fail(err)
	}
	handler, err := httpapi.New(s.app, httpapi.Config{Address: listener.Addr().String(), Web: assets, DevOrigin: o.DevOrigin, Desktop: o.Desktop})
	if err != nil {
		listener.Close()
		return fail(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.URL = "http://" + listener.Addr().String()
	s.http = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		err := s.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.done <- err
	}()
	return s, nil
}

// Done delivers the serve result: an error, or nil after Shutdown.
func (s *Server) Done() <-chan error { return s.done }

// Shutdown cancels in-flight requests, waits up to the context deadline for
// handlers, then cancels and drains background jobs and releases the store.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()
	err := s.http.Shutdown(ctx)
	s.close()
	return err
}

func (s *Server) close() {
	if s.app != nil {
		_ = s.app.Close()
	}
	if s.store != nil {
		_ = s.store.Close()
	}
	if s.root != nil {
		_ = s.root.Close()
	}
}
