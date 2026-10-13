package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"skim/internal/domain"
)

func TestHTTPCommentLifecycleTransitionsAndContentErasingDelete(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	h.request(t, "POST", "/api/review", struct{}{}, &state)
	for _, c := range state.Comments {
		if c.Status != "suggested" || c.Revision != 1 {
			t.Fatalf("AI did not create suggestion: %+v", c)
		}
	}
	first, second := state.Comments[0], state.Comments[1]
	var manual domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(state, "Human pending comment."), &manual)
	if manual.Status != "pending" || manual.Revision != 1 {
		t.Fatalf("human comment not pending: %+v", manual)
	}
	h.request(t, "PATCH", "/api/comments/"+first.ID, map[string]string{"body": "Edited suggestion, still awaiting approval."}, &first)
	if first.Status != "suggested" || first.Revision != 2 {
		t.Fatalf("edit silently approved suggestion: %+v", first)
	}
	h.request(t, "PATCH", "/api/comments/"+first.ID, map[string]string{"status": "pending"}, &first)
	if first.Status != "pending" || first.Revision != 3 {
		t.Fatalf("approval failed: %+v", first)
	}
	h.request(t, "PATCH", "/api/comments/"+first.ID, map[string]string{"status": "pending"}, &first)
	if first.Revision != 3 {
		t.Fatal("idempotent approval re-versioned comment")
	}
	before := h.store.State().Comments
	for _, status := range []string{"suggested", "published", "deleted", "draft", "approved", "denied"} {
		var failure map[string]string
		if h.request(t, "PATCH", "/api/comments/"+first.ID, map[string]string{"status": status, "body": "Must not apply invalid mutation"}, &failure) != http.StatusBadRequest {
			t.Fatalf("invalid transition to %s accepted", status)
		}
	}
	if !reflect.DeepEqual(before, h.store.State().Comments) {
		t.Fatal("invalid transition partially changed content")
	}
	var erased domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(state, "erase-this-private-comment-body"), &erased)
	var tombstone map[string]any
	if h.request(t, "DELETE", "/api/comments/"+erased.ID, nil, &tombstone) != 200 {
		t.Fatal("pending comment could not be deleted")
	}
	want := map[string]any{"id": erased.ID, "revision": float64(2), "status": "deleted"}
	if !reflect.DeepEqual(tombstone, want) {
		t.Fatalf("delete exposed content beyond tombstone: %+v", tombstone)
	}
	var repeated map[string]any
	h.request(t, "DELETE", "/api/comments/"+erased.ID, nil, &repeated)
	if !reflect.DeepEqual(repeated, want) {
		t.Fatal("repeat delete changed tombstone")
	}
	var deletedSuggestion domain.Comment
	h.request(t, "DELETE", "/api/comments/"+second.ID, nil, &deletedSuggestion)
	if deletedSuggestion != (domain.Comment{ID: second.ID, Revision: 2, Status: "deleted"}) {
		t.Fatalf("suggestion deletion retained content: %+v", deletedSuggestion)
	}
	var failure map[string]string
	if h.request(t, "PATCH", "/api/comments/"+erased.ID, map[string]string{"body": "Resurrect"}, &failure) != 400 {
		t.Fatal("deleted comment was edited")
	}
	encoded, err := os.ReadFile(filepath.Join(h.dataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "erase-this-private-comment-body") {
		t.Fatal("deleted body remained on disk")
	}
	h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &state)
	h.fixture.mu.Lock()
	payload, posts := h.fixture.last, h.fixture.posts
	h.fixture.mu.Unlock()
	if posts != 1 || len(payload["comments"].([]any)) != 2 || state.Comments[0].Status != "published" || state.Comments[2].Status != "published" || state.Comments[1].Status != "deleted" {
		t.Fatalf("publication included something other than pending comments: %+v", state)
	}
	if h.request(t, "DELETE", "/api/comments/"+manual.ID, nil, &failure) != 400 {
		t.Fatal("published comment was deleted locally")
	}
	if h.request(t, "PATCH", "/api/comments/"+manual.ID, map[string]string{"body": "Change published text"}, &failure) != 400 {
		t.Fatal("published comment was edited locally")
	}
}

