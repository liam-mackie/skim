package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"skim/internal/domain"
)

func startStateOperation(t *testing.T, h *harness, ctx context.Context, path string, input any) <-chan verificationResponse {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, bytes.NewReader(payload))
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
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		result := verificationResponse{status: res.StatusCode, err: err}
		if err == nil && res.StatusCode != http.StatusOK {
			result.err = fmt.Errorf("%s HTTP %d: %s", path, res.StatusCode, body)
		}
		if result.err == nil {
			result.err = json.Unmarshal(body, &result.state)
		}
		done <- result
	}()
	return done
}

func awaitStateOperation(t *testing.T, done <-chan verificationResponse) verificationResponse {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(30 * time.Second):
		t.Fatal("operation did not finish")
		return verificationResponse{}
	}
}

func concurrentManual(state domain.State, body string) domain.ManualComment {
	pr := state.PullRequest
	return domain.ManualComment{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA, Path: "src/calc.go", StartLine: 2, EndLine: 2, Side: "RIGHT", Body: body}
}

func TestHTTPReviewChatVerificationAndManualWritesOverlap(t *testing.T) {
	for _, reviewFirst := range []bool{false, true} {
		name := "verification-first"
		if reviewFirst {
			name = "review-first"
		}
		t.Run(name, func(t *testing.T) {
			control := newAnalysisModelControl()
			h := newHarnessWithModels(t, control)
			t.Cleanup(func() { control.release(nil, false, true) })
			h.agentReview(t, "", true)
			var loaded domain.State
			h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &loaded)
			h.awaitAnalysis(t, analysisTerminal)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			review := startStateOperation(t, h, ctx, "/api/review", struct{}{})
			reviewPermission := awaitReviewPermission(t, h)
			verification := startVerification(t, h, ctx)
			permission := awaitPermission(t, h, verification)
			var chat domain.ChatThread
			if h.request(t, "POST", "/api/chat", map[string]string{"message": "Explain while review and ACP are waiting."}, &chat) != 200 || chat.Revision != 1 {
				t.Fatalf("chat did not complete: %+v", chat)
			}
			var manual domain.Comment
			if h.request(t, "POST", "/api/comments", concurrentManual(loaded, "Preserve the manual draft."), &manual) != 200 || manual.Revision != 1 {
				t.Fatalf("manual did not complete: %+v", manual)
			}
			for _, request := range []struct {
				method, path string
				input        any
			}{
				{"POST", "/api/review", struct{}{}},
				{"POST", "/api/verify", verificationRequest(h, h.app.State().Chats[0].ID)},
				{"POST", "/api/publish", map[string]string{"event": "COMMENT", "body": "not while AI runs"}},
				{"POST", "/api/review/finish", finishRequest(loaded, false)},
				{"POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/8"}},
				{"PUT", "/api/settings", h.app.Settings()},
				{"POST", "/api/style/import", struct{}{}},
			} {
				var failure map[string]string
				if status := h.request(t, request.method, request.path, request.input, &failure); status != http.StatusConflict {
					t.Fatalf("%s conflict HTTP %d: %v", request.path, status, failure)
				}
			}
			finishVerification := func() domain.State {
				// This permission runs the real focused command in the isolated fixture checkout.
				h.request(t, "POST", "/api/permissions/"+permission, map[string]string{"optionId": "allow-once"}, nil)
				result := awaitStateOperation(t, verification)
				if result.err != nil {
					t.Fatal(result.err)
				}
				found := false
				for _, thread := range result.state.Chats {
					if thread.Kind == "verification" && len(thread.Messages) == 2 {
						found = true
					}
				}
				if !found || result.job.Status != "complete" {
					t.Fatal("ACP report was not retained in its thread")
				}
				return result.state
			}
			finishReview := func() domain.State {
				h.request(t, "POST", "/api/permissions/"+reviewPermission, map[string]string{"optionId": "allow-once"}, nil)
				result := awaitStateOperation(t, review)
				if result.err != nil {
					t.Fatal(result.err)
				}
				return result.state
			}
			var first, last domain.State
			if reviewFirst {
				first = finishReview()
			} else {
				first = finishVerification()
			}
			if first.Revision != h.store.State().Revision {
				t.Fatal("first completion returned a pre-commit revision")
			}
			var edited domain.Comment
			h.request(t, "PATCH", "/api/comments/"+manual.ID, map[string]string{"body": "Edited after the first AI completion.", "status": "pending"}, &edited)
			if edited.Revision != 2 {
				t.Fatalf("manual revision: %+v", edited)
			}
			if reviewFirst {
				last = finishVerification()
			} else {
				last = finishReview()
			}
			saved := h.store.State()
			if last.Revision != saved.Revision || !reflect.DeepEqual(last.Comments, saved.Comments) || !reflect.DeepEqual(last.Chats, saved.Chats) {
				t.Fatal("last response did not describe its latest committed snapshot")
			}
			if len(saved.Comments) != 4 || len(saved.Chats) != 2 || saved.Comments[0] != edited || saved.Chats[1].ID != chat.ID || saved.Workspace.FinishedAt != "" {
				t.Fatalf("AI completion overwrote unrelated work: %+v", saved)
			}
			if workspace := h.app.Workspace().Workspace; workspace.Status != "ready" {
				t.Fatalf("verification lease leaked: %+v", workspace)
			}
		})
	}
}

