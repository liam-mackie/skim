package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"skim/internal/domain"
)

func TestCommentLifecycleMigrationPersistsWithoutLosingCredentialsOrPublication(t *testing.T) {
	dir := t.TempDir()
	settings := domain.Settings{Providers: []domain.ModelProvider{{ID: "account", Name: "Account", Kind: "openai", BaseURL: "https://model.invalid/v1"}}}
	s, err := Open(dir, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			_ = s.Close()
		}
	}()
	d := s.Snapshot()
	if err := d.ApplySettings(settings, []domain.ProviderKeyUpdate{{ProviderID: "account", APIKey: "preserve-private-key"}}); err != nil {
		t.Fatal(err)
	}
	d.Active = "github/owner/repo/1"
	pr := &domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, BaseSHA: "base", HeadSHA: "head"}
	comments := []domain.Comment{
		{ID: "manual-draft", Source: "manual", Status: "draft", Body: "manual", Revision: 2, Stale: true, HeadSHA: "older"},
		{ID: "manual-approved", Source: "manual", Status: "approved", Body: "manual approved", Revision: 4},
		{ID: "review-draft", Source: "review", Status: "draft", Body: "review"},
		{ID: "verification-draft", Source: "verification", Status: "draft", Body: "verification", Revision: 1},
		{ID: "unknown-draft", Status: "draft", Body: "unknown"},
		{ID: "review-approved", Source: "review", Status: "approved", Body: "approved review", Revision: 6},
		{ID: "denied", Source: "review", Status: "denied", Body: "erase-denied-secret", Path: "private-path.go", StartLine: 2, EndLine: 4, Side: "RIGHT", ThreadID: "private-thread", CreatedAt: "private-time", HeadSHA: "private-head", Revision: 5, Stale: true},
		{ID: "published", Source: "review", Status: "published", Body: "published", Revision: 9},
		{ID: "pending", Source: "manual", Status: "pending", Body: "pending", Revision: 8},
		{ID: "suggested", Source: "review", Status: "suggested", Body: "suggested", Revision: 7},
	}
	pending := &Publication{Marker: "uncertain-marker", CommentIDs: []string{"review-approved"}, Event: "COMMENT", Body: "original publication body", HeadSHA: "head"}
	d.Reviews[d.Active] = Record{State: domain.State{PullRequest: pr, Comments: comments, Chats: []domain.ChatThread{{ID: "history", Revision: 4, Messages: []domain.Message{{Role: "user", Content: "retain conversation"}}}}}, Pending: pending}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	after := s.Snapshot()
	if after.Revision != before.Revision+1 {
		t.Fatal("migration did not persist a new durable revision")
	}
	if !reflect.DeepEqual(after.ProviderKeys, before.ProviderKeys) || !reflect.DeepEqual(after.Reviews[d.Active].Pending, pending) || !reflect.DeepEqual(after.Reviews[d.Active].State.Chats, before.Reviews[d.Active].State.Chats) {
		t.Fatal("migration lost keys, publication journal, or conversation")
	}
	if key, err := s.ResolveCredential(s.Settings().Providers[0]); err != nil || key != "preserve-private-key" {
		t.Fatalf("migrated key no longer resolves: %v", err)
	}
	wantStatuses := []string{"pending", "pending", "suggested", "suggested", "suggested", "pending", "deleted", "published", "pending", "suggested"}
	for i, c := range after.State().Comments {
		want := comments[i]
		want.Status = wantStatuses[i]
		if i < 7 {
			want.Revision++
		}
		if i == 6 {
			want = domain.Comment{ID: want.ID, Revision: want.Revision, Status: "deleted"}
		}
		if c != want {
			t.Fatalf("migration comment %d: got %+v want %+v", i, c, want)
		}
	}
	encoded, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"erase-denied-secret", "private-path.go", "private-thread", "private-time", "private-head"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("migration retained deleted content %q", secret)
		}
	}
	var disk Data
	if err := json.Unmarshal(encoded, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Revision != after.Revision || !reflect.DeepEqual(disk.Reviews[d.Active].State.Comments, after.Reviews[d.Active].State.Comments) {
		t.Fatal("migration was not durable")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if s.State().Revision != after.Revision || !reflect.DeepEqual(s.State().Comments, after.State().Comments) {
		t.Fatal("reopening repeated comment migration")
	}
}
