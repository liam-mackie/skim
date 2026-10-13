package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"skim/internal/domain"
)

func fixture(t *testing.T, token string, handler http.HandlerFunc) *GitHub {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	g := NewGitHub(token)
	g.BaseURL, g.Client = server.URL, server.Client()
	return g
}

func respondJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func pullFixture(head string, changed int) map[string]any {
	return map[string]any{"number": 7, "title": "Real fixture PR", "body": "description", "user": map[string]any{"login": "author"}, "head": map[string]any{"sha": head, "ref": "feature"}, "base": map[string]any{"sha": "base-sha", "ref": "main"}, "changed_files": changed}
}

func loadedPR() domain.PullRequest {
	return domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 7, HeadSHA: "head-sha"}
}

func TestCanonicalGitHubURL(t *testing.T) {
	for _, input := range []string{"https://github.com/Owner/repo.name/pull/7", "https://github.com/Owner/repo.name/pull/7/"} {
		pr, err := ParseGitHubURL(input)
		if err != nil || pr.Owner != "Owner" || pr.Repo != "repo.name" || pr.Number != 7 || pr.URL != "https://github.com/Owner/repo.name/pull/7" {
			t.Fatalf("%q: %#v, %v", input, pr, err)
		}
	}
	for _, input := range []string{
		"http://github.com/o/r/pull/1", "https://github.com.evil.test/o/r/pull/1", "https://github.com:443/o/r/pull/1",
		"https://secret@github.com/o/r/pull/1", "https://github.com/o/r/pull/1?token=secret", "https://github.com/o/r/pull/1?",
		"https://github.com/o/r/pull/1#diff", "https://github.com/o/r/pull/1#", "https://github.com/o/r/pull/1/files",
		"https://github.com/o/r/pull/0", "https://github.com/o/r/pull/-1", "https://github.com/o/r/pull/+1", "https://github.com/o/r/pull/01",
		"https://github.com/o/r/pull/999999999999999999999999", "https://github.com/o/r/pull/1//", "https://github.com/o/../pull/1",
		"https://github.com/o/r%2fevil/pull/1", "https://github.com/o/r/pull/%31", "https://github.com/o/r/issues/1", "not a URL",
	} {
		if _, err := ParseGitHubURL(input); err == nil {
			t.Errorf("accepted %q", input)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("leaked credentials: %v", err)
		}
	}
}

