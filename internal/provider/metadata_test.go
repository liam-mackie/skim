package provider

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"skim/internal/domain"
)

func inboxItem(owner, repo string, number int, updated string) map[string]any {
	return map[string]any{"number": number, "title": "Review <unsafe> & Unicode Ω", "html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number), "state": "open", "updated_at": updated, "draft": number == 7, "pull_request": map[string]any{}, "user": map[string]string{"login": "contributor"}}
}

func TestInboxUnionPaginationOrderingAndCanonicalIdentity(t *testing.T) {
	searches := 0
	g := fixture(t, "fixture-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("authentication not sent")
		}
		if r.URL.Path == "/user" {
			respondJSON(t, w, map[string]string{"login": "reviewer"})
			return
		}
		if r.URL.Path != "/search/issues" {
			t.Errorf("inbox loaded unexpected resource %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		searches++
		query := r.URL.Query()
		if query.Get("sort") != "updated" || query.Get("order") != "desc" || query.Get("per_page") != "100" {
			t.Error("missing ordering/pagination query")
		}
		items := []any{}
		switch query.Get("q") {
		case "is:open is:pr review-requested:someone repo:Acme/Widgets":
			if query.Get("page") == "1" {
				items = append(items, inboxItem("Acme", "Widgets", 7, "2026-10-01T12:00:00Z"))
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/search/issues?page=2&q=untrusted>; rel="next"`, r.Host))
			} else {
				items = append(items, inboxItem("Acme", "Widgets", 8, "2026-10-02T12:00:00Z"))
			}
		case "is:open is:pr assignee:someone repo:Acme/Widgets":
			items = append(items, inboxItem("acme", "widgets", 7, "2026-10-03T12:00:00Z"), inboxItem("Acme", "Widgets", 9, "2026-10-02T12:00:00Z"))
		default:
			t.Errorf("query changed or was not escaped: %q", query.Get("q"))
		}
		respondJSON(t, w, map[string]any{"total_count": 2, "incomplete_results": false, "items": items})
	})
	got, err := g.Inbox(context.Background(), domain.InboxFilter{Username: "someone", Repo: "Acme/Widgets"})
	if err != nil || !got.Complete || len(got.Warnings) != 0 || len(got.Reviews) != 3 || searches != 3 {
		t.Fatalf("inbox=%+v searches=%d error=%v", got, searches, err)
	}
	if got.User.Login != "reviewer" || got.Username != "someone" || got.Repo != "Acme/Widgets" {
		t.Fatalf("wrong scope: %+v", got)
	}
	for i, number := range []int{7, 8, 9} {
		if got.Reviews[i].Number != number {
			t.Fatalf("unstable order: %+v", got.Reviews)
		}
	}
	both := got.Reviews[0]
	if !both.Requested || !both.Assigned || !both.Draft || both.PRKey != "github/Acme/Widgets/7" || both.URL != "https://github.com/Acme/Widgets/pull/7" || both.Title != "Review <unsafe> & Unicode Ω" {
		t.Fatalf("lost sources or canonical identity: %+v", both)
	}
	if got.Reviews[1].Assigned || got.Reviews[2].Requested {
		t.Fatalf("invented source flags: %+v", got.Reviews)
	}
}

func TestInboxReportsIncompleteAndCappedSearch(t *testing.T) {
	for _, mode := range []string{"incomplete", "missing-page", "cap", "empty"} {
		t.Run(mode, func(t *testing.T) {
			pages := 0
			g := fixture(t, "fixture-token", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/user" {
					respondJSON(t, w, map[string]string{"login": "reviewer"})
					return
				}
				if !strings.Contains(r.URL.Query().Get("q"), ":reviewer") {
					t.Error("did not default to authenticated login")
				}
				items, total := []any{}, 0
				if strings.Contains(r.URL.Query().Get("q"), "review-requested:") {
					pages++
					if mode != "empty" {
						items = append(items, inboxItem("o", "r", pages, "2026-10-01T00:00:00Z"))
						total = 2
					}
					if mode == "cap" {
						total = 1001
						w.Header().Set("Link", fmt.Sprintf(`<http://%s/search/issues?page=%d>; rel="next"`, r.Host, pages+1))
					}
				}
				respondJSON(t, w, map[string]any{"total_count": total, "incomplete_results": mode == "incomplete", "items": items})
			})
			got, err := g.Inbox(context.Background(), domain.InboxFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "empty" {
				if !got.Complete || got.Reviews == nil || len(got.Reviews) != 0 {
					t.Fatalf("empty result: %+v", got)
				}
			} else if got.Complete || len(got.Warnings) == 0 {
				t.Fatalf("silent truncation: %+v", got)
			}
			if mode == "cap" && (pages != 10 || !strings.Contains(got.Warnings[0], "1,000")) {
				t.Fatalf("cap pages=%d warnings=%v", pages, got.Warnings)
			}
		})
	}
}

