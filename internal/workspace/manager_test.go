package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skim/internal/domain"
)

type gitFixture struct {
	t        *testing.T
	gitPath  string
	home     string
	peer     string
	producer string
	fork     string
	private  string
	root     string
	manager  *Manager
	pr       domain.PullRequest
	source   domain.GitSource
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("real Git is required for workspace integration tests")
	}
	parent, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &gitFixture{t: t, gitPath: gitPath, home: filepath.Join(parent, "isolated home"), peer: filepath.Join(parent, "remote peer.git"), producer: filepath.Join(parent, "base checkout"), fork: filepath.Join(parent, "fork checkout"), private: filepath.Join(parent, "Application Support", "Skim ' ; private"), root: filepath.Join(parent, "managed root ' ; spaces")}
	mustMkdir(t, f.home)
	f.run(parent, "init", "--bare", "--template=", f.peer)
	f.run(parent, "init", "--template=", f.producer)
	f.identity(f.producer)
	mustWrite(t, filepath.Join(f.producer, "file.txt"), "base\n")
	mustWrite(t, filepath.Join(f.producer, ".gitignore"), "build/\n")
	f.run(f.producer, "add", ".")
	f.run(f.producer, "commit", "-m", "base")
	base := f.run(f.producer, "rev-parse", "HEAD")
	f.run(f.producer, "push", f.peer, "HEAD:refs/heads/main")
	f.run(parent, "clone", "--no-hardlinks", "--template=", f.producer, f.fork)
	f.identity(f.fork)
	mustWrite(t, filepath.Join(f.fork, "file.txt"), "fork head\n")
	f.run(f.fork, "commit", "-am", "fork-only head")
	head := f.run(f.fork, "rev-parse", "HEAD")
	f.run(f.fork, "push", f.peer, "HEAD:refs/pull/1/head", "HEAD:refs/pull/2/head")
	f.pr = domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, BaseSHA: base, HeadSHA: head, BaseRef: "main", HeadRef: "fork/feature"}
	f.source = domain.GitSource{URL: "https://github.com/owner/repo.git", HeadRef: "refs/pull/1/head"}
	f.manager = New(f.private)
	if f.manager.err != nil {
		t.Fatal(f.manager.err)
	}
	if err := f.manager.SetLocalRemoteForTesting(f.source.URL, f.peer); err != nil {
		t.Fatal(err)
	}
	return f
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
}
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func missing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected path to be absent: %s (%v)", path, err)
	}
}

func (f *gitFixture) command(directory string, args ...string) *exec.Cmd {
	options := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "protocol.file.allow=always", "-c", "init.defaultBranch=main", "-c", "commit.gpgSign=false"}
	cmd := exec.Command(f.gitPath, append(options, args...)...)
	cmd.Dir = directory
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home, "XDG_CONFIG_HOME=" + f.home, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	return cmd
}

