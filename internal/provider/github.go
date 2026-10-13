package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"skim/internal/domain"
)

const (
	maxResponseBytes = 8 << 20
	maxGitHubFiles   = 3000
	maxReviewPages   = 100
	maxSamplePages   = 10
	maxSamples       = 50
)

var (
	ownerPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	markerPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// GitHub talks to the GitHub REST API. Configure fields before concurrent use.
// BaseURL and Client can be overridden for a private HTTP fixture. Redirects
// are always refused, including when a supplied Client would follow them.
type GitHub struct {
	BaseURL string
	Client  *http.Client
	token   string
}

var _ Provider = (*GitHub)(nil)

func NewGitHub(token string) *GitHub {
	return &GitHub{BaseURL: "https://api.github.com", Client: &http.Client{Timeout: 30 * time.Second}, token: token}
}

func validRepository(owner, repo string) bool {
	return ownerPattern.MatchString(owner) && repoPattern.MatchString(repo) && repo != "." && repo != ".."
}

// ParseGitHubURL accepts only canonical github.com pull-request URLs. A single
// trailing slash is normalized; credentials, queries, fragments and subpages
// are rejected rather than sent to an arbitrary remote service.
func ParseGitHubURL(raw string) (domain.PullRequest, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.RawPath != "" {
		return domain.PullRequest{}, fmt.Errorf("expected https://github.com/OWNER/REPO/pull/NUMBER")
	}
	parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "" || parts[3] != "pull" || !validRepository(parts[1], parts[2]) {
		return domain.PullRequest{}, fmt.Errorf("expected https://github.com/OWNER/REPO/pull/NUMBER")
	}
	number, err := strconv.Atoi(parts[4])
	if err != nil || number <= 0 || strconv.Itoa(number) != parts[4] {
		return domain.PullRequest{}, fmt.Errorf("GitHub pull-request number must be a positive canonical integer")
	}
	return domain.PullRequest{Provider: "github", Owner: parts[1], Repo: parts[2], Number: number, URL: "https://github.com" + strings.TrimSuffix(u.Path, "/")}, nil
}

func repositoryPath(owner, repo string) (string, error) {
	if !validRepository(owner, repo) {
		return "", fmt.Errorf("invalid GitHub repository owner or name")
	}
	return "/repos/" + owner + "/" + repo, nil
}

func pullPath(pr domain.PullRequest) (string, error) {
	root, err := repositoryPath(pr.Owner, pr.Repo)
	if err != nil {
		return "", err
	}
	if pr.Provider != "github" || pr.Number <= 0 {
		return "", fmt.Errorf("invalid GitHub pull request")
	}
	return root + "/pulls/" + strconv.Itoa(pr.Number), nil
}

func (g *GitHub) endpoint(path string, query url.Values) (*url.URL, error) {
	u, err := url.Parse(g.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return nil, fmt.Errorf("invalid GitHub API base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()
	return u, nil
}

// request never includes response bodies or transport error strings in errors:
// those are uncontrolled data and can reflect configured credentials.
func (g *GitHub) request(ctx context.Context, method string, endpoint *url.URL, payload any, target any) (http.Header, error) {
	return g.requestAs(ctx, method, endpoint, "application/vnd.github+json", payload, target)
}

// requestAs selects a GitHub media type, e.g. html+json for rendered bodies.
func (g *GitHub) requestAs(ctx context.Context, method string, endpoint *url.URL, accept string, payload any, target any) (http.Header, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("could not encode GitHub request")
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, fmt.Errorf("could not construct GitHub request")
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "skim")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Timeout: 30 * time.Second}
	if g.Client != nil {
		client = *g.Client
		if client.Timeout <= 0 || client.Timeout > 30*time.Second {
			client.Timeout = 30 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if method == http.MethodPost {
			return nil, &AmbiguousError{Operation: "review submission"}
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("GitHub request canceled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("GitHub request failed; check network access and API configuration")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if method == http.MethodPost && (response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout) {
			return nil, &AmbiguousError{Operation: "review submission"}
		}
		hint := "request rejected"
		switch response.StatusCode {
		case http.StatusUnauthorized:
			hint = "check the configured GitHub token"
		case http.StatusForbidden, http.StatusTooManyRequests:
			hint = "check token permissions and GitHub rate limits"
		case http.StatusNotFound:
			hint = "resource not found or token lacks repository access"
		case http.StatusUnprocessableEntity:
			hint = "GitHub rejected review fields or repository review policy"
		default:
			if response.StatusCode >= 300 && response.StatusCode < 400 {
				hint = "API redirects are refused to protect credentials"
			}
		}
		return nil, fmt.Errorf("GitHub HTTP %d: %s", response.StatusCode, hint)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil || len(data) > maxResponseBytes {
		if method == http.MethodPost {
			return nil, &AmbiguousError{Operation: "review submission"}
		}
		return nil, fmt.Errorf("GitHub response could not be read or exceeded the 8 MiB limit")
	}
	if err := json.Unmarshal(data, target); err != nil {
		if method == http.MethodPost {
			return nil, &AmbiguousError{Operation: "review submission"}
		}
		return nil, fmt.Errorf("GitHub returned an invalid JSON response")
	}
	return response.Header, nil
}

// samePaginationPath accepts the identical path, or GitHub's documented-in-practice
// canonical form of a repository resource: /repos/OWNER/REPO/SUFFIX is linked as
// /repositories/NUMERIC_ID/SUFFIX. The numeric ID is never trusted or used for
// routing: callers rebuild every next request from their own named-repo path, so
// the alias only avoids rejecting real pagination. The suffix must match exactly.
func samePaginationPath(link, current string) bool {
	if link == current {
		return true
	}
	prefix, rest, ok := strings.Cut(current, "/repos/")
	if !ok {
		return false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	alias, ok := strings.CutPrefix(link, prefix+"/repositories/")
	if !ok {
		return false
	}
	id, suffix, ok := strings.Cut(alias, "/")
	if !ok || id == "" || suffix != parts[2] {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// nextPage follows only sequential pages of the same API resource and origin.
// Other query values come from our original request, not the Link header.
func nextPage(header http.Header, current *url.URL, page int) (bool, error) {
	found := false
	for _, value := range header.Values("Link") {
		for _, link := range strings.Split(value, ",") {
			sections := strings.Split(link, ";")
			isNext := false
			for _, parameter := range sections[1:] {
				key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
				if ok && key == "rel" {
					for _, relation := range strings.Fields(strings.Trim(value, `"`)) {
						isNext = isNext || relation == "next"
					}
				}
			}
			if !isNext {
				continue
			}
			if found {
				return false, fmt.Errorf("GitHub returned duplicate next-page links")
			}
			found = true
			raw := strings.TrimSpace(sections[0])
			if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
				return false, fmt.Errorf("GitHub returned an invalid pagination link")
			}
			u, err := url.Parse(raw[1 : len(raw)-1])
			if err != nil {
				return false, fmt.Errorf("GitHub returned an invalid pagination link")
			}
			u = current.ResolveReference(u)
			next, err := strconv.Atoi(u.Query().Get("page"))
			if err != nil || next != page+1 || u.Scheme != current.Scheme || u.Host != current.Host || u.User != nil || !samePaginationPath(u.Path, current.Path) || u.RawPath != "" || u.Fragment != "" || len(u.Query()["page"]) != 1 {
				return false, fmt.Errorf("GitHub returned an unsafe or nonsequential pagination link")
			}
		}
	}
	return found, nil
}

type githubPull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	ChangedFiles *int `json:"changed_files"`
	Commits      *int `json:"commits"`
}

func (g *GitHub) getPull(ctx context.Context, pr domain.PullRequest) (githubPull, error) {
	var result githubPull
	path, err := pullPath(pr)
	if err != nil {
		return result, err
	}
	u, err := g.endpoint(path, nil)
	if err != nil {
		return result, err
	}
	if _, err := g.request(ctx, http.MethodGet, u, nil, &result); err != nil {
		return result, err
	}
	if result.Number != pr.Number || result.Head.SHA == "" || result.Base.SHA == "" {
		return result, fmt.Errorf("GitHub returned incomplete or mismatched pull-request metadata")
	}
	return result, nil
}

func (g *GitHub) Head(ctx context.Context, pr domain.PullRequest) (string, error) {
	result, err := g.getPull(ctx, pr)
	if err != nil {
		return "", err
	}
	return result.Head.SHA, nil
}

func (g *GitHub) Load(ctx context.Context, rawURL string) (domain.PullRequest, []domain.File, error) {
	pr, err := ParseGitHubURL(rawURL)
	if err != nil {
		return domain.PullRequest{}, nil, err
	}
	metadata, err := g.getPull(ctx, pr)
	if err != nil {
		return domain.PullRequest{}, nil, err
	}
	if metadata.ChangedFiles == nil || *metadata.ChangedFiles < 0 {
		return domain.PullRequest{}, nil, fmt.Errorf("GitHub did not report a valid changed-file count; cannot guarantee a complete diff")
	}
	if *metadata.ChangedFiles > maxGitHubFiles {
		return domain.PullRequest{}, nil, fmt.Errorf("pull request exceeds GitHub's 3000-file API limit; a complete diff cannot be loaded")
	}
	pr.Title, pr.Body, pr.Author = metadata.Title, metadata.Body, metadata.User.Login
	pr.BaseSHA, pr.HeadSHA = metadata.Base.SHA, metadata.Head.SHA
	pr.BaseRef, pr.HeadRef = metadata.Base.Ref, metadata.Head.Ref
	path, _ := pullPath(pr)
	files := make([]domain.File, 0, *metadata.ChangedFiles)
	seen := make(map[string]bool, *metadata.ChangedFiles)
	for page := 1; ; page++ {
		u, err := g.endpoint(path+"/files", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}})
		if err != nil {
			return domain.PullRequest{}, nil, err
		}
		var batch []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
			Status           string `json:"status"`
			Additions        int    `json:"additions"`
			Deletions        int    `json:"deletions"`
			Patch            string `json:"patch"`
		}
		header, err := g.request(ctx, http.MethodGet, u, nil, &batch)
		if err != nil {
			return domain.PullRequest{}, nil, err
		}
		if batch == nil || len(batch) > 100 {
			return domain.PullRequest{}, nil, fmt.Errorf("GitHub returned an invalid file page")
		}
		for _, file := range batch {
			if file.Filename == "" || seen[file.Filename] || file.Additions < 0 || file.Deletions < 0 {
				return domain.PullRequest{}, nil, fmt.Errorf("GitHub returned invalid or duplicate file metadata")
			}
			seen[file.Filename] = true
			files = append(files, domain.File{Path: file.Filename, PreviousPath: file.PreviousFilename, Status: file.Status, Additions: file.Additions, Deletions: file.Deletions, Patch: file.Patch, PatchStatus: patchStatus(file.Patch, file.Additions, file.Deletions)})
		}
		next, err := nextPage(header, u, page)
		if err != nil {
			return domain.PullRequest{}, nil, err
		}
		if len(files) > maxGitHubFiles || (next && (page >= maxGitHubFiles/100 || len(files) >= maxGitHubFiles)) {
			return domain.PullRequest{}, nil, fmt.Errorf("pull request reaches GitHub's 3000-file API limit before pagination completes")
		}
		if !next {
			break
		}
		if len(batch) == 0 {
			return domain.PullRequest{}, nil, fmt.Errorf("GitHub file pagination returned an empty intermediate page")
		}
	}
	if len(files) != *metadata.ChangedFiles {
		return domain.PullRequest{}, nil, fmt.Errorf("GitHub returned %d of %d changed files; refusing a partial or changing diff", len(files), *metadata.ChangedFiles)
	}
	// A same-count force-push (or moving base) can change patches between
	// metadata and file pages. Never return a mixed snapshot as a loaded PR.
	current, err := g.getPull(ctx, pr)
	if err != nil {
		return domain.PullRequest{}, nil, err
	}
	if current.Head.SHA != pr.HeadSHA || current.Base.SHA != pr.BaseSHA {
		return domain.PullRequest{}, nil, fmt.Errorf("pull-request head or base changed while loading the diff; reload the pull request")
	}
	return pr, files, nil
}

func (g *GitHub) authenticatedUser(ctx context.Context) (string, error) {
	if g.token == "" {
		return "", fmt.Errorf("a configured GitHub token is required for this operation")
	}
	u, err := g.endpoint("/user", nil)
	if err != nil {
		return "", err
	}
	var user struct {
		Login string `json:"login"`
	}
	if _, err := g.request(ctx, http.MethodGet, u, nil, &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", fmt.Errorf("GitHub returned no authenticated user")
	}
	return user.Login, nil
}

func (g *GitHub) ReviewSamples(ctx context.Context, owner, repo string) ([]string, error) {
	path, err := repositoryPath(owner, repo)
	if err != nil {
		return nil, err
	}
	login, err := g.authenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	samples := make([]string, 0, maxSamples)
	seen := make(map[int64]bool)
	for page := 1; page <= maxSamplePages; page++ {
		u, err := g.endpoint(path+"/pulls/comments", url.Values{"sort": {"created"}, "direction": {"desc"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}})
		if err != nil {
			return nil, err
		}
		var batch []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		header, err := g.request(ctx, http.MethodGet, u, nil, &batch)
		if err != nil {
			return nil, err
		}
		if batch == nil || len(batch) > 100 {
			return nil, fmt.Errorf("GitHub returned an invalid review-comment page")
		}
		for _, comment := range batch {
			if strings.EqualFold(comment.User.Login, login) && strings.TrimSpace(comment.Body) != "" && comment.ID > 0 && !seen[comment.ID] {
				seen[comment.ID] = true
				// Bound imported text as well as the number of samples. Oversized
				// comments are skipped, never replaced with invented examples.
				if len(comment.Body) <= 16000 {
					samples = append(samples, comment.Body)
				}
				if len(samples) == maxSamples {
					return samples, nil
				}
			}
		}
		next, err := nextPage(header, u, page)
		if err != nil {
			return nil, err
		}
		if !next {
			break
		}
		if len(batch) == 0 {
			return nil, fmt.Errorf("GitHub review-comment pagination returned an empty intermediate page")
		}
	}
	return samples, nil
}

func reviewMarker(marker string) (string, error) {
	if strings.HasPrefix(marker, "<!-- skim-review:") && strings.HasSuffix(marker, " -->") {
		marker = strings.TrimSuffix(strings.TrimPrefix(marker, "<!-- skim-review:"), " -->")
	}
	if !markerPattern.MatchString(marker) || len(marker) > 200 {
		return "", fmt.Errorf("review reconciliation marker must be a nonempty identifier")
	}
	return "<!-- skim-review:" + marker + " -->", nil
}

type reviewComment struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
}

func (g *GitHub) Publish(ctx context.Context, pr domain.PullRequest, comments []domain.Comment, event, body, marker string) (int64, error) {
	if g.token == "" {
		return 0, fmt.Errorf("a configured GitHub token is required to publish reviews")
	}
	path, err := pullPath(pr)
	if err != nil {
		return 0, err
	}
	footer, err := reviewMarker(marker)
	if err != nil {
		return 0, err
	}
	if event != "COMMENT" && event != "APPROVE" && event != "REQUEST_CHANGES" {
		return 0, fmt.Errorf("unsupported GitHub review event")
	}
	if pr.HeadSHA == "" {
		return 0, fmt.Errorf("review requires the loaded pull-request head SHA")
	}
	if event == "COMMENT" && len(comments) == 0 && strings.TrimSpace(body) == "" {
		return 0, fmt.Errorf("COMMENT review requires a body or comments")
	}
	if event == "REQUEST_CHANGES" && strings.TrimSpace(body) == "" {
		return 0, fmt.Errorf("REQUEST_CHANGES review requires a summary body")
	}
	payload := struct {
		CommitID string          `json:"commit_id"`
		Event    string          `json:"event"`
		Body     string          `json:"body"`
		Comments []reviewComment `json:"comments,omitempty"`
	}{CommitID: pr.HeadSHA, Event: event, Body: strings.TrimRight(body, "\r\n") + "\n\n" + footer, Comments: make([]reviewComment, 0, len(comments))}
	for _, comment := range comments {
		if comment.Stale || comment.HeadSHA != pr.HeadSHA {
			return 0, fmt.Errorf("cannot publish stale or mismatched-head review comments")
		}
		if comment.Path == "" || comment.StartLine <= 0 || comment.EndLine < comment.StartLine || (comment.Side != "LEFT" && comment.Side != "RIGHT") || strings.TrimSpace(comment.Body) == "" {
			return 0, fmt.Errorf("cannot publish an invalid review comment")
		}
		item := reviewComment{Path: comment.Path, Body: comment.Body, Line: comment.EndLine, Side: comment.Side}
		if comment.StartLine != comment.EndLine {
			item.StartLine, item.StartSide = comment.StartLine, comment.Side
		}
		payload.Comments = append(payload.Comments, item)
	}
	u, err := g.endpoint(path+"/reviews", nil)
	if err != nil {
		return 0, err
	}
	// This is the last remote operation before POST. Never automatically retry
	// submission: a dropped response can still correspond to a real review.
	head, err := g.Head(ctx, pr)
	if err != nil {
		return 0, err
	}
	if head != pr.HeadSHA {
		return 0, fmt.Errorf("pull-request head changed; reload before publishing")
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if _, err := g.request(ctx, http.MethodPost, u, payload, &result); err != nil {
		return 0, err
	}
	if result.ID <= 0 {
		return 0, &AmbiguousError{Operation: "review submission"}
	}
	return result.ID, nil
}

func (g *GitHub) FindReview(ctx context.Context, pr domain.PullRequest, marker string) (int64, error) {
	path, err := pullPath(pr)
	if err != nil {
		return 0, err
	}
	footer, err := reviewMarker(marker)
	if err != nil {
		return 0, err
	}
	if pr.HeadSHA == "" {
		return 0, fmt.Errorf("review reconciliation requires a head SHA")
	}
	login, err := g.authenticatedUser(ctx)
	if err != nil {
		return 0, err
	}
	for page := 1; page <= maxReviewPages; page++ {
		u, err := g.endpoint(path+"/reviews", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}})
		if err != nil {
			return 0, err
		}
		var batch []struct {
			ID          int64   `json:"id"`
			Body        string  `json:"body"`
			CommitID    string  `json:"commit_id"`
			State       string  `json:"state"`
			SubmittedAt *string `json:"submitted_at"`
			User        struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		header, err := g.request(ctx, http.MethodGet, u, nil, &batch)
		if err != nil {
			return 0, err
		}
		if batch == nil || len(batch) > 100 {
			return 0, fmt.Errorf("GitHub returned an invalid review page")
		}
		for _, review := range batch {
			submitted := review.State == "APPROVED" || review.State == "COMMENTED" || review.State == "CHANGES_REQUESTED" || review.State == "DISMISSED"
			body := strings.TrimRight(review.Body, "\r\n \t")
			matches := body == footer || strings.HasSuffix(body, "\n"+footer)
			if review.ID > 0 && submitted && review.SubmittedAt != nil && *review.SubmittedAt != "" && review.CommitID == pr.HeadSHA && strings.EqualFold(review.User.Login, login) && matches {
				return review.ID, nil
			}
		}
		next, err := nextPage(header, u, page)
		if err != nil {
			return 0, err
		}
		if !next {
			return 0, nil
		}
		if len(batch) == 0 {
			return 0, fmt.Errorf("GitHub review pagination returned an empty intermediate page")
		}
	}
	return 0, fmt.Errorf("review reconciliation exceeded 100 pages; absence cannot be established safely")
}
