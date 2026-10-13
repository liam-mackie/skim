package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"skim/internal/domain"
)

const maxSearchPages = 10
const maxPRCommits = 250

var commitSHAPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

// Inbox uses GitHub's review-requested qualifier, which includes team requests.
// It never loads a PR's files or changes local review state.
func (g *GitHub) Inbox(ctx context.Context, filter domain.InboxFilter) (domain.ReviewInbox, error) {
	result := domain.ReviewInbox{Reviews: []domain.ReviewSummary{}, Warnings: []string{}, Complete: true}
	filter.Username, filter.Repo = strings.TrimSpace(filter.Username), strings.TrimSpace(filter.Repo)
	if filter.Username != "" && !ownerPattern.MatchString(filter.Username) {
		return result, fmt.Errorf("inbox username must be a GitHub login")
	}
	if filter.Repo != "" {
		owner, repo, ok := strings.Cut(filter.Repo, "/")
		if !ok || !validRepository(owner, repo) {
			return result, fmt.Errorf("inbox repository must be OWNER/REPO")
		}
	}
	login, err := g.authenticatedUser(ctx)
	if err != nil {
		return result, fmt.Errorf("could not load review inbox: %w", err)
	}
	if !ownerPattern.MatchString(login) {
		return result, fmt.Errorf("GitHub returned an invalid authenticated login")
	}
	result.User = domain.InboxUser{Login: login, URL: "https://github.com/" + login}
	result.Username, result.Repo = filter.Username, filter.Repo
	if result.Username == "" {
		result.Username = login
	}
	seen := make(map[string]int)
	for _, source := range []string{"review-requested", "assignee"} {
		query := "is:open is:pr " + source + ":" + result.Username
		if filter.Repo != "" {
			query += " repo:" + filter.Repo
		}
		sourceSeen := make(map[string]bool)
		total, incomplete, capped := -1, false, false
		for page := 1; ; page++ {
			u, err := g.endpoint("/search/issues", url.Values{"q": {query}, "sort": {"updated"}, "order": {"desc"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}})
			if err != nil {
				return domain.ReviewInbox{}, err
			}
			var batch struct {
				Total      *int  `json:"total_count"`
				Incomplete *bool `json:"incomplete_results"`
				Items      []struct {
					Number      int       `json:"number"`
					Title       string    `json:"title"`
					URL         string    `json:"html_url"`
					State       string    `json:"state"`
					UpdatedAt   time.Time `json:"updated_at"`
					Draft       bool      `json:"draft"`
					PullRequest *struct{} `json:"pull_request"`
					User        struct {
						Login string `json:"login"`
					} `json:"user"`
				} `json:"items"`
			}
			header, err := g.request(ctx, http.MethodGet, u, nil, &batch)
			if err != nil {
				return domain.ReviewInbox{}, fmt.Errorf("inbox search failed; check repository access, search scope, and GitHub rate limits: %w", err)
			}
			if batch.Total == nil || *batch.Total < 0 || batch.Incomplete == nil || batch.Items == nil || len(batch.Items) > 100 {
				return domain.ReviewInbox{}, fmt.Errorf("GitHub returned an invalid inbox search page")
			}
			incomplete = incomplete || *batch.Incomplete || (total != -1 && total != *batch.Total)
			total = *batch.Total
			capped = capped || total > maxSearchPages*100
			for _, item := range batch.Items {
				pr, err := ParseGitHubURL(item.URL)
				if err != nil || item.Number != pr.Number || item.PullRequest == nil || item.State != "open" || item.UpdatedAt.IsZero() {
					return domain.ReviewInbox{}, fmt.Errorf("GitHub returned invalid inbox pull-request metadata")
				}
				key := strings.ToLower(pr.Key())
				sourceSeen[key] = true
				summary := domain.ReviewSummary{PRKey: pr.Key(), Provider: "github", Owner: pr.Owner, Repo: pr.Repo, Number: pr.Number, Title: item.Title, URL: pr.URL, Author: item.User.Login, UpdatedAt: item.UpdatedAt.UTC().Format(time.RFC3339), Draft: item.Draft, Requested: source == "review-requested", Assigned: source == "assignee"}
				if index, ok := seen[key]; ok {
					old := &result.Reviews[index]
					summary.Requested = summary.Requested || old.Requested
					summary.Assigned = summary.Assigned || old.Assigned
					// Preserve the first canonical response URL/key, including casing,
					// so opening this row reuses existing review state.
					summary.PRKey, summary.Owner, summary.Repo, summary.URL = old.PRKey, old.Owner, old.Repo, old.URL
					if summary.UpdatedAt > old.UpdatedAt {
						*old = summary
					} else {
						old.Requested, old.Assigned = summary.Requested, summary.Assigned
					}
				} else {
					seen[key] = len(result.Reviews)
					result.Reviews = append(result.Reviews, summary)
				}
			}
			next, err := nextPage(header, u, page)
			if err != nil {
				return domain.ReviewInbox{}, err
			}
			if page == maxSearchPages || !next || len(batch.Items) == 0 {
				capped = capped || (page == maxSearchPages && next)
				incomplete = incomplete || len(sourceSeen) != total || next
				break
			}
		}
		if capped {
			result.Complete = false
			result.Warnings = append(result.Warnings, source+" search reached GitHub's 1,000-result limit; narrow the repository filter or inspect the remaining results on GitHub.")
		} else if incomplete {
			result.Complete = false
			result.Warnings = append(result.Warnings, source+" search returned incomplete or changing results; refresh or narrow the repository filter before assuming all reviews are shown.")
		}
	}
	slices.SortFunc(result.Reviews, func(a, b domain.ReviewSummary) int {
		if order := strings.Compare(b.UpdatedAt, a.UpdatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.PRKey, b.PRKey)
	})
	return result, nil
}

