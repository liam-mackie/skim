package httpapi

import (
	"reflect"
	"testing"

	"skim/internal/domain"
)

func TestHTTPFileReviewAtomicScopeAndBackgroundAnalysis(t *testing.T) {
	control := newAnalysisModelControl()
	control.hold([]string{"src/calc.go", "src/calc_test.go"}, true)
	h := newHarnessWithModels(t, control)
	defer control.release(nil, true, true)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	pr := *state.PullRequest
	body := func(paths []string, reviewed any) map[string]any {
		return map[string]any{"prKey": pr.Key(), "baseSha": pr.BaseSHA, "headSha": pr.HeadSHA, "paths": paths, "reviewed": reviewed}
	}
	var update domain.FileReviewUpdate
	if code := h.request(t, "POST", "/api/reviewed", body([]string{"src/calc.go", "src/calc_test.go"}, true), &update); code != 200 {
		t.Fatalf("batch status %d", code)
	}
	if update.PRKey != pr.Key() || update.FileReview.Revision != 1 || !reflect.DeepEqual(update.FileReview.ReviewedPaths, []string{"src/calc.go", "src/calc_test.go"}) {
		t.Fatalf("review resource: %+v", update)
	}
	before := h.store.Snapshot().Revision
	invalid := []map[string]any{
		body([]string{"src/calc.go", "unknown.go"}, false),
		body([]string{"src/calc.go", "src/calc.go"}, false),
		body(nil, false), body([]string{"src/calc.go"}, nil),
		{"prKey": pr.Key(), "baseSha": pr.BaseSHA, "headSha": pr.HeadSHA, "paths": []string{"src/calc.go"}},
		{"prKey": pr.Key(), "baseSha": pr.BaseSHA, "headSha": "stale", "paths": []string{"src/calc.go"}, "reviewed": false},
	}
	for _, request := range invalid {
		if code := h.request(t, "POST", "/api/reviewed", request, &map[string]string{}); code != 400 {
			t.Fatalf("accepted invalid request %+v: %d", request, code)
		}
	}
	h.request(t, "POST", "/api/reviewed", body([]string{"src/calc_test.go", "src/calc.go"}, true), &update)
	if h.store.Snapshot().Revision != before || update.FileReview.Revision != 1 {
		t.Fatal("invalid or unchanged batch committed progress")
	}
	var manual domain.Comment
	h.request(t, "POST", "/api/comments", concurrentManual(state, "Keep this pending comment."), &manual)
	h.request(t, "POST", "/api/reviewed", body([]string{"src/calc.go"}, false), &update)
	control.release(nil, true, true)
	h.awaitAnalysis(t, analysisTerminal)
	h.request(t, "GET", "/api/state", nil, &state)
	if state.FileReview.Revision != 2 || !reflect.DeepEqual(state.FileReview.ReviewedPaths, []string{"src/calc_test.go"}) || state.Comments[0].ID != manual.ID {
		t.Fatal("analysis or local writes lost reviewed state or comments")
	}
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/8"}, &state)
	h.request(t, "POST", "/api/reviewed", body([]string{"src/calc.go"}, true), &update)
	h.request(t, "GET", "/api/state", nil, &state)
	if state.PullRequest.Number != 8 || state.FileReview != nil {
		t.Fatal("scoped review write changed another active PR")
	}
}
