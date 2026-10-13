package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"skim/internal/app"
	"skim/internal/domain"
	"skim/internal/model"
	"skim/internal/provider"
	"skim/internal/store"
)

// External protocol fixtures are confined to this test file. Requests exercise
// the production HTTP handler, not an intercepted/mock /api surface.
type fixture struct {
	mu        sync.Mutex
	head      string
	base      string
	rejected  bool
	reviews   []map[string]any
	posts     int
	last      map[string]any
	mixed     bool
	empty     bool
	ambiguous bool
	// pagedSamples serves authenticated-user review comments across pages with
	// GitHub's canonical /repositories/ID Link form, recording each request.
	pagedSamples  bool
	sampleRequest []string
}

const patch = "@@ -1,3 +1,3 @@\n package calc\n-func Add(a, b int) int { return a + b }\n+func Add(a, b int) int { return a + b + 1 }\n // arithmetic"

func (f *fixture) github(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/user":
		reply(map[string]any{"login": "reviewer"})
	case f.pagedSamples && strings.HasSuffix(r.URL.Path, "/pulls/comments"):
		f.sampleRequest = append(f.sampleRequest, r.URL.RequestURI())
		mine := func(id int, body string) map[string]any {
			return map[string]any{"id": id, "body": body, "user": map[string]string{"login": "reviewer"}}
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repositories/424242/pulls/comments?page=2&per_page=100&sort=updated&evil=1>; rel="next"`, r.Host))
			reply([]any{map[string]any{"id": 9, "body": "someone else's style", "user": map[string]string{"login": "other"}}})
			return
		}
		reply([]any{mine(1, "Could we cover the zero case? This can change callers' results."), mine(2, "Please keep the return value unchanged here; the API depends on it.")})
	case r.URL.Path == "/repos/acme/widgets/pulls/comments":
		reply([]any{map[string]any{"id": 1, "body": "Could we cover the zero case? This can change callers' results.", "user": map[string]string{"login": "reviewer"}}, map[string]any{"id": 2, "body": "Please keep the return value unchanged here; the API depends on it.", "user": map[string]string{"login": "reviewer"}}})
	case strings.HasSuffix(r.URL.Path, "/files"):
		if f.empty {
			reply([]any{})
			return
		}
		files := []any{map[string]any{"filename": "src/calc.go", "status": "modified", "additions": 1, "deletions": 1, "patch": patch}, map[string]any{"filename": "src/calc_test.go", "status": "modified", "additions": 1, "deletions": 1, "patch": "@@ -1,3 +1,3 @@\n package calc\n-func expected() int { return 2 }\n+func expected() int { return 3 }\n // expected result"}}
		if f.mixed {
			files = append(files, map[string]any{"filename": "assets/logo.png", "status": "added", "additions": 0, "deletions": 0}, map[string]any{"filename": "generated.txt", "status": "modified", "additions": 100, "deletions": 100, "patch": "@@ -1 +1 @@\n-old\n+new"})
		}
		reply(files)
	case strings.HasSuffix(r.URL.Path, "/reviews"):
		if r.Method == "POST" {
			var v map[string]any
			if json.NewDecoder(r.Body).Decode(&v) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			f.posts++
			f.last = v
			if f.rejected {
				http.Error(w, "review rejected", http.StatusUnprocessableEntity)
				return
			}
			review := map[string]any{"id": f.posts, "body": v["body"], "commit_id": v["commit_id"], "state": "COMMENTED", "submitted_at": domain.Now(), "user": map[string]string{"login": "reviewer"}}
			f.reviews = append(f.reviews, review)
			if f.ambiguous {
				http.Error(w, "uncertain upstream response", 500)
				return
			}
			reply(review)
		} else {
			reply(f.reviews)
		}
	case r.URL.Path == "/repos/acme/widgets/pulls/7" || r.URL.Path == "/repos/acme/widgets/pulls/8":
		number := 7
		if strings.HasSuffix(r.URL.Path, "/8") {
			number = 8
		}
		count := 2
		if f.mixed {
			count = 4
		}
		if f.empty {
			count = 0
		}
		base := f.base
		if base == "" {
			base = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}
		reply(map[string]any{"number": number, "title": "Correct arithmetic result handling", "body": "Keep arithmetic and its regression expectations consistent.", "html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number), "user": map[string]string{"login": "contributor"}, "base": map[string]string{"sha": base, "ref": "main"}, "head": map[string]string{"sha": f.head, "ref": "fix/arithmetic"}, "changed_files": count})
	default:
		http.Error(w, "unexpected GitHub fixture path", 404)
	}
}
func modelFixture(w http.ResponseWriter, r *http.Request) {
	var input map[string]json.RawMessage
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "bad fixture input", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v1/systemone" {
		var questions map[string]struct {
			Type     string            `json:"type"`
			Criteria map[string]string `json:"criteria"`
		}
		_ = json.Unmarshal(input["questions"], &questions)
		answers := map[string]any{}
		for key, q := range questions {
			if q.Type != "choice" || (key != "risk" && key != "test_scope") {
				http.Error(w, "not real Choice protocol", 400)
				return
			}
			choice := "medium"
			if key == "test_scope" {
				choice = "other_or_uncertain"
			}
			probs := map[string]float64{}
			for option := range q.Criteria {
				probs[option] = 0.1 / float64(len(q.Criteria)-1)
			}
			probs[choice] = 0.9
			answers[key] = map[string]any{"type": "choice", "choice": choice, "confidence": 0.9, "probabilities": probs}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "model": "jev-latest"})
		return
	}
	if r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "unexpected model path", 404)
		return
	}
	var messages []domain.Message
	_ = json.Unmarshal(input["messages"], &messages)
	if len(messages) == 0 {
		http.Error(w, "no messages", 400)
		return
	}
	schemaName := ""
	if strings.Contains(messages[0].Content, "Extract only concise, actionable") {
		schemaName = "review_comments"
	} else if strings.HasPrefix(messages[0].Content, "Task: group") {
		schemaName = "review_groups"
	} else if strings.HasPrefix(messages[0].Content, "Task: a separate classifier") {
		schemaName = "risk_factors"
	}
	if err := requireFixtureResponseFormat(input["response_format"], schemaName); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	content := "The arithmetic implementation is in src/calc.go:2; related regression expectations changed in src/calc_test.go:2."
	if strings.HasPrefix(messages[0].Content, "Derive concise") {
		content = "Use concise, direct review comments. Ask specific questions and explain consequences. Suggest focused regression coverage."
	} else if strings.Contains(messages[0].Content, "Extract only concise, actionable") {
		b, _ := json.Marshal(map[string]any{"comments": []any{map[string]any{
			"path": "src/calc.go", "startLine": 2, "endLine": 2, "side": "RIGHT",
			"body": "The checked return value adds one to every sum. Preserve the caller-visible result.\n\n```suggestion\nfunc Add(a, b int) int { return a + b }\n```",
		}}})
		content = string(b)
	} else if strings.HasPrefix(messages[0].Content, "Task: group") {
		var payload struct {
			Files []domain.File `json:"files"`
		}
		if err := json.Unmarshal([]byte(messages[len(messages)-1].Content), &payload); err != nil {
			http.Error(w, "invalid grouping payload", 400)
			return
		}
		groups := []map[string]any{}
		for end := len(payload.Files); end > 0; {
			start := end - 2
			if start < 0 {
				start = 0
			}
			paths := []string{}
			for i := end - 1; i >= start; i-- {
				paths = append(paths, payload.Files[i].Path)
			}
			groups = append(groups, map[string]any{"title": fmt.Sprintf("Change unit %d", len(groups)+1), "summary": "Updates related implementation and regression expectations.", "paths": paths})
			end = start
		}
		b, _ := json.Marshal(map[string]any{"groups": groups})
		content = string(b)
	} else if schemaName == "risk_factors" {
		content = `{"factors":[{"text":"The changed arithmetic result reaches every caller; confirm callers expect it.","side":"RIGHT","startLine":2,"endLine":2}]}`
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
}
func workspace(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "calc.go"), []byte("package calc\nfunc Add(a, b int) int { return a + b }\n// arithmetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.23\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if got := Add(1,1); got != 2 { t.Fatalf(\"Add(1, 1) = %d; want 2\", got) } }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string { return fixtureGit(t, dir, args...) }
	run("init", "--quiet")
	run("add", "go.mod", "src/calc.go", "src/calc_test.go")
	run("commit", "--quiet", "-m", "base fixture")
	if err := os.WriteFile(filepath.Join(dir, "src", "calc.go"), []byte("package calc\nfunc Add(a, b int) int { return a + b + 1 }\n// arithmetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "src/calc.go")
	run("commit", "--quiet", "-m", "PR fixture")
	return dir, run("rev-parse", "HEAD")
}

type harness struct {
	base    string
	app     *app.App
	store   *store.Store
	fixture *fixture
	dataDir string
	remote  string
	source  string
}

func httpFixtureSettings(baseURL string) domain.Settings {
	return domain.Settings{
		Providers: []domain.ModelProvider{
			{ID: "openai", Name: "Text fixture", Kind: "openai", BaseURL: baseURL},
			{ID: "typesafe", Name: "Risk fixture", Kind: "typesafe", BaseURL: baseURL},
		},
		Roles: domain.Roles{
			Chat:   domain.ModelRole{Provider: "openai", Model: "fixture-chat"},
			Review: domain.ModelRole{Provider: "openai", Model: "fixture-review"},
			Jev:    domain.ModelRole{Provider: "typesafe", Model: "fixture-risk"},
		},
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithModels(t, nil)
}

func newHarnessWithModels(t *testing.T, control *analysisModelControl) *harness {
	t.Helper()
	dir, sha := workspace(t)
	base := fixtureGit(t, dir, "rev-parse", "HEAD^")
	remote := filepath.Join(t.TempDir(), "remote.git")
	fixtureGit(t, dir, "clone", "--bare", "--no-local", dir, remote)
	fixtureGit(t, remote, "update-ref", "refs/pull/7/head", sha)
	fixtureGit(t, remote, "update-ref", "refs/pull/8/head", sha)
	f := &fixture{head: sha, base: base, reviews: []map[string]any{}}
	gh := httptest.NewServer(http.HandlerFunc(f.github))
	t.Cleanup(gh.Close)
	modelHandler := http.Handler(http.HandlerFunc(modelFixture))
	if control != nil {
		modelHandler = control.wrap(modelHandler)
	}
	models := httptest.NewServer(modelHandler)
	t.Cleanup(models.Close)
	settings := httpFixtureSettings(models.URL + "/v1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	settings.ACP = domain.ACPSettings{Command: exe, Args: []string{"-test.run=^TestHTTPAgentProcess$", "--", "skim-acp-fixture"}, ManagedRoot: t.TempDir()}
	dataDir := t.TempDir()
	s, err := store.Open(dataDir, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	a := app.New(s)
	if err := a.Workspaces.SetLocalRemoteForTesting("https://github.com/acme/widgets.git", remote); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	a.Provider = func(domain.Settings) provider.Provider {
		p := provider.NewGitHub("fixture-token")
		p.BaseURL = gh.URL
		p.Client = gh.Client()
		return p
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(a, Config{Address: ln.Addr().String(), Web: os.DirFS(t.TempDir()), DevOrigin: "http://localhost:5173"})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return &harness{base: "http://" + ln.Addr().String(), app: a, store: s, fixture: f, dataDir: dataDir, remote: remote, source: dir}
}
func (h *harness) request(t *testing.T, method, path string, input, out any) int {
	t.Helper()
	// Normal fixture submissions carry the currently displayed snapshot. Tests
	// for missing identity use raw HTTP; explicitly supplied fields stay intact.
	if method == "POST" && path == "/api/publish" {
		if fields, ok := input.(map[string]string); ok {
			payload := make(map[string]string, len(fields)+3)
			for key, value := range fields {
				payload[key] = value
			}
			if pr := h.store.PullRequest(); pr != nil {
				for key, value := range map[string]string{"prKey": pr.Key(), "baseSha": pr.BaseSHA, "headSha": pr.HeadSHA} {
					if _, supplied := payload[key]; !supplied {
						payload[key] = value
					}
				}
			}
			input = payload
		}
	}
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		switch value := out.(type) {
		case *domain.State:
			*value = domain.State{}
		case *domain.ChatThread:
			*value = domain.ChatThread{}
		case *domain.Comment:
			*value = domain.Comment{}
		case *domain.AnalysisUpdate:
			*value = domain.AnalysisUpdate{}
		}
		if err = json.Unmarshal(b, out); err != nil {
			t.Fatalf("decode %s: %v body=%s", path, err, b)
		}
	}
	if res.StatusCode >= 400 {
		if _, expectedError := out.(*map[string]string); !expectedError {
			t.Fatalf("%s %s: HTTP %d %s", method, path, res.StatusCode, b)
		}
	}
	return res.StatusCode
}
func TestHTTPStyleImportTraversesCanonicalRepositoryLink(t *testing.T) {
	h := newHarness(t)
	h.fixture.pagedSamples = true
	var style struct {
		Style       string `json:"style"`
		SampleCount int    `json:"sampleCount"`
	}
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
	h.request(t, "POST", "/api/style/import", struct{}{}, &style)
	if style.SampleCount != 2 || style.Style == "" {
		t.Fatalf("second page of the authenticated user's comments was not imported: %+v", style)
	}
	h.fixture.mu.Lock()
	requests := append([]string(nil), h.fixture.sampleRequest...)
	h.fixture.mu.Unlock()
	if len(requests) != 2 || !strings.HasPrefix(requests[1], "/repos/acme/widgets/pulls/comments?") || strings.Contains(requests[1], "evil") || strings.Contains(requests[1], "sort=updated") {
		t.Fatalf("follow-up request must keep the named repository and original query: %v", requests)
	}
}

func TestHTTPWorkflow(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	if state.PullRequest == nil || len(state.Files) != 2 {
		t.Fatalf("bad load: %+v", state)
	}
	update := h.awaitAnalysis(t, func(u domain.AnalysisUpdate) bool {
		return u.Analysis != nil && u.Analysis.Risk.Status == "complete" && u.Analysis.Grouping.Status == "complete"
	})
	if len(update.Groups) != 1 || len(update.Groups[0].Paths) != 2 || update.Files[0].Risk == nil {
		t.Fatalf("background risk/grouping not applied: %+v", update)
	}
	var style struct {
		Style       string `json:"style"`
		SampleCount int    `json:"sampleCount"`
	}
	h.request(t, "POST", "/api/style/import", struct{}{}, &style)
	if style.SampleCount != 2 || style.Style == "" {
		t.Fatal("style not derived from real samples")
	}
	h.request(t, "POST", "/api/review", struct{}{}, &state)
	if len(state.Comments) != 2 {
		t.Fatalf("expected 2 drafts, got %d", len(state.Comments))
	}
	id := state.Comments[0].ID
	h.request(t, "PATCH", "/api/comments/"+id, map[string]string{"status": "pending", "body": "Please preserve the expected sum."}, nil)
	h.request(t, "DELETE", "/api/comments/"+state.Comments[1].ID, nil, nil)
	h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &state)
	if state.Comments[0].Status != "published" || state.Comments[1].Status != "deleted" {
		t.Fatal("publication statuses incorrect")
	}
	var failure map[string]string
	if h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &failure) != 400 {
		t.Fatal("empty duplicate publication accepted")
	}
	h.fixture.mu.Lock()
	posts := h.fixture.posts
	comments := h.fixture.last["comments"].([]any)
	h.fixture.mu.Unlock()
	if posts != 1 || len(comments) != 1 {
		t.Fatal("publication duplicated or included non-approved drafts")
	}
	var chat domain.ChatThread
	h.request(t, "POST", "/api/chat", map[string]string{"message": "Where did arithmetic change?"}, &chat)
	if len(chat.Messages) != 2 {
		t.Fatal("chat not persisted")
	}
	h.request(t, "POST", "/api/chat", map[string]string{"threadId": chat.ID, "message": "And its tests?"}, &chat)
	if len(chat.Messages) != 4 {
		t.Fatal("chat continuation lost history")
	}
	// Acceptance returns before the real subprocess asks for permission.
	var accepted domain.VerificationAccepted
	if status := h.request(t, "POST", "/api/verify", verificationRequest(h, ""), &accepted); status != http.StatusAccepted || !accepted.Job.Active() || len(accepted.Thread.Messages) != 1 {
		t.Fatalf("verification was not admitted immediately: HTTP %d %+v", status, accepted)
	}
	waitCondition(t, func() bool { return len(h.app.Agent.Permissions()) > 0 })
	permission := h.app.Agent.Permissions()[0]
	h.request(t, "POST", "/api/permissions/"+permission.ID, map[string]string{"optionId": "allow-once"}, nil)
	awaitVerificationJob(t, h, accepted.Job.ID)
	h.request(t, "GET", "/api/state", nil, &state)
	if len(state.Comments) != 3 || state.Comments[2].Source != "verification" || len(state.Chats[1].Messages) != 2 {
		t.Fatalf("verification failed: %+v", state)
	}
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/8"}, &state)
	if len(state.Comments) != 0 {
		t.Fatal("PR state leaked")
	}
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	if len(state.Comments) != 3 {
		t.Fatal("PR switch lost work")
	}
	// Reopen the actual persisted file with the process lock released.
	if err := h.app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(h.dataDir, model.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.Snapshot().State().Comments) != 3 {
		t.Fatal("reload lost drafts")
	}
}
func TestEmptyPRStateArrays(t *testing.T) {
	control := newAnalysisModelControl()
	h := newHarnessWithModels(t, control)
	h.fixture.mu.Lock()
	h.fixture.empty = true
	h.fixture.mu.Unlock()
	var state map[string]json.RawMessage
	assertArrays := func() {
		t.Helper()
		for _, key := range []string{"files", "groups", "comments", "chats"} {
			if string(state[key]) != "[]" {
				t.Fatalf("%s must be an empty JSON array, got %s", key, state[key])
			}
		}
	}
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	assertArrays()
	var update domain.AnalysisUpdate
	h.request(t, "POST", "/api/analysis", struct{}{}, &update)
	if update.Files == nil || update.Groups == nil || len(update.Files) != 0 || len(update.Groups) != 0 {
		t.Fatalf("empty analysis resources must be arrays: %+v", update)
	}
	update = h.awaitAnalysis(t, analysisTerminal)
	if update.Analysis.Risk.Status != "complete" || update.Analysis.Grouping.Status != "complete" {
		t.Fatalf("empty analysis failed: %+v", update.Analysis)
	}
	control.mu.Lock()
	calls := len(control.riskStarted) + control.groupStarted
	control.mu.Unlock()
	if calls != 0 {
		t.Fatalf("empty PR made %d paid model calls", calls)
	}
	h.request(t, "GET", "/api/state", nil, &state)
	assertArrays()
}
func TestReviewScopeAndStaleHead(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.fixture.mu.Lock()
	h.fixture.mixed = true
	h.fixture.mu.Unlock()
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.request(t, "POST", "/api/review", struct{}{}, &state)
	if len(state.Comments) != 2 || state.ReviewRun == nil || state.ReviewRun.HeadSHA != state.PullRequest.HeadSHA || state.ReviewRun.Error != "" {
		t.Fatalf("review of available patches missing: %+v %+v", state.Comments, state.ReviewRun)
	}
	id := state.Comments[0].ID
	h.fixture.mu.Lock()
	h.fixture.head = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h.fixture.mu.Unlock()
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	if !state.Comments[0].Stale || state.ReviewRun != nil {
		t.Fatal("moving head retained current-head scope/anchors")
	}
	var failure map[string]string
	if h.request(t, "PATCH", "/api/comments/"+id, map[string]string{"status": "pending"}, &failure) != 400 {
		t.Fatal("stale approval accepted")
	}
}
func TestAmbiguousPublishReconcilesWithoutResend(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.request(t, "POST", "/api/review", struct{}{}, &state)
	h.request(t, "PATCH", "/api/comments/"+state.Comments[0].ID, map[string]string{"status": "pending"}, nil)
	h.fixture.mu.Lock()
	h.fixture.ambiguous = true
	h.fixture.mu.Unlock()
	var failure map[string]string
	if h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &failure) != 400 {
		t.Fatal("ambiguous response was hidden")
	}
	snapshot := h.store.Snapshot()
	if snapshot.Reviews[snapshot.Active].Pending == nil {
		t.Fatal("ambiguous publication was not journaled")
	}
	h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &state)
	if state.Comments[0].Status != "published" {
		t.Fatal("real delivered review not reconciled")
	}
	h.fixture.mu.Lock()
	posts := h.fixture.posts
	h.fixture.mu.Unlock()
	if posts != 1 {
		t.Fatal("uncertain publication was resent")
	}
}
func TestHTTPBoundary(t *testing.T) {
	h := newHarness(t)
	specific := "specific"
	valid, err := json.Marshal(domain.SettingsUpdate{Settings: h.app.Settings(), Style: &specific})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, host, origin, ctype, body string
		status                          int
	}{
		{"foreign-host", "evil.example", "", "application/json", "{}", 403},
		{"foreign-origin", "", "https://evil.example", "application/json", "{}", 403},
		{"null-origin", "", "null", "application/json", "{}", 403},
		{"form-csrf", "", "", "text/plain", "{}", 415},
		{"oversized", "", "", "application/json", `{"style":"` + strings.Repeat("x", 1<<20) + `"}`, 413},
		{"trailing-json", "", "", "application/json", string(valid) + "{}", 400},
		{"trusted-vite", "", "http://localhost:5173", "application/json", string(valid), 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("PUT", h.base+"/api/settings", strings.NewReader(tc.body))
			if tc.host != "" {
				req.Host = tc.host
			}
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Content-Type", tc.ctype)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.status {
				b, _ := io.ReadAll(res.Body)
				t.Fatalf("got %d expected %d: %s", res.StatusCode, tc.status, b)
			}
		})
	}
}

// agentReview configures the fixture agent's review turn; call it before
// starting operations. A non-empty reply replaces the default report (one
// anchored comment per available patch), and hold makes the agent wait on a
// permission request before replying.
func (h *harness) agentReview(t *testing.T, reply string, hold bool) {
	t.Helper()
	settings := h.app.Settings()
	settings.ACP.Args = []string{"-test.run=^TestHTTPAgentProcess$", "--", "skim-acp-fixture"}
	if reply != "" {
		path := filepath.Join(t.TempDir(), "review-reply")
		if err := os.WriteFile(path, []byte(reply), 0600); err != nil {
			t.Fatal(err)
		}
		settings.ACP.Args = append(settings.ACP.Args, "review-reply="+path)
	}
	if hold {
		settings.ACP.Args = append(settings.ACP.Args, "review-hold")
	}
	if _, err := h.app.SetSettings(context.Background(), domain.SettingsUpdate{Settings: settings}); err != nil {
		t.Fatal(err)
	}
}

// fixtureReviewReport comments on line 2 of every available patch listed in the
// review prompt's trailing evidence JSON.
func fixtureReviewReport(prompt string) string {
	var evidence struct {
		Files []struct {
			Path        string `json:"path"`
			PatchStatus string `json:"patchStatus"`
		} `json:"files"`
	}
	_ = json.Unmarshal([]byte(prompt[strings.LastIndex(prompt, "\n")+1:]), &evidence)
	comments := []map[string]any{}
	for _, f := range evidence.Files {
		if f.PatchStatus == "available" {
			comments = append(comments, map[string]any{"path": f.Path, "startLine": 2, "endLine": 2, "side": "RIGHT", "body": "Could we preserve the original arithmetic result? The extra one changes every caller's result."})
		}
	}
	b, _ := json.Marshal(map[string]any{"comments": comments})
	return "Inspected `git diff BASE HEAD` in the fixture checkout; no commands were run.\n\n```json\n" + string(b) + "\n```"
}

// TestHTTPAgentProcess is the fixture ACP agent, not a test. Verification
// prompts request permission, then run `go test ./src` when allowed. Review
// prompts reply with fixtureReviewReport, or the review-reply=<file> contents;
// review-hold requests a permission first.
func TestHTTPAgentProcess(t *testing.T) {
	marker := slices.Index(os.Args, "skim-acp-fixture")
	if marker < 0 {
		return
	}
	reviewReply, hold := "", false
	for _, option := range os.Args[marker+1:] {
		if path, ok := strings.CutPrefix(option, "review-reply="); ok {
			b, err := os.ReadFile(path)
			if err != nil {
				os.Exit(2)
			}
			reviewReply = string(b)
		}
		hold = hold || option == "review-hold"
	}
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 2<<20)
	enc := json.NewEncoder(os.Stdout)
	var promptID json.RawMessage
	review := ""
	answer := func(text string) {
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "fixture-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}}}})
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]string{"stopReason": "end_turn"}})
		promptID, review = nil, ""
	}
	for scan.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &msg) != nil {
			os.Exit(2)
		}
		reply := func(v any) { _ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": v}) }
		switch msg.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}})
		case "session/new":
			reply(map[string]string{"sessionId": "fixture-session"})
		case "session/prompt":
			promptID = msg.ID
			if len(msg.Params.Prompt) == 1 && strings.Contains(msg.Params.Prompt[0].Text, "Review this pull request in the checked-out workspace") {
				review = reviewReply
				if review == "" {
					review = fixtureReviewReport(msg.Params.Prompt[0].Text)
				}
				if !hold {
					answer(review)
					continue
				}
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": "fixture-session", "toolCall": map[string]any{"toolCallId": "review-diff", "title": "Inspect the pull-request diff"}, "options": []any{map[string]string{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"}}}})
				continue
			}
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": "fixture-session", "toolCall": map[string]any{"toolCallId": "test-calc", "title": "Run focused arithmetic test", "rawInput": map[string]any{"command": "go", "args": []string{"test", "./src"}, "workingDirectory": "configured clean fixture workspace"}}, "options": []any{map[string]string{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"}, map[string]string{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"}}}})
		case "session/cancel":
			os.Exit(0)
		case "":
			if len(promptID) == 0 {
				continue
			}
			if review != "" {
				answer(review)
				continue
			}
			var result struct {
				Outcome struct {
					Outcome  string `json:"outcome"`
					OptionID string `json:"optionId"`
				} `json:"outcome"`
			}
			_ = json.Unmarshal(msg.Result, &result)
			text := "Verification was not run: permission was denied."
			if result.Outcome.OptionID == "allow-once" {
				cmd := exec.Command("go", "test", "./src")
				output, err := cmd.CombinedOutput()
				exit := 0
				if err != nil {
					exit = -1
					if e, ok := err.(*exec.ExitError); ok {
						exit = e.ExitCode()
					}
				}
				text = fmt.Sprintf("Executed `go test ./src` in the configured clean fixture workspace.\nExit status: %d\n\n```text\n%s\n```\nThis is the actual command output; no additional checks were run.", exit, output)
			}
			answer(text)
		}
	}
	os.Exit(0)
}
