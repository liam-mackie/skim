package app

import (
	"context"
	"errors"

	"skim/internal/domain"
)

// Inbox is independent of active PR state and never starts model work.
func (a *App) Inbox(ctx context.Context, filter domain.InboxFilter) (domain.ReviewInbox, error) {
	return a.Provider(a.Store.Settings()).Inbox(ctx, filter)
}

// Commits copies only PR metadata. Network work does not hold the foreground
// gate, and the active identity is checked again before exposing the response.
func (a *App) Commits(ctx context.Context, identity domain.PRIdentity) (domain.CommitList, error) {
	pr := a.Store.PullRequest()
	if !identity.Matches(pr) {
		return domain.CommitList{}, errors.New("commit request does not match the loaded PR and base/head; reload the pull request")
	}
	result, err := a.Provider(a.Store.Settings()).Commits(ctx, *pr)
	if err != nil {
		return domain.CommitList{}, err
	}
	if !identity.Matches(a.Store.PullRequest()) {
		return domain.CommitList{}, errors.New("active pull request changed while loading commits; reload the pull request")
	}
	return result, nil
}
