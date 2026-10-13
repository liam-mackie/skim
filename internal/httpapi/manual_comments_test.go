package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"skim/internal/domain"
	"skim/internal/store"
)

func manualRequest(pr *domain.PullRequest, side string, start, end int, body string) domain.ManualComment {
	return domain.ManualComment{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA, Path: "src/calc.go", Side: side, StartLine: start, EndLine: end, Body: body}
}

func TestHTTPManualCommentLifecycle(t *testing.T) {
	control := newAnalysisModelControl()
	control.hold([]string{"src/calc.go", "src/calc_test.go"}, true)
	h := newHarnessWithModels(t, control)
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	pr := *state.PullRequest
	var left, right, removed domain.Comment
	h.request(t, http.MethodPost, "/api/comments", manualRequest(&pr, "LEFT", 2, 2, "  Keep the old arithmetic contract.  "), &left)
	h.request(t, http.MethodPost, "/api/comments", manualRequest(&pr, "RIGHT", 2, 3, "Please explain this range."), &right)
	h.request(t, http.MethodPost, "/api/comments", manualRequest(&pr, "RIGHT", 1, 1, "Remove before publication."), &removed)
	for _, c := range []domain.Comment{left, right, removed} {
		if c.ID == "" || c.Source != "manual" || c.Status != "pending" || c.HeadSHA != pr.HeadSHA || c.Stale || c.ThreadID != "" {
			t.Fatalf("manual comment identity/status: %+v", c)
		}
		if _, err := time.Parse(time.RFC3339, c.CreatedAt); err != nil {
			t.Fatal(err)
		}
	}
	if left.ID == right.ID {
		t.Fatal("different manual comments shared an identity")
	}
	control.mu.Lock()
	calls := control.foregroundStarted
	control.mu.Unlock()
	if calls != 0 {
		t.Fatal("manual comment invoked a text model")
	}
	control.release(nil, true, true)
	h.awaitAnalysis(t, analysisTerminal)
	h.request(t, http.MethodPost, "/api/review", struct{}{}, &state)
	if len(state.Comments) != 5 || state.Comments[0].ID != left.ID || state.Comments[1].ID != right.ID || state.Files[0].Risk == nil {
		t.Fatalf("analysis/review clobbered manual work: %+v", state.Comments)
	}
	h.request(t, http.MethodPatch, "/api/comments/"+right.ID, map[string]string{"body": "Edited human range comment.", "status": "pending"}, &right)
	h.request(t, http.MethodDelete, "/api/comments/"+removed.ID, nil, &removed)
	h.request(t, http.MethodPost, "/api/publish", map[string]string{"event": "COMMENT"}, &state)
	h.fixture.mu.Lock()
	posts, payload := h.fixture.posts, h.fixture.last
	h.fixture.mu.Unlock()
	comments := payload["comments"].([]any)
	if posts != 1 || payload["commit_id"] != pr.HeadSHA || len(comments) != 2 {
		t.Fatalf("pending-only manual publication: %+v", payload)
	}
	old, next := comments[0].(map[string]any), comments[1].(map[string]any)
	if old["path"] != "src/calc.go" || old["side"] != "LEFT" || old["line"] != float64(2) || old["start_line"] != nil {
		t.Fatalf("LEFT single-line payload: %+v", old)
	}
	if next["path"] != "src/calc.go" || next["side"] != "RIGHT" || next["line"] != float64(3) || next["start_line"] != float64(2) || next["start_side"] != "RIGHT" || next["body"] != right.Body {
		t.Fatalf("RIGHT multiline payload: %+v", next)
	}
	if code := h.request(t, http.MethodPatch, "/api/comments/"+left.ID, map[string]string{"body": "Cannot change a published comment"}, &map[string]string{}); code != http.StatusBadRequest {
		t.Fatal("published manual comment was editable")
	}
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/8"}, &state)
	if len(state.Comments) != 0 {
		t.Fatal("PR8 inherited PR7 manual comments")
	}
	if code := h.request(t, http.MethodPost, "/api/comments", manualRequest(&pr, "RIGHT", 2, 2, "Wrong PR"), &map[string]string{}); code != http.StatusBadRequest {
		t.Fatal("stale PR identity accepted")
	}
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	if len(state.Comments) != 5 || state.Comments[0].Status != "published" || state.Comments[2].Status != "deleted" {
		t.Fatal("manual comments did not survive PR switch")
	}
	if err := h.app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(h.dataDir, state.Settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted := reopened.State()
	if len(persisted.Comments) != 5 || persisted.Comments[1].ID != right.ID || persisted.Comments[1].Body != right.Body || persisted.Comments[2] != removed {
		t.Fatalf("manual comments did not persist on disk: %+v", persisted.Comments)
	}
}

func TestHTTPManualCommentRejectsInvalidAndStaleAnchors(t *testing.T) {
	h := newHarness(t)
	h.fixture.mu.Lock()
	h.fixture.mixed = true
	h.fixture.mu.Unlock()
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	valid := manualRequest(state.PullRequest, "RIGHT", 2, 3, "Human comment")
	cases := []struct {
		name string
		edit func(*domain.ManualComment)
	}{
		{"blank", func(v *domain.ManualComment) { v.Body = " \n\t " }},
		{"oversized", func(v *domain.ManualComment) { v.Body = strings.Repeat("a", 60001) }},
		{"missing identity", func(v *domain.ManualComment) { v.PRKey = "" }},
		{"stale head", func(v *domain.ManualComment) { v.HeadSHA = "older" }},
		{"stale base", func(v *domain.ManualComment) { v.BaseSHA = "older" }},
		{"missing file", func(v *domain.ManualComment) { v.Path = "not-in-pr.go" }},
		{"outside hunk", func(v *domain.ManualComment) { v.EndLine = 4 }},
		{"reversed", func(v *domain.ManualComment) { v.StartLine = 3; v.EndLine = 2 }},
		{"zero", func(v *domain.ManualComment) { v.StartLine = 0 }},
		{"side", func(v *domain.ManualComment) { v.Side = "BOTH" }},
		{"truncated", func(v *domain.ManualComment) { v.Path = "generated.txt"; v.StartLine = 1; v.EndLine = 1 }},
		{"unavailable", func(v *domain.ManualComment) { v.Path = "assets/logo.png"; v.StartLine = 1; v.EndLine = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := valid
			tc.edit(&v)
			var failure map[string]string
			if code := h.request(t, http.MethodPost, "/api/comments", v, &failure); code != http.StatusBadRequest || failure["error"] == "" {
				t.Fatalf("invalid manual comment accepted: %d %+v", code, failure)
			}
		})
	}
	h.request(t, http.MethodGet, "/api/state", nil, &state)
	if len(state.Comments) != 0 {
		t.Fatal("invalid comments were persisted")
	}
	var c domain.Comment
	h.request(t, http.MethodPost, "/api/comments", valid, &c)
	h.fixture.mu.Lock()
	h.fixture.head = strings.Repeat("b", 40)
	h.fixture.mu.Unlock()
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	if len(state.Comments) != 1 || !state.Comments[0].Stale {
		t.Fatal("old manual draft was not retained as stale")
	}
	if code := h.request(t, http.MethodPost, "/api/comments", valid, &map[string]string{}); code != http.StatusBadRequest {
		t.Fatal("old snapshot accepted after reload")
	}
	if code := h.request(t, http.MethodPost, "/api/publish", map[string]string{"event": "COMMENT"}, &map[string]string{}); code != http.StatusBadRequest {
		t.Fatal("stale pending manual comment could be published")
	}
}
