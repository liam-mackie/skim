package app

import (
	"context"
	"errors"
	"time"

	"skim/internal/domain"
	"skim/internal/store"
)

var ErrReviewBusy = errors.New("review is busy; wait for the conflicting action to finish")

type FinishReview struct {
	domain.PRIdentity
	DiscardChanges bool   `json:"discardChanges"`
	WorkspaceID    string `json:"workspaceId,omitempty"`
}

func (a *App) withWorkspace(state domain.State) domain.State {
	key := ""
	if state.PullRequest != nil {
		key = state.PullRequest.Key()
	}
	state.Workspace = a.workspaceStatus(key, state.Settings.ACP.ManagedRoot, state.Workspace)
	return state
}
func (a *App) Workspace() domain.WorkspaceUpdate {
	result := a.Store.Workspace()
	result.Workspace = a.workspaceStatus(result.PRKey, result.Workspace.Root, result.Workspace)
	return result
}

func (a *App) workspaceStatus(key, root string, saved domain.Workspace) domain.Workspace {
	current := a.Workspaces.Status(key, root)
	if saved.FinishedAt != "" {
		current.FinishedAt = saved.FinishedAt
		if current.Status == "not_created" {
			current.Status = "finished"
		}
	}
	current.Error = a.Store.Redact(current.Error)
	return current
}

func (a *App) markReviewFinished(r *store.Record, settings domain.Settings) {
	r.State.Workspace = domain.Workspace{Root: settings.ACP.ManagedRoot, Status: "cleanup_pending", FinishedAt: domain.Now()}
}

// The completion intent is committed together with publication success, before
// touching Git. A crash between those steps is recovered without another POST.
func (a *App) recoverCompletedWorkspaces() {
	d := a.Store.Snapshot()
	for key, r := range d.Reviews {
		if r.State.Workspace.FinishedAt == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		a.Workspaces.Finish(ctx, key, false, "")
		cancel()
	}
}

func (a *App) finishWorkspace(d store.Data, r store.Record, discard bool, expectedID string) domain.State {
	// A disconnected publication client must not skip local cleanup after GitHub
	// accepted the review. A bounded, independent context preserves that outcome.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := a.Workspaces.Finish(ctx, r.State.PullRequest.Key(), discard, expectedID)
	state := a.Store.State()
	state.Workspace = result
	if state.Workspace.Root == "" {
		state.Workspace.Root = d.Settings.ACP.ManagedRoot
	}
	state.Workspace.FinishedAt = r.State.Workspace.FinishedAt
	state.Workspace.Error = a.Store.Redact(state.Workspace.Error)
	return state
}

// FinishReview ends a local review round without deleting any review material.
// The discard flag is only accepted by this explicit action, never publication.
func (a *App) FinishReview(ctx context.Context, request FinishReview) (domain.State, error) {
	if err := a.tryLock(ctx); err != nil {
		return domain.State{}, err
	}
	defer a.unlock()
	if err := a.checkOperations("finish"); err != nil {
		return domain.State{}, err
	}
	d := a.Store.Snapshot()
	r, err := active(d)
	if err != nil {
		return domain.State{}, err
	}
	if !request.PRIdentity.Matches(r.State.PullRequest) {
		return domain.State{}, errors.New("finish request does not match the loaded PR and base/head; reload the pull request")
	}
	if r.Pending != nil {
		return domain.State{}, errors.New("publication delivery is uncertain; reconcile it before finishing this review")
	}
	if request.DiscardChanges {
		current := a.Workspaces.Status(r.State.PullRequest.Key(), d.Settings.ACP.ManagedRoot)
		if request.WorkspaceID == "" || request.WorkspaceID != current.ID {
			return domain.State{}, errors.New("workspace changed since discard confirmation; refresh its status before discarding")
		}
	}
	a.markReviewFinished(&r, d.Settings)
	if err := a.save(d, r); err != nil {
		return domain.State{}, err
	}
	return a.finishWorkspace(d, r, request.DiscardChanges, request.WorkspaceID), nil
}