func (f *gitFixture) run(directory string, args ...string) string {
	f.t.Helper()
	out, err := f.command(directory, args...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("temporary fixture Git %s failed: %v: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *gitFixture) identity(path string) {
	f.run(path, "config", "user.name", "Workspace Test")
	f.run(path, "config", "user.email", "workspace@example.invalid")
}

func (f *gitFixture) prepare() *Lease {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lease, err := f.manager.Prepare(ctx, f.root, f.pr, f.source, "", "")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(lease.Release)
	return lease
}

func (f *gitFixture) finish(discard bool) domain.Workspace {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id := ""
	if discard {
		id = f.manager.Status(f.pr.Key(), f.root).ID
	}
	return f.manager.Finish(ctx, f.pr.Key(), discard, id)
}

func (f *gitFixture) advance() string {
	f.t.Helper()
	mustWrite(f.t, filepath.Join(f.fork, "file.txt"), "advanced fork head\n")
	f.run(f.fork, "commit", "-am", "advanced fork head")
	f.run(f.fork, "push", f.peer, "HEAD:refs/pull/1/head")
	return f.run(f.fork, "rev-parse", "HEAD")
}

func TestLazyExactForkHeadTwoPullRequestsAndReuse(t *testing.T) {
	f := newGitFixture(t)
	w := f.manager.Status(f.pr.Key(), f.root)
	if w.Status != "not_created" || w.Root != f.root || w.Path != "" {
		t.Fatalf("unexpected initial status: %+v", w)
	}
	missing(t, f.root)
	lease := f.prepare()
	path := lease.Path
	if !strings.Contains(path, "managed root ' ; spaces") {
		t.Fatal("fixture did not exercise root quoting")
	}
	if got := f.run(path, "rev-parse", "HEAD"); got != f.pr.HeadSHA {
		t.Fatal("checkout is not the inspected fork-only commit")
	}
	if err := f.command(path, "symbolic-ref", "-q", "HEAD").Run(); err == nil {
		t.Fatal("managed checkout is not detached")
	}
	if f.run(path, "cat-file", "-t", f.pr.BaseSHA) != "commit" {
		t.Fatal("inspected base was not fetched")
	}
	if got := f.manager.Status(f.pr.Key(), f.root); got.Status != "in_use" || got.ID == "" {
		t.Fatalf("missing active identity: %+v", got)
	}
	lease.Release()
	lease.Release()
	if got := f.manager.Status(f.pr.Key(), f.root).Status; got != "ready" {
		t.Fatalf("released status = %s", got)
	}
	reused := f.prepare()
	if reused.Path != path {
		t.Fatal("clean matching checkout was not reused")
	}
	reused.Release()
	secondPR := f.pr
	secondPR.Number = 2
	secondSource := f.source
	secondSource.HeadRef = "refs/pull/2/head"
	second, err := f.manager.Prepare(context.Background(), f.root, secondPR, secondSource, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if second.Path == path {
		t.Fatal("different pull requests share a worktree")
	}
	f.manager.mu.Lock()
	cacheCount := len(f.manager.data.Caches)
	f.manager.mu.Unlock()
	if cacheCount != 1 {
		t.Fatalf("expected one lazy repository cache, got %d", cacheCount)
	}
	if got := f.finish(false); got.Status != "finished" || got.FinishedAt == "" || got.Path != "" {
		t.Fatalf("clean finish: %+v", got)
	}
	missing(t, path)
	if mustRead(t, filepath.Join(second.Path, "file.txt")) != "fork head\n" {
		t.Fatal("finishing first PR changed second PR")
	}
	if f.run(f.producer, "status", "--porcelain") != "" || f.run(f.fork, "status", "--porcelain") != "" {
		t.Fatal("fixture source checkouts changed")
	}
}

func TestIgnoredBuildOutputsReuseButRequireDiscardForCleanup(t *testing.T) {
	f := newGitFixture(t)
	lease := f.prepare()
	path := lease.Path
	lease.Release()
	mustMkdir(t, filepath.Join(path, "build"))
	mustWrite(t, filepath.Join(path, "build", "output"), "keep generated output")
	reused := f.prepare()
	if reused.Path != path {
		t.Fatal("ignored build output prevented clean reuse")
	}
	reused.Release()
	w := f.finish(false)
	if w.Status != "cleanup_failed" || !w.CanDiscardChanges || w.ID == "" {
		t.Fatalf("ignored files were not conservatively retained: %+v", w)
	}
	if mustRead(t, filepath.Join(path, "build", "output")) != "keep generated output" {
		t.Fatal("ignored output changed")
	}
	if got := f.finish(true); got.Status != "finished" || got.CanDiscardChanges {
		t.Fatalf("explicit discard failed: %+v", got)
	}
	missing(t, path)
}

func TestTrackedAndUntrackedChangesAreNeverOverwritten(t *testing.T) {
	for _, filename := range []string{"file.txt", "notes.private"} {
		t.Run(filename, func(t *testing.T) {
			f := newGitFixture(t)
			lease := f.prepare()
			path := lease.Path
			lease.Release()
			mustWrite(t, filepath.Join(path, filename), "user-owned edits\n")
			if _, err := f.manager.Prepare(context.Background(), f.root, f.pr, f.source, "", ""); err == nil {
				t.Fatal("dirty checkout was reused")
			}
			w := f.finish(false)
			if w.Status != "cleanup_failed" || !w.CanDiscardChanges {
				t.Fatalf("dirty status: %+v", w)
			}
			if mustRead(t, filepath.Join(path, filename)) != "user-owned edits\n" {
				t.Fatal("local changes were overwritten")
			}
			if got := f.finish(true); got.Status != "finished" {
				t.Fatalf("explicit discard: %+v", got)
			}
			missing(t, path)
		})
	}
}

func TestHeadRaceAndRootChangesPreserveOldRegistry(t *testing.T) {
	f := newGitFixture(t)
	lease := f.prepare()
	oldPath := lease.Path
	lease.Release()
	newHead := f.advance()
	if _, err := f.manager.Prepare(context.Background(), f.root, f.pr, f.source, "", ""); err == nil || !strings.Contains(err.Error(), "head changed") {
		t.Fatalf("head race was not rejected: %v", err)
	}
	if f.run(oldPath, "rev-parse", "HEAD") != f.pr.HeadSHA {
		t.Fatal("racing head changed prior checkout")
	}
	f.pr.HeadSHA = newHead
	newLease := f.prepare()
	if newLease.Path == oldPath {
		t.Fatal("head change reused old worktree")
	}
	missing(t, oldPath)
	newLease.Release()
	mustWrite(t, filepath.Join(newLease.Path, "file.txt"), "keep dirty old-root file")
	oldRoot := f.root
	f.root = filepath.Join(filepath.Dir(f.root), "second managed root")
	if _, err := f.manager.Prepare(context.Background(), f.root, f.pr, f.source, "", ""); err == nil {
		t.Fatal("root change erased dirty old checkout")
	}
	if got := f.manager.Status(f.pr.Key(), f.root); got.Root != oldRoot || got.Path != newLease.Path || !got.CanDiscardChanges {
		t.Fatalf("old-root cleanup status lost: %+v", got)
	}
	if got := f.finish(true); got.Status != "finished" {
		t.Fatalf("old root discard: %+v", got)
	}
	last := f.prepare()
	last.Release()
	f.manager.mu.Lock()
	count := len(f.manager.data.Caches)
	f.manager.mu.Unlock()
	if count != 2 {
		t.Fatalf("old root cache disappeared from registry: %d", count)
	}
	if got := f.finish(false); got.Status != "finished" {
		t.Fatalf("new root finish: %+v", got)
	}
}

func TestDiscardConfirmationCannotDeleteReplacementWorkspace(t *testing.T) {
	f := newGitFixture(t)
	first := f.prepare()
	first.Release()
	oldID := f.manager.Status(f.pr.Key(), f.root).ID
	f.pr.HeadSHA = f.advance()
	replacement := f.prepare()
	replacement.Release()
	mustWrite(t, filepath.Join(replacement.Path, "notes"), "newer workspace data")
	w := f.manager.Finish(context.Background(), f.pr.Key(), true, oldID)
	if w.Status != "cleanup_failed" || w.CanDiscardChanges || !strings.Contains(w.Error, "changed") {
		t.Fatalf("stale discard accepted: %+v", w)
	}
	if mustRead(t, filepath.Join(replacement.Path, "notes")) != "newer workspace data" {
		t.Fatal("stale discard touched replacement")
	}
}

func TestLeaseBlocksFinishAcrossManagerInstancesAndStatusDoesNotBlock(t *testing.T) {
	f := newGitFixture(t)
	lease := f.prepare()
	defer lease.Release()
	restarted := New(f.private)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	w := restarted.Finish(ctx, f.pr.Key(), false, "")
	cancel()
	if w.Status != "cleanup_failed" || w.CanDiscardChanges {
		t.Fatalf("another manager removed a live lease: %+v", w)
	}
	if _, err := os.Stat(lease.Path); err != nil {
		t.Fatal("leased checkout disappeared")
	}
	done := make(chan domain.Workspace, 1)
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { done <- f.manager.Finish(ctx, f.pr.Key(), false, "") }()
	deadline := time.After(3 * time.Second)
	for f.manager.Status(f.pr.Key(), f.root).Status != "cleanup_pending" {
		select {
		case <-deadline:
			t.Fatal("Finish did not reach observable pending state")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-done:
		t.Fatal("Finish completed while lease active")
	default:
	}
	lease.Release()
	select {
	case w := <-done:
		if w.Status != "finished" {
			t.Fatalf("finish after release: %+v", w)
		}
	case <-ctx.Done():
		t.Fatal("Finish failed to resume after Release")
	}
}

func TestRestartRecoveryAndFinishWithoutWorkspace(t *testing.T) {
	f := newGitFixture(t)
	w := f.manager.Finish(context.Background(), "github/owner/repo/unused", false, "")
	if w.Status != "finished" || w.FinishedAt == "" {
		t.Fatalf("no-workspace finish: %+v", w)
	}
	lease := f.prepare()
	lease.Release()
	r := f.manager.get(f.pr.Key())
	r.Workspace.Status = "preparing"
	if err := f.manager.put(f.pr.Key(), r); err != nil {
		t.Fatal(err)
	}
	f.manager = New(f.private)
	if got := f.manager.Status(f.pr.Key(), f.root); got.Status != "cleanup_pending" || got.Path != lease.Path {
		t.Fatalf("lost interrupted state: %+v", got)
	}
	if got := f.finish(false); got.Status != "finished" {
		t.Fatalf("restart recovery: %+v", got)
	}
	f.manager = New(f.private)
	if got := f.manager.Status(f.pr.Key(), f.root); got.Status != "finished" || got.FinishedAt == "" {
		t.Fatalf("finish not durable: %+v", got)
	}
	if got := f.manager.Status("github/owner/repo/unused", f.root); got.Status != "finished" || got.Root != f.root {
		t.Fatalf("no-workspace finish not durable: %+v", got)
	}
}

func TestFetchFailureCancellationAndRetryUseOnlyTemporaryRemotes(t *testing.T) {
	f := newGitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.manager.Prepare(ctx, f.root, f.pr, f.source, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel not returned: %v", err)
	}
	missing(t, f.root)
	f.run(f.peer, "update-ref", "-d", f.source.HeadRef)
	if _, err := f.manager.Prepare(context.Background(), f.root, f.pr, f.source, "", ""); err == nil {
		t.Fatal("missing PR ref accepted")
	} else {
		var gitErr *GitError
		if !errors.As(err, &gitErr) || gitErr.Kind != "fetch_failed" {
			t.Fatalf("wrong fetch classification: %v", err)
		}
	}
	if w := f.manager.Status(f.pr.Key(), f.root); w.Status != "failed" || w.Path != "" || w.CanDiscardChanges {
		t.Fatalf("failed fetch allocated checkout: %+v", w)
	}
	f.run(f.fork, "push", f.peer, "HEAD:refs/pull/1/head")
	f.manager = New(f.private)
	if err := f.manager.SetLocalRemoteForTesting(f.source.URL, f.peer); err != nil {
		t.Fatal(err)
	}
	lease := f.prepare()
	lease.Release()
	if got := f.finish(false); got.Status != "finished" {
		t.Fatalf("retry finish: %+v", got)
	}
}

func TestMissingGitExecutableIsReportedWithoutClaimingCheckout(t *testing.T) {
	f := newGitFixture(t)
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	_, err := f.manager.Prepare(context.Background(), f.root, f.pr, f.source, "", "")
	var gitErr *GitError
	if !errors.As(err, &gitErr) || gitErr.Kind != "missing_executable" {
		t.Fatalf("missing executable classification: %v", err)
	}
	if w := f.manager.Status(f.pr.Key(), f.root); w.Path != "" || w.CanDiscardChanges {
		t.Fatalf("missing git claimed a checkout: %+v", w)
	}
}

func TestInvalidSourcesNeverReachGitOrCreateManagedRoot(t *testing.T) {
	f := newGitFixture(t)
	cases := []struct {
		pr     domain.PullRequest
		source domain.GitSource
	}{}
	for _, value := range []string{f.peer, "file://" + f.peer, "https://user:secret@github.com/owner/repo.git", "https://github.com/owner/repo.git?token=secret", "https://github.com/other/repo.git", "https://example.invalid/owner/repo.git"} {
		source := f.source
		source.URL = value
		cases = append(cases, struct {
			pr     domain.PullRequest
			source domain.GitSource
		}{f.pr, source})
	}
	for _, ref := range []string{"refs/heads/main", "refs/pull/2/head", "--upload-pack=evil", "refs/pull/1/head\nother"} {
		source := f.source
		source.HeadRef = ref
		cases = append(cases, struct {
			pr     domain.PullRequest
			source domain.GitSource
		}{f.pr, source})
	}
	for _, change := range []func(*domain.PullRequest){func(pr *domain.PullRequest) { pr.Provider = "other" }, func(pr *domain.PullRequest) { pr.Owner = "../escape" }, func(pr *domain.PullRequest) { pr.HeadSHA = "HEAD" }, func(pr *domain.PullRequest) { pr.BaseSHA = "--option" }} {
		pr := f.pr
		change(&pr)
		cases = append(cases, struct {
			pr     domain.PullRequest
			source domain.GitSource
		}{pr, f.source})
	}
	for i, item := range cases {
		if _, err := f.manager.Prepare(context.Background(), f.root, item.pr, item.source, "", ""); err == nil {
			t.Fatalf("invalid source %d accepted", i)
		}
	}
	missing(t, f.root)
}

func TestRootSymlinksCollisionsAndRealCheckoutsRemainUntouched(t *testing.T) {
	f := newGitFixture(t)
	mustMkdir(t, f.root)
	if err := os.Chmod(f.root, 0755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.root, "user.txt"), "untouched")
	mustMkdir(t, filepath.Join(f.root, ".skim-existing-user-directory"))
	mustWrite(t, filepath.Join(f.root, ".skim-existing-user-directory", "private"), "untouched")
	if err := os.Symlink(f.producer, filepath.Join(f.root, ".skim-user-symlink")); err != nil {
		t.Fatal(err)
	}
	rootAlias := filepath.Join(filepath.Dir(f.root), "root alias")
	if err := os.Symlink(f.root, rootAlias); err != nil {
		t.Fatal(err)
	}
	lease, err := f.manager.Prepare(context.Background(), rootAlias, f.pr, f.source, "", "")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if got := f.finish(false); got.Status != "finished" {
		t.Fatalf("finish: %+v", got)
	}
	if mustRead(t, filepath.Join(f.root, "user.txt")) != "untouched" || mustRead(t, filepath.Join(f.root, ".skim-existing-user-directory", "private")) != "untouched" {
		t.Fatal("user files changed")
	}
	info, err := os.Stat(f.root)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("manager chmodded configured root")
	}
	inside := filepath.Join(f.root, ".skim-user-symlink", "new managed child")
	if _, err := f.manager.Prepare(context.Background(), inside, f.pr, f.source, "", ""); err == nil {
		t.Fatal("root within real checkout accepted")
	}
	missing(t, filepath.Join(f.producer, "new managed child"))
}

func TestWrongOwnershipAndChangedIdentityNeverEnableDiscard(t *testing.T) {
	for _, corruption := range []string{"marker", "gitfile", "head", "slot-symlink", "cache-config", "objects-symlink"} {
		t.Run(corruption, func(t *testing.T) {
			f := newGitFixture(t)
			lease := f.prepare()
			lease.Release()
			r := f.manager.get(f.pr.Key())
			c := f.manager.data.Caches[r.Entry.CacheID]
			switch corruption {
			case "marker":
				mustWrite(t, filepath.Join(r.Entry.Slot, markerName), `{ "owner": "not ours" }`)
			case "gitfile":
				mustWrite(t, filepath.Join(lease.Path, ".git"), "gitdir: "+filepath.Join(f.producer, ".git")+"\n")
			case "head":
				f.run(lease.Path, "checkout", "--detach", f.pr.BaseSHA)
			case "slot-symlink":
				if err := os.Rename(r.Entry.Slot, r.Entry.Slot+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(r.Entry.Slot+"-moved", r.Entry.Slot); err != nil {
					t.Fatal(err)
				}
			case "cache-config":
				mustWrite(t, filepath.Join(c.repo(), "config"), bareConfig+"[include]\n\tpath = /not/owned/config\n")
			case "objects-symlink":
				objects := filepath.Join(c.repo(), "objects")
				if err := os.Rename(objects, objects+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.peer, "objects"), objects); err != nil {
					t.Fatal(err)
				}
			}
			for _, discard := range []bool{false, true} {
				w := f.finish(discard)
				if w.Status != "cleanup_failed" || w.CanDiscardChanges || !strings.Contains(w.Error, "ownership") {
					t.Fatalf("unsafe ownership enabled cleanup: %+v", w)
				}
			}
			if _, err := os.Lstat(lease.Path); err != nil {
				t.Fatal("unsafe checkout was removed")
			}
			if f.run(f.producer, "status", "--porcelain") != "" {
				t.Fatal("user checkout changed")
			}
		})
	}
}

func TestJournalCollisionAndCorruptionFailClosed(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "journal"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			private := filepath.Join(root, "private")
			mustMkdir(t, private)
			target := filepath.Join(private, "managed-workspaces")
			switch kind {
			case "directory":
				mustMkdir(t, target)
				mustWrite(t, filepath.Join(target, "user"), "user data")
			case "symlink":
				if err := os.Symlink(root, target); err != nil {
					t.Fatal(err)
				}
			case "journal":
				m := New(private)
				if m.err != nil {
					t.Fatal(m.err)
				}
				mustWrite(t, filepath.Join(target, "journal.json"), "corrupt journal")
			}
			m := New(private)
			if m.Status("pr", "").Status != "failed" {
				t.Fatal("unsafe private journal accepted")
			}
			w := m.Finish(context.Background(), "pr", true, "anything")
			if w.Status != "cleanup_failed" || w.CanDiscardChanges {
				t.Fatalf("unsafe journal cleanup: %+v", w)
			}
			if kind == "directory" && mustRead(t, filepath.Join(target, "user")) != "user data" {
				t.Fatal("journal collision overwrote user data")
			}
		})
	}
}