func TestLoadPaginationAndPatchStatuses(t *testing.T) {
	requests := 0
	g := fixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "" {
			t.Error("public load unexpectedly authenticated")
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Error("missing GitHub headers")
		}
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/7":
			respondJSON(t, w, pullFixture("head-sha", 3))
		case "/repos/owner/repo/pulls/7/files":
			if r.URL.Query().Get("per_page") != "100" {
				t.Error("wrong page size")
			}
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2&per_page=100>; rel="next"`, r.Host, r.URL.Path))
				respondJSON(t, w, []map[string]any{{"filename": "new.go", "previous_filename": "old.go", "status": "renamed", "additions": 1, "deletions": 1, "patch": "@@ -1 +1 @@\n-old\n+new"}})
			} else if r.URL.Query().Get("page") == "2" {
				respondJSON(t, w, []map[string]any{{"filename": "binary.png", "status": "added", "additions": 0, "deletions": 0}, {"filename": "partial.go", "status": "modified", "additions": 2, "deletions": 1, "patch": "@@ -1 +1 @@\n-old\n+new"}})
			} else {
				t.Error("unexpected page")
				http.Error(w, "bad page", 400)
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	pr, files, err := g.Load(context.Background(), "https://github.com/owner/repo/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 4 || len(files) != 3 {
		t.Fatalf("requests=%d files=%d", requests, len(files))
	}
	if pr.HeadSHA != "head-sha" || pr.BaseSHA != "base-sha" || pr.Author != "author" || pr.BaseRef != "main" || pr.HeadRef != "feature" {
		t.Fatalf("metadata: %#v", pr)
	}
	if files[0].PreviousPath != "old.go" || files[0].PatchStatus != "available" || files[1].PatchStatus != "unavailable" || files[2].PatchStatus != "truncated" {
		t.Fatalf("files: %#v", files)
	}
}

func TestLoadRejectsSameCountMovingHeadOrBase(t *testing.T) {
	for _, changedRef := range []string{"head", "base"} {
		t.Run(changedRef, func(t *testing.T) {
			metadataCalls, fileCalls := 0, 0
			g := fixture(t, "", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/owner/repo/pulls/7" {
					metadataCalls++
					metadata := pullFixture("head-sha", 2)
					if metadataCalls == 2 {
						metadata[changedRef] = map[string]any{"sha": "moved-sha", "ref": "branch"}
					}
					respondJSON(t, w, metadata)
					return
				}
				fileCalls++
				if fileCalls == 1 {
					w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
				}
				respondJSON(t, w, []map[string]any{{"filename": fmt.Sprintf("file-%d", fileCalls), "status": "modified", "additions": 1, "deletions": 1, "patch": "@@ -1 +1 @@\n-old\n+new"}})
			})
			pr, files, err := g.Load(context.Background(), "https://github.com/owner/repo/pull/7")
			if err == nil || !strings.Contains(err.Error(), "reload") || files != nil || pr.HeadSHA != "" {
				t.Fatalf("mixed snapshot returned: pr=%#v files=%v error=%v", pr, files, err)
			}
			if metadataCalls != 2 || fileCalls != 2 {
				t.Fatalf("expected final metadata check without automatic retry: metadata=%d files=%d", metadataCalls, fileCalls)
			}
		})
	}
}

func TestLoadRejectsPartialAndUnsafePagination(t *testing.T) {
	for _, mode := range []string{"over-cap", "missing-count", "partial", "duplicate", "external-next", "skip-page", "wrong-path", "empty-next"} {
		t.Run(mode, func(t *testing.T) {
			fileCalls := 0
			g := fixture(t, "top-secret", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer top-secret" {
					t.Error("missing bearer authorization")
				}
				if r.URL.Path == "/repos/owner/repo/pulls/7" {
					metadata := pullFixture("head-sha", 2)
					if mode == "over-cap" {
						metadata["changed_files"] = 3001
					}
					if mode == "missing-count" {
						delete(metadata, "changed_files")
					}
					respondJSON(t, w, metadata)
					return
				}
				fileCalls++
				file := map[string]any{"filename": "one", "status": "modified", "additions": 1, "deletions": 1, "patch": "@@ -1 +1 @@\n-a\n+b"}
				switch mode {
				case "duplicate":
					respondJSON(t, w, []any{file, file})
					return
				case "external-next":
					w.Header().Set("Link", `<https://evil.test/repos/owner/repo/pulls/7/files?page=2>; rel="next"`)
				case "skip-page":
					w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=3>; rel="next"`, r.Host, r.URL.Path))
				case "wrong-path":
					w.Header().Set("Link", fmt.Sprintf(`<http://%s/user?page=2>; rel="next"`, r.Host))
				case "empty-next":
					w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
					respondJSON(t, w, []any{})
					return
				}
				respondJSON(t, w, []any{file})
			})
			_, files, err := g.Load(context.Background(), "https://github.com/owner/repo/pull/7")
			if err == nil || files != nil {
				t.Fatalf("partial success: files=%v error=%v", files, err)
			}
			if strings.Contains(err.Error(), "top-secret") {
				t.Fatal("token leaked")
			}
			if (mode == "over-cap" || mode == "missing-count") && fileCalls != 0 {
				t.Fatal("fetched files after invalid metadata")
			}
			if fileCalls > 1 {
				t.Fatal("followed invalid pagination")
			}
		})
	}
}

func TestLoadExactlyThreeThousandFiles(t *testing.T) {
	for _, hasMore := range []bool{false, true} {
		t.Run(fmt.Sprint(hasMore), func(t *testing.T) {
			pages := 0
			g := fixture(t, "", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/owner/repo/pulls/7" {
					respondJSON(t, w, pullFixture("head-sha", 3000))
					return
				}
				pages++
				if pages < 30 || hasMore {
					w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, pages+1))
				}
				batch := make([]map[string]any, 100)
				for i := range batch {
					batch[i] = map[string]any{"filename": fmt.Sprintf("file-%d-%d", pages, i), "status": "modified", "additions": 0, "deletions": 0}
				}
				respondJSON(t, w, batch)
			})
			_, files, err := g.Load(context.Background(), "https://github.com/owner/repo/pull/7")
			if pages != 30 {
				t.Fatalf("fetched %d pages", pages)
			}
			if hasMore {
				if err == nil || files != nil || !strings.Contains(err.Error(), "3000") {
					t.Fatalf("expected explicit cap failure, got %v", err)
				}
			} else if err != nil || len(files) != 3000 {
				t.Fatalf("complete 3000-file load: %d, %v", len(files), err)
			}
		})
	}
}

