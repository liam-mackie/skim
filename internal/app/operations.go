package app

import (
	"context"
	"errors"
	"fmt"

	"skim/internal/domain"
	"skim/internal/store"
)

// An operation owns captured evidence, not a store transaction. Admission and
// removal happen under the gate; external work and shutdown draining do not.
type operation struct {
	ctx            context.Context
	cancel         context.CancelFunc
	stop           func() bool
	kind, threadID string
	identity       domain.PRIdentity
	snapshot       string
}

func (a *App) tryLock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case a.gate <- struct{}{}:
		return nil
	default:
		return ErrReviewBusy
	}
}

// Context changes conflict with captured AI evidence and remote replies.
// Cleanup does not touch chat/replies; publication must not race a remote write.
func (a *App) checkOperations(action string) error {
	if a.lifetime.Err() != nil {
		return errors.New("application is closed")
	}
	key := ""
	if pr := a.Store.PullRequest(); pr != nil {
		key = pr.Key()
	}
	if action == "publish" || action == "finish" {
		for _, run := range a.verifications {
			if run.identity.PRKey == key {
				return fmt.Errorf("%w: verification is running for this PR; wait or cancel its checks", ErrReviewBusy)
			}
		}
	}
	for op := range a.operations {
		if action == "context" || (op.identity.PRKey == key && (op.kind == "review" || (action == "publish" && op.kind == "reply"))) {
			return fmt.Errorf("%w: %s is running", ErrReviewBusy, op.kind)
		}
	}
	return nil
}

func (a *App) beginOperation(ctx context.Context, kind, threadID string, r store.Record) (*operation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.lifetime.Err() != nil {
		return nil, errors.New("application is closed")
	}
	for op := range a.operations {
		if (kind == "review" && op.kind == kind) || (threadID != "" && op.threadID == threadID) {
			return nil, fmt.Errorf("%w: %s already running", ErrReviewBusy, kind)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	pr := r.State.PullRequest
	op := &operation{ctx: ctx, cancel: cancel, stop: context.AfterFunc(a.lifetime, cancel), kind: kind, threadID: threadID,
		identity: domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA},
		snapshot: analysisSnapshot(*pr, r.State.Files)}
	if a.operations == nil {
		a.operations = make(map[*operation]struct{})
	}
	a.operations[op] = struct{}{}
	a.jobs.Add(1)
	return op, nil
}

func (a *App) endOperation(op *operation) {
	op.stop()
	op.cancel()
	_ = a.lock(context.Background())
	delete(a.operations, op)
	a.unlock()
	a.jobs.Done()
}

// Call with the completion gate held. Never commit a captured full record: a
// manual edit, independent conversation, or analysis may have completed since.
func (a *App) operationState(op *operation) (store.Data, store.Record, error) {
	if err := op.ctx.Err(); err != nil {
		return store.Data{}, store.Record{}, err
	}
	if err := a.lifetime.Err(); err != nil {
		return store.Data{}, store.Record{}, err
	}
	d := a.Store.Snapshot()
	r, err := active(d)
	if err != nil || d.Active != op.identity.PRKey || !op.identity.Matches(r.State.PullRequest) || analysisSnapshot(*r.State.PullRequest, r.State.Files) != op.snapshot {
		return store.Data{}, store.Record{}, fmt.Errorf("%w: pull request evidence changed; no result saved", ErrReviewBusy)
	}
	return d, r, nil
}

func currentThreadIndex(r store.Record, captured domain.ChatThread, existed bool) (int, error) {
	for i, current := range r.State.Chats {
		if current.ID == captured.ID {
			if !existed || current.Revision != captured.Revision || current.Stale || current.Kind != captured.Kind || current.HeadSHA != captured.HeadSHA {
				return 0, fmt.Errorf("%w: conversation changed; no turn saved", ErrReviewBusy)
			}
			return i, nil
		}
	}
	if existed {
		return 0, fmt.Errorf("%w: conversation no longer exists", ErrReviewBusy)
	}
	return -1, nil
}