func TestHTTPChatCompletionPreservesPublicationAndCleanup(t *testing.T) {
	control := newAnalysisModelControl()
	control.chatGate = make(chan struct{})
	h := newHarnessWithModels(t, control)
	t.Cleanup(func() { control.release(nil, false, true) })
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	var manual domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(state, "Approved human finding."), &manual)
	h.request(t, "PATCH", "/api/comments/"+manual.ID, map[string]string{"body": "Edited pending human finding.", "status": "pending"}, &manual)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	chatDone := make(chan error, 1)
	go func() { _, err := h.app.Chat(ctx, "Finish after publication.", ""); chatDone <- err }()
	waitCondition(t, func() bool { control.mu.Lock(); defer control.mu.Unlock(); return control.chatStarted > 0 })
	if h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &state) != 200 {
		t.Fatal("chat blocked publication")
	}
	if state.Comments[0].Revision != 3 || state.Comments[0].Status != "published" || state.Revision != h.store.State().Revision {
		t.Fatalf("publication revision: %+v", state)
	}
	if h.request(t, "POST", "/api/review/finish", finishRequest(state, false), &state) != 200 || state.Revision != h.store.State().Revision {
		t.Fatal("chat blocked cleanup or cleanup returned old revision")
	}
	finished := state.Workspace.FinishedAt
	control.release(nil, false, true)
	select {
	case err := <-chatDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("chat did not complete")
	}
	state = h.app.State()
	if len(state.Chats) != 1 || state.Comments[0].Status != "published" || state.Comments[0].Revision != 3 || state.Workspace.FinishedAt != finished {
		t.Fatalf("late chat rewound publication/cleanup: %+v", state)
	}
}

func TestHTTPVerificationStaleCommitAndCancellationPreserveOtherWork(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint("stale=", stale), func(t *testing.T) {
			h := newHarness(t)
			var state domain.State
			h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
			h.awaitAnalysis(t, analysisTerminal)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			done := startVerification(t, h, ctx)
			permission := awaitPermission(t, h, done)
			var chat domain.ChatThread
			h.request(t, "POST", "/api/chat", map[string]string{"message": "Keep this completed conversation."}, &chat)
			var manual domain.Comment
			h.request(t, "POST", "/api/comments", concurrentManual(state, "Keep this completed manual edit."), &manual)
			if stale {
				d := h.store.Snapshot()
				r := d.Reviews[d.Active]
				r.State.Files[0].Patch += "\n+new evidence"
				d.Reviews[d.Active] = r
				if err := h.store.Commit(d); err != nil {
					t.Fatal(err)
				}
				h.request(t, "POST", "/api/permissions/"+permission, map[string]string{"optionId": "reject-once"}, nil)
			} else {
				job := h.app.State().Chats[0].Verification
				h.request(t, "POST", "/api/verifications/"+job.ID+"/cancel", domain.PRIdentity{PRKey: job.PRKey, BaseSHA: job.BaseSHA, HeadSHA: job.HeadSHA}, &domain.VerificationJob{})
			}
			result := awaitStateOperation(t, done)
			if result.err != nil || (stale && result.job.Status != "complete") || (!stale && result.job.Status != "cancelled") {
				t.Fatalf("verification did not retain its terminal outcome: %+v", result)
			}
			waitCondition(t, func() bool {
				return h.app.Workspace().Workspace.Status != "in_use" && len(h.app.Agent.Permissions()) == 0
			})
			state = h.app.State()
			if len(state.Chats) != 2 || state.Chats[1].ID != chat.ID || state.Comments[0] != manual {
				t.Fatalf("verification lost other work: %+v", state)
			}
			thread := state.Chats[0]
			if stale {
				if !thread.Stale || len(thread.Messages) != 2 || len(state.Comments) != 2 || !state.Comments[1].Stale || state.Comments[1].ThreadID != thread.ID {
					t.Fatalf("changed evidence lost the report or attached a current-code finding: %+v", state)
				}
			} else if len(thread.Messages) != 1 || len(state.Comments) != 1 {
				t.Fatalf("cancellation fabricated a report or finding: %+v", state)
			}
		})
	}
}