func TestReviewSamplesUsesAuthenticatedAuthorAndPagination(t *testing.T) {
	calls := []string{}
	g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing bearer token")
		}
		if r.URL.Path == "/user" {
			respondJSON(t, w, map[string]any{"login": "Reviewer"})
			return
		}
		if r.URL.Path != "/repos/owner/repo/pulls/comments" || r.URL.Query().Get("sort") != "created" || r.URL.Query().Get("direction") != "desc" {
			t.Errorf("wrong sample endpoint: %s", r.URL)
		}
		comment := func(id int, author, body string) map[string]any {
			return map[string]any{"id": id, "user": map[string]any{"login": author}, "body": body}
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			respondJSON(t, w, []any{comment(5, "other", "not ours"), comment(4, "reviewer", "first sample"), comment(3, "Reviewer", "   ")})
		} else {
			respondJSON(t, w, []any{comment(4, "Reviewer", "first sample"), comment(2, "Reviewer", "second sample")})
		}
	})
	samples, err := g.ReviewSamples(context.Background(), "owner", "repo")
	if err != nil || len(samples) != 2 || samples[0] != "first sample" || samples[1] != "second sample" {
		t.Fatalf("samples=%v err=%v", samples, err)
	}
	if len(calls) != 3 || calls[0] != "/user" {
		t.Fatalf("calls: %v", calls)
	}
}

func TestPublishPayloadAndHeadCheck(t *testing.T) {
	for _, event := range []string{"COMMENT", "APPROVE", "REQUEST_CHANGES"} {
		t.Run(event, func(t *testing.T) {
			calls := []string{}
			g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if r.Method == http.MethodGet {
					respondJSON(t, w, pullFixture("head-sha", 1))
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/repos/owner/repo/pulls/7/reviews" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var payload map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				var commit, gotEvent, body string
				_ = json.Unmarshal(payload["commit_id"], &commit)
				_ = json.Unmarshal(payload["event"], &gotEvent)
				_ = json.Unmarshal(payload["body"], &body)
				if commit != "head-sha" || gotEvent != event || body != "Summary\n\n<!-- skim-review:unique-id -->" {
					t.Errorf("unexpected review payload: %s %s %q", commit, gotEvent, body)
				}
				var comments []map[string]any
				if err := json.Unmarshal(payload["comments"], &comments); err != nil {
					t.Fatal(err)
				}
				if len(comments) != 3 {
					t.Fatalf("sent %d comments", len(comments))
				}
				if comments[0]["line"] != float64(10) || comments[0]["side"] != "LEFT" || comments[0]["body"] != "deleted line" {
					t.Errorf("single line: %v", comments[0])
				}
				if _, ok := comments[0]["start_line"]; ok {
					t.Error("single line includes start_line")
				}
				if _, ok := comments[0]["start_side"]; ok {
					t.Error("single line includes start_side")
				}
				for i, side := range []string{"LEFT", "RIGHT"} {
					comment := comments[i+1]
					if comment["start_line"] != float64(11) || comment["line"] != float64(13) || comment["side"] != side || comment["start_side"] != side {
						t.Errorf("multiline: %v", comment)
					}
					if _, ok := comment["position"]; ok {
						t.Error("legacy position field sent")
					}
				}
				w.WriteHeader(http.StatusCreated)
				respondJSON(t, w, map[string]any{"id": 42})
			})
			comments := []domain.Comment{
				{Path: "file", StartLine: 10, EndLine: 10, Side: "LEFT", Body: "deleted line", HeadSHA: "head-sha"},
				{Path: "file", StartLine: 11, EndLine: 13, Side: "LEFT", Body: "left range", HeadSHA: "head-sha"},
				{Path: "file", StartLine: 11, EndLine: 13, Side: "RIGHT", Body: "right range", HeadSHA: "head-sha"},
			}
			id, err := g.Publish(context.Background(), loadedPR(), comments, event, "Summary", "unique-id")
			if err != nil || id != 42 {
				t.Fatalf("id=%d err=%v", id, err)
			}
			if len(calls) != 2 || calls[0] != "GET /repos/owner/repo/pulls/7" || calls[1] != "POST /repos/owner/repo/pulls/7/reviews" {
				t.Fatalf("head must immediately precede one POST: %v", calls)
			}
		})
	}
}

