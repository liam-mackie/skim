package app

import (
	"errors"

	"skim/internal/domain"
)

// ReviewProgress copies only PR metadata and the small progress value, without
// contending with operation admission or completion commits.
func (a *App) ReviewProgress(identity domain.PRIdentity) (domain.ReviewProgressUpdate, error) {
	if !identity.Matches(a.Store.PullRequest()) {
		return domain.ReviewProgressUpdate{}, errors.New("review progress request does not match the loaded PR and base/head; reload the pull request")
	}
	result := domain.ReviewProgressUpdate{PRIdentity: identity}
	a.reviewMu.RLock()
	if a.reviewIdentity == identity && a.reviewProgress.ID != "" {
		run := a.reviewProgress
		result.Run = &run
	}
	a.reviewMu.RUnlock()
	if !identity.Matches(a.Store.PullRequest()) {
		return domain.ReviewProgressUpdate{}, errors.New("active pull request changed while loading review progress; reload the pull request")
	}
	return result, nil
}

func (a *App) setReviewProgress(identity domain.PRIdentity, progress domain.ReviewProgress) {
	progress.UpdatedAt = domain.Now()
	a.reviewMu.Lock()
	a.reviewIdentity = identity
	a.reviewProgress = progress
	a.reviewMu.Unlock()
}
