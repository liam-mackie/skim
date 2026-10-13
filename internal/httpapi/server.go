// Package httpapi is the HTTP transport only. Review orchestration is in app so
// a future desktop host can call the same application services directly.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"skim/internal/app"
	"skim/internal/domain"
	"skim/internal/provider"
)

type Config struct {
	Address string
	// Web holds the built Svelte app; nil serves a "not built" notice.
	Web       fs.FS
	DevOrigin string
	// Desktop is set by the desktop shell; nil when Skim runs in a browser.
	Desktop *Desktop
}

// Desktop adapts the server to a desktop web view.
type Desktop struct {
	// LaunchToken, a per-launch secret, admits the web view's navigation from
	// the Wails start page to GET /desktop-launch?token=... Every other
	// cross-site request stays refused.
	LaunchToken string
	// OpenExternal opens an http(s) URL in the system browser: web views do
	// not open target=_blank links themselves.
	OpenExternal func(url string) error
}

func New(a *app.App, c Config) (http.Handler, error) {
	host, port, err := net.SplitHostPort(c.Address)
	if err != nil || port == "" {
		return nil, errors.New("HTTP address must include a loopback host and port")
	}
	ip := net.ParseIP(host)
	if (ip == nil || !ip.IsLoopback()) && host != "localhost" {
		return nil, errors.New("Skim only serves on loopback interfaces")
	}
	hosts := map[string]bool{net.JoinHostPort("127.0.0.1", port): true, net.JoinHostPort("localhost", port): true, net.JoinHostPort("::1", port): true}
	if c.DevOrigin != "" {
		u, err := url.Parse(c.DevOrigin)
		if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("dev origin must be an exact loopback HTTP origin without a path")
		}
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("dev origin must be loopback")
		}
	}
	if c.Desktop != nil && (len(c.Desktop.LaunchToken) < 26 || c.Desktop.OpenExternal == nil) {
		return nil, errors.New("desktop configuration needs a 128-bit launch token (crypto/rand.Text) and a browser opener")
	}
	mux := http.NewServeMux()
	handle := func(pattern string, fn func(http.ResponseWriter, *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			v, err := fn(w, r)
			if err != nil {
				if errors.Is(err, provider.ErrReplyDeliveryUncertain) {
					writeJSON(w, http.StatusBadGateway, map[string]string{"error": a.Store.Redact(err.Error()), "code": "reply_delivery_uncertain"})
					return
				}
				status := http.StatusBadRequest
				var size *http.MaxBytesError
				if errors.As(err, &size) {
					status = http.StatusRequestEntityTooLarge
				}
				if errors.Is(err, app.ErrReviewBusy) {
					status = http.StatusConflict
				}
				writeJSON(w, status, map[string]string{"error": a.Store.Redact(err.Error())})
				return
			}
			status := http.StatusOK
			if _, accepted := v.(domain.VerificationAccepted); accepted {
				status = http.StatusAccepted
			}
			writeJSON(w, status, v)
		})
	}
	handle("GET /api/state", func(w http.ResponseWriter, r *http.Request) (any, error) { return a.State(), nil })
	handle("GET /api/workspace", func(w http.ResponseWriter, r *http.Request) (any, error) { return a.Workspace(), nil })
	handle("GET /api/inbox", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return a.Inbox(r.Context(), domain.InboxFilter{Username: r.URL.Query().Get("username"), Repo: r.URL.Query().Get("repo")})
	})
	handle("GET /api/commits", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return a.Commits(r.Context(), domain.PRIdentity{PRKey: r.URL.Query().Get("prKey"), BaseSHA: r.URL.Query().Get("baseSha"), HeadSHA: r.URL.Query().Get("headSha")})
	})
	handle("GET /api/review/progress", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return a.ReviewProgress(domain.PRIdentity{PRKey: r.URL.Query().Get("prKey"), BaseSHA: r.URL.Query().Get("baseSha"), HeadSHA: r.URL.Query().Get("headSha")})
	})
	handle("GET /api/review-comments", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return a.ReviewThreads(r.Context(), domain.PRIdentity{PRKey: r.URL.Query().Get("prKey"), BaseSHA: r.URL.Query().Get("baseSha"), HeadSHA: r.URL.Query().Get("headSha")})
	})
	handle("GET /api/attachments", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return a.Attachments(r.Context(), domain.PRIdentity{PRKey: r.URL.Query().Get("prKey"), BaseSHA: r.URL.Query().Get("baseSha"), HeadSHA: r.URL.Query().Get("headSha")})
	})
	handle("POST /api/review-comments/replies", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v domain.ReviewReply
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.ReplyToReviewThread(r.Context(), v)
	})
	handle("GET /api/analysis", func(w http.ResponseWriter, r *http.Request) (any, error) { return a.Analysis(), nil })
	handle("GET /api/settings", func(w http.ResponseWriter, r *http.Request) (any, error) { return a.Settings(), nil })
	handle("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var s domain.SettingsUpdate
		if err := decode(w, r, &s); err != nil {
			if size := (*http.MaxBytesError)(nil); errors.As(err, &size) {
				return nil, err
			}
			// Other decoder errors may include an unknown field containing a pasted key.
			return nil, errors.New("invalid settings JSON; check the settings fields and credential updates")
		}
		return a.SetSettings(r.Context(), s)
	})
	handle("POST /api/pull-request", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v struct {
			URL string `json:"url"`
		}
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.Load(r.Context(), v.URL)
	})
	handle("POST /api/analysis", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v struct{}
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.RefreshAnalysis(r.Context())
	})
	handle("POST /api/review", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v struct{}
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.Review(r.Context())
	})
	handle("POST /api/reviewed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v struct {
			domain.PRIdentity
			Paths    []string `json:"paths"`
			Reviewed *bool    `json:"reviewed"`
		}
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		if v.Reviewed == nil {
			return nil, errors.New("reviewed must be explicitly supplied")
		}
		return a.SetReviewed(r.Context(), v.PRIdentity, v.Paths, *v.Reviewed)
	})
	handle("POST /api/style/import", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v struct {
			Owner string `json:"owner"`
			Repo  string `json:"repo"`
		}
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		style, n, err := a.ImportStyle(r.Context(), v.Owner, v.Repo)
		return map[string]any{"style": style, "sampleCount": n}, err
	})
	handle("POST /api/comments", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v domain.ManualComment
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.CreateComment(r.Context(), v)
	})
	handle("PATCH /api/comments/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v app.CommentUpdate
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.UpdateComment(r.Context(), r.PathValue("id"), v)
	})
	handle("DELETE /api/comments/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return a.DeleteComment(r.Context(), r.PathValue("id"))
	})
	handle("POST /api/publish", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v struct {
			domain.PRIdentity
			Event string `json:"event"`
			Body  string `json:"body"`
		}
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.Publish(r.Context(), v.PRIdentity, v.Event, v.Body)
	})
	handle("POST /api/review/finish", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v app.FinishReview
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.FinishReview(r.Context(), v)
	})
	handle("POST /api/chat", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v struct {
			Message  string `json:"message"`
			ThreadID string `json:"threadId"`
		}
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.Chat(r.Context(), v.Message, v.ThreadID)
	})
	handle("POST /api/verify", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v app.Verification
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return a.Verify(r.Context(), v)
	})
	handle("GET /api/verifications", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return a.Verifications(domain.PRIdentity{PRKey: r.URL.Query().Get("prKey"), BaseSHA: r.URL.Query().Get("baseSha"), HeadSHA: r.URL.Query().Get("headSha")})
	})
	handle("POST /api/verifications/{id}/cancel", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var identity domain.PRIdentity
		if err := decode(w, r, &identity); err != nil {
			return nil, err
		}
		return a.CancelVerification(r.Context(), r.PathValue("id"), identity)
	})
	handle("POST /api/verifications/clear", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := decode(w, r, &struct{}{}); err != nil {
			return nil, err
		}
		jobs, err := a.ClearVerificationActivity(r.Context())
		return map[string]any{"jobs": jobs}, err
	})
	handle("GET /api/permissions", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"permissions": a.Agent.Permissions()}, nil
	})
	handle("POST /api/permissions/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var v struct {
			OptionID string `json:"optionId"`
		}
		if err := decode(w, r, &v); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, a.Agent.Resolve(r.PathValue("id"), v.OptionID)
	})
	handle("GET /api/host", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]bool{"desktop": c.Desktop != nil}, nil
	})
	if c.Desktop != nil {
		handle("POST /api/open-external", func(w http.ResponseWriter, r *http.Request) (any, error) {
			var v struct {
				URL string `json:"url"`
			}
			if err := decode(w, r, &v); err != nil {
				return nil, err
			}
			u, err := url.Parse(v.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
				return nil, errors.New("only absolute http(s) links can be opened in the browser")
			}
			return map[string]bool{"ok": true}, c.Desktop.OpenExternal(u.String())
		})
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "API route or method not found"})
	})
	mux.HandleFunc("/", static(c.Web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		// Remote HTTPS images appear only in human-authored Markdown (PR descriptions, GitHub discussions); model output never emits img.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !hosts[r.Host] {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "untrusted Host header"})
			return
		}
		if c.Desktop != nil && r.Method == "GET" && r.URL.Path == "/desktop-launch" &&
			subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(c.Desktop.LaunchToken)) == 1 {
			// The only admitted cross-site navigation: from the Wails start page.
			// Its same-origin meta refresh makes the app document load same-origin.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=/"><title>Skim</title>`)
			return
		}
		origin := r.Header.Get("Origin")
		if (origin != "" && origin != "http://"+r.Host && origin != c.DevOrigin) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin access denied"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || typ != "application/json" {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	}), nil
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request must contain exactly one JSON value")
		}
		return err
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// static serves the built app. Directory-backed callers pass an os.Root FS so
// symbolic links cannot escape the build directory.
func static(fsys fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		clean := path.Clean("/" + r.URL.Path)
		for _, part := range strings.Split(clean, "/") {
			if strings.HasPrefix(part, ".") {
				http.NotFound(w, r)
				return
			}
		}
		name := strings.TrimPrefix(clean, "/")
		if name == "" {
			name = "index.html"
		}
		var info fs.FileInfo
		err := errors.New("web app is not embedded")
		if fsys != nil {
			info, err = fs.Stat(fsys, name)
			if err != nil && path.Ext(name) == "" {
				name = "index.html"
				info, err = fs.Stat(fsys, name)
			}
		}
		if err != nil {
			http.Error(w, "Web app is not built. Run npm --prefix web install && npm --prefix web run build, or use the Vite development server.", http.StatusNotFound)
			return
		}
		if !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, fsys, name)
	}
}
