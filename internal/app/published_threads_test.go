package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"skim/internal/domain"
	"skim/internal/provider"
	"skim/internal/store"
)

type publishedThreadsProvider struct {
	provider.Provider
	read  func(context.Context, domain.PullRequest) (domain.ReviewThreads, error)
	reply func(context.Context, domain.PullRequest, string, string) (domain.PublishedComment, error)
}

func (p *publishedThreadsProvider) ReviewThreads(ctx context.Context, pr domain.PullRequest) (domain.ReviewThreads, error) {
	return p.read(ctx, pr)
}

func (p *publishedThreadsProvider) ReplyToReviewThread(ctx context.Context, pr domain.PullRequest, id, body string) (domain.PublishedComment, error) {
	return p.reply(ctx, pr, id, body)
}

func publishedThreadsApp(t *testing.T) (*App, domain.PRIdentity, *publishedThreadsProvider) {
	t.Helper()
	s, err := store.Open(t.TempDir(), domain.Settings{GitHubTokenEnv: "SKIM_TEST_THREAD_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 7, BaseSHA: "base", HeadSHA: "head"}
	d := s.Snapshot()
	d.Active = pr.Key()
	state := domain.EmptyState()
	state.PullRequest = &pr
	state.Comments = []domain.Comment{{ID: "local-draft", Body: "keep local draft", Status: "pending", HeadSHA: pr.HeadSHA}}
	state.Chats = []domain.ChatThread{{ID: "local-chat", Messages: []domain.Message{{Role: "user", Content: "keep conversation"}}}}
	d.Reviews[d.Active] = store.Record{State: state, Pending: &store.Publication{Marker: "keep-review-publication"}}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	p := &publishedThreadsProvider{}
	a := &App{Store: s, gate: make(chan struct{}, 1), lifetime: context.Background(), Provider: func(domain.Settings) provider.Provider { return p }}
	return a, domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}, p
}

func TestReviewThreadsReadDoesNotHoldGateOrPersistRemoteComments(t *testing.T) {
	a, identity, p := publishedThreadsApp(t)
	before := a.Store.Snapshot()
	p.read = func(ctx context.Context, pr domain.PullRequest) (domain.ReviewThreads, error) {
		if !identity.Matches(&pr) {
			t.Fatal("provider received the wrong PR")
		}
		select {
		case a.gate <- struct{}{}:
			a.unlock()
		default:
			t.Fatal("remote read held foreground gate")
		}
		return domain.ReviewThreads{PRIdentity: identity, Threads: []domain.ReviewThread{{ID: "123", Comments: []domain.PublishedComment{{ID: "123", Body: "remote only"}}}}}, nil
	}
	result, err := a.ReviewThreads(context.Background(), identity)
	if err != nil || len(result.Threads) != 1 || result.Threads[0].Comments[0].Body != "remote only" || result.Warnings == nil {
		t.Fatalf("incorrect thread response: %+v %v", result, err)
	}
	if !reflect.DeepEqual(before, a.Store.Snapshot()) {
		t.Fatal("remote discussion changed local review state")
	}
	p.read = func(context.Context, domain.PullRequest) (domain.ReviewThreads, error) {
		return domain.ReviewThreads{PRIdentity: identity}, nil
	}
	result, err = a.ReviewThreads(context.Background(), identity)
	if err != nil || result.Threads == nil || result.Warnings == nil {
		t.Fatalf("empty lists must be non-nil: %+v %v", result, err)
	}
}

func TestReviewThreadsRejectsStaleAndChangingActiveIdentity(t *testing.T) {
	for _, field := range []string{"key", "base", "head"} {
		for _, timing := range []string{"before", "during", "response"} {
			t.Run(field+"-"+timing, func(t *testing.T) {
				a, identity, p := publishedThreadsApp(t)
				changed := identity
				switch field {
				case "key":
					changed.PRKey = "other-pr"
				case "base":
					changed.BaseSHA = "changed-base"
				case "head":
					changed.HeadSHA = "changed-head"
				}
				calls := 0
				p.read = func(context.Context, domain.PullRequest) (domain.ReviewThreads, error) {
					calls++
					if timing == "during" {
						d := a.Store.Snapshot()
						r := d.Reviews[d.Active]
						switch field {
						case "key":
							r.State.PullRequest.Number++
						case "base":
							r.State.PullRequest.BaseSHA = changed.BaseSHA
						case "head":
							r.State.PullRequest.HeadSHA = changed.HeadSHA
						}
						d.Reviews[d.Active] = r
						if err := a.Store.Commit(d); err != nil {
							t.Fatal(err)
						}
					}
					responseIdentity := identity
					if timing == "response" {
						responseIdentity = changed
					}
					return domain.ReviewThreads{PRIdentity: responseIdentity, Threads: []domain.ReviewThread{{ID: "wrong-snapshot"}}}, nil
				}
				request := identity
				if timing == "before" {
					request = changed
				}
				result, err := a.ReviewThreads(context.Background(), request)
				if err == nil || !strings.Contains(err.Error(), "reload") || len(result.Threads) != 0 || (timing == "before" && calls != 0) || (timing != "before" && calls != 1) {
					t.Fatalf("mixed snapshot escaped validation: %+v %v calls=%d", result, err, calls)
				}
			})
		}
	}
}

func TestPublishedThreadErrorsRedactConfiguredCredentials(t *testing.T) {
	t.Setenv("SKIM_TEST_THREAD_TOKEN", "thread-secret")
	a, identity, p := publishedThreadsApp(t)
	p.read = func(context.Context, domain.PullRequest) (domain.ReviewThreads, error) {
		return domain.ReviewThreads{}, errors.New("read failed: thread-secret")
	}
	p.reply = func(context.Context, domain.PullRequest, string, string) (domain.PublishedComment, error) {
		return domain.PublishedComment{}, errors.New("reply failed: thread-secret")
	}
	_, readErr := a.ReviewThreads(context.Background(), identity)
	_, replyErr := a.ReplyToReviewThread(context.Background(), domain.ReviewReply{PRIdentity: identity, ThreadID: "123", Body: "reply"})
	for _, err := range []error{readErr, replyErr} {
		if err == nil || strings.Contains(err.Error(), "thread-secret") || !strings.Contains(err.Error(), "[redacted]") {
			t.Fatalf("unsafe service error: %v", err)
		}
	}
}

func TestReplyReleasesGateAndReturnsSuccessWithoutPersistenceOrRead(t *testing.T) {
	a, identity, p := publishedThreadsApp(t)
	before := a.Store.Snapshot()
	body := "  Reply 日本語\n  "
	want := domain.PublishedComment{ID: "9007199254740993", Body: body, Author: "reviewer", URL: "https://github.com/owner/repo/pull/7#discussion_r9007199254740993"}
	calls := 0
	p.reply = func(ctx context.Context, pr domain.PullRequest, id, text string) (domain.PublishedComment, error) {
		calls++
		if !identity.Matches(&pr) || id != "123" || text != body || len(a.gate) != 0 {
			t.Fatalf("reply held gate or changed request: %+v id=%s body=%q gate=%d", pr, id, text, len(a.gate))
		}
		if err := a.SetStyle(context.Background(), "must not replace captured instructions"); !errors.Is(err, ErrReviewBusy) {
			t.Fatalf("context change during remote write: %v", err)
		}
		if _, err := a.Publish(context.Background(), identity, "COMMENT", "do not race reply"); !errors.Is(err, ErrReviewBusy) {
			t.Fatalf("publication during remote reply: %v", err)
		}
		return want, nil
	}
	p.read = func(context.Context, domain.PullRequest) (domain.ReviewThreads, error) {
		t.Fatal("successful reply initiated a dependent refresh")
		return domain.ReviewThreads{}, errors.New("should not run")
	}
	got, err := a.ReplyToReviewThread(context.Background(), domain.ReviewReply{PRIdentity: identity, ThreadID: "123", Body: body})
	if err != nil || got != want || calls != 1 || len(a.gate) != 0 {
		t.Fatalf("incorrect reply success: %+v %v calls=%d", got, err, calls)
	}
	if !reflect.DeepEqual(before, a.Store.Snapshot()) {
		t.Fatal("remote reply mutated drafts, chats, or pending review publication")
	}
}

func TestReplyRejectsBusyAndStaleAdmission(t *testing.T) {
	a, identity, p := publishedThreadsApp(t)
	p.reply = func(context.Context, domain.PullRequest, string, string) (domain.PublishedComment, error) {
		t.Error("stale reply reached provider")
		return domain.PublishedComment{}, nil
	}
	if err := a.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ReplyToReviewThread(context.Background(), domain.ReviewReply{PRIdentity: identity, ThreadID: "123", Body: "reply"}); !errors.Is(err, ErrReviewBusy) {
		a.unlock()
		t.Fatalf("reply queued behind conflicting action: %v", err)
	}
	d := a.Store.Snapshot()
	r := d.Reviews[d.Active]
	r.State.PullRequest.HeadSHA = "new-head"
	d.Reviews[d.Active] = r
	if err := a.Store.Commit(d); err != nil {
		a.unlock()
		t.Fatal(err)
	}
	a.unlock()
	if _, err := a.ReplyToReviewThread(context.Background(), domain.ReviewReply{PRIdentity: identity, ThreadID: "123", Body: "reply"}); err == nil || !strings.Contains(err.Error(), "reload") {
		t.Fatalf("stale reply was not rejected: %v", err)
	}
}

func TestReplyRejectsInvalidInputCanceledGateAndClosedApp(t *testing.T) {
	a, identity, p := publishedThreadsApp(t)
	p.reply = func(context.Context, domain.PullRequest, string, string) (domain.PublishedComment, error) {
		t.Error("invalid reply reached provider")
		return domain.PublishedComment{}, nil
	}
	for _, reply := range []domain.ReviewReply{
		{PRIdentity: identity, ThreadID: "123", Body: " "},
		{PRIdentity: identity, ThreadID: "0123", Body: "reply"},
		{PRIdentity: identity, ThreadID: "123", Body: strings.Repeat("x", 60001)},
		{ThreadID: "123", Body: "reply"},
	} {
		if _, err := a.ReplyToReviewThread(context.Background(), reply); err == nil {
			t.Fatal("invalid reply accepted")
		}
	}
	if err := a.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.ReplyToReviewThread(ctx, domain.ReviewReply{PRIdentity: identity, ThreadID: "123", Body: "reply"})
	a.unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled gate wait was not respected: %v", err)
	}
	a.lifetime = ctx
	_, err = a.ReplyToReviewThread(context.Background(), domain.ReviewReply{PRIdentity: identity, ThreadID: "123", Body: "reply"})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed application accepted reply: %v", err)
	}
}

func TestReplyUncertaintyPreservesSentinelAndNeverRetries(t *testing.T) {
	a, identity, p := publishedThreadsApp(t)
	before := a.Store.Snapshot()
	calls := 0
	p.reply = func(context.Context, domain.PullRequest, string, string) (domain.PublishedComment, error) {
		calls++
		return domain.PublishedComment{}, provider.ErrReplyDeliveryUncertain
	}
	result, err := a.ReplyToReviewThread(context.Background(), domain.ReviewReply{PRIdentity: identity, ThreadID: "123", Body: "keep this composed reply"})
	if !errors.Is(err, provider.ErrReplyDeliveryUncertain) || calls != 1 || result.ID != "" {
		t.Fatalf("uncertainty lost or retried: %+v %v calls=%d", result, err, calls)
	}
	if !reflect.DeepEqual(before, a.Store.Snapshot()) {
		t.Fatal("uncertain reply changed local drafts or publication state")
	}
}
