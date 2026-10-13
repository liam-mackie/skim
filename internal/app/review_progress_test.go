package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"skim/internal/acp"
	"skim/internal/domain"
	"skim/internal/provider"
	"skim/internal/store"
)

// reviewProvider serves the fixture PR head and canonical Git source; the
// workspace manager maps that source to a private local bare repository.
type reviewProvider struct {
	provider.Provider
	head  string
	err   error
	heads atomic.Int32
}

func (p *reviewProvider) Head(context.Context, domain.PullRequest) (string, error) {
	p.heads.Add(1)
	return p.head, p.err
}

func (p *reviewProvider) GitSource(pr domain.PullRequest) (domain.GitSource, error) {
	return domain.GitSource{URL: "https://github.com/" + pr.Owner + "/" + pr.Repo + ".git", HeadRef: fmt.Sprintf("refs/pull/%d/head", pr.Number)}, nil
}

// reviewFixture drives the scripted ACP agent (TestReviewAgentProcess). Every
// prompt holds on a permission request; resolving it makes the agent reply with
// the reply file's contents, or exit without replying when the file is absent.
type reviewFixture struct {
	app      *App
	reply    string
	root     string
	provider *reviewProvider
}

func reviewGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	base := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "init.templateDir=", "-c", "commit.gpgsign=false", "-c", "user.name=Skim Fixture", "-c", "user.email=fixture@invalid"}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = directory
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func reviewProgressFile(path string) domain.File {
	return domain.File{Path: path, Status: "modified", Additions: 1, Deletions: 1, PatchStatus: "available", Patch: "@@ -1 +1 @@\n-old\n+new"}
}

func newReviewProgressFixture(t *testing.T, files []domain.File) (*App, domain.PRIdentity, *reviewFixture) {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewGit(t, source, "init", "--quiet")
	reviewGit(t, source, "add", "a.go")
	reviewGit(t, source, "commit", "--quiet", "-m", "base")
	if err := os.WriteFile(filepath.Join(source, "a.go"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewGit(t, source, "commit", "--quiet", "-am", "head")
	base, head := reviewGit(t, source, "rev-parse", "HEAD^"), reviewGit(t, source, "rev-parse", "HEAD")
	remote := filepath.Join(t.TempDir(), "remote.git")
	reviewGit(t, source, "clone", "--quiet", "--bare", "--no-local", source, remote)
	reviewGit(t, remote, "update-ref", "refs/pull/1/head", head)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture := &reviewFixture{reply: filepath.Join(t.TempDir(), "reply"), root: t.TempDir(), provider: &reviewProvider{head: head}}
	settings := domain.Settings{
		Providers: []domain.ModelProvider{{ID: "review", Name: "Review", Kind: "openai", BaseURL: "http://127.0.0.1/v1"}},
		Roles:     domain.Roles{Review: domain.ModelRole{Provider: "review", Model: "test-model"}},
		ACP:       domain.ACPSettings{Command: exe, Args: []string{"-test.run=^TestReviewAgentProcess$", "--", "skim-review-agent", fixture.reply}, ManagedRoot: fixture.root},
	}
	s, err := store.Open(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, URL: "https://github.com/owner/repo/pull/1", BaseSHA: base, HeadSHA: head}
	d := s.Snapshot()
	d.Active = pr.Key()
	state := domain.EmptyState()
	state.PullRequest = &pr
	state.Files = files
	state.Comments = []domain.Comment{{ID: "old-draft", Path: "a.go", StartLine: 1, EndLine: 1, Side: "RIGHT", Body: "Existing finding", Status: "suggested", HeadSHA: pr.HeadSHA}}
	state.ReviewRun = &domain.ReviewRun{HeadSHA: pr.HeadSHA, CreatedAt: "previous-run", Report: "Previous report.", Rejected: []domain.RejectedComment{}}
	d.Reviews[d.Active] = store.Record{State: state}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	a := New(s)
	t.Cleanup(func() { _ = a.Close() })
	if err := a.Workspaces.SetLocalRemoteForTesting("https://github.com/owner/repo.git", remote); err != nil {
		t.Fatal(err)
	}
	a.Provider = func(domain.Settings) provider.Provider { return fixture.provider }
	fixture.app = a
	return a, domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}, fixture
}

// await returns the running agent's permission request: the review is past
// checkout and held inside the agent turn.
func (f *reviewFixture) await(t *testing.T) acp.Permission {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if permissions := f.app.Agent.Permissions(); len(permissions) > 0 {
			return permissions[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("agent review did not reach its agent")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// answer releases the held agent, which replies with report; an empty report
// makes the agent exit without replying.
func (f *reviewFixture) answer(t *testing.T, permission acp.Permission, report string) {
	t.Helper()
	if report == "" {
		if err := os.Remove(f.reply); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(f.reply, []byte(report), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Agent.Resolve(permission.ID, "allow-once"); err != nil {
		t.Fatal(err)
	}
}

func agentReport(comments string) string {
	return "Inspected `git diff BASE HEAD` for a.go; no commands were run.\n\n```json\n" + comments + "\n```\n"
}

// TestReviewAgentProcess is the scripted ACP agent, not a test: it runs only
// when re-executed by newReviewProgressFixture.
func TestReviewAgentProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "skim-review-agent" {
		return
	}
	replyPath := os.Args[len(os.Args)-1]
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 4<<20)
	enc := json.NewEncoder(os.Stdout)
	var promptID json.RawMessage
	for scan.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scan.Bytes(), &msg) != nil {
			os.Exit(2)
		}
		reply := func(v any) { _ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": v}) }
		switch msg.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}})
		case "session/new":
			reply(map[string]string{"sessionId": "review-session"})
		case "session/prompt":
			promptID = msg.ID
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": "review-session", "toolCall": map[string]any{"toolCallId": "diff", "title": "Inspect the pull-request diff"}, "options": []any{map[string]string{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"}}}})
		case "session/cancel":
			os.Exit(0)
		case "":
			if len(promptID) == 0 {
				continue
			}
			text, err := os.ReadFile(replyPath)
			if err != nil {
				os.Exit(3)
			}
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "review-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": string(text)}}}})
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]string{"stopReason": "end_turn"}})
			promptID = nil
		}
	}
	os.Exit(0)
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

