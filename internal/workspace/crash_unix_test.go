//go:build unix

package workspace

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"skim/internal/domain"
	"skim/internal/processgroup"
)

// The controller is a test-binary subprocess that owns a managed worktree and
// starts a real agent stand-in in its own process group. The parent test hard-kills
// only the controller, like a crash of the Skim server.
func TestWorkspaceCrashController(t *testing.T) {
	if os.Getenv("SKIM_WORKSPACE_CONTROLLER") != "1" {
		return
	}
	var pr domain.PullRequest
	var source domain.GitSource
	if json.Unmarshal([]byte(os.Getenv("SKIM_CTRL_PR")), &pr) != nil || json.Unmarshal([]byte(os.Getenv("SKIM_CTRL_SOURCE")), &source) != nil {
		os.Exit(90)
	}
	m := New(os.Getenv("SKIM_CTRL_PRIVATE"))
	if m.err != nil || m.SetLocalRemoteForTesting(source.URL, os.Getenv("SKIM_CTRL_PEER")) != nil {
		os.Exit(91)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lease, err := m.Prepare(ctx, os.Getenv("SKIM_CTRL_ROOT"), pr, source, "", os.Getenv("SKIM_CTRL_TASK"))
	if err != nil {
		os.Exit(92)
	}
	marker := os.Getenv("SKIM_CTRL_MARKER")
	if os.Getenv("SKIM_CTRL_RECORD") == "1" {
		agent := exec.Command("/bin/sleep", "300")
		agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if agent.Start() != nil || lease.RecordProcess(ctx, agent.Process.Pid) != nil {
			os.Exit(93)
		}
		if os.WriteFile(marker+".group", []byte(strconv.Itoa(agent.Process.Pid)), 0600) != nil {
			os.Exit(94)
		}
	}
	if os.WriteFile(marker+".ready", []byte(lease.Path), 0600) != nil {
		os.Exit(95)
	}
	time.Sleep(time.Hour)
}

func (f *gitFixture) crashController(t *testing.T, record bool, taskID string) (path string, group int) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := json.Marshal(f.pr)
	source, _ := json.Marshal(f.source)
	marker := filepath.Join(t.TempDir(), "controller")
	cmd := exec.Command(executable, "-test.run=^TestWorkspaceCrashController$")
	cmd.Env = append(os.Environ(), "SKIM_WORKSPACE_CONTROLLER=1", "SKIM_CTRL_PR="+string(pr), "SKIM_CTRL_SOURCE="+string(source),
		"SKIM_CTRL_PRIVATE="+f.private, "SKIM_CTRL_PEER="+f.peer, "SKIM_CTRL_ROOT="+f.root, "SKIM_CTRL_MARKER="+marker, "SKIM_CTRL_RECORD="+map[bool]string{true: "1", false: "0"}[record],
		"HOME="+f.home, "GIT_CONFIG_GLOBAL="+os.DevNull, "SKIM_CTRL_TASK="+taskID)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, err := os.ReadFile(marker + ".ready")
		if err == nil {
			path = string(data)
			break
		}
		select {
		case <-done:
			t.Fatal("controller exited before owning a worktree")
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("controller never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if record {
		data, err := os.ReadFile(marker + ".group")
		if err != nil {
			t.Fatal(err)
		}
		group, _ = strconv.Atoi(string(data))
		t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
	}
	// Hard-kill only the controller; its flock dies with it, its agent group does not.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	if record {
		if stopped, err := processgroup.Stopped(group); err != nil || stopped {
			t.Fatalf("test agent did not survive the controller crash: %v", err)
		}
	}
	return path, group
}

func (f *gitFixture) restarted(t *testing.T) *Manager {
	t.Helper()
	m := New(f.private)
	if m.err != nil || m.SetLocalRemoteForTesting(f.source.URL, f.peer) != nil {
		t.Fatalf("restarted manager: %v", m.err)
	}
	return m
}

