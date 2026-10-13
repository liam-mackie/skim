package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"skim/internal/domain"
)

func TestStateRevisionPersistsAcrossRestartAndScopedCommits(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			_ = s.Close()
		}
	}()
	if s.State().Revision != 0 || s.Snapshot().State().Revision != 0 {
		t.Fatal("new store revision is not zero")
	}
	d := s.Snapshot()
	d.Active = "github/owner/repo/1"
	pr := &domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, BaseSHA: "base", HeadSHA: "head"}
	analysis := &domain.Analysis{Key: d.Active, BaseSHA: "base", HeadSHA: "head", Generation: "generation", Revision: 1}
	d.Reviews[d.Active] = Record{AnalysisSnapshot: "snapshot", State: domain.State{PullRequest: pr, Analysis: analysis,
		Comments: []domain.Comment{{ID: "comment", Revision: 3, Body: "Keep this revision"}},
		Chats:    []domain.ChatThread{{ID: "thread", Revision: 2, Messages: []domain.Message{{Role: "user", Content: "Keep history"}}}}}}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	if s.State().Revision != 1 || s.Snapshot().State().Revision != 1 || d.State().Revision != 0 {
		t.Fatal("commit clock was not bound to detached snapshots")
	}
	if err := s.UpdateAnalysis(context.Background(), *analysis, "snapshot", func(r *Record) { r.State.Analysis.Risk.Status = "complete" }); err != nil {
		t.Fatal(err)
	}
	state := s.State()
	if state.Revision != 2 || state.Comments[0].Revision != 3 || state.Chats[0].Revision != 2 {
		t.Fatalf("analysis re-versioned entities or omitted commit clock: %+v", state)
	}
	if _, err := s.UpdateSettings(domain.SettingsUpdate{Settings: s.Settings()}, func(*Record, domain.Settings) {}); err != nil {
		t.Fatal(err)
	}
	if s.State().Revision != 3 {
		t.Fatal("settings commit did not advance durable clock")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	state = s.State()
	if state.Revision != 3 || state.Comments[0].Revision != 3 || state.Chats[0].Revision != 2 {
		t.Fatalf("restart reset revisions: %+v", state)
	}
	if err := s.Commit(s.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if s.State().Revision != 4 {
		t.Fatal("restart reused a durable revision")
	}
}

func TestLegacyRevisionZeroAndFailedCommitDoesNotAdvance(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":1,"active":"legacy","settings":{},"reviews":{"legacy":{"state":{"comments":[{"id":"old-comment"}],"chats":[{"id":"old-chat"}]}}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := s.State()
	if state.Revision != 0 || state.Comments[0].Revision != 0 || state.Chats[0].Revision != 0 {
		t.Fatalf("legacy entries fabricated revisions: %+v", state)
	}
	if err := s.Commit(s.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(s.Snapshot()); err == nil {
		t.Fatal("failed commit was accepted")
	}
	if s.State().Revision != 1 || s.Snapshot().State().Revision != 1 {
		t.Fatal("failed write advanced the durable public revision")
	}
}

func TestVerificationJobAndWorkspaceSnapshotsAreDetached(t *testing.T) {
	s, err := Open(t.TempDir(), domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := s.Snapshot()
	d.Active = "github/owner/repo/1"
	job := &domain.VerificationJob{ID: "job", ThreadID: "thread", PRKey: d.Active, Status: "running",
		Workspace: &domain.Workspace{ID: "workspace", Path: "/owned/worktree", Status: "in_use", Count: 1}}
	d.Reviews[d.Active] = Record{State: domain.State{Chats: []domain.ChatThread{{
		ID: "thread", Revision: 3, Kind: "verification", Verification: job,
		Messages: []domain.Message{{Role: "user", Content: "Original question"}},
	}}}}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	job.Status = "mutated commit input"
	job.Workspace.Path = "/mutated/commit/input"
	for _, snapshot := range []domain.State{s.State(), s.Snapshot().State()} {
		snapshot.Chats[0].Verification.Status = "mutated read result"
		snapshot.Chats[0].Verification.Workspace.Path = "/mutated/read/result"
		snapshot.Chats[0].Messages[0].Content = "mutated history"
	}
	state := s.State()
	thread := state.Chats[0]
	if thread.Verification.Status != "running" || thread.Verification.Workspace.Path != "/owned/worktree" || thread.Messages[0].Content != "Original question" {
		t.Fatalf("verification snapshots share mutable job/workspace/history storage: %+v", thread)
	}
	changed := s.Snapshot()
	record := changed.Reviews[changed.Active]
	record.State.Chats[0].Verification.Status = "cancelling"
	record.State.Chats[0].Verification.Workspace.Error = "Unresolved checkout status"
	record.State.Chats[0].Revision++
	changed.Reviews[changed.Active] = record
	if err := s.Commit(changed); err != nil {
		t.Fatal(err)
	}
	if thread.Verification.Status != "running" || thread.Verification.Workspace.Error != "" || thread.Revision != 3 {
		t.Fatal("a later job commit mutated an earlier snapshot")
	}
}

func TestRestartInterruptsUnfinishedVerificationWithoutReplayingOrLosingReports(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	d := s.Snapshot()
	d.Active = "other-review"
	key := "github/owner/repo/1"
	record := Record{State: domain.State{Comments: []domain.Comment{{ID: "manual", Revision: 2, Status: "pending", Body: "Keep this draft"}}}}
	statuses := []string{"queued", "preparing", "running", "summarizing", "cancelling", "complete", "failed", "cancelled"}
	for _, status := range statuses {
		record.State.Chats = append(record.State.Chats, domain.ChatThread{
			ID: status, Revision: 4, Kind: "verification",
			Messages: []domain.Message{{Role: "user", Content: "Saved question"}, {Role: "assistant", Content: "Saved full report"}},
			Verification: &domain.VerificationJob{ID: status, ThreadID: status, PRKey: key, Status: status, UpdatedAt: "before",
				Workspace: &domain.Workspace{ID: status, Path: "/owned/" + status, Status: "in_use"}},
		})
	}
	d.Reviews[key] = record
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for restart := range 2 {
		s, err = Open(dir, domain.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		snapshot := s.Snapshot()
		recovered := snapshot.Reviews[key]
		if snapshot.Active != "other-review" || len(recovered.State.Comments) != 1 || recovered.State.Comments[0] != record.State.Comments[0] {
			t.Fatal("restart selected or overwrote the inactive verification review")
		}
		for i, thread := range recovered.State.Chats {
			job := thread.Verification
			if len(thread.Messages) != 2 || thread.Messages[1].Content != "Saved full report" || job.Workspace.Path != "/owned/"+statuses[i] {
				t.Fatalf("restart %d discarded a report or checkout identity: %+v", restart, thread)
			}
			if i < 5 {
				if job.Status != "failed" || job.Active() || job.Error == "" || job.FinishedAt == "" || job.UpdatedAt == "before" || thread.Revision != 5 {
					t.Fatalf("restart %d replayed or failed to durably interrupt %s: %+v", restart, statuses[i], thread)
				}
			} else if job.Status != statuses[i] || job.UpdatedAt != "before" || thread.Revision != 4 {
				t.Fatalf("restart changed a terminal job: %+v", thread)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