func TestHTTPFinishReviewAllowsBareApprovalAndRequiresNeedsChangesSummary(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	h.request(t, "POST", "/api/review", struct{}{}, &state)
	var failure map[string]string
	if h.request(t, "POST", "/api/publish", map[string]string{"event": "REQUEST_CHANGES", "body": "  "}, &failure) != 400 {
		t.Fatal("needs-changes omitted required summary")
	}
	if h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &failure) != 400 {
		t.Fatal("empty COMMENT with only suggestions was submitted")
	}
	if h.request(t, "POST", "/api/publish", map[string]string{"event": "APPROVE"}, &state) != 200 {
		t.Fatal("bare approval rejected")
	}
	h.fixture.mu.Lock()
	payload, posts := h.fixture.last, h.fixture.posts
	h.fixture.mu.Unlock()
	if posts != 1 || payload["event"] != "APPROVE" || payload["comments"] != nil {
		t.Fatalf("bare approval published suggestions: %+v", payload)
	}
	for _, c := range state.Comments {
		if c.Status != "suggested" {
			t.Fatal("bare approval changed suggestion status")
		}
	}
}

func TestHTTPStalePendingBlocksFinishAndStaleSuggestionCannotBeApproved(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	h.request(t, "POST", "/api/review", struct{}{}, &state)
	suggestion := state.Comments[0]
	var manual domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(state, "Pending old-head comment."), &manual)
	h.fixture.mu.Lock()
	h.fixture.head = strings.Repeat("b", 40)
	h.fixture.mu.Unlock()
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	var failure map[string]string
	if h.request(t, "PATCH", "/api/comments/"+suggestion.ID, map[string]string{"status": "pending"}, &failure) != 400 {
		t.Fatal("stale suggestion could become pending")
	}
	if h.request(t, "POST", "/api/publish", map[string]string{"event": "APPROVE"}, &failure) != 400 || !strings.Contains(failure["error"], "pending comments") {
		t.Fatalf("stale pending comment silently omitted: %+v", failure)
	}
	var tombstone domain.Comment
	h.request(t, "DELETE", "/api/comments/"+manual.ID, nil, &tombstone)
	if tombstone.Status != "deleted" {
		t.Fatal("stale pending blocker could not be deleted")
	}
}

func TestHTTPUncertainPublicationProtectsCommentMutations(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	var manual domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(state, "Uncertain publication comment."), &manual)
	h.fixture.mu.Lock()
	h.fixture.ambiguous = true
	h.fixture.mu.Unlock()
	var failure map[string]string
	if h.request(t, "POST", "/api/publish", map[string]string{"event": "COMMENT"}, &failure) != 400 {
		t.Fatal("ambiguous delivery hidden")
	}
	before := h.store.Snapshot()
	if h.request(t, "DELETE", "/api/comments/"+manual.ID, nil, &failure) != 400 {
		t.Fatal("uncertain publication allowed deletion")
	}
	if h.request(t, "PATCH", "/api/comments/"+manual.ID, map[string]string{"body": "Must not change"}, &failure) != 400 {
		t.Fatal("uncertain publication allowed edit")
	}
	if h.request(t, "POST", "/api/comments", concurrentManual(state, "Must not add"), &failure) != 400 {
		t.Fatal("uncertain publication allowed new comment")
	}
	if !reflect.DeepEqual(before, h.store.Snapshot()) {
		t.Fatal("rejected mutation changed uncertain publication")
	}
}

func TestHTTPVerificationDoesNotResurrectDeletedPendingComment(t *testing.T) {
	h := newHarness(t)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	var manual domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(state, "Delete while verification is held."), &manual)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := startVerification(t, h, ctx)
	permission := awaitPermission(t, h, done)
	var tombstone domain.Comment
	h.request(t, "DELETE", "/api/comments/"+manual.ID, nil, &tombstone)
	h.request(t, "POST", "/api/permissions/"+permission, map[string]string{"optionId": "reject-once"}, nil)
	result := awaitStateOperation(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.state.Comments) != 2 || result.state.Comments[0] != tombstone || result.state.Comments[1].Status != "suggested" || result.state.Comments[1].Source != "verification" {
		t.Fatalf("verification resurrected deletion or auto-approved output: %+v", result.state.Comments)
	}
	encoded, err := json.Marshal(result.state.Comments[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 {
		t.Fatalf("state tombstone exposed deleted content: %s", encoded)
	}
}
