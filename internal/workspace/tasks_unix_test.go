//go:build unix

package workspace

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"skim/internal/domain"
)

func (f *gitFixture) prepareTask(taskID string) *Lease {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lease, err := f.manager.Prepare(ctx, f.root, f.pr, f.source, "", taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(lease.Release)
	return lease
}

func TestTaskLeasesIsolateConcurrentCheckoutsAndPerPRCleanup(t *testing.T) {
	f := newGitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type result struct {
		task  string
		lease *Lease
		err   error
	}
	results := make(chan result, 2)
	for _, task := range []string{"thread/one", "thread/two"} {
		go func(task string) {
			lease, err := f.manager.Prepare(ctx, f.root, f.pr, f.source, "", task)
			results <- result{task, lease, err}
		}(task)
	}
	leases := map[string]*Lease{}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Error(r.err)
			continue
		}
		t.Cleanup(r.lease.Release)
		leases[r.task] = r.lease
	}
	if t.Failed() {
		t.FailNow()
	}
	first, second := leases["thread/one"], leases["thread/two"]
	if first.Path == second.Path || first.id == second.id {
		t.Fatal("simultaneously held task leases shared a checkout")
	}
	w := f.manager.Status(f.pr.Key(), f.root)
	if w.Count != 2 || w.Path != "" || w.ID == "" || w.Status != "in_use" {
		t.Fatalf("aggregate hid concurrent checkout ownership: %+v", w)
	}
	for task, lease := range leases {
		if got := f.manager.TaskStatus(f.pr.Key(), task, f.root); got.Path != lease.Path || got.ID != lease.id || got.Count != 1 || got.Status != "in_use" {
			t.Fatalf("task status mixed leases: %+v", got)
		}
		if r := f.manager.get(lease.key); r.PRKey != f.pr.Key() || r.TaskID != task || r.Entry.PRKey != f.pr.Key() {
			t.Fatalf("task key replaced canonical PR identity: %+v", r)
		}
	}
	if f.manager.get(first.key).Entry.CacheID != f.manager.get(second.key).Entry.CacheID {
		t.Fatal("isolated tasks did not share their repository cache")
	}
	mustWrite(t, filepath.Join(first.Path, "file.txt"), "first task only\n")
	if got := mustRead(t, filepath.Join(second.Path, "file.txt")); got != "fork head\n" {
		t.Fatalf("one task modified the other checkout: %q", got)
	}
	if overlap, err := f.restarted(t).Prepare(ctx, f.root, f.pr, f.source, "", "thread/one"); err == nil || !strings.Contains(err.Error(), "in use") {
		if overlap != nil {
			overlap.Release()
		}
		t.Fatalf("same-slot overlap was not rejected: %v", err)
	}
	mustWrite(t, filepath.Join(first.Path, "file.txt"), "fork head\n")
	first.Release()
	first.Release()
	if got := f.manager.TaskStatus(f.pr.Key(), "thread/two", f.root); got.Status != "in_use" || got.ID != second.id {
		t.Fatalf("one task released another lease: %+v", got)
	}
	reused := f.prepareTask("thread/one")
	if reused.Path != first.Path {
		t.Fatal("same clean task did not reuse its own checkout")
	}
	if err := first.RecordProcess(ctx, os.Getpid()); err == nil {
		t.Fatal("released lease changed a later lease's process metadata")
	}
	if r := f.manager.get(reused.key); !r.Verifying || r.ProcessGroup != 0 {
		t.Fatalf("stale lease corrupted its replacement: %+v", r)
	}
	legacy := f.prepare()
	legacy.Release()
	reused.Release()
	second.Release()
	otherPR, otherSource := f.pr, f.source
	otherPR.Number, otherSource.HeadRef = 2, "refs/pull/2/head"
	other, err := f.manager.Prepare(ctx, f.root, otherPR, otherSource, "", "thread/one")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if got := f.finish(false); got.Status != "finished" || got.Count != 0 || got.Path != "" || got.ID != "" {
		t.Fatalf("all-slot cleanup did not finish: %+v", got)
	}
	for _, lease := range []*Lease{first, second, legacy} {
		missing(t, lease.Path)
	}
	if mustRead(t, filepath.Join(other.Path, "file.txt")) != "fork head\n" {
		t.Fatal("cleanup crossed the PR boundary")
	}
	restarted := f.restarted(t)
	for _, task := range []string{"", "thread/one", "thread/two"} {
		if got := restarted.TaskStatus(f.pr.Key(), task, f.root); got.Status != "finished" || got.Count != 0 || got.Path != "" || got.FinishedAt == "" {
			t.Fatalf("task cleanup was not durable: %+v", got)
		}
	}
}

func startTaskAgent(t *testing.T, path string) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "300")
	cmd.Dir = path
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		})
	}
	t.Cleanup(stop)
	return cmd, stop
}

