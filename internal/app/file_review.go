package app

import (
	"context"
	"errors"
	"fmt"

	"skim/internal/domain"
)

// SetReviewed commits one file or an entire unit atomically. Like manual edits,
// it uses the short commit gate, not AI operation admission or publication state.
func (a *App) SetReviewed(ctx context.Context, identity domain.PRIdentity, paths []string, reviewed bool) (domain.FileReviewUpdate, error) {
	if err := a.lock(ctx); err != nil {
		return domain.FileReviewUpdate{}, err
	}
	defer a.unlock()
	if err := ctx.Err(); err != nil {
		return domain.FileReviewUpdate{}, err
	}
	if a.lifetime.Err() != nil {
		return domain.FileReviewUpdate{}, errors.New("application is closed")
	}
	d := a.Store.Snapshot()
	r, ok := d.Reviews[identity.PRKey]
	if !ok || !identity.Matches(r.State.PullRequest) {
		return domain.FileReviewUpdate{}, errors.New("reviewed request does not match a saved PR and base/head; reload the pull request")
	}
	if len(paths) == 0 {
		return domain.FileReviewUpdate{}, errors.New("reviewed paths must not be empty")
	}
	current := make(map[string]bool, len(r.State.Files))
	for _, file := range r.State.Files {
		current[file.Path] = false
	}
	for _, path := range paths {
		seen, exists := current[path]
		if path == "" || !exists {
			return domain.FileReviewUpdate{}, fmt.Errorf("reviewed path %q is not in the saved PR", path)
		}
		if seen {
			return domain.FileReviewUpdate{}, fmt.Errorf("reviewed path %q is repeated", path)
		}
		current[path] = true
	}

	progress := domain.FileReview{ReviewedPaths: []string{}}
	if r.State.FileReview != nil {
		progress = *r.State.FileReview
	}
	marked := make(map[string]bool, len(progress.ReviewedPaths)+len(paths))
	for _, path := range progress.ReviewedPaths {
		marked[path] = true
	}
	changed := false
	for _, path := range paths {
		if marked[path] != reviewed {
			marked[path] = reviewed
			changed = true
		}
	}
	if !changed {
		return domain.FileReviewUpdate{PRIdentity: identity, FileReview: progress}, nil
	}
	// Saved file order is stable regardless of unit ordering or request order.
	progress.ReviewedPaths = progress.ReviewedPaths[:0]
	for _, file := range r.State.Files {
		if marked[file.Path] {
			progress.ReviewedPaths = append(progress.ReviewedPaths, file.Path)
		}
	}
	progress.Revision++
	r.State.FileReview = &progress
	d.Reviews[identity.PRKey] = r
	if err := a.Store.Commit(d); err != nil {
		return domain.FileReviewUpdate{}, err
	}
	return domain.FileReviewUpdate{PRIdentity: identity, FileReview: progress}, nil
}

// Reconcile before replacing the detached state's files on Load. Risk and unit
// assignments are not displayed diff evidence. Incomplete patches additionally
// require the same refs, since unchanged visible text cannot prove hidden bytes.
func reconcileFileReview(state *domain.State, pr domain.PullRequest, files []domain.File) {
	progress := state.FileReview
	if progress == nil || len(progress.ReviewedPaths) == 0 {
		return
	}
	sameRefs := state.PullRequest != nil && state.PullRequest.BaseSHA == pr.BaseSHA && state.PullRequest.HeadSHA == pr.HeadSHA
	previous := make(map[string]domain.File, len(state.Files))
	for _, file := range state.Files {
		file.Risk = nil
		file.TestOnly = nil
		previous[file.Path] = file
	}
	unchanged := make(map[string]bool, len(files))
	for _, file := range files {
		before, exists := previous[file.Path]
		file.Risk = nil
		file.TestOnly = nil
		if exists && before == file && (file.PatchStatus == "available" || sameRefs) {
			unchanged[file.Path] = true
		}
	}
	retained := progress.ReviewedPaths[:0]
	for _, path := range progress.ReviewedPaths {
		if unchanged[path] {
			retained = append(retained, path)
		}
	}
	if len(retained) != len(progress.ReviewedPaths) {
		progress.ReviewedPaths = retained
		progress.Revision++
	}
}
