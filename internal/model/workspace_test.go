package model

import (
	"path/filepath"
	"testing"

	"skim/internal/domain"
)

func TestManagedRootEnvironmentReplacesManualWorkspace(t *testing.T) {
	t.Setenv("SKIM_ACP_WORKSPACE", filepath.Join(t.TempDir(), "legacy-user-checkout"))
	t.Setenv("SKIM_WORKTREE_ROOT", "")
	if got := Defaults().ACP.ManagedRoot; got != "" {
		t.Fatalf("legacy checkout was adopted as a managed root: %q", got)
	}
	root := filepath.Join(t.TempDir(), "managed worktrees")
	t.Setenv("SKIM_WORKTREE_ROOT", root)
	if got := Defaults().ACP.ManagedRoot; got != root {
		t.Fatalf("explicit managed root default = %q; want %q", got, root)
	}
}

func TestManagedRootRequiresAbsolutePath(t *testing.T) {
	for _, root := range []string{"relative/worktrees", "bad\x00root"} {
		if err := Validate(domain.Settings{ACP: domain.ACPSettings{ManagedRoot: root}}); err == nil {
			t.Fatalf("unsafe managed root accepted: %q", root)
		}
	}
	for _, root := range []string{"", filepath.Join(t.TempDir(), "Application Support", "worktrees")} {
		if err := Validate(domain.Settings{ACP: domain.ACPSettings{ManagedRoot: root}}); err != nil {
			t.Fatalf("valid managed root rejected: %v", err)
		}
	}
}
