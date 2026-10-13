package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skim/internal/app"
	"skim/internal/domain"
	"skim/internal/store"
)

func fixtureGit(t *testing.T, directory string, args ...string) string {
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

type verificationResponse struct {
	state  domain.State
	job    domain.VerificationJob
	status int
	err    error
}

func startVerification(t *testing.T, h *harness, ctx context.Context) <-chan verificationResponse {
	return startVerificationInThread(t, h, ctx, "")
}

func verificationRequest(h *harness, threadID string) app.Verification {
	pr := h.app.State().PullRequest
	return app.Verification{PRIdentity: domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}, Path: "src/calc.go", StartLine: 2, EndLine: 2, Side: "RIGHT", Question: "Check this result.", ThreadID: threadID}
}

func startVerificationInThread(t *testing.T, h *harness, ctx context.Context, threadID string) <-chan verificationResponse {
	t.Helper()
	payload, err := json.Marshal(verificationRequest(h, threadID))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/api/verify", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	done := make(chan verificationResponse, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- verificationResponse{err: err}
			return
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		result := verificationResponse{status: res.StatusCode, err: err}
		if err == nil && res.StatusCode != http.StatusAccepted {
			result.err = fmt.Errorf("verify HTTP %d: %s", res.StatusCode, body)
		}
		var accepted domain.VerificationAccepted
		if result.err == nil {
			result.err = json.Unmarshal(body, &accepted)
		}
		if result.err == nil {
			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				update, err := h.app.Verifications(domain.PRIdentity{})
				if err != nil {
					result.err = err
					break
				}
				for _, job := range update.Jobs {
					if job.ID == accepted.Job.ID && !job.Active() {
						result.job, result.state = job, h.app.State()
						done <- result
						return
					}
				}
				select {
				case <-tick.C:
				case <-deadline.C:
					result.err = fmt.Errorf("verification %s did not complete", accepted.Job.ID)
					done <- result
					return
				}
			}
		}
		done <- result
	}()
	return done
}

// awaitPermission returns a verification's permission request, never a held
// agent review's (reviews have no thread).
func awaitPermission(t *testing.T, h *harness, done <-chan verificationResponse) string {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case result := <-done:
			t.Fatalf("verification ended before permission: %v", result.err)
		case <-deadline.C:
			t.Fatal("ACP permission did not arrive")
		case <-tick.C:
			for _, permission := range h.app.Agent.Permissions() {
				if permission.ThreadID != "" {
					return permission.ID
				}
			}
		}
	}
}

// awaitReviewPermission returns the permission request of an agent review held
// by harness.agentReview(..., true), allowing time for its checkout.
func awaitReviewPermission(t *testing.T, h *harness) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, permission := range h.app.Agent.Permissions() {
			if permission.ThreadID == "" {
				return permission.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("agent review did not reach its agent")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func verifyRound(t *testing.T, h *harness) domain.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	threadID := ""
	for _, thread := range h.app.State().Chats {
		if thread.Kind == "verification" && !thread.Stale {
			threadID = thread.ID
		}
	}
	done := startVerificationInThread(t, h, ctx, threadID)
	id := awaitPermission(t, h, done)
	// The helper truthfully reports that execution was declined; the full HTTP
	// workflow separately approves its real focused Go command.
	h.request(t, http.MethodPost, "/api/permissions/"+id, map[string]string{"optionId": "reject-once"}, nil)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.state.Workspace.Status != "ready" || result.state.Workspace.Path == "" {
		t.Fatalf("workspace not ready: %+v", result.state.Workspace)
	}
	return result.state
}

func finishRequest(state domain.State, discard bool) app.FinishReview {
	pr := state.PullRequest
	request := app.FinishReview{PRIdentity: domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}, DiscardChanges: discard}
	if discard {
		request.WorkspaceID = state.Workspace.ID
	}
	return request
}

func requireRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists at %s: %v", path, err)
	}
}

