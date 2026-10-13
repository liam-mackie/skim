package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"skim/internal/domain"
)

const (
	maxPublishedCommentBytes = 4 * 65536
	maxPublishedThreadBytes  = 32 << 20
)

// ErrReplyDeliveryUncertain means the POST may have reached GitHub. There is no
// automatic retry or review-publication reconciliation for discussion replies.
var ErrReplyDeliveryUncertain = errors.New("GitHub reply delivery is uncertain; keep your composed text and refresh the thread or check GitHub before submitting again to avoid a duplicate")

type githubPublishedComment struct {
	ID        int64  `json:"id"`
	InReplyTo int64  `json:"in_reply_to_id"`
	Body      string `json:"body"`
	Path      string `json:"path"`
	Side      string `json:"side"`
	StartSide string `json:"start_side"`
	StartLine *int   `json:"start_line"`
	Line      *int   `json:"line"`
	Subject   string `json:"subject_type"`
	DiffHunk  string `json:"diff_hunk"`
	User      *struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (c githubPublishedComment) validate() error {
	if c.ID <= 0 || c.InReplyTo < 0 || c.InReplyTo == c.ID || c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		return fmt.Errorf("GitHub returned invalid review-comment identity or timestamps")
	}
	if c.Path == "" || len(c.Path) > 4096 || len(c.Body) > maxPublishedCommentBytes || len(c.DiffHunk) > 1<<20 {
		return fmt.Errorf("GitHub returned missing or oversized review-comment fields")
	}
	for _, value := range []string{c.Path, c.Body, c.DiffHunk} {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("GitHub returned invalid review-comment text")
		}
	}
	if c.User != nil && (len(c.User.Login) > 256 || !utf8.ValidString(c.User.Login) || strings.ContainsAny(c.User.Login, "\x00\r\n")) {
		return fmt.Errorf("GitHub returned an invalid review-comment author")
	}
	if (c.Side != "" && c.Side != "LEFT" && c.Side != "RIGHT") || (c.StartSide != "" && c.StartSide != "LEFT" && c.StartSide != "RIGHT") || (c.Subject != "" && c.Subject != "line" && c.Subject != "file") {
		return fmt.Errorf("GitHub returned invalid review-comment anchor metadata")
	}
	for _, line := range []*int{c.StartLine, c.Line} {
		if line != nil && (*line <= 0 || *line > 1<<31-1) {
			return fmt.Errorf("GitHub returned an invalid review-comment line")
		}
	}
	if c.Line != nil && c.Subject != "file" {
		if c.Side == "" || (c.StartLine != nil && (c.StartSide == "" || (c.StartSide == c.Side && *c.StartLine > *c.Line))) {
			return fmt.Errorf("GitHub returned an invalid current review-comment anchor")
		}
	}
	return nil
}

func (c githubPublishedComment) published(pr domain.PullRequest) domain.PublishedComment {
	author := "[deleted]"
	if c.User != nil && c.User.Login != "" {
		author = c.User.Login
	}
	id := strconv.FormatInt(c.ID, 10)
	return domain.PublishedComment{
		ID: id, Author: author, Body: c.Body,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339Nano),
		URL: "https://github.com/" + pr.Owner + "/" + pr.Repo + "/pull/" + strconv.Itoa(pr.Number) + "#discussion_r" + id,
	}
}

func (g *GitHub) checkThreadSnapshot(ctx context.Context, pr domain.PullRequest) error {
	if pr.BaseSHA == "" || pr.HeadSHA == "" {
		return fmt.Errorf("review comments require the loaded pull-request base and head; reload the pull request")
	}
	current, err := g.getPull(ctx, pr)
	if err != nil {
		return err
	}
	if current.Base.SHA != pr.BaseSHA || current.Head.SHA != pr.HeadSHA {
		return fmt.Errorf("pull-request head or base changed while loading review comments; reload the pull request")
	}
	return nil
}

// reviewComments reads every page between two snapshot checks. Replies reuse
// this raw read without constructing the display-only thread representation.
func (g *GitHub) reviewComments(ctx context.Context, pr domain.PullRequest) ([]githubPublishedComment, error) {
	if err := g.checkThreadSnapshot(ctx, pr); err != nil {
		return nil, err
	}
	path, _ := pullPath(pr)
	comments := []githubPublishedComment{}
	seen := make(map[int64]bool)
	totalBytes := 0
	for page := 1; ; page++ {
		u, err := g.endpoint(path+"/comments", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}, "sort": {"created"}, "direction": {"asc"}})
		if err != nil {
			return nil, err
		}
		var batch []githubPublishedComment
		header, err := g.request(ctx, http.MethodGet, u, nil, &batch)
		if err != nil {
			return nil, err
		}
		if batch == nil || len(batch) > 100 {
			return nil, fmt.Errorf("GitHub returned an invalid review-comment page")
		}
		for _, comment := range batch {
			if err := comment.validate(); err != nil {
				return nil, err
			}
			if seen[comment.ID] {
				return nil, fmt.Errorf("GitHub returned duplicate review comments; refresh the thread list")
			}
			seen[comment.ID] = true
			totalBytes += len(comment.Body) + len(comment.Path) + len(comment.DiffHunk)
			if totalBytes > maxPublishedThreadBytes {
				return nil, fmt.Errorf("review comments exceed the 32 MiB limit; view this discussion on GitHub")
			}
			comments = append(comments, comment)
		}
		next, err := nextPage(header, u, page)
		if err != nil {
			return nil, err
		}
		if !next {
			break
		}
		if len(batch) == 0 || page == maxReviewPages {
			return nil, fmt.Errorf("GitHub review-comment pagination is incomplete or exceeds 100 pages; view this discussion on GitHub")
		}
	}
	if err := g.checkThreadSnapshot(ctx, pr); err != nil {
		return nil, err
	}
	return comments, nil
}

