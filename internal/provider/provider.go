// Package provider loads pull requests and publishes anchored reviews.
package provider

import (
	"context"
	"fmt"

	"skim/internal/domain"
)

// Provider is the remote pull-request service used by the application.
type Provider interface {
	Inbox(context.Context, domain.InboxFilter) (domain.ReviewInbox, error)
	Commits(context.Context, domain.PullRequest) (domain.CommitList, error)
	ReviewThreads(context.Context, domain.PullRequest) (domain.ReviewThreads, error)
	ReplyToReviewThread(context.Context, domain.PullRequest, string, string) (domain.PublishedComment, error)
	Attachments(context.Context, domain.PullRequest) (domain.Attachments, error)
	Load(context.Context, string) (domain.PullRequest, []domain.File, error)
	Head(context.Context, domain.PullRequest) (string, error)
	GitSource(domain.PullRequest) (domain.GitSource, error)
	ReviewSamples(context.Context, string, string) ([]string, error)
	Publish(context.Context, domain.PullRequest, []domain.Comment, string, string, string) (int64, error)
	FindReview(context.Context, domain.PullRequest, string) (int64, error)
}

// AmbiguousError means a review may have been submitted. Reconcile with
// FindReview before deciding whether it is safe to issue another POST.
// The underlying transport error is deliberately not exposed: it can contain
// credential-bearing URLs or data supplied by an untrusted HTTP transport.
type AmbiguousError struct {
	Operation string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("GitHub %s delivery is uncertain; reconcile submitted reviews before retrying", e.Operation)
}
