package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"skim/internal/acp"
	"skim/internal/domain"
)

func verificationIdentity(job domain.VerificationJob) domain.PRIdentity {
	return domain.PRIdentity{PRKey: job.PRKey, BaseSHA: job.BaseSHA, HeadSHA: job.HeadSHA}
}

func verificationUpdate(t *testing.T, h *harness, identity domain.PRIdentity) domain.VerificationUpdate {
	t.Helper()
	query := url.Values{}
	if identity != (domain.PRIdentity{}) {
		query.Set("prKey", identity.PRKey)
		query.Set("baseSha", identity.BaseSHA)
		query.Set("headSha", identity.HeadSHA)
	}
	var update domain.VerificationUpdate
	h.request(t, "GET", "/api/verifications?"+query.Encode(), nil, &update)
	return update
}

func awaitVerificationJob(t *testing.T, h *harness, id string) domain.VerificationJob {
	t.Helper()
	var result domain.VerificationJob
	waitCondition(t, func() bool {
		for _, job := range verificationUpdate(t, h, domain.PRIdentity{}).Jobs {
			if job.ID == id {
				result = job
				return !job.Active()
			}
		}
		return false
	})
	return result
}

func acceptVerification(t *testing.T, h *harness, threadID, question string) domain.VerificationAccepted {
	t.Helper()
	request := verificationRequest(h, threadID)
	request.Question = question
	var accepted domain.VerificationAccepted
	if status := h.request(t, "POST", "/api/verify", request, &accepted); status != http.StatusAccepted || !accepted.Job.Active() || accepted.Thread.Verification == nil || accepted.Thread.Verification.ID != accepted.Job.ID || accepted.Thread.ID != accepted.Job.ThreadID {
		t.Fatalf("verification admission did not return its persisted job: HTTP %d %+v", status, accepted)
	}
	return accepted
}

func permissionForJob(t *testing.T, h *harness, job domain.VerificationJob) acp.Permission {
	t.Helper()
	var result acp.Permission
	waitCondition(t, func() bool {
		for _, permission := range h.app.Agent.Permissions() {
			if permission.JobID == job.ID {
				result = permission
				return true
			}
		}
		return false
	})
	if result.ThreadID != job.ThreadID || result.PRKey != job.PRKey || result.PRURL != job.PRURL || result.Path != job.Path || result.Question != job.Question {
		t.Fatalf("permission lost its captured owner: %+v / %+v", result, job)
	}
	return result
}