func TestHTTPManagedWorkspaceLifecycle(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.request(t, "POST", "/api/chat", map[string]string{"message": "Explain this diff."}, &domain.ChatThread{})
	if state = h.app.State(); state.Workspace.Status != "not_created" || state.Workspace.Path != "" {
		t.Fatalf("model-only chat created a checkout: %+v", state.Workspace)
	}
	state = verifyRound(t, h)
	path := state.Workspace.Path
	if path == h.source || fixtureGit(t, path, "rev-parse", "HEAD") != state.PullRequest.HeadSHA {
		t.Fatal("verification did not use isolated exact PR head")
	}
	if fixtureGit(t, path, "cat-file", "-t", state.PullRequest.BaseSHA) != "commit" {
		t.Fatal("base commit missing for LEFT context")
	}
	state = verifyRound(t, h)
	if state.Workspace.Path != path {
		t.Fatal("clean matching worktree was not reused")
	}
	// The agent review runs in its own checkout, separate from verification's.
	h.request(t, "POST", "/api/review", struct{}{}, &state)
	review := h.app.Workspaces.TaskStatus(state.PullRequest.Key(), "review", h.app.Settings().ACP.ManagedRoot)
	if review.Status != "ready" || review.Path == "" || review.Path == path || fixtureGit(t, review.Path, "rev-parse", "HEAD") != state.PullRequest.HeadSHA {
		t.Fatalf("agent review did not use its own exact-head checkout: %+v", review)
	}
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/8"}, &state)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("switching PR removed unfinished worktree: %v", err)
	}
	var scoped domain.WorkspaceUpdate
	h.request(t, "GET", "/api/workspace", nil, &scoped)
	if !scoped.PRIdentity.Matches(state.PullRequest) || scoped.PRKey != "github/acme/widgets/8" || scoped.Workspace.Path != "" {
		t.Fatalf("workspace poll leaked another PR's same-SHA checkout: %+v", scoped)
	}
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	comments, chats := len(state.Comments), len(state.Chats)
	h.request(t, "POST", "/api/review/finish", finishRequest(state, false), &state)
	if state.Workspace.Status != "finished" || state.Workspace.FinishedAt == "" || len(state.Comments) != comments || len(state.Chats) != chats || state.ReviewRun == nil {
		t.Fatalf("finish lost review history or status: %+v", state)
	}
	requireRemoved(t, path)
	requireRemoved(t, review.Path)
	state = verifyRound(t, h)
	if state.Workspace.Path == path || state.Workspace.FinishedAt != "" || len(state.Comments) != comments {
		t.Fatal("verification did not reopen completed review")
	}
}

func TestHTTPPublishFinishesManagedWorkspace(t *testing.T) {
	for _, event := range []string{"COMMENT", "APPROVE", "REQUEST_CHANGES"} {
		t.Run(event, func(t *testing.T) {
			h := newHarness(t)
			h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
			state := verifyRound(t, h)
			path := state.Workspace.Path
			h.request(t, "POST", "/api/publish", map[string]string{"event": event, "body": "Review complete."}, &state)
			if state.Workspace.Status != "finished" {
				t.Fatalf("published review did not finish: %+v", state.Workspace)
			}
			requireRemoved(t, path)
		})
	}
}

