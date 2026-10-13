package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"skim/internal/domain"
)

func publishedPR() domain.PullRequest {
	pr := loadedPR()
	pr.BaseSHA = "base-sha"
	return pr
}

func publishedFixture(id int64, parent int64) map[string]any {
	return map[string]any{
		"id": id, "in_reply_to_id": parent, "body": "Comment 日本語", "path": "src/日本語.go",
		"side": "RIGHT", "line": 9, "start_line": nil, "start_side": nil, "subject_type": "line",
		"user": map[string]any{"login": "reviewer[bot]"}, "diff_hunk": "@@ -8 +8 @@\n-old\n+new",
		"created_at": "2026-10-01T10:00:00Z", "updated_at": "2026-10-02T10:00:00Z",
		"html_url": "https://untrusted.invalid/comment", "original_line": 999,
	}
}

func TestReviewThreadsPagesAnchorsOrderingAndPublicAccess(t *testing.T) {
	root := publishedFixture(10, 0)
	root["side"], root["start_side"], root["start_line"], root["line"] = "LEFT", "LEFT", 3, 6
	reply12, reply11 := publishedFixture(12, 10), publishedFixture(11, 10)
	reply12["created_at"], reply11["created_at"] = "2026-10-02T10:00:00Z", "2026-10-02T10:00:00Z"
	reply11["user"] = nil
	outdated := publishedFixture(30, 0)
	outdated["line"], outdated["start_line"] = nil, nil
	nullAnchor := publishedFixture(31, 0)
	nullAnchor["line"], nullAnchor["original_line"] = nil, nil
	file := publishedFixture(40, 0)
	file["line"], file["subject_type"] = nil, "file"
	orphan := publishedFixture(51, 50)
	large := publishedFixture(9007199254740993, 0)
	metadataCalls, pageCalls := 0, 0
	g := fixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("public comments should not require a token")
		}
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/7":
			metadataCalls++
			respondJSON(t, w, pullFixture("head-sha", 1))
		case "/repos/owner/repo/pulls/7/comments":
			pageCalls++
			if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("sort") != "created" || r.URL.Query().Get("direction") != "asc" {
				t.Error("missing deterministic pagination query")
			}
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/repositories/123/pulls/7/comments?page=2>; rel="next"`, r.Host))
				respondJSON(t, w, []any{root, reply12})
			} else {
				respondJSON(t, w, []any{reply11, outdated, nullAnchor, file, orphan, large})
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	pr := publishedPR()
	result, err := g.ReviewThreads(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if metadataCalls != 2 || pageCalls != 2 || !result.PRIdentity.Matches(&pr) || len(result.Threads) != 6 || result.Warnings == nil {
		t.Fatalf("incomplete result: %+v; metadata=%d pages=%d", result, metadataCalls, pageCalls)
	}
	byID := make(map[string]domain.ReviewThread)
	for _, thread := range result.Threads {
		byID[thread.ID] = thread
		if thread.CanReply || thread.ReplyDisabledReason == "" || thread.Comments == nil {
			t.Fatalf("invalid public reply state: %+v", thread)
		}
		for _, comment := range thread.Comments {
			if strings.Contains(comment.URL, "untrusted") || !strings.HasPrefix(comment.URL, "https://github.com/owner/repo/pull/7#discussion_r") {
				t.Fatalf("untrusted comment URL: %s", comment.URL)
			}
		}
	}
	left := byID["10"]
	if left.Side != "LEFT" || left.StartLine != 3 || left.EndLine != 6 || left.Outdated || len(left.Comments) != 3 || left.Comments[0].ID != "10" || left.Comments[1].ID != "11" || left.Comments[2].ID != "12" {
		t.Fatalf("incorrect multiline thread or reply ordering: %+v", left)
	}
	if left.Comments[1].Author != "[deleted]" || left.Comments[1].Body != "Comment 日本語" {
		t.Fatalf("lost deleted author or Unicode: %+v", left)
	}
	for _, id := range []string{"30", "31", "50"} {
		thread := byID[id]
		if !thread.Outdated || thread.StartLine != 0 || thread.EndLine != 0 {
			t.Fatalf("fabricated current anchor for %s: %+v", id, thread)
		}
	}
	if byID["40"].Outdated || byID["40"].EndLine != 0 || byID["9007199254740993"].Comments[0].ID != "9007199254740993" || len(result.Warnings) != 1 {
		t.Fatalf("lost file-level, orphan, or large-ID discussion: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(encoded), `"id":"9007199254740993"`) {
		t.Fatalf("comment ID lost decimal-string representation: %s %v", encoded, err)
	}
}

func TestReviewThreadsRejectsUnsafeOrIncompletePages(t *testing.T) {
	for _, mode := range []string{"external", "wrong-path", "skipped-page", "duplicate-link", "empty-next", "duplicate-id", "second-page-error", "null-page", "too-many", "oversized-body", "invalid-id", "invalid-line", "invalid-timestamp"} {
		t.Run(mode, func(t *testing.T) {
			pageCalls := 0
			g := fixture(t, "secret", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/owner/repo/pulls/7" {
					respondJSON(t, w, pullFixture("head-sha", 1))
					return
				}
				if r.URL.Path != "/repos/owner/repo/pulls/7/comments" {
					t.Errorf("followed unsafe resource %s", r.URL.Path)
				}
				pageCalls++
				link := fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path)
				comment := publishedFixture(10, 0)
				switch mode {
				case "external":
					link = `<https://attacker.invalid/comments?page=2>; rel="next"`
				case "wrong-path":
					link = fmt.Sprintf(`<http://%s/repos/owner/repo/pulls/8/comments?page=2>; rel="next"`, r.Host)
				case "skipped-page":
					link = fmt.Sprintf(`<http://%s%s?page=3>; rel="next"`, r.Host, r.URL.Path)
				case "duplicate-link":
					link += ", " + link
				case "null-page":
					respondJSON(t, w, nil)
					return
				case "too-many":
					respondJSON(t, w, make([]any, 101))
					return
				case "oversized-body":
					comment["body"] = strings.Repeat("x", maxPublishedCommentBytes+1)
				case "invalid-id":
					comment["id"] = 0
				case "invalid-line":
					comment["line"] = -1
				case "invalid-timestamp":
					comment["created_at"] = "not a timestamp"
				}
				if pageCalls == 1 {
					w.Header().Set("Link", link)
				} else if mode == "second-page-error" {
					http.Error(w, "secret", http.StatusForbidden)
					return
				}
				if mode == "empty-next" {
					respondJSON(t, w, []any{})
				} else {
					respondJSON(t, w, []any{comment})
				}
			})
			result, err := g.ReviewThreads(context.Background(), publishedPR())
			if err == nil || len(result.Threads) != 0 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("accepted partial/unsafe result or leaked credentials: %+v %v", result, err)
			}
			wantPages := 1
			if mode == "duplicate-id" || mode == "second-page-error" {
				wantPages = 2
			}
			if pageCalls != wantPages {
				t.Fatalf("unexpected page requests: %d, want %d", pageCalls, wantPages)
			}
		})
	}
}