func awaitProgressReview(t *testing.T, done <-chan reviewProgressResult) reviewProgressResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(15 * time.Second):
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
		t.Fatal("progress polling waited for the running agent")
		return domain.ReviewProgressUpdate{}
	}
}

func assertProgressCounts(t *testing.T, run *domain.ReviewProgress, findings, rejected int) {
	t.Helper()
	if run == nil {
		t.Fatal("missing review progress")
	}
	if run.Findings != findings || run.Rejected != rejected {
		t.Fatalf("wrong findings/rejected counts: %+v; want %d/%d", run, findings, rejected)
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
		t.Fatalf("unsuccessful or in-progress review changed saved drafts or report: before=%+v after=%+v", before, after)
	}
}

// firstProgressFindings repeats the saved draft, adds one finding twice (once
// with surrounding whitespace), and proposes a line outside the diff.
var firstProgressFindings = agentReport(`{"comments":[{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":"Existing finding"},{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":"New finding"},{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":" New finding "},{"path":"a.go","startLine":5,"endLine":5,"side":"RIGHT","body":"Outside the diff"}]}`)

func TestReviewProgressPhasesAndAtomicDrafts(t *testing.T) {
	a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
	before := a.Store.State()
	initial := readProgress(t, a, identity)
	encoded, err := json.Marshal(initial)
	if want := fmt.Sprintf(`{"prKey":"github/owner/repo/1","baseSha":%q,"headSha":%q,"run":null}`, identity.BaseSHA, identity.HeadSHA); err != nil || string(encoded) != want {
		t.Fatalf("no-run contract changed: %s, %v", encoded, err)
	}

	done, _ := startProgressReview(t, a)
	permission := agent.await(t)
	run := readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 0, 0)
	if run.Status != "running" || run.Phase != "reviewing" || run.FinishedAt != "" || run.Error != "" {
		t.Fatalf("wrong running progress: %+v", run)
	}
	id, started := run.ID, run.StartedAt
	run.Phase, run.Findings = "caller-mutated", 99
	if fresh := readProgress(t, a, identity).Run; fresh.Phase != "reviewing" || fresh.Findings != 0 {
		t.Fatalf("caller mutation changed shared progress: %+v", fresh)
	}
	assertReviewDraftsUnchanged(t, a, before)
	agent.answer(t, permission, firstProgressFindings)
	result := awaitProgressReview(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}
	run = readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 1, 1)
	if run.Status != "complete" || run.Phase != "finished" || run.ID != id || run.StartedAt != started || run.FinishedAt == "" || run.Error != "" {
		t.Fatalf("wrong successful terminal progress: %+v", run)
	}
	encoded, err = json.Marshal(run)
	if err != nil || strings.Contains(string(encoded), `"error"`) {
		t.Fatalf("empty optional terminal fields were emitted: %s, %v", encoded, err)
	}
	saved := a.Store.State()
	if len(saved.Comments) != 2 || len(result.state.Comments) != 2 || saved.Comments[0].ID != "old-draft" {
		t.Fatalf("validated unique findings were not atomically saved: %+v", saved.Comments)
	}
	if c := saved.Comments[1]; c.Body != "New finding" || c.Status != "suggested" || c.Source != "review" || c.HeadSHA != identity.HeadSHA || c.Revision != 1 {
		t.Fatalf("finding was not saved as a current-head suggestion: %+v", c)
	}
	if r := saved.ReviewRun; r == nil || r.Report != firstProgressFindings || r.Error != "" || r.HeadSHA != identity.HeadSHA || len(r.Rejected) != 1 || r.Rejected[0].StartLine != 5 || r.Rejected[0].Body != "Outside the diff" || r.Rejected[0].Reason == "" {
		t.Fatalf("saved report or rejected comment disagrees with the agent reply: %+v", saved.ReviewRun)
	}

	// A new run is not an extension of the last run, even on identical refs.
	done, _ = startProgressReview(t, a)
	permission = agent.await(t)
	run = readProgress(t, a, identity).Run
	assertProgressCounts(t, run, 0, 0)
	if run.ID == id || run.Status != "running" || run.FinishedAt != "" || run.Error != "" {
		t.Fatalf("new run reused terminal identity or counters: %+v", run)
	}
	assertReviewDraftsUnchanged(t, a, saved)
	agent.answer(t, permission, firstProgressFindings)
	if result := awaitProgressReview(t, done); result.err != nil {
		t.Fatal(result.err)
	}
	assertProgressCounts(t, readProgress(t, a, identity).Run, 0, 1)
	if !reflect.DeepEqual(a.Store.State().Comments, saved.Comments) {
		t.Fatal("repeated review duplicated existing drafts")
	}
}