func TestHTTPDirtyCleanupDoesNotRepublishOrLoseDrafts(t *testing.T) {
	h := newHarness(t)
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
	state := verifyRound(t, h)
	path := state.Workspace.Path
	changed := filepath.Join(path, "notes-from-agent.txt")
	if err := os.WriteFile(changed, []byte("preserve these changes"), 0600); err != nil {
		t.Fatal(err)
	}
	id := state.Comments[0].ID
	h.request(t, "PATCH", "/api/comments/"+id, map[string]string{"status": "pending"}, nil)
	h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &state)
	if state.Comments[0].Status != "published" || state.Workspace.Status != "cleanup_failed" || state.Workspace.Error == "" {
		t.Fatalf("cleanup obscured publication success: %+v", state)
	}
	if data, err := os.ReadFile(changed); err != nil || string(data) != "preserve these changes" {
		t.Fatal("dirty worktree was removed")
	}
	h.request(t, "POST", "/api/review/finish", finishRequest(state, false), &state)
	if state.Workspace.Status != "cleanup_failed" {
		t.Fatal("unconfirmed dirty cleanup was accepted")
	}
	h.request(t, "POST", "/api/review/finish", finishRequest(state, true), &state)
	if state.Workspace.Status != "finished" || state.Comments[0].ID != id {
		t.Fatal("confirmed cleanup failed or discarded comments")
	}
	requireRemoved(t, path)
	h.fixture.mu.Lock()
	posts := h.fixture.posts
	h.fixture.mu.Unlock()
	if posts != 1 {
		t.Fatalf("cleanup resubmitted review %d times", posts)
	}
}

func TestHTTPPublicationFailureRetainsManagedWorkspace(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint("ambiguous=", ambiguous), func(t *testing.T) {
			h := newHarness(t)
			h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
			state := verifyRound(t, h)
			path := state.Workspace.Path
			h.fixture.mu.Lock()
			h.fixture.ambiguous = ambiguous
			h.fixture.rejected = !ambiguous
			h.fixture.mu.Unlock()
			var failure map[string]string
			if h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT", "body": "Review complete."}, &failure) != 400 {
				t.Fatal("failure was not surfaced")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("failed publication removed workspace")
			}
			if ambiguous {
				if h.request(t, "POST", "/api/review/finish", finishRequest(state, false), &failure) != 400 {
					t.Fatal("uncertain review was finished")
				}
				h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &state)
				if state.Workspace.Status != "finished" {
					t.Fatal("reconciliation did not finish workspace")
				}
				requireRemoved(t, path)
				h.fixture.mu.Lock()
				posts := h.fixture.posts
				h.fixture.mu.Unlock()
				if posts != 1 {
					t.Fatal("reconciliation resent review")
				}
			}
		})
	}
}