func TestHTTPConcurrentVerificationJobsCompleteInReverseOrder(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	first := acceptVerification(t, h, "", "Check the first independent concern.")
	firstPermission := permissionForJob(t, h, first.Job)
	second := acceptVerification(t, h, "", "Check the second independent concern.")
	secondPermission := permissionForJob(t, h, second.Job)
	if first.Job.ID == second.Job.ID || first.Thread.ID == second.Thread.ID || firstPermission.ID == secondPermission.ID || firstPermission.SessionID != secondPermission.SessionID || len(h.app.Agent.Permissions()) != 2 {
		t.Fatal("independent processes with the same session ID did not retain two permission owners")
	}
	update := verificationUpdate(t, h, verificationIdentity(first.Job))
	if len(update.Threads) != 2 || len(update.Jobs) != 2 {
		t.Fatalf("concurrent jobs not visible: %+v", update)
	}
	paths := map[string]bool{}
	for _, thread := range update.Threads {
		job := thread.Verification
		if job.Status != "running" || job.Workspace == nil || job.Workspace.Status != "in_use" || job.Workspace.Path == "" || paths[job.Workspace.Path] {
			t.Fatalf("agents shared a checkout or were serialized: %+v", job)
		}
		paths[job.Workspace.Path] = true
	}
	if workspace := h.app.Workspace().Workspace; workspace.Count != 2 || workspace.Path != "" || workspace.Status != "in_use" {
		t.Fatalf("aggregate checkout status hid concurrent execution: %+v", workspace)
	}
	var failure map[string]string
	if h.request(t, "POST", "/api/verify", verificationRequest(h, first.Thread.ID), &failure) != http.StatusConflict {
		t.Fatal("admitted a second in-flight turn in the same thread")
	}
	for _, route := range []string{"/api/publish", "/api/review/finish"} {
		var input any = map[string]string{"event": "COMMENT", "body": "Wait for checks."}
		if route == "/api/review/finish" {
			input = finishRequest(state, false)
		}
		if h.request(t, "POST", route, input, &failure) != http.StatusConflict {
			t.Fatalf("%s did not block this PR's active jobs", route)
		}
	}
	// Identical SHAs do not make another PR own either job or its cleanup blocker.
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/8"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	other := domain.PRIdentity{PRKey: state.PullRequest.Key(), BaseSHA: state.PullRequest.BaseSHA, HeadSHA: state.PullRequest.HeadSHA}
	if got := verificationUpdate(t, h, other); len(got.Threads) != 0 || len(got.Comments) != 0 || len(got.Jobs) != 2 {
		t.Fatalf("polling merged jobs into the wrong PR: %+v", got)
	}
	if h.request(t, "POST", "/api/verifications/"+first.Job.ID+"/cancel", other, &failure) != http.StatusBadRequest {
		t.Fatal("another PR cancelled a same-SHA job")
	}
	h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT", "body": "Other review complete."}, &state)
	h.request(t, "POST", "/api/review/finish", finishRequest(state, false), &state)
	for path := range paths {
		if got := fixtureGit(t, path, "rev-parse", "HEAD"); got != first.Job.HeadSHA {
			t.Fatal("finishing another PR removed or changed an active worktree")
		}
	}
	h.request(t, "POST", "/api/permissions/"+secondPermission.ID, map[string]string{"optionId": "reject-once"}, nil)
	if job := awaitVerificationJob(t, h, second.Job.ID); job.Status != "complete" || job.SuggestionsError != "" {
		t.Fatalf("second job did not finish independently: %+v", job)
	}
	if permissions := h.app.Agent.Permissions(); len(permissions) != 1 || permissions[0].ID != firstPermission.ID {
		t.Fatal("completing the second agent consumed the first agent's permission")
	}
	partial := verificationUpdate(t, h, verificationIdentity(first.Job))
	if len(partial.Threads[0].Messages) != 1 || len(partial.Threads[1].Messages) != 2 || !partial.Threads[0].Verification.Active() || len(partial.Comments) != 1 || partial.Comments[0].ThreadID != second.Thread.ID {
		t.Fatalf("reverse completion attached a report/finding to the wrong thread: %+v", partial)
	}
	var edited domain.Comment
	// Editing the completed finding requires returning to its PR, not a worker changing Active.
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": first.Job.PRURL}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	h.request(t, "PATCH", "/api/comments/"+partial.Comments[0].ID, map[string]string{"status": "pending"}, &edited)
	h.request(t, "POST", "/api/permissions/"+firstPermission.ID, map[string]string{"optionId": "reject-once"}, nil)
	if job := awaitVerificationJob(t, h, first.Job.ID); job.Status != "complete" {
		t.Fatalf("first job failed: %+v", job)
	}
	final := verificationUpdate(t, h, verificationIdentity(first.Job))
	if len(final.Threads) != 2 || len(final.Threads[0].Messages) != 2 || len(final.Threads[1].Messages) != 2 || len(final.Comments) != 1 || final.Comments[0] != edited {
		t.Fatalf("late completion lost a report, duplicated a finding, or rewound approval: %+v", final)
	}
	h.request(t, "POST", "/api/review/finish", finishRequest(h.app.State(), false), &state)
	for path := range paths {
		requireRemoved(t, path)
	}
}