func TestTaskProcessJournalsAndPartialCleanupSurviveRestart(t *testing.T) {
	f := newGitFixture(t)
	first := f.prepareTask("first")
	otherManager := f.restarted(t)
	second, err := otherManager.Prepare(context.Background(), f.root, f.pr, f.source, "", "orphan")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Release)
	firstAgent, stopFirst := startTaskAgent(t, first.Path)
	secondAgent, stopSecond := startTaskAgent(t, second.Path)
	results := make(chan error, 2)
	go func() { results <- first.RecordProcess(context.Background(), firstAgent.Process.Pid) }()
	go func() { results <- second.RecordProcess(context.Background(), secondAgent.Process.Pid) }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := first.RecordProcess(context.Background(), secondAgent.Process.Pid); err == nil {
		t.Fatal("one task overwrote its recorded process with the other task's group")
	}
	restarted := f.restarted(t)
	for _, item := range []struct {
		lease *Lease
		group int
	}{{first, firstAgent.Process.Pid}, {second, secondAgent.Process.Pid}} {
		r := restarted.get(item.lease.key)
		if !r.Verifying || r.ProcessGroup != item.group || r.Entry.ID != item.lease.id {
			t.Fatalf("concurrent process journal update was lost: %+v", r)
		}
	}
	stopFirst()
	first.Release()
	// A live group is deliberately released without drainage, modeling the
	// durable orphan refusal after the owning server loses its lease.
	second.Release()
	f.manager = f.restarted(t)
	if r := f.manager.get(first.key); r.Verifying || r.ProcessGroup != 0 {
		t.Fatalf("clean release did not survive restart: %+v", r)
	}
	if r := f.manager.get(second.key); !r.Verifying || r.ProcessGroup != secondAgent.Process.Pid {
		t.Fatalf("another task's release cleared the orphan proof: %+v", r)
	}
	clean := f.prepareTask("clean")
	clean.Release()
	dirty := f.prepareTask("dirty")
	dirty.Release()
	mustWrite(t, filepath.Join(dirty.Path, "notes"), "retain my notes")
	w := f.finish(false)
	if w.Status != "cleanup_failed" || w.Count != 2 || w.Path != "" || w.CanDiscardChanges || !strings.Contains(w.Error, "orphan") || !strings.Contains(w.Error, "dirty") {
		t.Fatalf("partial cleanup lost unsafe/dirty tasks: %+v", w)
	}
	missing(t, first.Path)
	missing(t, clean.Path)
	if mustRead(t, filepath.Join(dirty.Path, "notes")) != "retain my notes" || mustRead(t, filepath.Join(second.Path, "file.txt")) != "fork head\n" {
		t.Fatal("partial cleanup changed a retained task")
	}
	restarted = f.restarted(t)
	if got := restarted.Status(f.pr.Key(), f.root); got.Count != 2 || got.ID != w.ID || got.CanDiscardChanges {
		t.Fatalf("partial cleanup ownership was not durable: %+v", got)
	}
	stopSecond()
	w = f.finish(false)
	if w.Count != 1 || w.ID != dirty.id || w.Path != dirty.Path || !w.CanDiscardChanges {
		t.Fatalf("stopped orphan did not leave only the dirty task: %+v", w)
	}
	missing(t, second.Path)
	if got := f.finish(true); got.Status != "finished" || got.Count != 0 {
		t.Fatalf("explicit dirty-task cleanup failed: %+v", got)
	}
	missing(t, dirty.Path)
}

func TestTaskDiscardIdentityRejectsChangedCheckoutSets(t *testing.T) {
	f := newGitFixture(t)
	first := f.prepareTask("first")
	first.Release()
	second := f.prepareTask("second")
	second.Release()
	mustWrite(t, filepath.Join(second.Path, "notes"), "second notes")
	originalID := f.manager.Status(f.pr.Key(), f.root).ID
	f.pr.HeadSHA = f.advance()
	replacement := f.prepareTask("first")
	replacement.Release()
	mustWrite(t, filepath.Join(replacement.Path, "notes"), "replacement notes")
	currentID := f.manager.Status(f.pr.Key(), f.root).ID
	if currentID == originalID {
		t.Fatal("replacing one child did not change aggregate discard identity")
	}
	for _, id := range []string{originalID, second.id, replacement.id, ""} {
		if got := f.manager.Finish(context.Background(), f.pr.Key(), true, id); got.Status != "cleanup_failed" || got.CanDiscardChanges || !strings.Contains(got.Error, "changed") {
			t.Fatalf("partial/stale discard confirmation was accepted: %+v", got)
		}
	}
	third := f.prepareTask("third")
	third.Release()
	threeID := f.manager.Status(f.pr.Key(), f.root).ID
	if got := f.manager.Finish(context.Background(), f.pr.Key(), true, currentID); got.Status != "cleanup_failed" || got.CanDiscardChanges {
		t.Fatalf("adding a slot did not invalidate confirmation: %+v", got)
	}
	if got := f.finish(false); got.Count != 2 || got.ID != currentID || !got.CanDiscardChanges {
		t.Fatalf("clean child cleanup did not restore the retained set identity: %+v", got)
	}
	missing(t, third.Path)
	if got := f.manager.Finish(context.Background(), f.pr.Key(), true, threeID); got.Status != "cleanup_failed" || got.CanDiscardChanges {
		t.Fatalf("removing a child did not invalidate confirmation: %+v", got)
	}
	if mustRead(t, filepath.Join(second.Path, "notes")) != "second notes" || mustRead(t, filepath.Join(replacement.Path, "notes")) != "replacement notes" {
		t.Fatal("stale confirmation removed retained task data")
	}
	f.manager = f.restarted(t)
	if got := f.manager.Status(f.pr.Key(), f.root); got.ID != currentID || got.Count != 2 || got.Path != "" {
		t.Fatalf("aggregate identity changed across restart: %+v", got)
	}
	if got := f.manager.Finish(context.Background(), f.pr.Key(), true, currentID); got.Status != "finished" || got.Count != 0 {
		t.Fatalf("matching aggregate discard failed: %+v", got)
	}
	missing(t, replacement.Path)
	missing(t, second.Path)
}