func TestInboxAuthenticationAndUnsafeResponses(t *testing.T) {
	for _, mode := range []string{"missing-token", "unauthorized", "forbidden", "redirect", "unsafe-link", "unsafe-url", "invalid-user-filter", "invalid-repo-filter"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			token := "fixture-secret"
			if mode == "missing-token" {
				token = ""
			}
			g := fixture(t, token, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "unauthorized" || mode == "forbidden" {
					status := 401
					if mode == "forbidden" {
						status = 403
					}
					http.Error(w, "fixture-secret", status)
					return
				}
				if mode == "redirect" {
					http.Redirect(w, r, "/secret", 302)
					return
				}
				if r.URL.Path == "/user" {
					respondJSON(t, w, map[string]string{"login": "reviewer"})
					return
				}
				item := inboxItem("o", "r", 7, "2026-10-01T00:00:00Z")
				if mode == "unsafe-url" {
					item["html_url"] = "https://evil.test/o/r/pull/7"
				}
				if mode == "unsafe-link" {
					w.Header().Set("Link", `<https://evil.test/search/issues?page=2>; rel="next"`)
				}
				respondJSON(t, w, map[string]any{"total_count": 1, "incomplete_results": false, "items": []any{item}})
			})
			filter := domain.InboxFilter{}
			if mode == "invalid-user-filter" {
				filter.Username = "reviewer is:closed"
			}
			if mode == "invalid-repo-filter" {
				filter.Repo = "o/r assignee:other"
			}
			_, err := g.Inbox(context.Background(), filter)
			if err == nil || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("unsafe success/error: %v", err)
			}
			if (mode == "missing-token" || strings.HasPrefix(mode, "invalid-")) && calls != 0 {
				t.Fatal("made request with missing credentials/invalid filter")
			}
			if calls > 2 {
				t.Fatal("followed unsafe request")
			}
		})
	}
}

func commitItem(number int) map[string]any {
	return map[string]any{"sha": fmt.Sprintf("%040x", number), "html_url": "https://evil.test/ignored", "author": map[string]string{"login": "author"}, "commit": map[string]any{"message": "Subject <script> & Ω\n\nFull body\n  whitespace stays\n", "author": map[string]string{"name": "Author Ω", "date": "2026-10-01T01:02:03Z"}}}
}

func TestCommitsPaginationAndExplicitLimit(t *testing.T) {
	for _, total := range []int{0, 201, 250, 251} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			pages, metadataCalls := 0, 0
			g := fixture(t, "", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/Owner/Repo/pulls/7" {
					metadataCalls++
					metadata := pullFixture("head-sha", 0)
					metadata["commits"] = total
					respondJSON(t, w, metadata)
					return
				}
				if r.URL.Path != "/repos/Owner/Repo/pulls/7/commits" || r.URL.Query().Get("per_page") != "100" {
					t.Errorf("wrong commits request: %s", r.URL)
				}
				pages++
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				limit := min(total, 250)
				items := []any{}
				for n := (page-1)*100 + 1; n <= min(page*100, limit); n++ {
					items = append(items, commitItem(n))
				}
				if page*100 < limit {
					w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
				}
				respondJSON(t, w, items)
			})
			pr := domain.PullRequest{Provider: "github", Owner: "Owner", Repo: "Repo", Number: 7, BaseSHA: "base-sha", HeadSHA: "head-sha"}
			got, err := g.Commits(context.Background(), pr)
			if err != nil || len(got.Commits) != min(total, 250) || got.Total != total || metadataCalls != 2 || pages != max(1, (min(total, 250)+99)/100) {
				t.Fatalf("commits=%+v pages=%d metadata=%d err=%v", got, pages, metadataCalls, err)
			}
			if got.Complete != (total <= 250) || (total > 250 && len(got.Warnings) == 0) || !got.PRIdentity.Matches(&pr) {
				t.Fatalf("wrong completeness/identity: %+v", got)
			}
			if total > 0 {
				c := got.Commits[0]
				if c.SHA != fmt.Sprintf("%040x", 1) || c.Message != "Subject <script> & Ω\n\nFull body\n  whitespace stays\n" || c.AuthorName != "Author Ω" || c.AuthorLogin != "author" || c.AuthoredAt != "2026-10-01T01:02:03Z" || c.URL != "https://github.com/Owner/Repo/commit/"+c.SHA {
					t.Fatalf("lost text or unsafe link: %+v", c)
				}
			}
		})
	}
}

func TestCommitsRejectMovingRefsAndUnsafePagination(t *testing.T) {
	for _, mode := range []string{"old-head", "moving-head", "moving-base", "moving-count", "unsafe-link", "bad-sha", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			metadataCalls, commitCalls := 0, 0
			g := fixture(t, "fixture-secret", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/owner/repo/pulls/7" {
					metadataCalls++
					metadata := pullFixture("head-sha", 0)
					metadata["commits"] = 1
					if mode == "old-head" || (mode == "moving-head" && metadataCalls == 2) {
						metadata["head"] = map[string]string{"sha": "new-head"}
					}
					if mode == "moving-base" && metadataCalls == 2 {
						metadata["base"] = map[string]string{"sha": "new-base"}
					}
					if mode == "moving-count" && metadataCalls == 2 {
						metadata["commits"] = 2
					}
					respondJSON(t, w, metadata)
					return
				}
				commitCalls++
				item := commitItem(1)
				items := []any{item}
				if mode == "bad-sha" {
					item["sha"] = "../other?token=fixture-secret"
				}
				if mode == "duplicate" {
					items = append(items, item)
				}
				if mode == "unsafe-link" {
					w.Header().Set("Link", `<https://evil.test/repos/owner/repo/pulls/7/commits?page=2>; rel="next"`)
				}
				respondJSON(t, w, items)
			})
			pr := loadedPR()
			pr.BaseSHA = "base-sha"
			got, err := g.Commits(context.Background(), pr)
			if err == nil || got.Commits != nil || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("unsafe commit response: %+v %v", got, err)
			}
			if mode == "old-head" && commitCalls != 0 {
				t.Fatal("read commits after stale snapshot")
			}
			if commitCalls > 1 {
				t.Fatal("followed unsafe pagination")
			}
		})
	}
}
