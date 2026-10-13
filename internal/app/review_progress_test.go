package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"skim/internal/domain"
	"skim/internal/store"
)

type reviewProgressResponse struct {
	status  int
	content string
}

type reviewProgressCall struct {
	path  string
	reply chan reviewProgressResponse
}

type reviewProgressTransport struct {
	calls chan reviewProgressCall
}

// The transport deliberately blocks each real model request until the test
// supplies its result. No provider, network server, or timing estimate is used.
func (p *reviewProgressTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var input struct {
		Messages []domain.Message `json:"messages"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		return nil, err
	}
	if len(input.Messages) != 2 {
		return nil, errors.New("unexpected review prompt")
	}
	var payload struct {
		File domain.File `json:"file"`
	}
	if err := json.Unmarshal([]byte(input.Messages[1].Content), &payload); err != nil {
		return nil, err
	}
	call := reviewProgressCall{path: payload.File.Path, reply: make(chan reviewProgressResponse, 1)}
	select {
	case p.calls <- call:
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	select {
	case response := <-call.reply:
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		body := response.content
		if status == http.StatusOK {
			encoded, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": body}}}})
			if err != nil {
				return nil, err
			}
			body = string(encoded)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
}

func reviewProgressFile(path string) domain.File {
	return domain.File{Path: path, Status: "modified", Additions: 1, Deletions: 1, PatchStatus: "available", Patch: "@@ -1 +1 @@\n-old\n+new"}
}

func newReviewProgressFixture(t *testing.T, files []domain.File) (*App, domain.PRIdentity, *reviewProgressTransport) {
	t.Helper()
	settings := domain.Settings{
		Providers: []domain.ModelProvider{{ID: "review", Name: "Review", Kind: "openai", BaseURL: "http://127.0.0.1/v1"}},
		Roles:     domain.Roles{Review: domain.ModelRole{Provider: "review", Model: "test-model"}},
	}
	s, err := store.Open(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, BaseSHA: "base", HeadSHA: "head"}
	d := s.Snapshot()
	d.Active = pr.Key()
	state := domain.EmptyState()
	state.PullRequest = &pr
	state.Files = files
	state.Comments = []domain.Comment{{ID: "old-draft", Path: "a.go", StartLine: 1, EndLine: 1, Side: "RIGHT", Body: "Existing finding", Status: "suggested", HeadSHA: pr.HeadSHA}}
	state.ReviewRun = &domain.ReviewRun{HeadSHA: pr.HeadSHA, CreatedAt: "previous-run", ReviewedPaths: []string{"previous.go"}, SkippedFiles: []domain.SkippedFile{}}
	d.Reviews[d.Active] = store.Record{State: state}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	a := New(s)
	t.Cleanup(func() { _ = a.Close() })
	transport := &reviewProgressTransport{calls: make(chan reviewProgressCall, 1)}
	a.Models.HTTP = &http.Client{Transport: transport}
	return a, domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}, transport
}

type reviewProgressResult struct {
	state domain.State
	err   error
}

func startProgressReview(t *testing.T, a *App) (<-chan reviewProgressResult, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan reviewProgressResult, 1)
	go func() {
		state, err := a.Review(ctx)
		done <- reviewProgressResult{state: state, err: err}
	}()
	return done, cancel
}

func nextProgressCall(t *testing.T, transport *reviewProgressTransport, path string) reviewProgressCall {
	t.Helper()
	select {
	case call := <-transport.calls:
		if call.path != path {
			t.Fatalf("review called model for %q, want %q", call.path, path)
		}
		return call
	case <-time.After(5 * time.Second):
		t.Fatalf("model did not start reviewing %s", path)
		return reviewProgressCall{}
	}
}

func awaitProgressReview(t *testing.T, done <-chan reviewProgressResult) reviewProgressResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("review did not finish")
		return reviewProgressResult{}
	}
}

func readProgress(t *testing.T, a *App, identity domain.PRIdentity) domain.ReviewProgressUpdate {
	t.Helper()
	type response struct {
		update domain.ReviewProgressUpdate
		err    error
	}
	done := make(chan response, 1)
	go func() {
		update, err := a.ReviewProgress(identity)
		done <- response{update: update, err: err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.update.PRIdentity != identity {
			t.Fatalf("wrong progress identity: %+v", result.update.PRIdentity)
		}
		return result.update
	case <-time.After(5 * time.Second):
		t.Fatal("progress polling waited for the foreground model call")
		return domain.ReviewProgressUpdate{}
	}
}

func assertProgressCounts(t *testing.T, run *domain.ReviewProgress, completed, total, reviewed, skipped, findings int) {
	t.Helper()
	if run == nil {
		t.Fatal("missing review progress")
	}
	if run.Completed != completed || run.Total != total || run.Reviewed != reviewed || run.Skipped != skipped || run.Findings != findings {
		t.Fatalf("wrong processed/total/reviewed/skipped/findings counts: %+v; want %d/%d/%d/%d/%d", run, completed, total, reviewed, skipped, findings)
	}
	if run.ID == "" || run.StartedAt == "" || run.UpdatedAt == "" {
		t.Fatalf("missing run identity or timestamps: %+v", run)
	}
	for _, timestamp := range []string{run.StartedAt, run.UpdatedAt, run.FinishedAt} {
		if timestamp != "" {
			if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
				t.Fatalf("invalid progress timestamp %q: %v", timestamp, err)
			}
		}
	}
}

func assertReviewDraftsUnchanged(t *testing.T, a *App, before domain.State) {
	t.Helper()
	after := a.Store.State()
	if !reflect.DeepEqual(after.Comments, before.Comments) || !reflect.DeepEqual(after.ReviewRun, before.ReviewRun) {
		t.Fatalf("unsuccessful or in-progress review changed saved drafts or coverage: before=%+v after=%+v", before, after)
	}
}

const firstProgressFindings = `{"comments":[{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":"Existing finding"},{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":"New finding"},{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":" New finding "}]}`

func TestReviewProgressTracksBlockedFilesAndAtomicDrafts(t *testing.T) {
	files := []domain.File{reviewProgressFile("a.go"), reviewProgressFile("b.go"), {Path: "image.png", PatchStatus: "binary"}, reviewProgressFile("c.go")}
	a, identity, transport := newReviewProgressFixture(t, files)
	before := a.Store.State()
	initial := readProgress(t, a, identity)
	encoded, err := json.Marshal(initial)
	if err != nil || string(encoded) != `{"prKey":"github/owner/repo/1","baseSha":"base","headSha":"head","run":null}` {
		t.Fatalf("no-run contract changed: %s, %v", encoded, err)
	}

	done, _ := startProgressReview(t, a)
	first := nextProgressCall(t, transport, "a.go")
	run := readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 0, 4, 0, 0, 0)
	if run.Status != "running" || run.Phase != "reviewing" || run.CurrentPath != "a.go" || run.FinishedAt != "" || run.Error != "" {
		t.Fatalf("wrong initial running progress: %+v", run)
	}
	id, started := run.ID, run.StartedAt
	run.CurrentPath, run.Completed = "caller-mutated", 99
	if fresh := readProgress(t, a, identity).Run; fresh.CurrentPath != "a.go" || fresh.Completed != 0 {
		t.Fatalf("caller mutation changed shared progress: %+v", fresh)
	}
	assertReviewDraftsUnchanged(t, a, before)
	first.reply <- reviewProgressResponse{content: firstProgressFindings}

	second := nextProgressCall(t, transport, "b.go")
	run = readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 1, 4, 1, 0, 1)
	if run.CurrentPath != "b.go" || run.ID != id || run.StartedAt != started {
		t.Fatalf("second model call lost first file progress: %+v", run)
	}
	assertReviewDraftsUnchanged(t, a, before)
	second.reply <- reviewProgressResponse{content: `{"comments":[]}`}

	third := nextProgressCall(t, transport, "c.go")
	run = readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 3, 4, 2, 1, 1)
	if run.CurrentPath != "c.go" || run.Status != "running" {
		t.Fatalf("skip was treated as a model review: %+v", run)
	}
	assertReviewDraftsUnchanged(t, a, before)
	third.reply <- reviewProgressResponse{content: `{"comments":[{"path":"c.go","startLine":1,"endLine":1,"side":"RIGHT","body":"Another finding"}]}`}
	result := awaitProgressReview(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}
	run = readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 4, 4, 3, 1, 2)
	if run.Status != "complete" || run.Phase != "finished" || run.ID != id || run.FinishedAt == "" || run.CurrentPath != "" || run.Error != "" {
		t.Fatalf("wrong successful terminal progress: %+v", run)
	}
	encoded, err = json.Marshal(run)
	if err != nil || strings.Contains(string(encoded), `"currentPath"`) || strings.Contains(string(encoded), `"error"`) {
		t.Fatalf("empty optional terminal fields were emitted: %s, %v", encoded, err)
	}
	saved := a.Store.State()
	if len(saved.Comments) != 3 || len(result.state.Comments) != 3 || saved.Comments[0].ID != "old-draft" {
		t.Fatalf("validated unique findings were not atomically saved: %+v", saved.Comments)
	}
	if !reflect.DeepEqual(saved.ReviewRun.ReviewedPaths, []string{"a.go", "b.go", "c.go"}) || len(saved.ReviewRun.SkippedFiles) != 1 || saved.ReviewRun.SkippedFiles[0].Path != "image.png" {
		t.Fatalf("saved coverage disagrees with progress: %+v", saved.ReviewRun)
	}

	// A new run is not an extension of the last run, even on identical refs.
	done, _ = startProgressReview(t, a)
	first = nextProgressCall(t, transport, "a.go")
	run = readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 0, 4, 0, 0, 0)
	if run.ID == id || run.Status != "running" || run.FinishedAt != "" || run.Error != "" {
		t.Fatalf("new run reused terminal identity or counters: %+v", run)
	}
	assertReviewDraftsUnchanged(t, a, saved)
	first.reply <- reviewProgressResponse{content: firstProgressFindings}
	second = nextProgressCall(t, transport, "b.go")
	assertProgressCounts(t, readProgress(t, a, identity).Run, 1, 4, 1, 0, 0)
	second.reply <- reviewProgressResponse{content: `{"comments":[]}`}
	third = nextProgressCall(t, transport, "c.go")
	third.reply <- reviewProgressResponse{content: `{"comments":[]}`}
	if result := awaitProgressReview(t, done); result.err != nil {
		t.Fatal(result.err)
	}
	assertProgressCounts(t, readProgress(t, a, identity).Run, 4, 4, 3, 1, 0)
	if !reflect.DeepEqual(a.Store.State().Comments, saved.Comments) {
		t.Fatal("repeated review duplicated existing drafts")
	}
}