func TestHTTPCloseDrainsReviewChatAndACP(t *testing.T) {
	control := newAnalysisModelControl()
	control.chatGate = make(chan struct{})
	h := newHarnessWithModels(t, control)
	t.Cleanup(func() { control.release(nil, false, true) })
	h.agentReview(t, "", true)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	review := startStateOperation(t, h, ctx, "/api/review", struct{}{})
	awaitReviewPermission(t, h)
	verification := startVerification(t, h, ctx)
	awaitPermission(t, h, verification)
	chat := make(chan error, 1)
	go func() { _, err := h.app.Chat(ctx, "Cancel on shutdown.", ""); chat <- err }()
	waitCondition(t, func() bool { control.mu.Lock(); defer control.mu.Unlock(); return control.chatStarted > 0 })
	var manual domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(state, "Already saved."), &manual)
	closed := make(chan error, 1)
	go func() { closed <- h.app.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Close did not drain ACP/model operations")
	}
	if result := awaitStateOperation(t, review); result.err == nil {
		t.Fatal("shutdown review saved")
	}
	if result := awaitStateOperation(t, verification); result.err != nil || result.job.Status != "cancelled" {
		t.Fatalf("shutdown did not persist admitted verification cancellation: %+v", result)
	}
	select {
	case err := <-chat:
		if err == nil {
			t.Fatal("shutdown chat saved")
		}
	case <-ctx.Done():
		t.Fatal("chat not drained")
	}
	state = h.app.State()
	if len(h.app.Agent.Permissions()) != 0 || state.Workspace.Status == "in_use" || len(state.Comments) != 1 || state.Comments[0] != manual || len(state.Chats) != 1 || state.Chats[0].Verification.Status != "cancelled" || len(state.Chats[0].Messages) != 1 {
		t.Fatalf("Close leaked ACP or lost work: %+v", state)
	}
}

func TestHTTPEntityRevisionsChangeOnlyWithTheirEntity(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	var manual domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(state, "Original body."), &manual)
	var chat domain.ChatThread
	h.request(t, "POST", "/api/chat", map[string]string{"message": "Initial turn."}, &chat)
	if manual.Revision != 1 || chat.Revision != 1 {
		t.Fatal("created entities did not start at revision one")
	}
	h.request(t, "PATCH", "/api/comments/"+manual.ID, map[string]string{"body": manual.Body, "status": manual.Status}, &manual)
	if manual.Revision != 1 {
		t.Fatal("no-op edit changed entity revision")
	}
	h.request(t, "POST", "/api/chat", map[string]string{"threadId": chat.ID, "message": "Second turn."}, &chat)
	if chat.Revision != 2 {
		t.Fatal("appended turn did not advance thread revision")
	}
	style := "Updated style."
	h.request(t, "PUT", "/api/settings", domain.SettingsUpdate{Settings: h.app.Settings(), Style: &style}, nil)
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	if state.Comments[0].Revision != 1 || state.Chats[0].Revision != 2 {
		t.Fatal("metadata or unchanged reload re-versioned entities")
	}
	h.fixture.mu.Lock()
	h.fixture.head = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h.fixture.mu.Unlock()
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	if !state.Comments[0].Stale || state.Comments[0].Revision != 2 || !state.Chats[0].Stale || state.Chats[0].Revision != 3 {
		t.Fatalf("staling did not advance entity revisions: %+v", state)
	}
	h.fixture.mu.Lock()
	h.fixture.head = "cccccccccccccccccccccccccccccccccccccccc"
	h.fixture.mu.Unlock()
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	if state.Comments[0].Revision != 2 || state.Chats[0].Revision != 3 {
		t.Fatal("already-stale entries were re-versioned")
	}
}