func TestReviewThreadsRejectsMovingSnapshot(t *testing.T) {
	for _, ref := range []string{"base", "head"} {
		for _, when := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s-check-%d", ref, when), func(t *testing.T) {
				metadataCalls, commentCalls := 0, 0
				g := fixture(t, "", func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/repos/owner/repo/pulls/7" {
						metadataCalls++
						metadata := pullFixture("head-sha", 1)
						if metadataCalls == when {
							metadata[ref] = map[string]any{"sha": "changed", "ref": "branch"}
						}
						respondJSON(t, w, metadata)
						return
					}
					commentCalls++
					respondJSON(t, w, []any{publishedFixture(10, 0)})
				})
				result, err := g.ReviewThreads(context.Background(), publishedPR())
				if err == nil || !strings.Contains(err.Error(), "reload") || len(result.Threads) != 0 || metadataCalls != when || commentCalls != when-1 {
					t.Fatalf("mixed snapshot exposed: %+v %v; metadata=%d comments=%d", result, err, metadataCalls, commentCalls)
				}
			})
		}
	}
}

func TestReplyToReviewThreadSuccessIsRootScopedAndDoesNotFetchAfterPost(t *testing.T) {
	posts, gets := 0, 0
	body := "  Reply 日本語\nkeep whitespace  "
	g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing authentication")
		}
		if r.Method == http.MethodPost {
			posts++
			if r.URL.Path != "/repos/owner/repo/pulls/7/comments/10/replies" || gets != 3 {
				t.Errorf("wrong root endpoint or missing preflight: %s GETs=%d", r.URL.Path, gets)
			}
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload) != 1 || payload["body"] != body {
				t.Errorf("changed reply text or payload: %+v %v", payload, err)
			}
			comment := publishedFixture(9007199254740993, 10)
			comment["body"] = body
			w.WriteHeader(http.StatusCreated)
			respondJSON(t, w, comment)
			return
		}
		gets++
		if posts != 0 {
			t.Error("success depended on a subsequent GET")
			http.Error(w, "should not be requested", 500)
			return
		}
		if r.URL.Path == "/repos/owner/repo/pulls/7" {
			respondJSON(t, w, pullFixture("head-sha", 1))
		} else {
			respondJSON(t, w, []any{publishedFixture(10, 0), publishedFixture(11, 10)})
		}
	})
	result, err := g.ReplyToReviewThread(context.Background(), publishedPR(), "10", body)
	if err != nil || posts != 1 || gets != 3 || result.ID != "9007199254740993" || result.Body != body || result.Author != "reviewer[bot]" || result.URL != "https://github.com/owner/repo/pull/7#discussion_r9007199254740993" || result.CreatedAt == "" || result.UpdatedAt == "" {
		t.Fatalf("incorrect definite reply success: %+v %v posts=%d GETs=%d", result, err, posts, gets)
	}
}