func TestReviewProgressFailureKeepsOldDraftsAndCoverage(t *testing.T) {
	cases := []struct {
		name     string
		response reviewProgressResponse
	}{
		{name: "invalid JSON", response: reviewProgressResponse{content: `{"comments":null}`}},
		{name: "wrong file", response: reviewProgressResponse{content: `{"comments":[{"path":"other.go","startLine":1,"endLine":1,"side":"RIGHT","body":"Finding"}]}`}},
		{name: "invalid anchor", response: reviewProgressResponse{content: `{"comments":[{"path":"b.go","startLine":99,"endLine":99,"side":"RIGHT","body":"Finding"}]}`}},
		{name: "empty body", response: reviewProgressResponse{content: `{"comments":[{"path":"b.go","startLine":1,"endLine":1,"side":"RIGHT","body":" "}]}`}},
		{name: "provider failure", response: reviewProgressResponse{status: http.StatusBadGateway, content: "upstream unavailable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, identity, transport := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go"), reviewProgressFile("b.go")})
			before := a.Store.State()
			done, _ := startProgressReview(t, a)
			first := nextProgressCall(t, transport, "a.go")
			first.reply <- reviewProgressResponse{content: firstProgressFindings}
			second := nextProgressCall(t, transport, "b.go")
			assertProgressCounts(t, readProgress(t, a, identity).Run, 1, 2, 1, 0, 1)
			second.reply <- tc.response
			if result := awaitProgressReview(t, done); result.err == nil {
				t.Fatal("invalid response unexpectedly saved a review")
			}
			run := readProgress(t, a, identity).Run
			assertProgressCounts(t, run, 1, 2, 1, 0, 1)
			if run.Status != "failed" || run.Phase != "finished" || run.FinishedAt == "" || run.Error == "" {
				t.Fatalf("failure left misleading progress: %+v", run)
			}
			assertReviewDraftsUnchanged(t, a, before)
		})
	}
}

