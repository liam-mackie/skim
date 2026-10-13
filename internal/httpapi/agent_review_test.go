package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"skim/internal/domain"
)

func reviewProgressPath(pr *domain.PullRequest) string {
	return "/api/review/progress?" + url.Values{"prKey": {pr.Key()}, "baseSha": {pr.BaseSHA}, "headSha": {pr.HeadSHA}}.Encode()
}

func TestHTTPAgentReviewSavesSuggestionsReportAndRejections(t *testing.T) {
	h := newHarness(t)
	report := "Inspected `git diff BASE HEAD` and src/calc_test.go; no commands were run.\n\n```json\n" +
		`{"comments":[` +
		`{"path":"src/calc.go","startLine":2,"endLine":2,"side":"RIGHT","body":"The extra one changes every sum.\n\n` + "```suggestion\\nfunc Add(a, b int) int { return a + b }\\n```" + `"},` +
		`{"path":"src/calc_test.go","startLine":2,"endLine":2,"side":"LEFT","body":"The old expectation matched Add."},` +
		`{"path":"src/calc.go","startLine":9,"endLine":9,"side":"RIGHT","body":"Outside the diff."}]}` +
		"\n```\n"
	h.agentReview(t, report, false)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.request(t, "POST", "/api/review", struct{}{}, &state)
	if len(state.Comments) != 2 {
		t.Fatalf("expected the two anchored comments, got %+v", state.Comments)
	}
	for i, want := range []struct{ path, side string }{{"src/calc.go", "RIGHT"}, {"src/calc_test.go", "LEFT"}} {
		c := state.Comments[i]
		if c.Path != want.path || c.Side != want.side || c.StartLine != 2 || c.Status != "suggested" || c.Source != "review" || c.HeadSHA != state.PullRequest.HeadSHA {
			t.Fatalf("comment %d was not saved as a suggestion: %+v", i, c)
		}
	}
	if !strings.Contains(state.Comments[0].Body, "```suggestion\nfunc Add(a, b int) int { return a + b }\n```") {
		t.Fatalf("code suggestion lost: %q", state.Comments[0].Body)
	}
	var raw struct {
		ReviewRun struct {
			Report   string                   `json:"report"`
			Rejected []domain.RejectedComment `json:"rejected"`
			Error    *string                  `json:"error"`
		} `json:"reviewRun"`
	}
	var body json.RawMessage
	h.request(t, "GET", "/api/state", nil, &body)
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	run := raw.ReviewRun
	if run.Report != report || run.Error != nil || len(run.Rejected) != 1 {
		t.Fatalf("review run did not keep the report and one rejection: %s", body)
	}
	if r := run.Rejected[0]; r.Path != "src/calc.go" || r.StartLine != 9 || r.Body != "Outside the diff." || !strings.Contains(r.Reason, "diff hunk") {
		t.Fatalf("rejected comment lost its anchor or reason: %+v", r)
	}
	var progress domain.ReviewProgressUpdate
	h.request(t, "GET", reviewProgressPath(state.PullRequest), nil, &progress)
	if p := progress.Run; p == nil || p.Status != "complete" || p.Phase != "finished" || p.Findings != 2 || p.Rejected != 1 || p.Error != "" {
		t.Fatalf("progress did not finish with the review counts: %+v", progress.Run)
	}
}

func TestHTTPAgentReviewWithoutCommentsBlockKeepsReportAndFails(t *testing.T) {
	h := newHarness(t)
	report := "Inspected src/calc.go; the arithmetic change looks intentional."
	h.agentReview(t, report, false)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	var failure map[string]string
	if status := h.request(t, "POST", "/api/review", struct{}{}, &failure); status != http.StatusBadRequest || !strings.Contains(failure["error"], "report is saved") {
		t.Fatalf("review without a comments block: HTTP %d %v", status, failure)
	}
	h.request(t, "GET", "/api/state", nil, &state)
	if len(state.Comments) != 0 || state.ReviewRun == nil || state.ReviewRun.Report != report || !strings.Contains(state.ReviewRun.Error, "no comments JSON block") {
		t.Fatalf("failed review did not keep its report and error: %+v %+v", state.Comments, state.ReviewRun)
	}
	var progress domain.ReviewProgressUpdate
	h.request(t, "GET", reviewProgressPath(state.PullRequest), nil, &progress)
	if p := progress.Run; p == nil || p.Status != "failed" || p.Phase != "finished" || p.Error == "" {
		t.Fatalf("progress did not report the failure: %+v", progress.Run)
	}
}
