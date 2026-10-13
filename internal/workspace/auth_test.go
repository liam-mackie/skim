package workspace

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRealGitAskpassQuotingIsolationAndPrivateCredentialLifetime(t *testing.T) {
	f := newGitFixture(t)
	token := "temporary-test-token-'\" $;&()with-metacharacters"
	bin := filepath.Join(t.TempDir(), "instrumented bin")
	mustMkdir(t, bin)
	argvLog := filepath.Join(bin, "argv.log")
	unsafeEnvironment := filepath.Join(bin, "unsafe-environment")
	// The wrapper changes no Git behavior: it records argv, checks that inherited
	// overrides disappeared, and supplies credential fill's nonsecret stdin.
	// Real Git selects and executes the actual ephemeral askpass helper.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + shellQuote(argvLog) + "\n" +
		"if [ -n \"${GIT_DIR-}${GIT_WORK_TREE-}${GIT_CONFIG_COUNT-}${GIT_CONFIG_PARAMETERS-}${GIT_TRACE-}${SSH_ASKPASS-}\" ]; then : > " + shellQuote(unsafeEnvironment) + "; fi\n" +
		"case \" $* \" in\n*' credential fill '*) printf 'protocol=https\\nhost=github.com\\npath=owner/repo.git\\n\\n' | " + shellQuote(f.gitPath) + " \"$@\" ;;\n*) exec " + shellQuote(f.gitPath) + " \"$@\" ;;\nesac\n"
	mustWrite(t, filepath.Join(bin, "git"), script)
	if err := os.Chmod(filepath.Join(bin, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_DIR", filepath.Join(f.producer, ".git"))
	t.Setenv("GIT_WORK_TREE", f.producer)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "!exit 97")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'credential.helper=unsafe-helper'")
	t.Setenv("GIT_TRACE", "1")
	t.Setenv("SSH_ASKPASS", "unsafe-helper")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := f.manager.git(ctx, f.manager.home, token, "credential", "credential", "fill")
	if err != nil {
		t.Fatalf("real Git credential fill failed: %v", err)
	}
	if !strings.Contains(out, "username=x-access-token\n") || !strings.Contains(out, "password="+token+"\n") {
		t.Fatal("real Git did not receive the configured credential through askpass")
	}
	missing(t, unsafeEnvironment)
	if strings.Contains(mustRead(t, argvLog), token) {
		t.Fatal("Git token appeared in command arguments")
	}
	lease, err := f.manager.Prepare(ctx, f.root, f.pr, f.source, token, "")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if strings.Contains(mustRead(t, argvLog), token) {
		t.Fatal("fetch token appeared in command arguments")
	}
	assertNoStoredToken(t, f.manager.private, token)
	assertNoStoredToken(t, f.root, token)
	children, err := os.ReadDir(f.manager.private)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if strings.HasPrefix(child.Name(), "askpass-") {
			t.Fatal("ephemeral credential helper survived completion")
		}
	}
	if got := f.run(f.producer, "rev-parse", "HEAD"); got != f.pr.BaseSHA {
		t.Fatal("inherited GIT_DIR redirected operations into a user checkout")
	}
}