func TestReviewProgressCancellationAndShutdownKeepOldDrafts(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "request cancellation"
		if shutdown {
			name = "application shutdown"
		}
		t.Run(name, func(t *testing.T) {
			a, identity, transport := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go"), reviewProgressFile("b.go")})
			before := a.Store.State()
			done, cancel := startProgressReview(t, a)
			first := nextProgressCall(t, transport, "a.go")
			first.reply <- reviewProgressResponse{content: firstProgressFindings}
			_ = nextProgressCall(t, transport, "b.go")
			closed := make(chan error, 1)
			if shutdown {
				go func() { closed <- a.Close() }()
			} else {
				cancel()
			}
			if result := awaitProgressReview(t, done); result.err == nil {
				t.Fatal("cancelled review unexpectedly succeeded")
			}
			if shutdown {
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("application shutdown waited on the blocked review")
				}
			}
			run := readProgress(t, a, identity).Run
			assertProgressCounts(t, run, 1, 2, 1, 0, 1)
			if run.Status != "cancelled" || run.Phase != "finished" || run.FinishedAt == "" || run.Error == "" {
				t.Fatalf("cancellation left misleading progress: %+v", run)
			}
			assertReviewDraftsUnchanged(t, a, before)
		})
	}
}

func TestReviewProgressSaveFailureNeverReportsSuccess(t *testing.T) {
	a, identity, transport := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
	before := a.Store.State()
	done, _ := startProgressReview(t, a)
	call := nextProgressCall(t, transport, "a.go")
	original := a.Store.Directory()
	moved := filepath.Join(t.TempDir(), "moved-store")
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(moved, original); err != nil {
			t.Error(err)
		}
	})
	call.reply <- reviewProgressResponse{content: firstProgressFindings}
	if result := awaitProgressReview(t, done); result.err == nil {
		t.Fatal("review succeeded despite an unavailable store directory")
	}
	run := readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 1, 1, 1, 0, 1)
	if run.Status != "failed" || run.Phase != "finished" || run.FinishedAt == "" || !strings.Contains(run.Error, "create state file") {
		t.Fatalf("save failure reported success or incomplete progress: %+v", run)
	}
	assertReviewDraftsUnchanged(t, a, before)
	data, err := os.ReadFile(filepath.Join(moved, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved store.Data
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Reviews[saved.Active].State.Comments, before.Comments) || !reflect.DeepEqual(saved.Reviews[saved.Active].State.ReviewRun, before.ReviewRun) {
		t.Fatal("failed save modified drafts or review coverage on disk")
	}
}