func TestReplyRejectsCrossPRChildAndMissingRoots(t *testing.T) {
	for _, target := range []string{"99", "11", "50"} {
		t.Run(target, func(t *testing.T) {
			posts := 0
			g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					http.Error(w, "unexpected POST", 500)
					return
				}
				if r.URL.Path == "/repos/owner/repo/pulls/7" {
					respondJSON(t, w, pullFixture("head-sha", 1))
				} else {
					respondJSON(t, w, []any{publishedFixture(10, 0), publishedFixture(11, 10), publishedFixture(51, 50)})
				}
			})
			_, err := g.ReplyToReviewThread(context.Background(), publishedPR(), target, "reply")
			if err == nil || !strings.Contains(err.Error(), "root comment") || posts != 0 {
				t.Fatalf("invalid root reached POST: %v posts=%d", err, posts)
			}
		})
	}
}

func TestReplyValidatesBodyIdentityAndAuthenticationBeforeNetwork(t *testing.T) {
	for _, test := range []struct{ token, id, body string }{
		{"", "10", "reply"}, {"token", "01", "reply"}, {"token", "-1", "reply"}, {"token", "9223372036854775808", "reply"},
		{"token", "10", "\n \t"}, {"token", "10", strings.Repeat("é", 30001)}, {"token", "10", "null\x00text"}, {"token", "10", string([]byte{0xff})},
	} {
		g := fixture(t, test.token, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("invalid input reached network: %s", r.URL.Path)
			http.Error(w, "unexpected network", 500)
		})
		if _, err := g.ReplyToReviewThread(context.Background(), publishedPR(), test.id, test.body); err == nil || errors.Is(err, ErrReplyDeliveryUncertain) {
			t.Fatalf("invalid input/auth not rejected definitively: %v", err)
		}
	}
}

