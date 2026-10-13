package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"skim/internal/acp"
	"skim/internal/domain"
	"skim/internal/provider"
)

const (
	// reviewTimeout bounds one agent review, including checkout preparation.
	reviewTimeout = 30 * time.Minute
	// reviewTask names the review's managed worktree; verification threads use their IDs.
	reviewTask = "review"
)

const reviewPrompt = "Review this pull request in the checked-out workspace. The workspace is the PR head (headSha) and the base commit (baseSha) is available, so inspect the whole change with git, for example `git diff BASE HEAD`, and read surrounding code, callers and tests as needed. Do not modify files, install dependencies, commit, push, or publish anything. Request permission for tool actions that need it, and run focused checks only when they add evidence.\n" +
	"Look for concrete correctness, security and regression problems introduced by this change. Do not comment on style, praise the change, or report speculative concerns.\n" +
	"Write your review report first: what you inspected, commands and their actual results, findings with evidence, and limitations. End your reply with exactly one fenced json block of the shape {\"comments\":[{\"path\":string,\"startLine\":number,\"endLine\":number,\"side\":\"LEFT\"|\"RIGHT\",\"body\":string}]} holding the comments to post; use {\"comments\":[]} when no defect is supported. Anchor each comment inside one hunk of GitHub's diff for that file: RIGHT uses new-file line numbers of added or context lines, LEFT uses old-file line numbers of removed or context lines, and startLine..endLine is one contiguous same-side range. Only files whose patchStatus is \"available\" can take comments. Each body explains the consequence and the evidence, and follows the supplied writing style."