func TestReviewProgressUnreadableReportIsSavedAndFails(t *testing.T) {
	for name, report := range map[string]string{
		"no JSON block":   "Inspected a.go and found nothing worth commenting on.",
		"null comments":   agentReport(`{"comments":null}`),
		"truncated block": agentReport(`{"comments":[`),
	} {
		t.Run(name, func(t *testing.T) {
			a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
			before := a.Store.State()
			done, _ := startProgressReview(t, a)
			agent.answer(t, agent.await(t), report)
			result := awaitProgressReview(t, done)
			if result.err == nil || !strings.Contains(result.err.Error(), "report is saved") {
				t.Fatalf("unreadable report did not fail the review: %v", result.err)
			}
			run := readProgress(t, a, identity).Run
			assertProgressCounts(t, run, 0, 0)
			if run.Status != "failed" || run.Phase != "finished" || run.FinishedAt == "" || run.Error == "" {
				t.Fatalf("failure left misleading progress: %+v", run)
			}
			saved := a.Store.State()
			if !reflect.DeepEqual(saved.Comments, before.Comments) {
				t.Fatalf("unreadable report changed drafts: %+v", saved.Comments)
			}
			if r := saved.ReviewRun; r == nil || r.Report != report || r.Error == "" || r.CreatedAt == "previous-run" || len(r.Rejected) != 0 {
				t.Fatalf("agent report was not kept with its error: %+v", saved.ReviewRun)
			}
		})
	}
}