func TestPublishRejectsChangedHead(t *testing.T) {
	posts := 0
	g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			respondJSON(t, w, map[string]any{"id": 99})
			return
		}
		respondJSON(t, w, pullFixture("changed-sha", 1))
	})
	if _, err := g.Publish(context.Background(), loadedPR(), nil, "APPROVE", "", "marker"); err == nil || !strings.Contains(err.Error(), "head changed") {
		t.Fatalf("error=%v", err)
	}
	if posts != 0 {
		t.Fatal("posted against stale head")
	}
}

func TestPublishRejectsInvalidLocalInputs(t *testing.T) {
	g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid input made a remote request")
		http.Error(w, "unexpected", 500)
	})
	for _, tc := range []struct {
		event, body, marker string
		comments            []domain.Comment
	}{
		{"COMMENT", "", "marker", nil}, {"PENDING", "body", "marker", nil}, {"APPROVE", "", "", nil}, {"REQUEST_CHANGES", "", "marker", nil},
		{"COMMENT", "body", "marker", []domain.Comment{{Path: "file", StartLine: 1, EndLine: 1, Side: "RIGHT", Body: "body", HeadSHA: "old-head"}}},
		{"COMMENT", "body", "marker", []domain.Comment{{Path: "file", StartLine: 1, EndLine: 1, Side: "RIGHT", Body: "body", HeadSHA: "head-sha", Stale: true}}},
		{"COMMENT", "body", "marker", []domain.Comment{{Path: "file", StartLine: 1, EndLine: 1, Side: "BAD", Body: "body", HeadSHA: "head-sha"}}},
	} {
		if _, err := g.Publish(context.Background(), loadedPR(), tc.comments, tc.event, tc.body, tc.marker); err == nil {
			t.Errorf("accepted %#v", tc)
		}
	}
}

func TestPublishAmbiguityAndNoAutomaticRetry(t *testing.T) {
	for _, mode := range []string{"server-error", "timeout-status", "disconnect", "bad-json", "missing-id", "validation-error", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			var posts atomic.Int32
			g := fixture(t, "top-secret", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					respondJSON(t, w, pullFixture("head-sha", 0))
					return
				}
				posts.Add(1)
				switch mode {
				case "server-error":
					http.Error(w, "top-secret", 503)
				case "timeout-status":
					http.Error(w, "top-secret", 408)
				case "validation-error":
					http.Error(w, "top-secret", 422)
				case "unauthorized":
					http.Error(w, "top-secret", 401)
				case "bad-json":
					w.WriteHeader(201)
					_, _ = io.WriteString(w, "top-secret")
				case "missing-id":
					respondJSON(t, w, map[string]any{})
				case "disconnect":
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
				}
			})
			_, err := g.Publish(context.Background(), loadedPR(), nil, "APPROVE", "", "id")
			if err == nil {
				t.Fatal("expected error")
			}
			var ambiguous *AmbiguousError
			wantAmbiguous := mode != "validation-error" && mode != "unauthorized"
			if errors.As(err, &ambiguous) != wantAmbiguous {
				t.Fatalf("ambiguous=%v: %v", wantAmbiguous, err)
			}
			if posts.Load() != 1 {
				t.Fatalf("POST retried: %d", posts.Load())
			}
			if strings.Contains(err.Error(), "top-secret") {
				t.Fatal("token leaked")
			}
		})
	}
}