func TestReviewProgressErrorsRedactEveryConfiguredCredential(t *testing.T) {
	t.Setenv("SKIM_TEST_REVIEW_PROGRESS_GITHUB", "github-progress-secret")
	t.Setenv("SKIM_TEST_REVIEW_PROGRESS_ENV", "environment-progress-secret")
	a, identity, transport := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
	d := a.Store.Snapshot()
	settings := d.Settings
	settings.GitHubTokenEnv = "SKIM_TEST_REVIEW_PROGRESS_GITHUB"
	settings.Providers[0].APIKeyEnv = "SKIM_TEST_REVIEW_PROGRESS_ENV"
	settings.Providers = append(settings.Providers, domain.ModelProvider{ID: "unused", Name: "Unused", BaseURL: "http://127.0.0.1/unused"})
	if err := d.ApplySettings(settings, []domain.ProviderKeyUpdate{{ProviderID: "unused", APIKey: "saved-progress-secret"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Commit(d); err != nil {
		t.Fatal(err)
	}
	done, _ := startProgressReview(t, a)
	call := nextProgressCall(t, transport, "a.go")
	call.reply <- reviewProgressResponse{status: http.StatusUnauthorized, content: "github-progress-secret environment-progress-secret saved-progress-secret"}
	if result := awaitProgressReview(t, done); result.err == nil {
		t.Fatal("provider failure unexpectedly succeeded")
	}
	run := readProgress(t, a, identity).Run
	if run == nil || run.Status != "failed" || strings.Contains(run.Error, "progress-secret") || strings.Count(run.Error, "[redacted]") != 3 {
		t.Fatalf("progress exposed a configured credential: %+v", run)
	}
}

func TestReviewProgressSkipsUnavailableFilesWithoutModelCalls(t *testing.T) {
	a, identity, transport := newReviewProgressFixture(t, []domain.File{{Path: "image.png", PatchStatus: "binary"}, {Path: "large.go", PatchStatus: "truncated"}})
	done, _ := startProgressReview(t, a)
	result := awaitProgressReview(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}
	select {
	case call := <-transport.calls:
		t.Fatalf("unavailable file was sent to model: %s", call.path)
	default:
	}
	run := readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 2, 2, 0, 2, 0)
	if run.Status != "complete" || len(result.state.ReviewRun.ReviewedPaths) != 0 || len(result.state.ReviewRun.SkippedFiles) != 2 {
		t.Fatalf("all-skipped review invented model coverage: %+v", run)
	}
}

func TestReviewProgressRequiresCurrentExactIdentity(t *testing.T) {
	for _, changed := range []string{"key", "base", "head"} {
		t.Run(changed, func(t *testing.T) {
			a, identity, _ := newReviewProgressFixture(t, []domain.File{{Path: "image.png", PatchStatus: "binary"}})
			for _, invalid := range []domain.PRIdentity{{}, {PRKey: identity.PRKey, BaseSHA: identity.BaseSHA}, {PRKey: identity.PRKey, HeadSHA: identity.HeadSHA}, {BaseSHA: identity.BaseSHA, HeadSHA: identity.HeadSHA}} {
				if update, err := a.ReviewProgress(invalid); err == nil || update.Run != nil {
					t.Fatalf("incomplete identity received progress: %+v, %v", update, err)
				}
			}
			done, _ := startProgressReview(t, a)
			if result := awaitProgressReview(t, done); result.err != nil {
				t.Fatal(result.err)
			}
			if run := readProgress(t, a, identity).Run; run == nil || run.Status != "complete" {
				t.Fatalf("missing original snapshot progress: %+v", run)
			}
			d := a.Store.Snapshot()
			record := d.Reviews[d.Active]
			pr := record.State.PullRequest
			switch changed {
			case "key":
				pr.Number++
			case "base":
				pr.BaseSHA = "new-base"
			case "head":
				pr.HeadSHA = "new-head"
			}
			d.Active = pr.Key()
			d.Reviews[d.Active] = record
			if err := a.Store.Commit(d); err != nil {
				t.Fatal(err)
			}
			if update, err := a.ReviewProgress(identity); err == nil || update.Run != nil {
				t.Fatalf("stale identity received progress: %+v, %v", update, err)
			}
			current := domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}
			if update := readProgress(t, a, current); update.Run != nil {
				t.Fatalf("old run leaked onto the changed snapshot: %+v", update)
			}
		})
	}
}