func assertNoStoredToken(t *testing.T, root, token string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), token) || strings.Contains(string(data), base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))) {
			t.Fatal("private credential was persisted to a workspace artifact")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRealGitAuthenticationFailureUsesConfiguredTokenAndRedactedError(t *testing.T) {
	f := newGitFixture(t)
	token := "throwaway-local-auth-token-123"
	var sawConfiguredCredential atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if ok && username == "x-access-token" && password == token {
			sawConfiguredCredential.Store(true)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="temporary fixture"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Only this package-local runner test opts into HTTP, and only for loopback.
	// Manager.Prepare always rejects this URL regardless of its test-remote seam.
	_, err := f.manager.git(ctx, f.manager.home, token, "fetch", "-c", "protocol.http.allow=always", "ls-remote", server.URL+"/repo.git")
	var gitErr *GitError
	if !errors.As(err, &gitErr) || gitErr.Kind != "authentication_failed" {
		t.Fatalf("wrong authentication classification: %v", err)
	}
	if !sawConfiguredCredential.Load() {
		t.Fatal("real Git never invoked the configured askpass credential")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("authentication diagnostic leaked token")
	}
	assertNoStoredToken(t, f.manager.private, token)
}

func TestGitDiagnosticBoundsAndNotRepositoryClassification(t *testing.T) {
	f := newGitFixture(t)
	_, err := f.manager.git(context.Background(), f.home, "", "inspect", "rev-parse", "--git-dir")
	var gitErr *GitError
	if !errors.As(err, &gitErr) || gitErr.Kind != "not_repository" {
		t.Fatalf("wrong repository classification: %v", err)
	}
	_, err = f.manager.git(context.Background(), f.home, "", "inspect", "unknown-"+strings.Repeat("x", 20000))
	if err == nil || len(err.Error()) > 9000 {
		t.Fatal("Git diagnostics were not bounded")
	}
	for _, token := range []string{"abc-secret", "credential+$&with spaces"} {
		input := "fatal: " + token + " " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
		if strings.Contains(redactDiagnostic(input, token), token) {
			t.Fatal("token was not redacted")
		}
	}
}

func TestGlobalHooksTemplatesAndFiltersAreNeverExecuted(t *testing.T) {
	f := newGitFixture(t)
	unsafe := filepath.Join(t.TempDir(), "untrusted Git setup")
	mustMkdir(t, unsafe)
	sentinel := filepath.Join(unsafe, "executed")
	hooks := filepath.Join(unsafe, "hooks")
	mustMkdir(t, hooks)
	hook := "#!/bin/sh\n: > " + shellQuote(sentinel) + "\n"
	mustWrite(t, filepath.Join(hooks, "post-checkout"), hook)
	if err := os.Chmod(filepath.Join(hooks, "post-checkout"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(unsafe, "global.gitconfig")
	// Quoted config values are produced by real Git rather than by string syntax.
	f.run(f.home, "config", "--file", config, "core.hooksPath", hooks)
	f.run(f.home, "config", "--file", config, "filter.unsafe.smudge", "touch "+shellQuote(sentinel)+"; cat")
	f.run(f.home, "config", "--file", config, "filter.unsafe.required", "true")
	mustWrite(t, filepath.Join(f.fork, ".gitattributes"), "file.txt filter=unsafe\n")
	f.run(f.fork, "add", ".gitattributes")
	f.run(f.fork, "commit", "-m", "attribute referencing untrusted global filter")
	f.run(f.fork, "push", f.peer, "HEAD:refs/pull/1/head")
	f.pr.HeadSHA = f.run(f.fork, "rev-parse", "HEAD")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_SYSTEM", config)
	t.Setenv("GIT_TEMPLATE_DIR", unsafe)
	lease := f.prepare()
	lease.Release()
	missing(t, sentinel)
	if mustRead(t, filepath.Join(lease.Path, "file.txt")) != "fork head\n" {
		t.Fatal("unconfigured filter altered checkout content")
	}
	if got := f.finish(false); got.Status != "finished" {
		t.Fatalf("finish: %+v", got)
	}
}

func TestIsolatedGitHomeRejectsUnexpectedCredentialFiles(t *testing.T) {
	f := newGitFixture(t)
	netrc := filepath.Join(f.manager.home, ".netrc")
	mustWrite(t, netrc, "machine github.com login unwanted password local-fixture-only\n")
	_, err := f.manager.Prepare(context.Background(), f.root, f.pr, f.source, "configured-test-token", "")
	if err == nil || !strings.Contains(err.Error(), "isolated Git home") {
		t.Fatalf("unexpected home credential file was accepted: %v", err)
	}
	if mustRead(t, netrc) != "machine github.com login unwanted password local-fixture-only\n" {
		t.Fatal("unexpected credential file was modified rather than refused")
	}
}

func TestRawGitStatusCannotStartGlobalFilesystemMonitor(t *testing.T) {
	f := newGitFixture(t)
	lease := f.prepare()
	path := lease.Path
	lease.Release()
	global := filepath.Join(f.home, "global.gitconfig")
	f.run(f.home, "config", "--file", global, "core.fsmonitor", "true")
	f.run(f.home, "config", "--file", global, "core.untrackedCache", "true")
	// This invocation intentionally has no command-line -c overrides, matching
	// an agent's ordinary Git usage. Repository-local defaults must win.
	cmd := exec.Command(f.gitPath, "status", "--porcelain")
	cmd.Dir = path
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + global}
	t.Cleanup(func() {
		// Only this exact test-owned checkout may have started a daemon.
		stop := exec.Command(f.gitPath, "fsmonitor--daemon", "stop")
		stop.Dir, stop.Env = path, cmd.Env
		_ = stop.Run()
	})
	if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("raw status failed: %v %s", err, output)
	}
	if err := filepath.WalkDir(f.root, func(path string, item fs.DirEntry, err error) error {
		if err == nil && item.Type()&os.ModeSocket != 0 {
			t.Error("raw Git started a filesystem monitor inside the managed cache")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reused := f.prepare()
	if reused.Path != path {
		t.Fatal("raw Git status prevented checkout reuse")
	}
	reused.Release()
	if result := f.finish(false); result.Status != "finished" {
		t.Fatalf("raw Git status prevented cleanup: %+v", result)
	}
}