func TestFindReviewMatchesSubmittedAuthorHeadAndFooter(t *testing.T) {
	for _, found := range []bool{true, false} {
		t.Run(fmt.Sprint(found), func(t *testing.T) {
			pages := 0
			g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/user" {
					respondJSON(t, w, map[string]any{"login": "reviewer"})
					return
				}
				if r.Method != "GET" || r.URL.Path != "/repos/owner/repo/pulls/7/reviews" {
					t.Errorf("unexpected reconciliation request: %s %s", r.Method, r.URL.Path)
				}
				pages++
				review := func(id int, author, head, state, body string, submitted any) map[string]any {
					return map[string]any{"id": id, "user": map[string]any{"login": author}, "commit_id": head, "state": state, "body": body, "submitted_at": submitted}
				}
				marker := "<!-- skim-review:id -->"
				if pages == 1 {
					w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
					respondJSON(t, w, []any{
						review(1, "other", "head-sha", "APPROVED", marker, "2026-01-01"),
						review(2, "reviewer", "old-sha", "APPROVED", marker, "2026-01-01"),
						review(3, "reviewer", "head-sha", "PENDING", marker, nil),
						review(4, "reviewer", "head-sha", "APPROVED", "quoted "+marker+" text", "2026-01-01"),
						review(5, "reviewer", "head-sha", "APPROVED", marker, nil),
					})
				} else if found {
					respondJSON(t, w, []any{review(42, "Reviewer", "head-sha", "COMMENTED", "Summary\n\n"+marker, "2026-01-01")})
				} else {
					respondJSON(t, w, []any{})
				}
			})
			id, err := g.FindReview(context.Background(), loadedPR(), "<!-- skim-review:id -->")
			if err != nil || (id == 42) != found || (!found && id != 0) || pages != 2 {
				t.Fatalf("id=%d pages=%d error=%v", id, pages, err)
			}
		})
	}
}

func TestRedirectsNeverForwardAuthentication(t *testing.T) {
	redirected := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++; respondJSON(t, w, map[string]any{}) }))
	defer target.Close()
	g := fixture(t, "top-secret", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	g.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	_, _, err := g.Load(context.Background(), "https://github.com/owner/repo/pull/7")
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error=%v", err)
	}
	if redirected != 0 {
		t.Fatalf("followed %d redirects", redirected)
	}
}

func TestResponseBoundAndMalformedJSON(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		g := fixture(t, "top-secret", func(w http.ResponseWriter, r *http.Request) {
			if oversized {
				_, _ = io.WriteString(w, strings.Repeat(" ", maxResponseBytes+1))
				return
			}
			_, _ = io.WriteString(w, `{"token":"top-secret"} trailing`)
		})
		if _, _, err := g.Load(context.Background(), "https://github.com/owner/repo/pull/7"); err == nil || strings.Contains(err.Error(), "top-secret") {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestUnauthenticatedMutationsAndSamplesRejected(t *testing.T) {
	g := fixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		t.Error("unauthenticated operation reached API")
		http.Error(w, "unexpected", 500)
	})
	if _, err := g.ReviewSamples(context.Background(), "owner", "repo"); err == nil {
		t.Error("samples accepted without token")
	}
	if _, err := g.Publish(context.Background(), loadedPR(), nil, "APPROVE", "", "id"); err == nil {
		t.Error("publish accepted without token")
	}
	if _, err := g.FindReview(context.Background(), loadedPR(), "id"); err == nil {
		t.Error("reconciliation accepted without token")
	}
}

func TestLostPublishResponseReconcilesActualReview(t *testing.T) {
	posts := 0
	var submitted map[string]any
	g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			respondJSON(t, w, map[string]any{"login": "reviewer"})
		case r.Method == http.MethodPost:
			posts++
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Error(err)
				return
			}
			submitted["id"] = 91
			submitted["state"] = "APPROVED"
			submitted["submitted_at"] = "2026-01-01T00:00:00Z"
			submitted["user"] = map[string]any{"login": "reviewer"}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			if submitted == nil {
				t.Error("reconciling before submission")
				respondJSON(t, w, []any{})
				return
			}
			respondJSON(t, w, []any{submitted})
		default:
			respondJSON(t, w, pullFixture("head-sha", 0))
		}
	})
	_, err := g.Publish(context.Background(), loadedPR(), nil, "APPROVE", "Looks good", "lost-response")
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("expected ambiguous delivery: %v", err)
	}
	id, err := g.FindReview(context.Background(), loadedPR(), "lost-response")
	if err != nil || id != 91 || posts != 1 {
		t.Fatalf("id=%d posts=%d err=%v", id, posts, err)
	}
}

