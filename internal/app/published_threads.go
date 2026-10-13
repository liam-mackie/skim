package app

import (
	"context"
	"errors"

	"skim/internal/domain"
	"skim/internal/provider"
	"skim/internal/store"
)

// ReviewThreads leaves remote discussions outside persisted State and drafts.
// Reads do not hold the foreground gate; both ends validate the active identity.
func (a *App) ReviewThreads(ctx context.Context, identity domain.PRIdentity) (domain.ReviewThreads, error) {
	pr := a.Store.PullRequest()
	if !identity.Matches(pr) {
		return domain.ReviewThreads{}, errors.New("review-comment request does not match the loaded PR and base/head; reload the pull request")
	}
	result, err := a.Provider(a.Store.Settings()).ReviewThreads(ctx, *pr)
	if err != nil {
		return domain.ReviewThreads{}, errors.New(a.Store.Redact(err.Error()))
	}
	if !identity.Matches(a.Store.PullRequest()) || !result.PRIdentity.Matches(pr) {
		return domain.ReviewThreads{}, errors.New("pull-request identity changed while loading review comments; reload the pull request")
	}
	if result.Threads == nil {
		result.Threads = []domain.ReviewThread{}
	}
	if result.Warnings == nil {
		result.Warnings = []string{}
	}
	return result, nil
}

// Attachments reads fresh pre-signed image URLs; like ReviewThreads it stays
// outside persisted State and validates the active identity at both ends.
func (a *App) Attachments(ctx context.Context, identity domain.PRIdentity) (domain.Attachments, error) {
	pr := a.Store.PullRequest()
	if !identity.Matches(pr) {
		return domain.Attachments{}, errors.New("attachment request does not match the loaded PR and base/head; reload the pull request")
	}
	result, err := a.Provider(a.Store.Settings()).Attachments(ctx, *pr)
	if err != nil {
		return domain.Attachments{}, errors.New(a.Store.Redact(err.Error()))
	}
	if !identity.Matches(a.Store.PullRequest()) || !result.PRIdentity.Matches(pr) {
		return domain.Attachments{}, errors.New("pull-request identity changed while loading attachments; reload the pull request")
	}
	return result, nil
}

// ReplyToReviewThread tracks the remote write without holding the commit gate.
// Neither success nor uncertainty changes local drafts or starts another POST.
func (a *App) ReplyToReviewThread(ctx context.Context, reply domain.ReviewReply) (domain.PublishedComment, error) {
	if err := reply.Validate(); err != nil {
		return domain.PublishedComment{}, err
	}
	if err := a.tryLock(ctx); err != nil {
		return domain.PublishedComment{}, err
	}
	if err := a.lifetime.Err(); err != nil {
		a.unlock()
		return domain.PublishedComment{}, errors.New("application is closed")
	}
	pr := a.Store.PullRequest()
	if !reply.PRIdentity.Matches(pr) {
		a.unlock()
		return domain.PublishedComment{}, errors.New("reply request does not match the loaded PR and base/head; reload the pull request")
	}
	settings := a.Store.Settings()
	op, err := a.beginOperation(ctx, "reply", "", store.Record{State: domain.State{PullRequest: pr}})
	a.unlock()
	if err != nil {
		return domain.PublishedComment{}, err
	}
	defer a.endOperation(op)
	result, err := a.Provider(settings).ReplyToReviewThread(op.ctx, *pr, reply.ThreadID, reply.Body)
	if errors.Is(err, provider.ErrReplyDeliveryUncertain) {
		return domain.PublishedComment{}, provider.ErrReplyDeliveryUncertain
	}
	if err != nil {
		return domain.PublishedComment{}, errors.New(a.Store.Redact(err.Error()))
	}
	// No dependent read or persistence can turn a successful external write into
	// a reported failure. The caller can append this comment and refresh manually.
	return result, nil
}