func TestTaskFinishWaitRevalidatesDiscardWithoutBlockingPreparation(t *testing.T) {
	f := newGitFixture(t)
	first := f.prepareTask("first")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan domain.Workspace, 1)
	go func() { done <- f.manager.Finish(ctx, f.pr.Key(), true, first.id) }()
	for f.manager.TaskStatus(f.pr.Key(), "first", f.root).Status != "cleanup_pending" {
		select {
		case <-ctx.Done():
			t.Fatal("Finish did not enter its lease wait")
		case <-time.After(time.Millisecond):
		}
	}
	// Git preparation and process recording can proceed while cleanup waits.
	second, err := f.manager.Prepare(ctx, f.root, f.pr, f.source, "", "second")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	mustWrite(t, filepath.Join(second.Path, "notes"), "created after confirmation")
	process := exec.Command("/bin/sh", "-c", "exit 0")
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := process.Run(); err != nil {
		t.Fatal(err)
	}
	if err := first.RecordProcess(ctx, process.Process.Pid); err != nil {
		t.Fatalf("Finish blocked process registration: %v", err)
	}
	second.Release()
	first.Release()
	select {
	case got := <-done:
		if got.Status != "cleanup_failed" || got.CanDiscardChanges || got.Count != 2 || !strings.Contains(got.Error, "changed") {
			t.Fatalf("wait bypassed changed discard identity: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("Finish failed to resume after the lease drained")
	}
	if mustRead(t, filepath.Join(first.Path, "file.txt")) != "fork head\n" || mustRead(t, filepath.Join(second.Path, "notes")) != "created after confirmation" {
		t.Fatal("changed confirmation removed a checkout")
	}
	if got := f.finish(true); got.Status != "finished" {
		t.Fatalf("fresh confirmation did not clean both tasks: %+v", got)
	}
}

func TestTaskJournalMigratesLegacyCheckoutAndFinishedRecords(t *testing.T) {
	f := newGitFixture(t)
	legacy := f.prepare()
	legacy.Release()
	unused := "github/owner/repo/unused"
	if got := f.manager.Finish(context.Background(), unused, false, ""); got.Status != "finished" {
		t.Fatalf("finished fixture: %+v", got)
	}
	path := filepath.Join(f.manager.private, "journal.json")
	data, err := privateRead(path)
	if err != nil {
		t.Fatal(err)
	}
	var old journal
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	old.Version = 1
	for key, r := range old.Reviews {
		r.PRKey, r.TaskID, r.Workspace.Count = "", "", 0
		old.Reviews[key] = r
	}
	if err := atomicWrite(path, old); err != nil {
		t.Fatal(err)
	}
	f.manager = f.restarted(t)
	if got := f.manager.TaskStatus(f.pr.Key(), "", f.root); got.Path != legacy.Path || got.ID != legacy.id || got.Count != 1 || got.Status != "ready" {
		t.Fatalf("migration lost legacy ownership: %+v", got)
	}
	if got := f.manager.Status(unused, f.root); got.Status != "finished" || got.FinishedAt == "" {
		t.Fatalf("migration lost a record without an entry: %+v", got)
	}
	task := f.prepareTask("new/thread")
	task.Release()
	if task.Path == legacy.Path || f.manager.get(task.key).Entry.CacheID != f.manager.get(f.pr.Key()).Entry.CacheID {
		t.Fatal("migration shared a checkout or abandoned its owned cache")
	}
	reused := f.prepare()
	if reused.Path != legacy.Path {
		t.Fatal("migration replaced the default slot")
	}
	reused.Release()
	f.manager = f.restarted(t)
	if got := f.manager.Status(f.pr.Key(), f.root); got.Count != 2 || got.Path != "" || got.Status != "ready" {
		t.Fatalf("migrated task records were not durable: %+v", got)
	}
	if got := f.finish(false); got.Status != "finished" || got.Count != 0 {
		t.Fatalf("migrated cleanup: %+v", got)
	}
	missing(t, task.Path)
	missing(t, legacy.Path)
}
