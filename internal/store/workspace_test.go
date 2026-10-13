package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"skim/internal/domain"
)

func TestLegacyWorkspaceMigrationLeavesUserCheckoutUntouched(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "user checkout")
	if err := os.MkdirAll(filepath.Join(legacy, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(legacy, "unsaved.txt")
	if err := os.WriteFile(sentinel, []byte("user changes"), 0644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "Application Support", "skim")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"version": 1, "settings": map[string]any{"acp": map[string]any{"command": "my-agent", "args": []string{"--model", "custom"}, "workingDirectory": legacy}}, "reviews": map[string]any{}}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings := s.Settings()
	if settings.ACP.ManagedRoot != filepath.Join(dir, "workspaces") || settings.ACP.Command != "my-agent" || len(settings.ACP.Args) != 2 {
		t.Fatalf("bad migration: %+v", settings.ACP)
	}
	if _, err := os.Lstat(settings.ACP.ManagedRoot); !os.IsNotExist(err) {
		t.Fatal("opening settings eagerly created a managed root")
	}
	if bytes, err := os.ReadFile(sentinel); err != nil || string(bytes) != "user changes" {
		t.Fatal("legacy checkout was mutated")
	}
	if err := s.Commit(s.Snapshot()); err != nil {
		t.Fatal(err)
	}
	encoded, err = os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "workingDirectory") || strings.Contains(string(encoded), legacy) {
		t.Fatal("legacy checkout setting survived migration write")
	}
}

func TestManagedRootDefaultsAndExplicitSettingsPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	settings := s.Settings()
	root := filepath.Join(t.TempDir(), "review worktrees")
	settings.ACP.ManagedRoot = root
	if committed, err := s.UpdateSettings(domain.SettingsUpdate{Settings: settings}, func(*Record, domain.Settings) {}); err != nil || !committed {
		t.Fatalf("save root: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Settings().ACP.ManagedRoot != root {
		t.Fatal("explicit root not persisted")
	}
	settings = s.Settings()
	settings.ACP.ManagedRoot = ""
	if _, err := s.UpdateSettings(domain.SettingsUpdate{Settings: settings}, func(*Record, domain.Settings) {}); err != nil {
		t.Fatal(err)
	}
	if s.Settings().ACP.ManagedRoot != filepath.Join(dir, "workspaces") {
		t.Fatal("clearing root did not restore private default")
	}
}
