package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"skim/internal/domain"
	"skim/internal/provider"
)

type operationCall struct {
	kind     string
	messages []domain.Message
	reply    chan string
}

type operationTransport struct {
	calls     chan operationCall
	cancelled chan struct{}
	drain     <-chan struct{}
}

func (p *operationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var input struct {
		Messages []domain.Message `json:"messages"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		return nil, err
	}
	kind := "chat"
	if len(input.Messages) > 0 && strings.Contains(input.Messages[0].Content, "Review this changed file") {
		kind = "review"
	}
	call := operationCall{kind: kind, messages: input.Messages, reply: make(chan string, 1)}
	select {
	case p.calls <- call:
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	select {
	case content := <-call.reply:
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
	case <-request.Context().Done():
		if p.cancelled != nil {
			p.cancelled <- struct{}{}
		}
		if p.drain != nil {
			<-p.drain
		}
		return nil, request.Context().Err()
	}
}

func operationFixture(t *testing.T) (*App, domain.PRIdentity, *operationTransport) {
	t.Helper()
	a, identity, _ := newReviewProgressFixture(t, []domain.File{reviewProgressFile("a.go")})
	d := a.Store.Snapshot()
	d.Settings.Roles.Chat = d.Settings.Roles.Review
	if err := a.Store.Commit(d); err != nil {
		t.Fatal(err)
	}
	transport := &operationTransport{calls: make(chan operationCall, 8)}
	a.Models.HTTP = &http.Client{Transport: transport}
	return a, identity, transport
}

func nextOperationCall(t *testing.T, p *operationTransport, kind string) operationCall {
	t.Helper()
	select {
	case call := <-p.calls:
		if call.kind != kind {
			t.Fatalf("model call %s, want %s", call.kind, kind)
		}
		return call
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not reach its model while independent work was held", kind)
		return operationCall{}
	}
}

type chatResult struct {
	thread domain.ChatThread
	err    error
}

func startOperationChat(t *testing.T, a *App, id string) (<-chan chatResult, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan chatResult, 1)
	go func() { thread, err := a.Chat(ctx, "Explain this change.", id); done <- chatResult{thread, err} }()
	return done, cancel
}

func awaitOperationChat(t *testing.T, done <-chan chatResult) chatResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("chat did not finish")
		return chatResult{}
	}
}

func operationComment(identity domain.PRIdentity, body string) domain.ManualComment {
	return domain.ManualComment{PRKey: identity.PRKey, BaseSHA: identity.BaseSHA, HeadSHA: identity.HeadSHA, Path: "a.go", StartLine: 1, EndLine: 1, Side: "RIGHT", Body: body}
}

func TestReviewChatAndManualWritesPreserveBothCompletionOrders(t *testing.T) {
	for _, reviewFirst := range []bool{false, true} {
		name := "chat-first"
		if reviewFirst {
			name = "review-first"
		}
		t.Run(name, func(t *testing.T) {
			a, identity, p := operationFixture(t)
			reviewDone, _ := startProgressReview(t, a)
			reviewCall := nextOperationCall(t, p, "review")
			chatDone, _ := startOperationChat(t, a, "")
			chatCall := nextOperationCall(t, p, "chat")
			manual, err := a.CreateComment(context.Background(), operationComment(identity, "New finding"))
			if err != nil || manual.Revision != 1 {
				t.Fatalf("manual during AI: %+v %v", manual, err)
			}
			body, approved := "User-edited finding", "pending"
			edited, err := a.UpdateComment(context.Background(), "old-draft", CommentUpdate{Body: &body, Status: &approved})
			if err != nil || edited.Revision != 1 {
				t.Fatalf("edit during AI: %+v %v", edited, err)
			}
			finishReview := func() {
				reviewCall.reply <- firstProgressFindings
				result := awaitProgressReview(t, reviewDone)
				if result.err != nil {
					t.Fatal(result.err)
				}
				if result.state.Revision != a.Store.State().Revision || len(result.state.Comments) != 2 {
					t.Fatalf("review returned stale revision/drafts: %+v", result.state)
				}
			}
			finishChat := func() {
				chatCall.reply <- "Independent answer"
				result := awaitOperationChat(t, chatDone)
				if result.err != nil || result.thread.Revision != 1 {
					t.Fatalf("chat: %+v", result)
				}
			}
			if reviewFirst {
				finishReview()
				finishChat()
			} else {
				finishChat()
				finishReview()
			}
			state := a.Store.State()
			if len(state.Comments) != 2 || state.Comments[0] != edited || state.Comments[1] != manual || len(state.Chats) != 1 || state.Chats[0].Messages[1].Content != "Independent answer" {
				t.Fatalf("completion overwrote another writer: %+v", state)
			}
			if run := readProgress(t, a, identity).Run; run.Status != "complete" || run.Findings != 0 {
				t.Fatalf("latest manual draft was not deduplicated: %+v", run)
			}
		})
	}
}

func TestOperationAdmissionRejectsConflictsWithoutQueueing(t *testing.T) {
	a, identity, p := operationFixture(t)
	done, cancel := startProgressReview(t, a)
	nextOperationCall(t, p, "review")
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	checks := []func() error{
		func() error { _, err := a.Review(ctx); return err },
		func() error { _, err := a.Publish(ctx, identity, "COMMENT", "summary"); return err },
		func() error { _, err := a.FinishReview(ctx, FinishReview{PRIdentity: identity}); return err },
		func() error { _, err := a.Load(ctx, "https://github.com/owner/repo/pull/2"); return err },
		func() error { _, err := a.SetSettings(ctx, a.Settings()); return err },
		func() error { return a.SetStyle(ctx, "changed instructions") },
		func() error { _, _, err := a.ImportStyle(ctx, "owner", "repo"); return err },
	}
	for i, check := range checks {
		if err := check(); !errors.Is(err, ErrReviewBusy) {
			t.Fatalf("conflict %d queued or accepted: %v", i, err)
		}
	}
	cancel()
	if result := awaitProgressReview(t, done); result.err == nil {
		t.Fatal("cancelled review saved")
	}
}

func TestConcurrentThreadsAndSameThreadAdmission(t *testing.T) {
	a, _, p := operationFixture(t)
	first, _ := startOperationChat(t, a, "")
	nextOperationCall(t, p, "chat").reply <- "First answer"
	initial := awaitOperationChat(t, first)
	if initial.err != nil {
		t.Fatal(initial.err)
	}
	turn, _ := startOperationChat(t, a, initial.thread.ID)
	turnCall := nextOperationCall(t, p, "chat")
	if len(turnCall.messages) != 4 || turnCall.messages[2].Content != "First answer" {
		t.Fatalf("turn did not capture its own prior conversation: %+v", turnCall.messages)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := a.Chat(ctx, "Duplicate turn", initial.thread.ID); !errors.Is(err, ErrReviewBusy) {
		t.Fatalf("same-thread turn: %v", err)
	}
	other, _ := startOperationChat(t, a, "")
	nextOperationCall(t, p, "chat").reply <- "Other conversation"
	if result := awaitOperationChat(t, other); result.err != nil {
		t.Fatal(result.err)
	}
	turnCall.reply <- "Second answer"
	result := awaitOperationChat(t, turn)
	if result.err != nil || result.thread.Revision != 2 || len(result.thread.Messages) != 4 || len(a.State().Chats) != 2 {
		t.Fatalf("turn lost conversation: %+v", result)
	}
}

func TestOperationCommitsRejectChangedEvidenceAndThreadRevision(t *testing.T) {
	for _, change := range []string{"key", "base", "head", "patch", "thread"} {
		t.Run(change, func(t *testing.T) {
			a, _, p := operationFixture(t)
			var reviewDone <-chan reviewProgressResult
			var chatDone <-chan chatResult
			var call operationCall
			if change == "thread" {
				first, _ := startOperationChat(t, a, "")
				nextOperationCall(t, p, "chat").reply <- "First answer"
				initial := awaitOperationChat(t, first)
				if initial.err != nil {
					t.Fatal(initial.err)
				}
				chatDone, _ = startOperationChat(t, a, initial.thread.ID)
				call = nextOperationCall(t, p, "chat")
			} else {
				reviewDone, _ = startProgressReview(t, a)
				call = nextOperationCall(t, p, "review")
			}
			d := a.Store.Snapshot()
			r := d.Reviews[d.Active]
			switch change {
			case "key":
				r.State.PullRequest.Number++
				d.Active = r.State.PullRequest.Key()
			case "base":
				r.State.PullRequest.BaseSHA = "replacement-base"
			case "head":
				r.State.PullRequest.HeadSHA = "replacement-head"
			case "patch":
				r.State.Files[0].Patch += "\n+different evidence"
			case "thread":
				r.State.Chats[0].Revision++
				r.State.Chats[0].Messages = append(r.State.Chats[0].Messages, domain.Message{Role: "assistant", Content: "Newer turn"})
			}
			d.Reviews[d.Active] = r
			if err := a.Store.Commit(d); err != nil {
				t.Fatal(err)
			}
			before := a.Store.State()
			call.reply <- firstProgressFindings
			var err error
			if chatDone != nil {
				err = awaitOperationChat(t, chatDone).err
			} else {
				err = awaitProgressReview(t, reviewDone).err
			}
			if !errors.Is(err, ErrReviewBusy) || !reflect.DeepEqual(before, a.Store.State()) {
				t.Fatalf("stale commit accepted or changed data: %v", err)
			}
		})
	}
}

func TestOperationCancellationDoesNotLoseIndependentCompletion(t *testing.T) {
	for _, cancelReview := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "review"}[cancelReview], func(t *testing.T) {
			a, _, p := operationFixture(t)
			reviewDone, stopReview := startProgressReview(t, a)
			reviewCall := nextOperationCall(t, p, "review")
			chatDone, stopChat := startOperationChat(t, a, "")
			chatCall := nextOperationCall(t, p, "chat")
			if cancelReview {
				chatCall.reply <- "Saved independently"
				if result := awaitOperationChat(t, chatDone); result.err != nil {
					t.Fatal(result.err)
				}
				stopReview()
				if result := awaitProgressReview(t, reviewDone); result.err == nil {
					t.Fatal("cancelled review succeeded")
				}
				if state := a.State(); len(state.Chats) != 1 || len(state.Comments) != 1 {
					t.Fatalf("cancel lost chat: %+v", state)
				}
			} else {
				reviewCall.reply <- firstProgressFindings
				if result := awaitProgressReview(t, reviewDone); result.err != nil {
					t.Fatal(result.err)
				}
				stopChat()
				if result := awaitOperationChat(t, chatDone); result.err == nil {
					t.Fatal("cancelled chat succeeded")
				}
				if state := a.State(); len(state.Comments) != 2 || len(state.Chats) != 0 {
					t.Fatalf("cancel lost review: %+v", state)
				}
			}
		})
	}
}

func TestCloseCancelsAndDrainsAllOperations(t *testing.T) {
	a, identity, p := operationFixture(t)
	drain := make(chan struct{})
	p.cancelled, p.drain = make(chan struct{}, 2), drain
	release := sync.OnceFunc(func() { close(drain) })
	defer release()
	reviewDone, _ := startProgressReview(t, a)
	nextOperationCall(t, p, "review")
	chatDone, _ := startOperationChat(t, a, "")
	nextOperationCall(t, p, "chat")
	manual, err := a.CreateComment(context.Background(), operationComment(identity, "Persist across shutdown"))
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	for range 2 {
		select {
		case <-p.cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("shutdown did not cancel every model")
		}
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before model draining: %v", err)
	default:
	}
	release()
	if result := awaitProgressReview(t, reviewDone); result.err == nil {
		t.Fatal("shutdown review saved")
	}
	if result := awaitOperationChat(t, chatDone); result.err == nil {
		t.Fatal("shutdown chat saved")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not drain")
	}
	state := a.Store.State()
	if len(state.Comments) != 2 || state.Comments[1] != manual || len(state.Chats) != 0 {
		t.Fatalf("shutdown lost completed work: %+v", state)
	}
	if _, err := a.Chat(context.Background(), "after close", ""); err == nil {
		t.Fatal("closed app admitted a new model")
	}
}

func TestReplyLeaseAllowsReviewManualAndFinishAndDrainsOnClose(t *testing.T) {
	a, identity, transport := operationFixture(t)
	started, cancelled, drain := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(drain) })
	defer release()
	p := &publishedThreadsProvider{}
	p.reply = func(ctx context.Context, _ domain.PullRequest, _, _ string) (domain.PublishedComment, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-drain
		return domain.PublishedComment{}, ctx.Err()
	}
	a.Provider = func(domain.Settings) provider.Provider { return p }
	replyDone := make(chan error, 1)
	go func() {
		_, err := a.ReplyToReviewThread(context.Background(), domain.ReviewReply{PRIdentity: identity, ThreadID: "123", Body: "Reply while local work continues."})
		replyDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("reply did not start")
	}
	manual, err := a.CreateComment(context.Background(), operationComment(identity, "Independent human comment."))
	if err != nil {
		t.Fatal(err)
	}
	finished, err := a.FinishReview(context.Background(), FinishReview{PRIdentity: identity})
	if err != nil || finished.Workspace.FinishedAt == "" {
		t.Fatalf("reply blocked workspace finish: %v", err)
	}
	reviewDone, _ := startProgressReview(t, a)
	nextOperationCall(t, transport, "review")
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel reply")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close did not drain reply: %v", err)
	default:
	}
	release()
	select {
	case err := <-replyDone:
		if err == nil {
			t.Fatal("cancelled reply succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reply did not finish")
	}
	if result := awaitProgressReview(t, reviewDone); result.err == nil {
		t.Fatal("shutdown review saved")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not finish")
	}
	state := a.State()
	if len(state.Comments) != 2 || state.Comments[1] != manual || state.Workspace.FinishedAt != finished.Workspace.FinishedAt {
		t.Fatalf("reply/shutdown lost local work: %+v", state)
	}
}

func TestDeleteDuringReviewAndChatSurvivesEitherCompletionOrder(t *testing.T) {
	for _, reviewFirst := range []bool{false, true} {
		name := "chat-first"
		if reviewFirst {
			name = "review-first"
		}
		t.Run(name, func(t *testing.T) {
			a, _, p := operationFixture(t)
			reviewDone, _ := startProgressReview(t, a)
			reviewCall := nextOperationCall(t, p, "review")
			chatDone, _ := startOperationChat(t, a, "")
			chatCall := nextOperationCall(t, p, "chat")
			deleted, err := a.DeleteComment(context.Background(), "old-draft")
			if err != nil || deleted != (domain.Comment{ID: "old-draft", Revision: 1, Status: "deleted"}) {
				t.Fatalf("delete during AI: %+v %v", deleted, err)
			}
			finishReview := func() {
				reviewCall.reply <- firstProgressFindings
				result := awaitProgressReview(t, reviewDone)
				if result.err != nil || result.state.Comments[0] != deleted {
					t.Fatalf("review resurrected deleted ID: %+v %v", result.state, result.err)
				}
			}
			finishChat := func() {
				chatCall.reply <- "Independent answer"
				if result := awaitOperationChat(t, chatDone); result.err != nil {
					t.Fatal(result.err)
				}
			}
			if reviewFirst {
				finishReview()
				finishChat()
			} else {
				finishChat()
				finishReview()
			}
			state := a.Store.State()
			if len(state.Comments) != 2 || state.Comments[0] != deleted || state.Comments[1].Status != "suggested" || len(state.Chats) != 1 {
				t.Fatalf("completion lost tombstone or new result: %+v", state)
			}
		})
	}
}