func TestReviewSamplesAreBoundedAndNeverInvented(t *testing.T) {
	for _, ours := range []bool{false, true} {
		t.Run(fmt.Sprint(ours), func(t *testing.T) {
			pages := 0
			g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/user" {
					respondJSON(t, w, map[string]any{"login": "reviewer"})
					return
				}
				pages++
				w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, pages+1))
				author := "someone-else"
				if ours {
					author = "reviewer"
				}
				batch := make([]map[string]any, 100)
				for i := range batch {
					batch[i] = map[string]any{"id": pages*100 + i, "user": map[string]any{"login": author}, "body": fmt.Sprintf("actual comment %d", i)}
				}
				respondJSON(t, w, batch)
			})
			samples, err := g.ReviewSamples(context.Background(), "owner", "repo")
			if err != nil {
				t.Fatal(err)
			}
			if ours {
				if len(samples) != maxSamples || pages != 1 {
					t.Fatalf("samples=%d pages=%d", len(samples), pages)
				}
			} else if len(samples) != 0 || pages != maxSamplePages {
				t.Fatalf("invented samples or unbounded scan: samples=%d pages=%d", len(samples), pages)
			}
		})
	}
}

func TestReconciliationDoesNotClaimAbsenceAfterPageLimit(t *testing.T) {
	pages := 0
	g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			respondJSON(t, w, map[string]any{"login": "reviewer"})
			return
		}
		pages++
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, pages+1))
		respondJSON(t, w, []map[string]any{{"id": pages, "state": "COMMENTED", "submitted_at": "2026-01-01", "commit_id": "head-sha", "user": map[string]any{"login": "reviewer"}, "body": "unrelated review"}})
	})
	id, err := g.FindReview(context.Background(), loadedPR(), "absent")
	if err == nil || id != 0 || pages != maxReviewPages {
		t.Fatalf("id=%d pages=%d err=%v", id, pages, err)
	}
}

func TestNextPageCanonicalRepositoryAlias(t *testing.T) {
	current, _ := url.Parse("https://api.github.com/repos/owner/repo/pulls/comments?page=1&per_page=2")
	link := func(target string) http.Header {
		return http.Header{"Link": {"<" + target + `>; rel="next"`}}
	}
	// Captured from GET /repos/golang/go/pulls/comments?per_page=2 on 2026-10-09.
	accepted := []string{
		"https://api.github.com/repositories/23096959/pulls/comments?per_page=2&page=2",
		"https://api.github.com/repos/owner/repo/pulls/comments?page=2",
	}
	for _, target := range accepted {
		if next, err := nextPage(link(target), current, 1); err != nil || !next {
			t.Errorf("%s: next=%v err=%v", target, next, err)
		}
	}
	rejected := []string{
		"https://api.github.com/repositories/23096959/pulls/other?page=2",
		"https://api.github.com/repositories/23096959/pulls/comments/extra?page=2",
		"https://api.github.com/repositories/abc/pulls/comments?page=2",
		"https://api.github.com/repositories//pulls/comments?page=2",
		"https://api.github.com/repositories/1/pulls/comments?page=3",
		"https://api.github.com/repositories/1/pulls/comments?page=2&page=2",
		"https://api.github.com/repos/other/repo/pulls/comments?page=2",
		"https://evil.test/repositories/1/pulls/comments?page=2",
		"https://user@api.github.com/repositories/1/pulls/comments?page=2",
		"http://api.github.com/repositories/1/pulls/comments?page=2",
	}
	for _, target := range rejected {
		if _, err := nextPage(link(target), current, 1); err == nil {
			t.Errorf("accepted %s", target)
		}
	}
}

func TestReviewSamplesFollowsCanonicalRepositoryLinkWithOriginalRequest(t *testing.T) {
	var requests []string
	g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			respondJSON(t, w, map[string]any{"login": "me"})
			return
		}
		requests = append(requests, r.URL.RequestURI())
		comment := func(id int, author string) map[string]any {
			return map[string]any{"id": id, "user": map[string]any{"login": author}, "body": fmt.Sprintf("body %d", id)}
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repositories/99/pulls/comments?page=2&per_page=100&evil=1>; rel="next"`, r.Host))
			respondJSON(t, w, []any{comment(1, "someone")})
			return
		}
		respondJSON(t, w, []any{comment(2, "me")})
	})
	samples, err := g.ReviewSamples(context.Background(), "owner", "repo")
	if err != nil || len(samples) != 1 || samples[0] != "body 2" {
		t.Fatalf("samples=%v err=%v", samples, err)
	}
	if len(requests) != 2 || !strings.HasPrefix(requests[1], "/repos/owner/repo/pulls/comments?") || strings.Contains(requests[1], "evil") {
		t.Fatalf("follow-up not rebuilt from original request: %v", requests)
	}
}