func (f *gitFixture) partialEntry(t *testing.T) (cache, entry) {
	t.Helper()
	lease := f.prepare()
	lease.Release()
	r := f.manager.get(f.pr.Key())
	c := f.manager.data.Caches[r.Entry.CacheID]
	if got := f.finish(false); got.Status != "finished" {
		t.Fatalf("fixture cleanup: %+v", got)
	}
	id, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(c.Namespace, "worktree-"+id)
	e := entry{ID: id, CacheID: c.ID, PRKey: f.pr.Key(), Slot: slot, Path: filepath.Join(slot, "tree"), Head: f.pr.HeadSHA, Base: f.pr.BaseSHA}
	r = record{PRKey: f.pr.Key(), Workspace: domain.Workspace{ID: id, Root: f.root, Path: e.Path, HeadSHA: e.Head, Status: "preparing"}, Entry: &e}
	if err := f.manager.put(f.pr.Key(), r); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, e.Slot)
	if err := exclusiveJSON(filepath.Join(e.Slot, markerName), f.manager.marker(c, &e)); err != nil {
		t.Fatal(err)
	}
	return c, e
}

func TestPartialPreparationAndInterruptedRemovalRecovery(t *testing.T) {
	for _, stage := range []string{"before-slot", "before-add", "registered-no-checkout", "after-add", "after-remove"} {
		t.Run(stage, func(t *testing.T) {
			f := newGitFixture(t)
			c, e := f.partialEntry(t)
			switch stage {
			case "before-slot":
				if err := os.Remove(filepath.Join(e.Slot, markerName)); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(e.Slot); err != nil {
					t.Fatal(err)
				}
			case "registered-no-checkout":
				f.run(c.repo(), "worktree", "add", "--detach", "--no-checkout", e.Path, e.Head)
			case "after-add", "after-remove":
				f.run(c.repo(), "worktree", "add", "--detach", e.Path, e.Head)
				if stage == "after-remove" {
					f.run(c.repo(), "worktree", "remove", e.Path)
				}
			}
			f.manager = New(f.private)
			if f.manager.err != nil {
				t.Fatal(f.manager.err)
			}
			w := f.finish(false)
			if stage == "registered-no-checkout" {
				if w.Status != "cleanup_failed" || !w.CanDiscardChanges {
					t.Fatalf("incomplete checkout should require consent: %+v", w)
				}
				w = f.finish(true)
			}
			if w.Status != "finished" {
				t.Fatalf("partial %s recovery: %+v", stage, w)
			}
			missing(t, e.Path)
		})
	}
}

