package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceGitErrorsAreTruthfulAndRedacted(t *testing.T) {
	t.Run("missing git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := workspaceGit(context.Background(), t.TempDir(), 128, "rev-parse", "HEAD")
		if err == nil || !strings.Contains(err.Error(), "git is not installed") {
			t.Fatalf("incorrect missing executable error: %v", err)
		}
	})
	t.Run("not repository", func(t *testing.T) {
		_, err := workspaceGit(context.Background(), t.TempDir(), 128, "rev-parse", "HEAD")
		if err == nil || !strings.Contains(err.Error(), "not a Git repository") || strings.Contains(err.Error(), "install git") {
			t.Fatalf("incorrect repository error: %v", err)
		}
	})
	t.Run("unborn head", func(t *testing.T) {
		directory := t.TempDir()
		gitTest(t, directory, "init", "--quiet")
		_, err := workspaceGit(context.Background(), directory, 128, "rev-parse", "--verify", "HEAD")
		if err == nil || !strings.Contains(err.Error(), "git rev-parse failed") || strings.Contains(err.Error(), "install git") {
			t.Fatalf("incorrect Git failure: %v", err)
		}
	})
	t.Run("credential bearing config", func(t *testing.T) {
		directory, _ := checkout(t)
		secret := "credential-that-must-not-appear"
		if err := os.WriteFile(filepath.Join(directory, ".git", "config"), []byte("["+strings.Repeat(secret, 1000)), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := workspaceGit(context.Background(), directory, 128, "rev-parse", "HEAD")
		if err == nil || !strings.Contains(err.Error(), "git rev-parse failed") || strings.Contains(err.Error(), secret) || len(err.Error()) > 256 {
			t.Fatalf("unsafe or misleading Git diagnostic: %v", err)
		}
	})
}

func TestWorkspaceIgnoresGitOverridesAndGlobalPrograms(t *testing.T) {
	directory, _ := checkout(t)
	if err := os.WriteFile(filepath.Join(directory, ".gitattributes"), []byte("tracked.txt filter=unsafe\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, directory, "add", ".gitattributes")
	gitTest(t, directory, "-c", "user.name=ACP Test", "-c", "user.email=acp@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "attributes")
	sha := strings.TrimSpace(gitTest(t, directory, "rev-parse", "HEAD"))
	home := t.TempDir()
	marker := filepath.Join(home, "executed")
	program := filepath.Join(home, "unsafe-filter")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf unsafe > '"+marker+"'\ncat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := "[filter \"unsafe\"]\n\tclean = " + program + "\n[core]\n\tfsmonitor = " + program + "\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_INDEX_FILE", filepath.Join(home, "wrong-index"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.bare=true'")
	if err := verifyWorkspace(context.Background(), directory, sha); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "tracked.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyWorkspace(context.Background(), directory, sha); err == nil || !strings.Contains(err.Error(), "tracked or untracked") {
		t.Fatalf("dirty checkout was not detected: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("workspace verification executed a global Git program")
	}
}