// ReviewThreads preserves created ordering, with numeric IDs breaking timestamp
// ties. Only current line/start_line anchors are exposed, never original_line.
func (g *GitHub) ReviewThreads(ctx context.Context, pr domain.PullRequest) (domain.ReviewThreads, error) {
	comments, err := g.reviewComments(ctx, pr)
	if err != nil {
		return domain.ReviewThreads{}, err
	}
	slices.SortFunc(comments, func(a, b githubPublishedComment) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	roots := make(map[int64]*githubPublishedComment)
	for i := range comments {
		if comments[i].InReplyTo == 0 {
			roots[comments[i].ID] = &comments[i]
		}
	}
	result := domain.ReviewThreads{
		PRIdentity: domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA},
		Threads:    []domain.ReviewThread{}, Warnings: []string{},
	}
	indices := make(map[int64]int)
	for _, comment := range comments {
		rootID := comment.ID
		if comment.InReplyTo != 0 {
			rootID = comment.InReplyTo
		}
		index, exists := indices[rootID]
		if !exists {
			thread := domain.ReviewThread{ID: strconv.FormatInt(rootID, 10), Path: comment.Path, Comments: []domain.PublishedComment{}}
			root := roots[rootID]
			if root == nil {
				thread.Outdated = true
				thread.ReplyDisabledReason = "The root comment is unavailable; open this discussion on GitHub."
				result.Warnings = append(result.Warnings, "A discussion's root comment is unavailable; its replies are shown without a current code anchor.")
			} else {
				thread.Path, thread.Side, thread.DiffHunk = root.Path, root.Side, root.DiffHunk
				thread.Outdated = root.Line == nil && root.Subject != "file"
				if root.Line != nil && root.Subject != "file" {
					if root.StartLine != nil && root.StartSide != root.Side {
						result.Warnings = append(result.Warnings, "A discussion spans both diff sides; view its range on GitHub. It is shown without a current code anchor.")
					} else {
						thread.StartLine, thread.EndLine = *root.Line, *root.Line
						if root.StartLine != nil {
							thread.StartLine = *root.StartLine
						}
					}
				}
				// Authentication permits trying a reply, not a claim of write access.
				// GitHub's POST remains authoritative for repository permissions.
				thread.CanReply = g.token != ""
				if !thread.CanReply {
					thread.ReplyDisabledReason = "Configure a GitHub token to reply; public comments can be read without authentication."
				}
			}
			index = len(result.Threads)
			indices[rootID] = index
			result.Threads = append(result.Threads, thread)
		}
		result.Threads[index].Comments = append(result.Threads[index].Comments, comment.published(pr))
	}
	return result, nil
}

// ReplyToReviewThread verifies the root belongs to this PR, checks both remote
// refs immediately before POST, and returns only the definitive POST response.
func (g *GitHub) ReplyToReviewThread(ctx context.Context, pr domain.PullRequest, threadID, body string) (domain.PublishedComment, error) {
	if err := (domain.ReviewReply{ThreadID: threadID, Body: body}).Validate(); err != nil {
		return domain.PublishedComment{}, err
	}
	if g.token == "" {
		return domain.PublishedComment{}, fmt.Errorf("configure a GitHub token before replying to review comments")
	}
	comments, err := g.reviewComments(ctx, pr)
	if err != nil {
		return domain.PublishedComment{}, err
	}
	found := false
	rootID, _ := strconv.ParseInt(threadID, 10, 64)
	for i := range comments {
		if comments[i].ID == rootID && comments[i].InReplyTo == 0 {
			found = true
			break
		}
	}
	if !found {
		return domain.PublishedComment{}, fmt.Errorf("reply target is not an available root comment on this pull request; refresh the threads or check GitHub")
	}
	path, _ := pullPath(pr)
	u, err := g.endpoint(path+"/comments/"+threadID+"/replies", nil)
	if err != nil {
		return domain.PublishedComment{}, err
	}
	var result githubPublishedComment
	payload := struct {
		Body string `json:"body"`
	}{Body: body}
	if _, err := g.request(ctx, http.MethodPost, u, payload, &result); err != nil {
		var ambiguous *AmbiguousError
		if errors.As(err, &ambiguous) {
			return domain.PublishedComment{}, ErrReplyDeliveryUncertain
		}
		return domain.PublishedComment{}, err
	}
	if err := result.validate(); err != nil || result.InReplyTo != rootID {
		return domain.PublishedComment{}, ErrReplyDeliveryUncertain
	}
	return result.published(pr), nil
}
