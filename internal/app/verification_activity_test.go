package app

import (
	"context"
	"testing"

	"skim/internal/domain"
	"skim/internal/store"
)

func TestClearVerificationActivityHidesOnlyFinishedJobs(t *testing.T) {
	s, err := store.Open(t.TempDir(), domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 7, BaseSHA: "base", HeadSHA: "head"}
	d := s.Snapshot()
	d.Active = pr.Key()
	state := domain.EmptyState()
	state.PullRequest = &pr
	for i, status := range []string{"complete", "failed", "cancelled", "running"} {
		state.Chats = append(state.Chats, domain.ChatThread{ID: status, Revision: uint64(i + 1), Kind: "verification",
			Messages:     []domain.Message{{Role: "assistant", Content: status + " report"}},
			Verification: &domain.VerificationJob{ID: "job-" + status, ThreadID: status, PRKey: pr.Key(), Status: status, Revision: uint64(i + 1)}})
	}
	d.Reviews[d.Active] = store.Record{State: state}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	a := &App{Store: s, gate: make(chan struct{}, 1), lifetime: context.Background()}

	cleared, err := a.ClearVerificationActivity(context.Background())
	if err != nil || len(cleared) != 3 {
		t.Fatalf("cleared %d jobs: %v", len(cleared), err)
	}
	for _, thread := range a.Store.State().Chats {
		job := thread.Verification
		if job.Cleared != (job.Status != "running") {
			t.Fatalf("%s job cleared=%v", job.Status, job.Cleared)
		}
		if job.Cleared && (job.Revision != thread.Revision || len(thread.Messages) != 1) {
			t.Fatalf("cleared %s job lost its report or did not advance its revision: %+v", job.Status, thread)
		}
	}
	if again, err := a.ClearVerificationActivity(context.Background()); err != nil || len(again) != 0 {
		t.Fatalf("repeat clear changed %d jobs: %v", len(again), err)
	}
}