func TestHTTPActiveVerificationBlocksFinishAndCancellationRetainsHistory(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startVerification(t, h, ctx)
	awaitPermission(t, h, done)
	var update domain.WorkspaceUpdate
	h.request(t, "GET", "/api/workspace", nil, &update)
	workspace := update.Workspace
	if !update.PRIdentity.Matches(state.PullRequest) || workspace.Status != "in_use" || workspace.Path == "" {
		t.Fatalf("no scoped active workspace status: %+v", update)
	}
	var failure map[string]string
	if h.request(t, "POST", "/api/review/finish", finishRequest(state, true), &failure) != http.StatusConflict {
		t.Fatal("active verification cleanup not blocked")
	}
	if _, err := os.Stat(workspace.Path); err != nil {
		t.Fatal("active checkout removed")
	}
	cancel() // A disconnected HTTP client must not cancel server-owned work.
	job := h.app.State().Chats[0].Verification
	var cancelled domain.VerificationJob
	h.request(t, "POST", "/api/verifications/"+job.ID+"/cancel", domain.PRIdentity{PRKey: job.PRKey, BaseSHA: job.BaseSHA, HeadSHA: job.HeadSHA}, &cancelled)
	if cancelled.Status != "cancelling" {
		t.Fatalf("cancellation did not retain draining state: %+v", cancelled)
	}
	<-done
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.app.Workspace().Workspace.Status != "in_use" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.request(t, "GET", "/api/state", nil, &state)
	if len(state.Comments) != 0 || len(state.Chats) != 1 || len(state.Chats[0].Messages) != 1 || state.Chats[0].Verification.Status != "cancelled" {
		t.Fatal("cancelled verification lost the admitted question or fabricated an outcome")
	}
	// The handler may still be persisting the lease release behind the foreground gate;
	// busy (409) is the documented transient answer, anything else is final.
	body, err := json.Marshal(finishRequest(state, false))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		res, err := http.Post(h.base+"/api/review/finish", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusConflict {
			if res.StatusCode != http.StatusOK {
				t.Fatalf("finish after cancellation: HTTP %d", res.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("finish stayed busy after the cancelled verification returned")
		}
	}
	h.request(t, "GET", "/api/state", nil, &state)
	if state.Workspace.Status != "finished" {
		t.Fatalf("cleanup after shutdown failed: %+v", state.Workspace)
	}
	requireRemoved(t, workspace.Path)
}

func TestHTTPCleanupIntentSurvivesRestartAndRootChange(t *testing.T) {
	h := newHarness(t)
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
	state := verifyRound(t, h)
	path := state.Workspace.Path
	settings := state.Settings
	settings.ACP.ManagedRoot = t.TempDir()
	h.request(t, "PUT", "/api/settings", settings, nil)
	// Emulate process death after the atomic publication/completion commit but
	// before Git cleanup. No external POST or fabricated provider is involved.
	d := h.store.Snapshot()
	r := d.Reviews[d.Active]
	r.State.Workspace = domain.Workspace{Status: "cleanup_pending", FinishedAt: domain.Now()}
	d.Reviews[d.Active] = r
	if err := h.store.Commit(d); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(h.dataDir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := app.New(s)
	defer a.Close()
	if got := a.State(); got.Workspace.Status != "finished" || len(got.Comments) != len(state.Comments) {
		t.Fatalf("restart lost completion/history: %+v", got)
	}
	requireRemoved(t, path)
}

func TestHTTPStaleDiscardDoesNotFinishReplacementReview(t *testing.T) {
	h := newHarness(t)
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
	state := verifyRound(t, h)
	stale := finishRequest(state, true)
	h.request(t, "POST", "/api/review/finish", finishRequest(state, false), &state)
	state = verifyRound(t, h)
	replacement := state.Workspace
	if replacement.ID == stale.WorkspaceID {
		t.Fatal("reopened workspace reused its old confirmation identity")
	}
	var failure map[string]string
	if h.request(t, "POST", "/api/review/finish", stale, &failure) != http.StatusBadRequest {
		t.Fatal("stale discard confirmation was accepted")
	}
	h.request(t, "GET", "/api/state", nil, &state)
	if state.Workspace.ID != replacement.ID || state.Workspace.FinishedAt != "" || state.Workspace.Status != "ready" {
		t.Fatalf("rejected discard finished replacement review: %+v", state.Workspace)
	}
	if _, err := os.Stat(replacement.Path); err != nil {
		t.Fatal("stale discard removed replacement checkout")
	}
	d := h.store.Snapshot()
	if d.Reviews[d.Active].State.Workspace.FinishedAt != "" {
		t.Fatal("stale discard persisted a future cleanup intent")
	}
}

func TestHTTPMissingAgentIsRefusedBeforeAnyManagedGitState(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	d := h.store.Snapshot()
	root := d.Settings.ACP.ManagedRoot
	for _, command := range []string{"", filepath.Join(t.TempDir(), "absent-agent")} {
		d.Settings.ACP.Command = command
		if err := h.store.Commit(d); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		result := <-startVerification(t, h, ctx)
		cancel()
		if result.err == nil || result.status == http.StatusOK {
			t.Fatalf("verification without a runnable agent was accepted (%q)", command)
		}
		entries, err := os.ReadDir(root)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("a missing agent still created managed Git state in %s: %v", root, entries)
		}
		var after domain.State
		h.request(t, "GET", "/api/state", nil, &after)
		if after.Workspace.Status != "not_created" || after.Workspace.Path != "" {
			t.Fatalf("checkout reported without an agent: %+v", after.Workspace)
		}
	}
}