func TestReplyErrorsNeverAutomaticallyResubmit(t *testing.T) {
	for _, mode := range []string{"network", "500", "408", "401", "403", "422", "malformed", "wrong-parent", "missing-fields"} {
		t.Run(mode, func(t *testing.T) {
			var posts atomic.Int32
			g := fixture(t, "secret", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					if r.URL.Path == "/repos/owner/repo/pulls/7" {
						respondJSON(t, w, pullFixture("head-sha", 1))
					} else {
						respondJSON(t, w, []any{publishedFixture(10, 0)})
					}
					return
				}
				posts.Add(1)
				switch mode {
				case "network":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				case "500":
					http.Error(w, "secret", 500)
				case "408":
					http.Error(w, "secret", 408)
				case "401":
					http.Error(w, "secret", 401)
				case "403":
					http.Error(w, "secret", 403)
				case "422":
					http.Error(w, "secret", 422)
				case "malformed":
					_, _ = w.Write([]byte("{secret"))
				case "wrong-parent":
					respondJSON(t, w, publishedFixture(11, 99))
				case "missing-fields":
					respondJSON(t, w, map[string]any{"id": 11})
				}
			})
			result, err := g.ReplyToReviewThread(context.Background(), publishedPR(), "10", "keep composed text")
			uncertain := mode != "401" && mode != "403" && mode != "422"
			if err == nil || errors.Is(err, ErrReplyDeliveryUncertain) != uncertain || result.ID != "" || posts.Load() != 1 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe retry/error behavior: %+v %v posts=%d", result, err, posts.Load())
			}
			if uncertain && (!strings.Contains(err.Error(), "keep your composed text") || !strings.Contains(err.Error(), "refresh") || !strings.Contains(err.Error(), "GitHub")) {
				t.Fatalf("uncertainty lacks actionable guidance: %v", err)
			}
		})
	}
}

func TestReviewThreadsAuthenticatedAnchorlessDiscussionsRemainVisible(t *testing.T) {
	crossSide := publishedFixture(10, 0)
	crossSide["start_line"], crossSide["start_side"], crossSide["side"] = 3, "LEFT", "RIGHT"
	outdated := publishedFixture(20, 0)
	outdated["line"] = nil
	file := publishedFixture(30, 0)
	file["line"], file["subject_type"] = nil, "file"
	g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/owner/repo/pulls/7" {
			respondJSON(t, w, pullFixture("head-sha", 1))
		} else {
			respondJSON(t, w, []any{crossSide, outdated, file})
		}
	})
	result, err := g.ReviewThreads(context.Background(), publishedPR())
	if err != nil || len(result.Threads) != 3 || len(result.Warnings) != 1 {
		t.Fatalf("anchorless discussions were lost: %+v %v", result, err)
	}
	for _, thread := range result.Threads {
		if !thread.CanReply || thread.ReplyDisabledReason != "" || thread.StartLine != 0 || thread.EndLine != 0 || len(thread.Comments) != 1 {
			t.Fatalf("invalid authenticated anchorless thread: %+v", thread)
		}
	}
}

func TestReviewThreadsPageCapFailsInsteadOfReturningPartialThreads(t *testing.T) {
	pages := 0
	g := fixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/owner/repo/pulls/7" {
			respondJSON(t, w, pullFixture("head-sha", 1))
			return
		}
		pages++
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, pages+1))
		respondJSON(t, w, []any{publishedFixture(int64(pages), 0)})
	})
	result, err := g.ReviewThreads(context.Background(), publishedPR())
	if err == nil || !strings.Contains(err.Error(), "100 pages") || len(result.Threads) != 0 || pages != maxReviewPages {
		t.Fatalf("page cap silently truncated discussions: %+v %v pages=%d", result, err, pages)
	}
}

func TestReplyRechecksRemoteSnapshotBeforePost(t *testing.T) {
	metadataCalls, posts := 0, 0
	g := fixture(t, "token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			http.Error(w, "must not post", 500)
			return
		}
		if r.URL.Path == "/repos/owner/repo/pulls/7" {
			metadataCalls++
			metadata := pullFixture("head-sha", 1)
			if metadataCalls == 2 {
				metadata["base"] = map[string]any{"sha": "new-base", "ref": "main"}
			}
			respondJSON(t, w, metadata)
		} else {
			respondJSON(t, w, []any{publishedFixture(10, 0)})
		}
	})
	_, err := g.ReplyToReviewThread(context.Background(), publishedPR(), "10", "reply")
	if err == nil || !strings.Contains(err.Error(), "reload") || posts != 0 || metadataCalls != 2 {
		t.Fatalf("reply used stale remote snapshot: %v posts=%d metadata=%d", err, posts, metadataCalls)
	}
}
