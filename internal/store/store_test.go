package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"skim/internal/domain"
)

func TestSnapshotsAndCommitDoNotAlias(t *testing.T) {
	s, err := Open(t.TempDir(), domain.Settings{Providers: []domain.ModelProvider{{ID: "provider", Name: "Provider"}}, ACP: domain.ACPSettings{Args: []string{"arg"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := s.Snapshot()
	state := domain.EmptyState()
	state.PullRequest = &domain.PullRequest{HeadSHA: "head"}
	state.Files = []domain.File{{Path: "file", Risk: &domain.Risk{Level: "low"}}}
	state.Groups = []domain.Group{{Paths: []string{"file"}}}
	state.Chats = []domain.ChatThread{{Messages: []domain.Message{{Content: "message"}}}}
	state.ReviewRun = &domain.ReviewRun{ReviewedPaths: []string{"file"}}
	state.Analysis = &domain.Analysis{Key: "key", Generation: "run", Revision: 1, Risk: domain.AnalysisTask{Status: "complete", SkippedFiles: []domain.SkippedFile{{Path: "unavailable"}}}}
	d.Active = "key"
	d.Reviews["key"] = Record{State: state, Pending: &Publication{CommentIDs: []string{"comment"}}}
	if err = s.Commit(d); err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's transaction after commit must not change store memory.
	d.Reviews["key"].State.Files[0].Risk.Level = "high"
	d.Reviews["key"].Pending.CommentIDs[0] = "changed"
	snapshot := s.Snapshot()
	snapshot.Reviews["key"].State.PullRequest.HeadSHA = "changed"
	snapshot.Reviews["key"].State.Groups[0].Paths[0] = "changed"
	snapshot.Reviews["key"].State.Chats[0].Messages[0].Content = "changed"
	snapshot.Reviews["key"].State.ReviewRun.ReviewedPaths[0] = "changed"
	snapshot.Reviews["key"].State.Analysis.Risk.SkippedFiles[0].Path = "changed"
	update := s.Analysis()
	update.Analysis.Generation = "changed"
	update.Analysis.Risk.SkippedFiles[0].Path = "changed"
	update.Groups[0].Paths[0] = "changed"
	update.Files[0].Risk.Level = "changed"
	active := s.State()
	active.Settings.Providers[0].ID = "changed"
	active.Settings.ACP.Args[0] = "changed"
	active.Files[0].Path = "changed"
	fresh := s.Snapshot()
	r := fresh.Reviews["key"]
	if r.State.PullRequest.HeadSHA != "head" || r.State.Files[0].Risk.Level != "low" || r.State.Groups[0].Paths[0] != "file" || r.State.Chats[0].Messages[0].Content != "message" || r.State.ReviewRun.ReviewedPaths[0] != "file" || r.Pending.CommentIDs[0] != "comment" || fresh.Settings.Providers[0].ID != "provider" || fresh.Settings.ACP.Args[0] != "arg" {
		t.Fatal("snapshot/commit shared mutable storage")
	}
	if r.State.Analysis.Generation != "run" || r.State.Analysis.Risk.SkippedFiles[0].Path != "unavailable" {
		t.Fatal("analysis polling/snapshot shared mutable storage")
	}
}
func TestExclusiveLockAndFailedCommit(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if other, err := Open(dir, domain.Settings{}); err == nil {
		other.Close()
		t.Fatal("second writer acquired same data directory")
	}
	d := s.Snapshot()
	d.Style = "saved"
	if err = s.Commit(d); err != nil {
		t.Fatal(err)
	}
	// A directory at the state destination makes the atomic rename fail.
	if err = os.Remove(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(dir, "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	d.Style = "not saved"
	if err = s.Commit(d); err == nil {
		t.Fatal("failed atomic replacement was hidden")
	}
	if s.State().Style != "saved" {
		t.Fatal("failed commit mutated memory")
	}
}

func TestLegacyGroupsAndInterruptedAnalysisMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":1,"active":"legacy","settings":{},"reviews":{"legacy":{"state":{"pullRequest":{"headSha":"head"},"files":[],"groups":[{"id":"old","title":"old","summary":"obsolete ordering prose","paths":["a.go"]}],"comments":[{"id":"kept","body":"Retain this draft."}],"chats":[]}},"interrupted":{"state":{"files":[],"groups":[],"comments":[],"chats":[],"analysis":{"generation":"saved","revision":7,"risk":{"status":"running","completed":1,"total":2},"grouping":{"status":"complete","completed":2,"total":2}}}}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.State().Groups) != 0 || len(s.State().Comments) != 1 || s.State().Comments[0].Body != "Retain this draft." {
		t.Fatal("legacy grouping migration lost review work or retained obsolete groups")
	}
	a := s.Snapshot().Reviews["interrupted"].State.Analysis
	if a.Risk.Status != "cancelled" || a.Risk.Completed != 1 || a.Grouping.Status != "complete" {
		t.Fatal("interrupted saved run did not preserve successful independent work")
	}
}

func TestBackgroundPersistenceFailureIsVisible(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := s.Snapshot()
	d.Active = "key"
	state := domain.EmptyState()
	state.PullRequest = &domain.PullRequest{BaseSHA: "base", HeadSHA: "head"}
	state.Files = []domain.File{{Path: "a.go"}}
	state.Analysis = &domain.Analysis{Key: "key", BaseSHA: "base", HeadSHA: "head", Generation: "run", Revision: 1, Risk: domain.AnalysisTask{Status: "running", Total: 1}, Grouping: domain.AnalysisTask{Status: "running", Total: 1}}
	d.Reviews["key"] = Record{State: state, AnalysisSnapshot: "snapshot"}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	err = s.UpdateAnalysis(context.Background(), *state.Analysis, "snapshot", func(r *Record) {
		r.State.Files[0].Risk = &domain.Risk{Level: "low"}
		r.State.Analysis.Risk.Completed = 1
	})
	if err == nil {
		t.Fatal("background disk failure was ignored")
	}
	update := s.Analysis()
	if update.Analysis.Risk.Status != "failed" || update.Analysis.Grouping.Status != "failed" || update.Analysis.Risk.Error == "" || update.Files[0].Risk != nil {
		t.Fatal("failed persistence left polling running or pretended the result was saved")
	}
}

func TestLegacyProviderNamesNormalizeWithoutReplacingSavedSettings(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":1,"settings":{"providers":[{"id":"account-a","kind":"openai","baseUrl":"https://model.invalid/v1","apiKeyEnv":""},{"id":"account-b","name":"Existing name","kind":"openai","baseUrl":"https://model.invalid/v1","apiKeyEnv":""}],"roles":{"chat":{"provider":"account-a","model":"chat"},"review":{"provider":"account-b","model":"review"},"jev":{"provider":"","model":""}}},"reviews":{},"providerKeys":{"account-a":{"key":"legacy-a-key","baseUrl":"https://model.invalid/v1","kind":"openai","revision":"revision-a"},"account-b":{"key":"legacy-b-key","baseUrl":"https://model.invalid/v1","kind":"openai","revision":"revision-b"}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	defaults := domain.Settings{
		Providers: []domain.ModelProvider{{ID: "default", Name: "Default", Kind: "typesafe", BaseURL: "https://default.invalid/v1"}},
		Roles:     domain.Roles{Jev: domain.ModelRole{Provider: "default", Model: "default-risk"}},
	}
	s, err := Open(dir, defaults)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings := s.Settings()
	wantRoles := domain.Roles{
		Chat:   domain.ModelRole{Provider: "account-a", Model: "chat"},
		Review: domain.ModelRole{Provider: "account-b", Model: "review"},
	}
	if len(settings.Providers) != 2 || settings.Providers[0].Name != "account-a" || settings.Providers[1].Name != "Existing name" || settings.Roles != wantRoles {
		t.Fatalf("legacy migration replaced saved providers, names, or roles: %+v", settings)
	}
	for i, p := range settings.Providers {
		suffix := []string{"a", "b"}[i]
		if p.CredentialStatus != "saved" || p.CredentialRevision != "revision-"+suffix {
			t.Fatalf("migration changed saved credential identity: %+v", p)
		}
		if key, err := s.ResolveCredential(p); err != nil || key != "legacy-"+suffix+"-key" {
			t.Fatalf("migration lost credential for %s: %v", p.ID, err)
		}
	}
	// The next normal write persists normalized names; another open is idempotent.
	saveCredentialSettings(t, s, settings)
	before := s.Snapshot()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, defaults)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(reopened.Settings(), settings) || !reflect.DeepEqual(reopened.Snapshot().ProviderKeys, before.ProviderKeys) {
		t.Fatal("normalized legacy settings or keys changed on second open")
	}
}

func TestSavedEmptySettingsDoNotAcquireCallerDefaultsOnRestart(t *testing.T) {
	defaults := domain.Settings{
		Providers: []domain.ModelProvider{{ID: "default", Name: "Default", Kind: "openai", BaseURL: "https://default.invalid/v1"}},
		Roles:     domain.Roles{Chat: domain.ModelRole{Provider: "default", Model: "chat"}},
	}
	for _, providers := range [][]domain.ModelProvider{nil, {}} {
		t.Run(map[bool]string{true: "nil", false: "empty"}[providers == nil], func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, defaults)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			saveCredentialSettings(t, s, domain.Settings{Providers: providers})
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir, defaults)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got := reopened.Settings()
			if got.Providers == nil || len(got.Providers) != 0 || got.Roles != (domain.Roles{}) || len(reopened.Snapshot().ProviderKeys) != 0 {
				t.Fatalf("restart seeded explicitly empty settings: %+v", got)
			}
		})
	}
}