func TestCleanupDoesNotPruneUnrelatedWorktreeEntries(t *testing.T) {
	f := newGitFixture(t)
	lease := f.prepare()
	lease.Release()
	r := f.manager.get(f.pr.Key())
	c := f.manager.data.Caches[r.Entry.CacheID]
	unrelated := filepath.Join(filepath.Dir(f.root), "unrelated temporary checkout")
	f.run(c.repo(), "worktree", "add", "--detach", unrelated, f.pr.BaseSHA)
	mustWrite(t, filepath.Join(unrelated, "user-notes"), "not managed")
	if got := f.finish(false); got.Status != "finished" {
		t.Fatalf("finish: %+v", got)
	}
	if mustRead(t, filepath.Join(unrelated, "user-notes")) != "not managed" {
		t.Fatal("unrelated worktree changed")
	}
	if !strings.Contains(f.run(c.repo(), "worktree", "list", "--porcelain"), unrelated) {
		t.Fatal("unrelated registration pruned")
	}
}

func TestCancellationAfterGitAddKeepsDurableOwnedEntry(t *testing.T) {
	f := newGitFixture(t)
	wrapper := filepath.Join(t.TempDir(), "wrapper bin")
	mustMkdir(t, wrapper)
	signal := filepath.Join(wrapper, "added")
	// Instrumentation delegates the actual worktree add to real Git, then models
	// cancellation before its caller receives successful completion.
	script := "#!/bin/sh\ncase \" $* \" in\n*' worktree add '*)\n  " + shellQuote(f.gitPath) + " \"$@\" || exit $?\n  : > " + shellQuote(signal) + "\n  sleep 30\n  ;;\n*) exec " + shellQuote(f.gitPath) + " \"$@\" ;;\nesac\n"
	mustWrite(t, filepath.Join(wrapper, "git"), script)
	if err := os.Chmod(filepath.Join(wrapper, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.manager.Prepare(ctx, f.root, f.pr, f.source, "", ""); done <- err }()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(signal); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("prepare ended before add signal: %v", err)
		case <-deadline:
			t.Fatal("real worktree add did not complete")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	w := f.manager.Status(f.pr.Key(), f.root)
	if w.Path == "" || w.Status != "cleanup_failed" {
		t.Fatalf("cancel lost journal intent: %+v", w)
	}
	f.manager = New(f.private)
	if got := f.finish(false); got.Status != "finished" {
		t.Fatalf("cancel recovery: %+v", got)
	}
	missing(t, w.Path)
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func TestPreparedWorkspaceIDIsUnique(t *testing.T) {
	f := newGitFixture(t)
	seen := map[string]bool{}
	for range 2 {
		lease := f.prepare()
		lease.Release()
		w := f.manager.Status(f.pr.Key(), f.root)
		if !validID(w.ID) || seen[w.ID] {
			t.Fatal("workspace identity is missing or reused")
		}
		seen[w.ID] = true
		if got := f.finish(false); got.Status != "finished" {
			t.Fatalf("finish: %+v", got)
		}
	}
}

func TestReplacedPrivateDirectoryNeverReceivesOperationLock(t *testing.T) {
	private := filepath.Join(t.TempDir(), "private")
	m := New(private)
	if m.err != nil {
		t.Fatal(m.err)
	}
	target := t.TempDir()
	if err := os.Rename(m.private, m.private+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, m.private); err != nil {
		t.Fatal(err)
	}
	w := m.Finish(context.Background(), "github/owner/repo/1", false, "")
	if w.Status != "cleanup_failed" || w.CanDiscardChanges {
		t.Fatalf("redirected private directory was accepted: %+v", w)
	}
	missing(t, filepath.Join(target, "journal.lock"))
	missing(t, filepath.Join(target, "journal.json"))
}