func TestVerificationAcceptedCopiesAndClientCancellationAreIndependent(t *testing.T) {
	h := newHarness(t)
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
	h.awaitAnalysis(t, analysisTerminal)
	ctx, cancel := context.WithCancel(context.Background())
	request := verificationRequest(h, "")
	accepted, err := h.app.Verify(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	job := accepted.Job
	accepted.Thread.Verification.Question = "mutated returned metadata"
	accepted.Thread.Messages[0].Content = "mutated returned history"
	cancel()
	firstPermission := permissionForJob(t, h, job)
	second := acceptVerification(t, h, "", "Keep this other agent waiting.")
	secondPermission := permissionForJob(t, h, second.Job)
	var cancelling domain.VerificationJob
	h.request(t, "POST", "/api/verifications/"+job.ID+"/cancel", verificationIdentity(job), &cancelling)
	if cancelling.Status != "cancelling" {
		t.Fatalf("cancel did not expose draining state: %+v", cancelling)
	}
	cancelled := awaitVerificationJob(t, h, job.ID)
	if cancelled.Status != "cancelled" || cancelled.FinishedAt == "" || cancelled.Workspace == nil || cancelled.Workspace.Status == "in_use" {
		t.Fatalf("cancellation did not drain its owned execution: %+v", cancelled)
	}
	var repeated domain.VerificationJob
	h.request(t, "POST", "/api/verifications/"+job.ID+"/cancel", verificationIdentity(job), &repeated)
	if repeated.Status != cancelled.Status || repeated.FinishedAt != cancelled.FinishedAt {
		t.Fatal("terminal cancellation was not idempotent")
	}
	var failure map[string]string
	if h.request(t, "POST", "/api/permissions/"+firstPermission.ID, map[string]string{"optionId": "allow-once"}, &failure) != http.StatusBadRequest {
		t.Fatal("cancelled permission still authorized an action")
	}
	if remaining := h.app.Agent.Permissions(); len(remaining) != 1 || remaining[0].ID != secondPermission.ID {
		t.Fatal("cancelling one session ID cancelled its independent namesake")
	}
	update := verificationUpdate(t, h, verificationIdentity(job))
	if update.Threads[0].Messages[0].Content != request.Question || update.Threads[0].Verification.Question != request.Question {
		t.Fatal("the accepted response exposed mutable worker/store state")
	}
	h.request(t, "POST", "/api/permissions/"+secondPermission.ID, map[string]string{"optionId": "reject-once"}, nil)
	awaitVerificationJob(t, h, second.Job.ID)
}

func TestHTTPVerificationRetainsReportsWithoutFindings(t *testing.T) {
	for _, mode := range []string{"no-findings", "invalid-json", "invalid-anchor", "left-suggestion", "model-error", "missing-model"} {
		t.Run(mode, func(t *testing.T) {
			control := newAnalysisModelControl()
			content := `{"comments":[]}`
			switch mode {
			case "invalid-json":
				content = `{"comments":`
			case "invalid-anchor":
				content = `{"comments":[{"path":"not-visible.go","startLine":200,"endLine":200,"side":"RIGHT","body":"Invalid evidence"}]}`
			case "left-suggestion":
				content = "{\"comments\":[{\"path\":\"src/calc.go\",\"startLine\":2,\"endLine\":2,\"side\":\"LEFT\",\"body\":\"Old-only replacement\\n```suggestion\\nreplacement\\n```\"}]}"
			case "model-error":
				control.findingsFail = true
			}
			control.findingsContent = &content
			h := newHarnessWithModels(t, control)
			h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
			h.awaitAnalysis(t, analysisTerminal)
			if mode == "missing-model" {
				settings := h.app.Settings()
				settings.Roles.Review = domain.ModelRole{}
				h.request(t, "PUT", "/api/settings", settings, nil)
			}
			accepted := acceptVerification(t, h, "", "Keep the report even if there are no publishable findings.")
			permission := permissionForJob(t, h, accepted.Job)
			h.request(t, "POST", "/api/permissions/"+permission.ID, map[string]string{"optionId": "reject-once"}, nil)
			job := awaitVerificationJob(t, h, accepted.Job.ID)
			update := verificationUpdate(t, h, verificationIdentity(job))
			if job.Status != "complete" || len(update.Comments) != 0 || len(update.Threads) != 1 || len(update.Threads[0].Messages) != 2 || !strings.Contains(update.Threads[0].Messages[1].Content, "permission was denied") {
				t.Fatalf("report was discarded or turned into a filler comment: %+v / %+v", job, update)
			}
			if (job.SuggestionsError == "") != (mode == "no-findings") {
				t.Fatalf("extraction failure was hidden or a clean result was called a failure: %+v", job)
			}
			control.mu.Lock()
			calls := control.findingsStarted
			control.mu.Unlock()
			want := 1
			if mode == "missing-model" {
				want = 0
			}
			if calls != want {
				t.Fatalf("extraction was retried or unexpectedly skipped: %d requests, want %d", calls, want)
			}
		})
	}
}

func TestHTTPVerificationRetainsReportWhenCancelledDuringExtraction(t *testing.T) {
	control := newAnalysisModelControl()
	control.findingsGate = make(chan struct{})
	h := newHarnessWithModels(t, control)
	t.Cleanup(func() { control.release(nil, false, true) })
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
	h.awaitAnalysis(t, analysisTerminal)
	accepted := acceptVerification(t, h, "", "Save the full report first.")
	permission := permissionForJob(t, h, accepted.Job)
	h.request(t, "POST", "/api/permissions/"+permission.ID, map[string]string{"optionId": "reject-once"}, nil)
	waitCondition(t, func() bool { control.mu.Lock(); defer control.mu.Unlock(); return control.findingsStarted == 1 })
	before := verificationUpdate(t, h, verificationIdentity(accepted.Job))
	if len(before.Threads[0].Messages) != 2 || before.Threads[0].Verification.Status != "summarizing" {
		t.Fatal("full report was not committed before extraction")
	}
	h.request(t, "POST", "/api/verifications/"+accepted.Job.ID+"/cancel", verificationIdentity(accepted.Job), &domain.VerificationJob{})
	job := awaitVerificationJob(t, h, accepted.Job.ID)
	after := verificationUpdate(t, h, verificationIdentity(job))
	if job.Status != "cancelled" || len(after.Comments) != 0 || len(after.Threads[0].Messages) != 2 || after.Threads[0].Messages[1] != before.Threads[0].Messages[1] {
		t.Fatalf("cancelling extraction lost the completed report: %+v", after)
	}
}

func TestHTTPVerificationPreservesOldSnapshotReports(t *testing.T) {
	for _, ref := range []string{"base", "head"} {
		t.Run(ref, func(t *testing.T) {
			h := newHarness(t)
			var state domain.State
			h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
			h.awaitAnalysis(t, analysisTerminal)
			accepted := acceptVerification(t, h, "", "Retain my old-snapshot evidence.")
			permission := permissionForJob(t, h, accepted.Job)
			h.fixture.mu.Lock()
			if ref == "base" {
				h.fixture.base = strings.Repeat("b", 40)
			} else {
				h.fixture.head = strings.Repeat("c", 40)
			}
			h.fixture.mu.Unlock()
			h.request(t, "POST", "/api/pull-request", map[string]string{"url": accepted.Job.PRURL}, &state)
			h.awaitAnalysis(t, analysisTerminal)
			staleRevision := state.Chats[0].Revision
			h.request(t, "POST", "/api/permissions/"+permission.ID, map[string]string{"optionId": "reject-once"}, nil)
			job := awaitVerificationJob(t, h, accepted.Job.ID)
			current := domain.PRIdentity{PRKey: state.PullRequest.Key(), BaseSHA: state.PullRequest.BaseSHA, HeadSHA: state.PullRequest.HeadSHA}
			update := verificationUpdate(t, h, current)
			if job.Status != "complete" || len(update.Threads) != 1 || !update.Threads[0].Stale || update.Threads[0].Revision <= staleRevision || len(update.Threads[0].Messages) != 2 || len(update.Comments) != 1 || !update.Comments[0].Stale || update.Comments[0].HeadSHA != accepted.Job.HeadSHA {
				t.Fatalf("completion rewound snapshot staleness: %+v", update)
			}
			query := url.Values{"prKey": {accepted.Job.PRKey}, "baseSha": {accepted.Job.BaseSHA}, "headSha": {accepted.Job.HeadSHA}}
			var failure map[string]string
			if h.request(t, "GET", "/api/verifications?"+query.Encode(), nil, &failure) != http.StatusBadRequest {
				t.Fatal("old snapshot polling returned current snapshot records")
			}
			// Cancellation identity refers to the job, not the now-loaded snapshot.
			h.request(t, "POST", "/api/verifications/"+job.ID+"/cancel", verificationIdentity(job), &domain.VerificationJob{})
		})
	}
}

func TestHTTPVerificationHeadFailureIsAnAcceptedJobOutcome(t *testing.T) {
	h := newHarness(t)
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
	h.awaitAnalysis(t, analysisTerminal)
	h.fixture.mu.Lock()
	h.fixture.head = strings.Repeat("d", 40)
	h.fixture.mu.Unlock()
	accepted := acceptVerification(t, h, "", "Check a remotely changed head.")
	job := awaitVerificationJob(t, h, accepted.Job.ID)
	update := verificationUpdate(t, h, verificationIdentity(job))
	if job.Status != "failed" || !strings.Contains(job.Error, "head changed") || len(update.Threads[0].Messages) != 1 || len(update.Comments) != 0 || len(h.app.Agent.Permissions()) != 0 || job.Workspace == nil || job.Workspace.Status != "not_created" {
		t.Fatalf("head preflight was not retained as a failed background job: %+v / %+v", job, update)
	}
}

func TestHTTPVerificationFollowupAndCodeSuggestionPublication(t *testing.T) {
	h := newHarness(t)
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &domain.State{})
	h.awaitAnalysis(t, analysisTerminal)
	first := acceptVerification(t, h, "", "Inspect the arithmetic.")
	permission := permissionForJob(t, h, first.Job)
	h.request(t, "POST", "/api/permissions/"+permission.ID, map[string]string{"optionId": "reject-once"}, nil)
	firstJob := awaitVerificationJob(t, h, first.Job.ID)
	second := acceptVerification(t, h, first.Thread.ID, "Check the same lines again with the earlier report.")
	if second.Job.ID == first.Job.ID || second.Job.ThreadID != first.Job.ThreadID || len(second.Thread.Messages) != 3 {
		t.Fatal("follow-up failed to retain prior conversation")
	}
	permission = permissionForJob(t, h, second.Job)
	h.request(t, "POST", "/api/permissions/"+permission.ID, map[string]string{"optionId": "reject-once"}, nil)
	secondJob := awaitVerificationJob(t, h, second.Job.ID)
	update := verificationUpdate(t, h, verificationIdentity(secondJob))
	if len(update.Threads) != 1 || len(update.Threads[0].Messages) != 4 || secondJob.Workspace.Path != firstJob.Workspace.Path || len(update.Comments) != 1 {
		t.Fatalf("follow-up lost history, worktree reuse, or finding deduplication: %+v", update)
	}
	finding := update.Comments[0]
	if finding.Status != "suggested" || finding.Source != "verification" || finding.ThreadID != first.Thread.ID || !strings.Contains(finding.Body, "```suggestion\n") || finding.Body == update.Threads[0].Messages[1].Content {
		t.Fatalf("extraction did not produce a separate actionable suggestion: %+v", finding)
	}
	h.request(t, "PATCH", "/api/comments/"+finding.ID, map[string]string{"status": "pending"}, &finding)
	manualBody := "Keep this exact replacement and its trailing newline.\n\n```suggestion\n\treturn `literal`\n```\n"
	var manual domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(h.app.State(), manualBody), &manual)
	var state domain.State
	h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &state)
	h.fixture.mu.Lock()
	comments := h.fixture.last["comments"].([]any)
	h.fixture.mu.Unlock()
	if len(comments) != 2 {
		t.Fatalf("published %d comments, want two pending findings", len(comments))
	}
	for i, want := range []string{finding.Body, manualBody} {
		payload := comments[i].(map[string]any)
		if payload["body"] != want || payload["path"] != "src/calc.go" || payload["side"] != "RIGHT" || fmt.Sprint(payload["line"]) != "2" {
			t.Fatalf("GitHub publication altered code suggestion body or range: %+v", payload)
		}
	}
	if state.Workspace.Status != "finished" || len(state.Chats[0].Messages) != 4 {
		t.Fatal("publication discarded the verification report or failed cleanup")
	}
}
