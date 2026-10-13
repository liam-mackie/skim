package app

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"skim/internal/domain"
	"skim/internal/provider"
	"skim/internal/store"
)

func reviewedFixture(t *testing.T) (*App, domain.PRIdentity, domain.State) {
	t.Helper()
	s, err := store.Open(t.TempDir(), domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	a := New(s)
	t.Cleanup(func() { _ = a.Close(); _ = s.Close() })
	pr := domain.PullRequest{Provider: "github", Owner: "team", Repo: "repo", Number: 7, BaseSHA: "base", HeadSHA: "head"}
	state := domain.EmptyState()
	state.PullRequest = &pr
	state.Files = []domain.File{
		{Path: "a.go", Status: "modified", PatchStatus: "available", Patch: "@@ -1 +1 @@\n-old\n+new", Additions: 1, Deletions: 1},
		{Path: "a_test.go", Status: "added", PatchStatus: "available", Patch: "@@ -0,0 +1 @@\n+test", Additions: 1},
		{Path: "image.png", Status: "added", PatchStatus: "unavailable"},
	}
	state.Comments = []domain.Comment{{ID: "human", Body: "Keep my finding", Status: "pending"}}
	d := s.Snapshot()
	d.Active = pr.Key()
	d.Reviews[pr.Key()] = store.Record{State: state}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	return a, domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}, state
}

func TestFileReviewBatchValidationPersistenceAndCloning(t *testing.T) {
	a, identity, initial := reviewedFixture(t)
	ctx := context.Background()
	progress, err := a.SetReviewed(ctx, identity, []string{"a_test.go", "a.go"}, true)
	if err != nil || progress.FileReview.Revision != 1 || !reflect.DeepEqual(progress.FileReview.ReviewedPaths, []string{"a.go", "a_test.go"}) {
		t.Fatalf("batch: %+v %v", progress, err)
	}
	before := a.Store.Snapshot()
	for _, paths := range [][]string{nil, {"a.go", "missing"}, {"a.go", "a.go"}, {""}} {
		if _, err := a.SetReviewed(ctx, identity, paths, false); err == nil {
			t.Fatalf("accepted invalid batch %v", paths)
		}
	}
	stale := identity
	stale.HeadSHA = "different"
	if _, err := a.SetReviewed(ctx, stale, []string{"a.go"}, false); err == nil {
		t.Fatal("accepted stale snapshot")
	}
	if _, err := a.SetReviewed(ctx, identity, []string{"a.go", "a_test.go"}, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Store.Snapshot(), before) {
		t.Fatal("invalid or idempotent request changed saved state")
	}
	progress.FileReview.ReviewedPaths[0] = "caller-mutation"
	copy := a.Store.Snapshot()
	copy.Reviews[identity.PRKey].State.FileReview.ReviewedPaths[0] = "snapshot-mutation"
	if !reflect.DeepEqual(a.Store.Snapshot().Reviews[identity.PRKey].State.FileReview.ReviewedPaths, []string{"a.go", "a_test.go"}) {
		t.Fatal("review paths share mutable snapshot storage")
	}
	result, err := a.SetReviewed(ctx, identity, []string{"a.go"}, false)
	if err != nil || result.FileReview.Revision != 2 || !reflect.DeepEqual(result.FileReview.ReviewedPaths, []string{"a_test.go"}) {
		t.Fatalf("unreview: %+v %v", result, err)
	}
	if !reflect.DeepEqual(a.Store.State().Comments, initial.Comments) {
		t.Fatal("review progress changed comments")
	}
	dir := a.Store.Directory()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(reopened.State().FileReview, &result.FileReview) {
		t.Fatal("review progress did not survive restart")
	}
}