type proposedComment struct {
	Path      string `json:"path"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Side      string `json:"side"`
	Body      string `json:"body"`
}

// Review runs the configured ACP agent over the whole PR in a managed checkout
// and saves its validated comments as suggestions, with its report.
func (a *App) Review(ctx context.Context) (state domain.State, err error) {
	if err := a.tryLock(ctx); err != nil {
		return domain.State{}, err
	}
	d := a.Store.Snapshot()
	r, err := active(d)
	if err == nil && strings.TrimSpace(d.Settings.ACP.Command) == "" {
		err = errors.New("configure an ACP agent in Settings to run agent reviews")
	}
	if err != nil {
		a.unlock()
		return domain.State{}, err
	}
	op, err := a.beginOperation(ctx, "review", "", r)
	a.unlock()
	if err != nil {
		return domain.State{}, err
	}
	defer a.endOperation(op)
	ctx, cancel := context.WithTimeout(op.ctx, reviewTimeout)
	defer cancel()
	identity := op.identity
	progress := domain.ReviewProgress{ID: domain.NewID(), Status: "running", Phase: "preparing", StartedAt: domain.Now()}
	a.setReviewProgress(identity, progress)
	defer func() {
		if err != nil {
			progress.Status = "failed"
			if op.ctx.Err() != nil {
				progress.Status = "cancelled"
			} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("agent review exceeded the %v limit", reviewTimeout)
			}
			progress.Error = a.Store.Redact(err.Error())
		} else if progress.Status != "complete" {
			return
		}
		progress.Phase = "finished"
		progress.FinishedAt = domain.Now()
		a.setReviewProgress(identity, progress)
	}()

	pr := *r.State.PullRequest
	remote := a.Provider(d.Settings)
	head, err := remote.Head(ctx, pr)
	if err != nil {
		return domain.State{}, err
	}
	if head != pr.HeadSHA {
		return domain.State{}, errors.New("GitHub PR head changed; reload before reviewing")
	}
	source, err := remote.GitSource(pr)
	if err != nil {
		return domain.State{}, err
	}
	lease, err := a.Workspaces.Prepare(ctx, d.Settings.ACP.ManagedRoot, pr, source, a.Store.GitHubToken(), reviewTask)
	if err != nil {
		return domain.State{}, err
	}
	defer lease.Release()
	progress.Phase = "reviewing"
	a.setReviewProgress(identity, progress)
	type fileSummary struct {
		Path         string `json:"path"`
		PreviousPath string `json:"previousPath,omitempty"`
		Status       string `json:"status"`
		Additions    int    `json:"additions"`
		Deletions    int    `json:"deletions"`
		PatchStatus  string `json:"patchStatus"`
	}
	files := make([]fileSummary, len(r.State.Files))
	for i, f := range r.State.Files {
		files[i] = fileSummary{f.Path, f.PreviousPath, f.Status, f.Additions, f.Deletions, f.PatchStatus}
	}
	evidence, _ := json.Marshal(map[string]any{"pullRequest": pr, "files": files, "writingStyle": d.Style})
	prompt := reviewPrompt + codeSuggestionRules + " " + evidenceRules + "\n" + string(evidence)
	scope := acp.PermissionScope{JobID: progress.ID, PRKey: pr.Key(), PRURL: pr.URL, Question: "Agent review of the whole pull request"}
	report, err := a.Agent.Run(ctx, d.Settings.ACP, lease.Path, head, prompt, scope, func(group int) error {
		return lease.RecordProcess(ctx, group)
	})
	lease.Release()
	if err != nil {
		return domain.State{}, err
	}

	progress.Phase = "saving"
	a.setReviewProgress(identity, progress)
	// The agent inherits the environment; never persist configured secrets it echoes.
	report = a.Store.Redact(report)
	run := &domain.ReviewRun{HeadSHA: pr.HeadSHA, CreatedAt: domain.Now(), Report: report, Rejected: []domain.RejectedComment{}}
	additions := []domain.Comment{}
	proposed, parseErr := parseReviewComments(report)
	if parseErr != nil {
		run.Error = parseErr.Error()
	}
	for _, p := range proposed {
		// JSON escapes can hide a secret from the raw-report redaction above.
		p.Body = a.Store.Redact(p.Body)
		c := domain.Comment{ID: domain.NewID(), Revision: 1, Path: p.Path, StartLine: p.StartLine, EndLine: p.EndLine, Side: p.Side, Body: p.Body,
			Status: "suggested", Source: "review", CreatedAt: domain.Now(), HeadSHA: pr.HeadSHA}
		if reason := reviewCommentProblem(r.State.Files, c); reason != "" {
			run.Rejected = append(run.Rejected, domain.RejectedComment{Path: p.Path, StartLine: p.StartLine, EndLine: p.EndLine, Side: p.Side, Body: p.Body, Reason: reason})
			continue
		}
		additions = append(additions, c)
	}
	progress.Rejected = len(run.Rejected)
	// Suggestions the user denied or edited while the agent ran must not reappear,
	// so compare against the comments at review start as well as the latest state.
	started := r.State.Comments
	if err = a.lock(ctx); err != nil {
		return domain.State{}, err
	}
	defer a.unlock()
	d, r, err = a.operationState(op)
	if err != nil {
		return domain.State{}, err
	}
	known := func(c domain.Comment, comments []domain.Comment) bool {
		for _, existing := range comments {
			if existing.Status != "deleted" && !existing.Stale && existing.HeadSHA == c.HeadSHA && sameFinding(existing, c) {
				return true
			}
		}
		return false
	}
	for _, c := range additions {
		if !known(c, started) && !known(c, r.State.Comments) {
			r.State.Comments = append(r.State.Comments, c)
			progress.Findings++
		}
	}
	r.State.ReviewRun = run
	if err = a.save(d, r); err != nil {
		return domain.State{}, err
	}
	if parseErr != nil {
		return domain.State{}, fmt.Errorf("agent review finished but %w; its report is saved with the review", parseErr)
	}
	progress.Status = "complete"
	return a.State(), nil
}

// parseReviewComments reads the comments JSON block that ends the agent's report.
// It decodes the first JSON value after the last ```json marker rather than cutting
// at the closing fence, because comment bodies may contain ```suggestion fences.
func parseReviewComments(report string) ([]proposedComment, error) {
	i := strings.LastIndex(report, "```json")
	if i < 0 {
		return nil, errors.New("the report has no comments JSON block")
	}
	var result struct {
		Comments []proposedComment `json:"comments"`
	}
	if err := json.NewDecoder(strings.NewReader(report[i+len("```json"):])).Decode(&result); err != nil || result.Comments == nil {
		return nil, errors.New("the report's comments JSON block is invalid")
	}
	return result.Comments, nil
}

// reviewCommentProblem explains why a proposed comment cannot be posted, or returns "".
func reviewCommentProblem(files []domain.File, c domain.Comment) string {
	if err := provider.ValidateAnchor(files, c.Path, c.StartLine, c.EndLine, c.Side); err != nil {
		return err.Error()
	}
	if strings.TrimSpace(c.Body) == "" || len(c.Body) > 60000 {
		return "comment body must contain 1–60000 bytes"
	}
	if err := validateCodeSuggestion(c); err != nil {
		return err.Error()
	}
	return ""
}