func TestCrashedControllerAgentBlocksPrepareAndFinishUntilGroupStops(t *testing.T) {
	f := newGitFixture(t)
	path, group := f.crashController(t, true, "")
	m := f.restarted(t)
	if w := m.Status(f.pr.Key(), f.root); w.Status != "cleanup_failed" || w.CanDiscardChanges {
		t.Fatalf("crashed in-use checkout was reported as safe: %+v", w)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if lease, err := m.Prepare(ctx, f.root, f.pr, f.source, "", ""); err == nil || !strings.Contains(err.Error(), "may still be running") {
		lease.Release()
		t.Fatalf("Prepare reused a worktree whose agent can still write: %v", err)
	}
	id := m.Status(f.pr.Key(), f.root).ID
	for _, discard := range []bool{false, true} {
		w := m.Finish(ctx, f.pr.Key(), discard, map[bool]string{true: id, false: ""}[discard])
		if w.Status != "cleanup_failed" || w.CanDiscardChanges || !strings.Contains(w.Error, "may still be running") {
			t.Fatalf("Finish(discard=%v) was not refused: %+v", discard, w)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("worktree was removed while its agent lives (discard=%v)", discard)
		}
	}
	// Ending only the owned group makes both reuse and cleanup possible again.
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		if stopped, err := processgroup.Stopped(group); err == nil && stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test agent did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	lease, err := m.Prepare(ctx, f.root, f.pr, f.source, "", "")
	if err != nil {
		t.Fatalf("recovery after the agent stopped: %v", err)
	}
	if lease.Path != path {
		t.Fatalf("recovery replaced the checkout: %s != %s", lease.Path, path)
	}
	lease.Release()
	if w := m.Status(f.pr.Key(), f.root); w.Status != "ready" {
		t.Fatalf("normal Release did not leave the checkout ready: %+v", w)
	}
	if w := m.Finish(ctx, f.pr.Key(), false, ""); w.Status != "finished" {
		t.Fatalf("cleanup after recovery: %+v", w)
	}
	missing(t, path)
}

func TestCrashBeforeAgentRecordFailsClosedWithManualRecovery(t *testing.T) {
	f := newGitFixture(t)
	path, _ := f.crashController(t, false, "")
	m := f.restarted(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if lease, err := m.Prepare(ctx, f.root, f.pr, f.source, "", ""); err == nil || !strings.Contains(err.Error(), "was recorded") {
		lease.Release()
		t.Fatalf("an unrecorded agent must refuse reuse: %v", err)
	}
	id := m.Status(f.pr.Key(), f.root).ID
	w := m.Finish(ctx, f.pr.Key(), true, id)
	if w.Status != "cleanup_failed" || w.CanDiscardChanges || !strings.Contains(w.Error, "Skim will not remove it") {
		t.Fatalf("discard bypassed the unknown-agent refusal: %+v", w)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("worktree removed although its agent could not be accounted for")
	}
	// The user, not Skim, removes the owned checkout through Git: both the folder and
	// the registration go away. Only then may the unknown-agent record be retired.
	r := m.get(f.pr.Key())
	repo := m.data.Caches[r.Entry.CacheID].repo()
	f.run(repo, "worktree", "remove", "--force", "--", path)
	w = m.Finish(ctx, f.pr.Key(), false, "")
	if w.Status != "finished" || w.Path != "" {
		t.Fatalf("Finish did not retire the record after manual removal: %+v", w)
	}
	if after := m.get(f.pr.Key()); after.Entry != nil || after.Verifying {
		t.Fatalf("record not retired: %+v", after)
	}
	lease, err := m.Prepare(ctx, f.root, f.pr, f.source, "", "")
	if err != nil {
		t.Fatalf("a fresh Prepare after recovery failed: %v", err)
	}
	defer lease.Release()
	if lease.Path == path || fixtureHead(t, f, lease.Path) != f.pr.HeadSHA {
		t.Fatalf("recovery did not create a fresh checkout: %s", lease.Path)
	}
}

func fixtureHead(t *testing.T, f *gitFixture, path string) string {
	t.Helper()
	return f.run(path, "rev-parse", "HEAD")
}

func TestUnknownAgentRecoveryRequiresFolderAndRegistrationRemoved(t *testing.T) {
	f := newGitFixture(t)
	path, _ := f.crashController(t, false, "")
	m := f.restarted(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Deleting only the folder leaves Git's registration, so the agent still cannot be excluded.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if w := m.Finish(ctx, f.pr.Key(), false, ""); w.Status != "cleanup_failed" || !strings.Contains(w.Error, "was recorded") {
		t.Fatalf("a registered but missing worktree was retired: %+v", w)
	}
	r := m.get(f.pr.Key())
	f.run(m.data.Caches[r.Entry.CacheID].repo(), "worktree", "prune")
	if w := m.Finish(ctx, f.pr.Key(), false, ""); w.Status != "finished" {
		t.Fatalf("folder and registration gone, yet not retired: %+v", w)
	}
}

func TestGracefulReleaseIsDurableAndNeverBlocksRestart(t *testing.T) {
	f := newGitFixture(t)
	lease := f.prepare()
	agent := exec.Command("/bin/sleep", "300")
	agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	if err := lease.RecordProcess(context.Background(), agent.Process.Pid); err != nil {
		t.Fatal(err)
	}
	// Release is only legal after the agent drained; a live recorded group is retained.
	lease.Release()
	if w := f.restarted(t).Status(f.pr.Key(), f.root); w.Status != "cleanup_failed" {
		t.Fatalf("Release cleared a live agent group: %+v", w)
	}
	_ = syscall.Kill(-agent.Process.Pid, syscall.SIGKILL)
	_ = agent.Wait()

	for _, recordGroup := range []bool{true, false} {
		lease, err := f.manager.Prepare(context.Background(), f.root, f.pr, f.source, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if recordGroup {
			done := exec.Command("sh", "-c", "exit 0")
			done.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := done.Run(); err != nil {
				t.Fatal(err)
			}
			if err := lease.RecordProcess(context.Background(), done.Process.Pid); err != nil {
				t.Fatal(err)
			}
		}
		lease.Release()
		restarted := f.restarted(t)
		if w := restarted.Status(f.pr.Key(), f.root); w.Status != "ready" || w.Error != "" {
			t.Fatalf("graceful restart is blocked (recordGroup=%v): %+v", recordGroup, w)
		}
		if r := restarted.get(f.pr.Key()); r.Verifying || r.ProcessGroup != 0 {
			t.Fatalf("Release left verification state in the journal: %+v", r)
		}
	}
	if w := f.restarted(t).Finish(context.Background(), f.pr.Key(), false, ""); w.Status != "finished" {
		t.Fatalf("finish after graceful restarts: %+v", w)
	}
}

func TestUnknownAgentRetirementIsMetadataOnlyAndKeepsProofOnDoubt(t *testing.T) {
	f := newGitFixture(t)
	path, _ := f.crashController(t, false, "")
	m := f.restarted(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r := m.get(f.pr.Key())
	repo := m.data.Caches[r.Entry.CacheID].repo()
	f.run(repo, "worktree", "remove", "--force", "--", path)
	// An unexpected file in the owned metadata slot means the slot is not conclusively empty.
	sentinel := filepath.Join(r.Entry.Slot, "unexpected.txt")
	mustWrite(t, sentinel, "keep")
	if w := m.Finish(ctx, f.pr.Key(), false, ""); w.Status != "cleanup_failed" || !strings.Contains(w.Error, "was recorded") {
		t.Fatalf("retired despite an unexpected file in the slot: %+v", w)
	}
	if after := m.get(f.pr.Key()); !after.Verifying || after.ProcessGroup != 0 || after.Entry == nil {
		t.Fatalf("unknown-agent proof was cleared by a failed retirement: %+v", after)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("Skim deleted an unexpected file")
	}
	// The checkout reappears at the very same path and head before the next attempt.
	f.run(repo, "worktree", "add", "--detach", "--", path, r.Entry.Head)
	mustWrite(t, filepath.Join(path, "precious.txt"), "work in progress")
	id := m.Status(f.pr.Key(), f.root).ID
	for _, discard := range []bool{false, true} {
		expected := ""
		if discard {
			expected = id
		}
		w := m.Finish(ctx, f.pr.Key(), discard, expected)
		if w.Status != "cleanup_failed" || w.CanDiscardChanges {
			t.Fatalf("Finish(discard=%v) did not refuse: %+v", discard, w)
		}
		if mustRead(t, filepath.Join(path, "precious.txt")) != "work in progress" {
			t.Fatalf("recreated checkout lost data (discard=%v)", discard)
		}
	}
	if after := m.get(f.pr.Key()); !after.Verifying || after.Entry == nil {
		t.Fatalf("unknown-agent proof lost: %+v", after)
	}
}

func TestUnknownTaskAgentPreservesOnlyItsCheckoutDuringAggregateFinish(t *testing.T) {
	f := newGitFixture(t)
	path, _ := f.crashController(t, false, "unrecorded")
	f.manager = f.restarted(t)
	clean := f.prepareTask("clean")
	clean.Release()
	dirty := f.prepareTask("dirty")
	dirty.Release()
	mustWrite(t, filepath.Join(dirty.Path, "notes"), "preserve this task")
	w := f.finish(false)
	if w.Status != "cleanup_failed" || w.Count != 2 || w.CanDiscardChanges || !strings.Contains(w.Error, "was recorded") {
		t.Fatalf("unknown task agent did not fail closed: %+v", w)
	}
	missing(t, clean.Path)
	key := recordKey(f.pr.Key(), "unrecorded")
	r := f.manager.get(key)
	if r.Entry == nil || !r.Verifying || r.ProcessGroup != 0 || r.Entry.Path != path {
		t.Fatalf("aggregate cleanup lost unknown-agent proof: %+v", r)
	}
	// A matching discard identity authorizes dirty files, not an unknown agent.
	w = f.manager.Finish(context.Background(), f.pr.Key(), true, w.ID)
	if w.Status != "cleanup_failed" || w.Count != 1 || w.ID != r.Entry.ID || w.CanDiscardChanges {
		t.Fatalf("discard bypassed unknown-task safety: %+v", w)
	}
	missing(t, dirty.Path)
	if mustRead(t, filepath.Join(path, "file.txt")) != "fork head\n" {
		t.Fatal("discard changed the unknown-agent checkout")
	}
	f.manager = f.restarted(t)
	if after := f.manager.get(key); after.Entry == nil || !after.Verifying || after.ProcessGroup != 0 {
		t.Fatalf("unknown task proof was not durable: %+v", after)
	}
	repo := f.manager.data.Caches[r.Entry.CacheID].repo()
	f.run(repo, "worktree", "remove", "--force", "--", path)
	if got := f.finish(false); got.Status != "finished" || got.Count != 0 {
		t.Fatalf("manual task recovery did not retire aggregate ownership: %+v", got)
	}
}