func TestFileReviewIndependentBatchesAndInactivePR(t *testing.T) {
	a, identity, initial := reviewedFixture(t)
	other := initial
	pr := *initial.PullRequest
	pr.Number = 8
	other.PullRequest = &pr
	d := a.Store.Snapshot()
	d.Active = pr.Key()
	d.Reviews[pr.Key()] = store.Record{State: other}
	if err := a.Store.Commit(d); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errors := make(chan error, 2)
	for _, path := range []string{"a.go", "a_test.go"} {
		group.Add(1)
		go func(path string) {
			defer group.Done()
			_, err := a.SetReviewed(context.Background(), identity, []string{path}, true)
			errors <- err
		}(path)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	after := a.Store.Snapshot()
	if after.Active != pr.Key() || after.Reviews[pr.Key()].State.FileReview != nil {
		t.Fatal("progress changed the active or unrelated PR")
	}
	if !reflect.DeepEqual(after.Reviews[identity.PRKey].State.FileReview.ReviewedPaths, []string{"a.go", "a_test.go"}) {
		t.Fatal("independent batches overwrote each other")
	}
}

func TestFileReviewReconcilesOnlyChangedEvidence(t *testing.T) {
	_, _, initial := reviewedFixture(t)
	initial.Files = append(initial.Files, domain.File{Path: "partial.go", Status: "modified", PatchStatus: "truncated", Patch: "+partial"})
	for _, changeRefs := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-refs", true: "new-refs"}[changeRefs], func(t *testing.T) {
			state := initial
			state.FileReview = &domain.FileReview{Revision: 4, ReviewedPaths: []string{"a.go", "a_test.go", "image.png", "partial.go"}}
			pr := *initial.PullRequest
			if changeRefs {
				pr.HeadSHA = "new-head"
			}
			files := append([]domain.File(nil), initial.Files...)
			files[0].Risk = &domain.Risk{Level: "high"}
			files[0].TestOnly = &domain.TestOnlyAssessment{OnlyTests: true, Confidence: 0.98, Probability: 0.98}
			reconcileFileReview(&state, pr, files)
			want := []string{"a.go", "a_test.go", "image.png", "partial.go"}
			if changeRefs {
				want = []string{"a.go", "a_test.go"}
			}
			if !reflect.DeepEqual(state.FileReview.ReviewedPaths, want) {
				t.Fatalf("unexpected retained evidence: %+v", state.FileReview)
			}
			if state.FileReview.Revision != 4+uint64(map[bool]int{false: 0, true: 1}[changeRefs]) {
				t.Fatal("incorrect reconciliation clock")
			}
		})
	}
	for _, mutate := range []func(*domain.File){
		func(f *domain.File) { f.Patch += "\n+changed" },
		func(f *domain.File) { f.Path = "renamed.go"; f.PreviousPath = "a.go" },
		func(f *domain.File) { f.Additions++ },
		func(f *domain.File) { f.PatchStatus = "truncated" },
	} {
		state := initial
		state.FileReview = &domain.FileReview{Revision: 8, ReviewedPaths: []string{"a.go", "a_test.go"}}
		files := append([]domain.File(nil), initial.Files...)
		mutate(&files[0])
		reconcileFileReview(&state, *initial.PullRequest, files)
		if state.FileReview.Revision != 9 || !reflect.DeepEqual(state.FileReview.ReviewedPaths, []string{"a_test.go"}) {
			t.Fatalf("changed file stayed reviewed: %+v", state.FileReview)
		}
	}
}

type reviewedProvider struct {
	provider.Provider
	pr    domain.PullRequest
	files []domain.File
}

func (p reviewedProvider) Load(context.Context, string) (domain.PullRequest, []domain.File, error) {
	return p.pr, p.files, nil
}

func TestFileReviewReloadPreservesUnchangedFilesAndDropsRemovedFiles(t *testing.T) {
	a, identity, initial := reviewedFixture(t)
	if _, err := a.SetReviewed(context.Background(), identity, []string{"a.go", "a_test.go", "image.png"}, true); err != nil {
		t.Fatal(err)
	}
	pr := *initial.PullRequest
	pr.HeadSHA = "next"
	files := []domain.File{initial.Files[0], initial.Files[2]}
	a.Provider = func(domain.Settings) provider.Provider { return reviewedProvider{pr: pr, files: files} }
	loaded, err := a.Load(context.Background(), "https://github.com/team/repo/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.FileReview.ReviewedPaths, []string{"a.go"}) {
		t.Fatalf("reload progress: %+v", loaded.FileReview)
	}
}
