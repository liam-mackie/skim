package httpapi

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"skim/internal/app"
	"skim/internal/domain"
	"skim/internal/provider"
	"skim/internal/store"
)

const metadataBody = "# Full PR description\n\nKeep <script> and & as text. Unicode Ω.\n\n" + "Final paragraph, not a summary.\n"
const metadataHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const metadataBase = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// newMetadataHarness runs the real server and real provider with local external
// peers. The opt-in browser launcher can supply web/dist without /api mocking.
func newMetadataHarness(t *testing.T, github http.Handler, webDir string) (*harness, *atomic.Int64) {
	t.Helper()
	gh := httptest.NewServer(github)
	t.Cleanup(gh.Close)
	modelCalls := &atomic.Int64{}
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		modelFixture(w, r)
	}))
	t.Cleanup(models.Close)
	settings := httpFixtureSettings(models.URL + "/v1")
	s, err := store.Open(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	a := app.New(s)
	t.Cleanup(func() { _ = a.Close() })
	a.Provider = func(domain.Settings) provider.Provider {
		p := provider.NewGitHub("fixture-token")
		p.BaseURL, p.Client = gh.URL, gh.Client()
		return p
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if webDir == "" {
		webDir = t.TempDir()
	}
	handler, err := New(a, Config{Address: ln.Addr().String(), Web: os.DirFS(webDir)})
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return &harness{base: "http://" + ln.Addr().String(), app: a, store: s}, modelCalls
}

type metadataPeer struct {
	mu            sync.Mutex
	head          string
	authStatus    int
	commitStarted chan struct{}
	commitRelease chan struct{}
	commitOnce    sync.Once
}

func (p *metadataPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	head, authStatus := p.head, p.authStatus
	started, release := p.commitStarted, p.commitRelease
	p.mu.Unlock()
	if head == "" {
		head = metadataHead
	}
	if r.Method != http.MethodGet {
		http.Error(w, "fixture only permits reads", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/user":
		if authStatus != 0 {
			http.Error(w, "private upstream auth text", authStatus)
			return
		}
		reply(map[string]string{"login": "reviewer"})
	case "/search/issues":
		requested := strings.Contains(r.URL.Query().Get("q"), "review-requested:")
		items := []any{map[string]any{"number": 7, "title": "Correct arithmetic", "html_url": "https://github.com/Acme/Widgets/pull/7", "state": "open", "updated_at": "2026-10-08T12:00:00Z", "pull_request": map[string]any{}, "user": map[string]string{"login": "contributor"}}}
		if requested {
			items = append(items, map[string]any{"number": 8, "title": "Second review", "html_url": "https://github.com/Acme/Widgets/pull/8", "state": "open", "updated_at": "2026-10-07T12:00:00Z", "draft": true, "pull_request": map[string]any{}, "user": map[string]string{"login": "another"}})
		}
		reply(map[string]any{"total_count": len(items), "incomplete_results": false, "items": items})
	case "/repos/Acme/Widgets/pulls/7", "/repos/Acme/Widgets/pulls/8":
		number := 7
		if strings.HasSuffix(r.URL.Path, "/8") {
			number = 8
		}
		reply(map[string]any{"number": number, "title": "Correct arithmetic", "body": metadataBody, "user": map[string]string{"login": "contributor"}, "base": map[string]string{"sha": metadataBase, "ref": "main"}, "head": map[string]string{"sha": head, "ref": "feature"}, "changed_files": 1, "commits": 2})
	case "/repos/Acme/Widgets/pulls/7/files", "/repos/Acme/Widgets/pulls/8/files":
		reply([]any{map[string]any{"filename": "src/calc.go", "status": "modified", "additions": 1, "deletions": 1, "patch": patch}})
	case "/repos/Acme/Widgets/pulls/7/commits", "/repos/Acme/Widgets/pulls/8/commits":
		if release != nil {
			p.commitOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		page := r.URL.Query().Get("page")
		sha, login := strings.Repeat("1", 40), "contributor"
		if page == "1" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
		} else {
			sha, login = metadataHead, ""
		}
		reply([]any{map[string]any{"sha": sha, "html_url": "javascript:ignored", "author": map[string]string{"login": login}, "commit": map[string]any{"message": "Fix <markup> & Ω\n\nFull commit body\n", "author": map[string]string{"name": "Commit Author", "date": "2026-10-07T12:34:56Z"}}}})
	default:
		http.NotFound(w, r)
	}
}

func metadataCommitPath(pr *domain.PullRequest) string {
	return "/api/commits?" + url.Values{"prKey": {pr.Key()}, "baseSha": {pr.BaseSHA}, "headSha": {pr.HeadSHA}}.Encode()
}

func TestHTTPInboxDescriptionCommitsPreserveReview(t *testing.T) {
	peer := &metadataPeer{}
	h, modelCalls := newMetadataHarness(t, peer, "")
	var inbox domain.ReviewInbox
	h.request(t, "GET", "/api/inbox", nil, &inbox)
	if len(inbox.Reviews) != 2 || !inbox.Reviews[0].Requested || !inbox.Reviews[0].Assigned || h.app.State().PullRequest != nil || modelCalls.Load() != 0 {
		t.Fatalf("inbox started review work or lost union: %+v calls=%d", inbox, modelCalls.Load())
	}
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": inbox.Reviews[0].URL}, &state)
	if state.PullRequest.Body != metadataBody || state.PullRequest.Key() != inbox.Reviews[0].PRKey || len(state.Files) != 1 || state.Files[0].Patch != patch {
		t.Fatalf("description, canonical identity, or diff lost: %+v", state)
	}
	h.awaitAnalysis(t, func(u domain.AnalysisUpdate) bool {
		return u.Analysis != nil && u.Analysis.Risk.Status != "running" && u.Analysis.Grouping.Status != "running"
	})
	var comment domain.Comment
	h.request(t, "POST", "/api/comments", domain.ManualComment{PRKey: state.PullRequest.Key(), BaseSHA: metadataBase, HeadSHA: metadataHead, Path: "src/calc.go", StartLine: 2, EndLine: 2, Side: "RIGHT", Body: "Human draft survives metadata navigation"}, &comment)
	before, beforeCalls := h.app.State(), modelCalls.Load()
	h.request(t, "GET", "/api/inbox", nil, &inbox)
	var commits domain.CommitList
	h.request(t, "GET", metadataCommitPath(state.PullRequest), nil, &commits)
	if !commits.Complete || commits.Total != 2 || len(commits.Commits) != 2 || !commits.PRIdentity.Matches(state.PullRequest) || commits.Commits[1].AuthorLogin != "" || commits.Commits[0].Message != "Fix <markup> & Ω\n\nFull commit body\n" || !strings.HasPrefix(commits.Commits[0].URL, "https://github.com/Acme/Widgets/commit/") {
		t.Fatalf("bad commit resource: %+v", commits)
	}
	if !reflect.DeepEqual(before, h.app.State()) || beforeCalls != modelCalls.Load() {
		t.Fatal("metadata reads changed review state or called models")
	}
	var failure map[string]string
	if h.request(t, "GET", "/api/commits?prKey=wrong&baseSha=x&headSha=y", nil, &failure) != 400 || !strings.Contains(failure["error"], "reload") {
		t.Fatalf("stale identity accepted: %v", failure)
	}
	peer.mu.Lock()
	peer.authStatus = 401
	peer.mu.Unlock()
	if h.request(t, "GET", "/api/inbox", nil, &failure) != 400 || !strings.Contains(failure["error"], "token") {
		t.Fatalf("auth error not actionable: %v", failure)
	}
	if !reflect.DeepEqual(before, h.app.State()) {
		t.Fatal("inbox auth failure changed active work")
	}
	// Opening the canonical inbox URL must reuse its saved manual comment.
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": inbox.Reviews[0].URL}, &state)
	if len(state.Comments) != 1 || state.Comments[0].ID != comment.ID {
		t.Fatal("inbox URL lost existing review comments")
	}
}

func TestHTTPCommitsRejectLocalSwitchWithoutBlockingLoad(t *testing.T) {
	peer := &metadataPeer{commitStarted: make(chan struct{}), commitRelease: make(chan struct{})}
	h, _ := newMetadataHarness(t, peer, "")
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(peer.commitRelease) }) }
	defer release()
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/Acme/Widgets/pull/7"}, &state)
	path := metadataCommitPath(state.PullRequest)
	type result struct {
		status int
		err    string
	}
	done := make(chan result, 1)
	go func() {
		response, err := (&http.Client{Timeout: 5 * time.Second}).Get(h.base + path)
		if err != nil {
			done <- result{err: err.Error()}
			return
		}
		defer response.Body.Close()
		var failure map[string]string
		_ = json.NewDecoder(response.Body).Decode(&failure)
		done <- result{status: response.StatusCode, err: failure["error"]}
	}()
	select {
	case <-peer.commitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("commit request did not start")
	}
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/Acme/Widgets/pull/8"}, &state)
	release()
	select {
	case got := <-done:
		if got.status != 400 || !strings.Contains(got.err, "active pull request changed") {
			t.Fatalf("mixed local PR commit response: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("commits remained blocked")
	}
	if h.app.State().PullRequest.Number != 8 {
		t.Fatal("late commit response overwrote active PR")
	}
}