func TestReviewRedactsConfiguredSecretsInAgentOutput(t *testing.T) {
	t.Setenv("SKIM_TEST_REVIEW_REPORT_GITHUB", "github-report-secret")
	a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
	d := a.Store.Snapshot()
	d.Settings.GitHubTokenEnv = "SKIM_TEST_REVIEW_REPORT_GITHUB"
	if err := a.Store.Commit(d); err != nil {
		t.Fatal(err)
	}
	done, _ := startProgressReview(t, a)
	report := "The checkout's environment held github-report-secret.\n\n```json\n" + `{"comments":[{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":"This logs github-report-secret."}]}` + "\n```\n"
	agent.answer(t, agent.await(t), report)
	if result := awaitProgressReview(t, done); result.err != nil {
		t.Fatal(result.err)
	}
	saved := a.Store.State()
	if run := saved.ReviewRun; run == nil || strings.Contains(run.Report, "github-report-secret") || !strings.Contains(run.Report, "held [redacted].") {
		t.Fatalf("saved report exposed the configured token: %+v", saved.ReviewRun)
	}
	if len(saved.Comments) != 2 || saved.Comments[1].Body != "This logs [redacted]." {
		t.Fatalf("saved suggestion exposed the configured token: %+v", saved.Comments)
	}
	data, err := os.ReadFile(filepath.Join(a.Store.Directory(), "state.json"))
	if err != nil || strings.Contains(string(data), "github-report-secret") {
		t.Fatalf("state file exposed the configured token: %v", err)
	}
	assertProgressCounts(t, readProgress(t, a, identity).Run, 1, 0)
}

func TestReviewDoesNotRestoreSuggestionsChangedWhileAgentRan(t *testing.T) {
	a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
	d := a.Store.Snapshot()
	r := d.Reviews[d.Active]
	r.State.Comments = append(r.State.Comments, domain.Comment{ID: "second-draft", Revision: 1, Path: "a.go", StartLine: 1, EndLine: 1, Side: "RIGHT", Body: "Second finding", Status: "suggested", Source: "review", HeadSHA: identity.HeadSHA})
	d.Reviews[d.Active] = r
	if err := a.Store.Commit(d); err != nil {
		t.Fatal(err)
	}
	done, _ := startProgressReview(t, a)
	permission := agent.await(t)
	deleted, err := a.DeleteComment(context.Background(), "old-draft")
	if err != nil {
		t.Fatal(err)
	}
	body := "User wording of the second finding"
	edited, err := a.UpdateComment(context.Background(), "second-draft", CommentUpdate{Body: &body})
	if err != nil {
		t.Fatal(err)
	}
	agent.answer(t, permission, agentReport(`{"comments":[{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":"Existing finding"},{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":"Second finding"},{"path":"a.go","startLine":1,"endLine":1,"side":"RIGHT","body":"New finding"}]}`))
	if result := awaitProgressReview(t, done); result.err != nil {
		t.Fatal(result.err)
	}
	comments := a.Store.State().Comments
	if len(comments) != 3 || comments[0] != deleted || comments[1] != edited || comments[2].Body != "New finding" || comments[2].Status != "suggested" {
		t.Fatalf("review restored a deleted or edited suggestion: %+v", comments)
	}
	assertProgressCounts(t, readProgress(t, a, identity).Run, 1, 0)
}

func TestReviewProgressFailureKeepsOldDraftsAndReport(t *testing.T) {
	for _, name := range []string{"agent exits without replying", "GitHub head moved"} {
		t.Run(name, func(t *testing.T) {
			a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
			before := a.Store.State()
			if name == "GitHub head moved" {
				agent.provider.head = strings.Repeat("b", 40)
			}
			done, _ := startProgressReview(t, a)
			if name == "agent exits without replying" {
				agent.answer(t, agent.await(t), "")
			}
			if result := awaitProgressReview(t, done); result.err == nil {
				t.Fatal("failed review unexpectedly succeeded")
			}
			run := readProgress(t, a, identity).Run
			assertProgressCounts(t, run, 0, 0)
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
			a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
			before := a.Store.State()
			done, cancel := startProgressReview(t, a)
			agent.await(t)
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
				case <-time.After(15 * time.Second):
					t.Fatal("application shutdown waited on the blocked review")
				}
			}
			run := readProgress(t, a, identity).Run
			assertProgressCounts(t, run, 0, 0)
			if run.Status != "cancelled" || run.Phase != "finished" || run.FinishedAt == "" || run.Error == "" {
				t.Fatalf("cancellation left misleading progress: %+v", run)
			}
			assertReviewDraftsUnchanged(t, a, before)
		})
	}
}

