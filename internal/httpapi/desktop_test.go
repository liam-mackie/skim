package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"skim/internal/app"
	"skim/internal/domain"
	"skim/internal/store"
)

const desktopAddress = "127.0.0.1:7437"
const desktopToken = "LAUNCHTOKENLAUNCHTOKEN0123"

func desktopHandler(t *testing.T, desktop *Desktop) http.Handler {
	t.Helper()
	s, err := store.Open(t.TempDir(), domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	a := app.New(s)
	t.Cleanup(func() { _ = a.Close() })
	web := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>app")}}
	handler, err := New(a, Config{Address: desktopAddress, Web: web, Desktop: desktop})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func desktopRequest(handler http.Handler, method, target, site, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+desktopAddress+target, strings.NewReader(body))
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// The desktop start page is cross-site to the loopback server. Only its exact
// tokenised launch hop may cross; desktop mode must not relax anything else.
func TestDesktopLaunchIsTheOnlyCrossSiteNavigation(t *testing.T) {
	var opened []string
	desktop := desktopHandler(t, &Desktop{LaunchToken: desktopToken, OpenExternal: func(u string) error { opened = append(opened, u); return nil }})
	browser := desktopHandler(t, nil)
	cases := []struct {
		name    string
		handler http.Handler
		method  string
		target  string
		site    string
		body    string
		status  int
	}{
		{"launch-hop", desktop, "GET", "/desktop-launch?token=" + desktopToken, "cross-site", "", 200},
		{"wrong-token", desktop, "GET", "/desktop-launch?token=" + strings.ToLower(desktopToken), "cross-site", "", 403},
		{"missing-token", desktop, "GET", "/desktop-launch", "cross-site", "", 403},
		{"deep-link", desktop, "GET", "/github/o/r/pull/1/diff", "cross-site", "", 403},
		{"cross-site-api", desktop, "POST", "/api/open-external", "cross-site", `{"url":"https://example.com/"}`, 403},
		{"browser-has-no-hop", browser, "GET", "/desktop-launch?token=" + desktopToken, "cross-site", "", 403},
		{"app-after-hop", desktop, "GET", "/", "same-origin", "", 200},
		{"non-http-link", desktop, "POST", "/api/open-external", "same-origin", `{"url":"file:///etc/passwd"}`, 400},
		{"browser-has-no-opener", browser, "POST", "/api/open-external", "same-origin", `{"url":"https://example.com/"}`, 404},
		{"open-link", desktop, "POST", "/api/open-external", "same-origin", `{"url":"https://github.com/o/r/pull/1"}`, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := desktopRequest(tc.handler, tc.method, tc.target, tc.site, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
		})
	}
	if len(opened) != 1 || opened[0] != "https://github.com/o/r/pull/1" {
		t.Fatalf("browser opened %q, want only the accepted https link", opened)
	}
	if body := desktopRequest(desktop, "GET", "/desktop-launch?token="+desktopToken, "cross-site", "").Body.String(); !strings.Contains(body, `url=/"`) {
		t.Fatalf("launch hop does not refresh to the app root: %s", body)
	}
	for _, host := range []struct {
		handler http.Handler
		want    string
	}{{desktop, `{"desktop":true}`}, {browser, `{"desktop":false}`}} {
		if got := strings.TrimSpace(desktopRequest(host.handler, "GET", "/api/host", "same-origin", "").Body.String()); got != host.want {
			t.Fatalf("/api/host = %s, want %s", got, host.want)
		}
	}
}