// Commits retains the PR endpoint's ordering and reports its documented cap.
// Both refs are checked before and after the paginated request.
func (g *GitHub) Commits(ctx context.Context, pr domain.PullRequest) (domain.CommitList, error) {
	identity := domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}
	result := domain.CommitList{PRIdentity: identity, Commits: []domain.Commit{}, Warnings: []string{}, Complete: true}
	metadata, err := g.getPull(ctx, pr)
	if err != nil {
		return domain.CommitList{}, err
	}
	if metadata.Base.SHA != pr.BaseSHA || metadata.Head.SHA != pr.HeadSHA {
		return domain.CommitList{}, fmt.Errorf("pull-request head or base changed; reload the pull request before viewing commits")
	}
	if metadata.Commits == nil || *metadata.Commits < 0 {
		return domain.CommitList{}, fmt.Errorf("GitHub did not report a valid PR commit count")
	}
	result.Total = *metadata.Commits
	path, _ := pullPath(pr)
	seen := make(map[string]bool)
	for page := 1; ; page++ {
		u, err := g.endpoint(path+"/commits", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}})
		if err != nil {
			return domain.CommitList{}, err
		}
		var batch []struct {
			SHA    string `json:"sha"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
					Date string `json:"date"`
				} `json:"author"`
			} `json:"commit"`
		}
		header, err := g.request(ctx, http.MethodGet, u, nil, &batch)
		if err != nil {
			return domain.CommitList{}, err
		}
		if batch == nil || len(batch) > 100 || len(result.Commits)+len(batch) > maxPRCommits {
			return domain.CommitList{}, fmt.Errorf("GitHub returned an invalid PR commit page")
		}
		for _, item := range batch {
			sha := strings.ToLower(item.SHA)
			if !commitSHAPattern.MatchString(sha) || seen[sha] {
				return domain.CommitList{}, fmt.Errorf("GitHub returned an invalid or duplicate commit SHA; reload the pull request")
			}
			seen[sha] = true
			result.Commits = append(result.Commits, domain.Commit{SHA: sha, Message: item.Commit.Message, AuthorName: item.Commit.Author.Name, AuthorLogin: item.Author.Login, AuthoredAt: item.Commit.Author.Date, URL: "https://github.com/" + pr.Owner + "/" + pr.Repo + "/commit/" + sha})
		}
		next, err := nextPage(header, u, page)
		if err != nil {
			return domain.CommitList{}, err
		}
		if !next || page == 3 || len(batch) == 0 {
			result.Complete = !next && len(result.Commits) == result.Total
			break
		}
	}
	current, err := g.getPull(ctx, pr)
	if err != nil {
		return domain.CommitList{}, err
	}
	if current.Head.SHA != pr.HeadSHA || current.Base.SHA != pr.BaseSHA || current.Commits == nil || *current.Commits != result.Total {
		return domain.CommitList{}, fmt.Errorf("pull-request head, base, or commit count changed while loading commits; reload the pull request")
	}
	if result.Total > maxPRCommits {
		result.Complete = false
		result.Warnings = append(result.Warnings, fmt.Sprintf("Showing %d of %d commits: GitHub's PR commits API exposes at most 250; view the complete history on GitHub.", len(result.Commits), result.Total))
	} else if !result.Complete {
		result.Warnings = append(result.Warnings, fmt.Sprintf("GitHub returned %d of %d commits; refresh the PR or view its history on GitHub before assuming the list is complete.", len(result.Commits), result.Total))
	}
	return result, nil
}