func TestReviewProgressSaveFailureNeverReportsSuccess(t *testing.T) {
	a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
	before := a.Store.State()
	done, _ := startProgressReview(t, a)
	permission := agent.await(t)
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
	agent.answer(t, permission, firstProgressFindings)
	if result := awaitProgressReview(t, done); result.err == nil {
		t.Fatal("review succeeded despite an unavailable store directory")
	}
	run := readProgress(t, a, identity).Run
	if run == nil || run.Status != "failed" || run.Phase != "finished" || run.FinishedAt == "" || !strings.Contains(run.Error, "create state file") {
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
		t.Fatal("failed save modified drafts or review report on disk")
	}
}

func TestReviewProgressErrorsRedactEveryConfiguredCredential(t *testing.T) {
	t.Setenv("SKIM_TEST_REVIEW_PROGRESS_GITHUB", "github-progress-secret")
	t.Setenv("SKIM_TEST_REVIEW_PROGRESS_ENV", "environment-progress-secret")
	a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
	d := a.Store.Snapshot()
	settings := d.Settings
	settings.GitHubTokenEnv = "SKIM_TEST_REVIEW_PROGRESS_GITHUB"
	settings.Providers[0].APIKeyEnv = "SKIM_TEST_REVIEW_PROGRESS_ENV"
	settings.Providers = append(settings.Providers, domain.ModelProvider{ID: "unused", Name: "Unused", BaseURL: "http://127.0.0.1/unused"})
	if err := d.ApplySettings(domain.SettingsUpdate{Settings: settings, ProviderKeys: []domain.ProviderKeyUpdate{{ProviderID: "unused", APIKey: "saved-progress-secret"}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Commit(d); err != nil {
		t.Fatal(err)
	}
	agent.provider.err = errors.New("github-progress-secret environment-progress-secret saved-progress-secret")
	done, _ := startProgressReview(t, a)
	if result := awaitProgressReview(t, done); result.err == nil {
		t.Fatal("provider failure unexpectedly succeeded")
	}
	run := readProgress(t, a, identity).Run
	if run == nil || run.Status != "failed" || strings.Contains(run.Error, "progress-secret") || strings.Count(run.Error, "[redacted]") != 3 {
		t.Fatalf("progress exposed a configured credential: %+v", run)
	}
}

func TestReviewRequiresConfiguredAgentBeforeCheckout(t *testing.T) {
	a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
	d := a.Store.Snapshot()
	d.Settings.ACP.Command = " "
	if err := a.Store.Commit(d); err != nil {
		t.Fatal(err)
	}
	before := a.Store.State()
	if _, err := a.Review(context.Background()); err == nil || !strings.Contains(err.Error(), "configure an ACP agent") {
		t.Fatalf("review without an agent: %v", err)
	}
	if update := readProgress(t, a, identity); update.Run != nil {
		t.Fatalf("rejected review published progress: %+v", update.Run)
	}
	entries, err := os.ReadDir(agent.root)
	if err != nil || len(entries) != 0 || agent.provider.heads.Load() != 0 {
		t.Fatalf("review contacted GitHub or prepared a checkout without an agent: %v %v", entries, err)
	}
	assertReviewDraftsUnchanged(t, a, before)
}

func TestReviewProgressRequiresCurrentExactIdentity(t *testing.T) {
	for _, changed := range []string{"key", "base", "head"} {
		t.Run(changed, func(t *testing.T) {
			a, identity, agent := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
			for _, invalid := range []domain.PRIdentity{{}, {PRKey: identity.PRKey, BaseSHA: identity.BaseSHA}, {PRKey: identity.PRKey, HeadSHA: identity.HeadSHA}, {BaseSHA: identity.BaseSHA, HeadSHA: identity.HeadSHA}} {
				if update, err := a.ReviewProgress(invalid); err == nil || update.Run != nil {
					t.Fatalf("incomplete identity received progress: %+v, %v", update, err)
				}
			}
			done, _ := startProgressReview(t, a)
			agent.answer(t, agent.await(t), agentReport(`{"comments":[]}`))
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
