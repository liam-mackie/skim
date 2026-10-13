// Package workspace owns verification-only Git caches and detached worktrees.
// Ownership is a journal-and-marker identity, never a directory-name convention.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"skim/internal/domain"
	"skim/internal/filelock"
	"skim/internal/processgroup"
)

type Manager struct {
	mu           sync.Mutex
	gate         chan struct{}
	private      string
	home         string
	owner        string
	data         journal
	err          error
	active       map[string]int
	localRemotes map[string]string
}

type Lease struct {
	Path     string
	manager  *Manager
	key      string
	id       string
	lock     *os.File
	once     sync.Once
	released bool
}

// New opens only the private ownership journal. Repositories and configured
// roots are untouched until Prepare; initialization failures remain reportable.
func New(privateDataDir string) *Manager {
	m := &Manager{gate: make(chan struct{}, 1), active: map[string]int{}, localRemotes: map[string]string{}}
	m.gate <- struct{}{}
	m.err = m.initialize(privateDataDir)
	return m
}

// Status takes no Git or filesystem locks, so preparation and active verification
// remain observable without blocking polling requests.
func (m *Manager) Status(prKey, configuredRoot string) domain.Workspace {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status(prKey, configuredRoot)
}

// TaskStatus describes only the checkout reserved for this verification thread.
// An empty task ID addresses the journal's original default checkout.
func (m *Manager) TaskStatus(prKey, taskID, configuredRoot string) domain.Workspace {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.data.Reviews[recordKey(prKey, taskID)]
	w := r.Workspace
	w.Count = 0
	if r.Entry != nil {
		w.Count = 1
	}
	if !ok {
		w.Status = "not_created"
	}
	return m.statusError(w, configuredRoot)
}

func (m *Manager) statusError(w domain.Workspace, configuredRoot string) domain.Workspace {
	if w.Root == "" {
		w.Root = configuredRoot
	}
	if m.err != nil {
		w.Status, w.Error = "failed", m.err.Error()
		w.CanDiscardChanges = false
	}
	return w
}

// status requires mu. Discard identity depends only on the current owned entry
// IDs, not task ordering or mutable progress, and retains singleton compatibility.
func (m *Manager) status(prKey, configuredRoot string) domain.Workspace {
	w := domain.Workspace{Status: "not_created"}
	var ids, messages []string
	var checkout domain.Workspace
	var finishedAt string
	allFinished, discardable := true, true
	dirty := false
	rank := 0
	for _, key := range m.reviewKeys(prKey) {
		r := m.data.Reviews[key]
		if r.Entry != nil {
			ids = append(ids, r.Entry.ID)
			checkout = r.Workspace
		}
		if r.Workspace.FinishedAt > finishedAt {
			finishedAt = r.Workspace.FinishedAt
		}
		if r.Workspace.Status == "finished" {
			if rank == 0 {
				w = r.Workspace
			}
			continue
		}
		allFinished = false
		priority := workspacePriority(r.Workspace.Status)
		if priority > rank {
			w = r.Workspace
			rank = priority
		}
		if r.Workspace.Error != "" {
			message := r.Workspace.Error
			if r.TaskID != "" {
				message = "task " + r.TaskID + ": " + message
			}
			messages = append(messages, message)
		}
		if r.Workspace.CanDiscardChanges && r.Entry != nil {
			dirty = true
		} else if r.Workspace.Status != "ready" {
			discardable = false
		}
	}
	w.Count = len(ids)
	w.Path, w.ID = "", ""
	if len(ids) == 1 {
		w.Path, w.ID, w.Root, w.HeadSHA = checkout.Path, ids[0], checkout.Root, checkout.HeadSHA
	} else if len(ids) > 1 {
		sort.Strings(ids)
		sum := sha256.Sum256([]byte(strings.Join(ids, "")))
		w.ID = "workspaces-" + hex.EncodeToString(sum[:])
		w.HeadSHA = ""
	}
	w.Error = strings.Join(messages, "\n")
	w.CanDiscardChanges = dirty && discardable
	w.FinishedAt = ""
	if allFinished && finishedAt != "" {
		w.Status, w.FinishedAt = "finished", finishedAt
	}
	return m.statusError(w, configuredRoot)
}

func workspacePriority(status string) int {
	switch status {
	case "cleanup_pending":
		return 7
	case "in_use":
		return 6
	case "preparing":
		return 5
	case "cleanup_failed":
		return 4
	case "failed":
		return 3
	case "ready":
		return 2
	default:
		return 1
	}
}

// reviewKeys requires mu; task record keys must never stand in for PR identity.
func (m *Manager) reviewKeys(prKey string) []string {
	var keys []string
	for key, r := range m.data.Reviews {
		if r.PRKey == prKey {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func (m *Manager) get(key string) record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data.Reviews[key]
}

func (m *Manager) put(key string, r record) error {
	m.mu.Lock()
	m.data.Reviews[key] = r
	m.mu.Unlock()
	return m.persist()
}

func (m *Manager) failure(key string, r record, err error) error {
	r.Workspace.Error = err.Error()
	r.Workspace.CanDiscardChanges = isLocalChanges(err)
	if r.Entry != nil {
		r.Workspace.Status = "cleanup_failed"
	} else {
		r.Workspace.Status = "failed"
	}
	if saveErr := m.put(key, r); saveErr != nil {
		return fmt.Errorf("%w; workspace journal could not be saved", err)
	}
	return err
}

func managedRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("configure an ACP managed root before verification")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// Resolve the nearest existing ancestor before creating anything: a symlink
	// must not redirect even a new empty root into a real checkout.
	existing := abs
	var suffix []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		suffix = append(suffix, filepath.Base(existing))
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", errors.New("managed root has no existing ancestor")
		}
		existing = parent
	}
	resolved, err := canonical(existing)
	if err != nil {
		return "", err
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, suffix[i])
	}
	for path := resolved; ; path = filepath.Dir(path) {
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			return "", errors.New("managed root must not be inside an existing Git checkout")
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if _, err := os.Lstat(filepath.Join(path, "HEAD")); err == nil {
			if info, err := os.Stat(filepath.Join(path, "objects")); err == nil && info.IsDir() {
				return "", errors.New("managed root must not be inside an existing bare repository")
			}
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	if err := os.MkdirAll(resolved, 0700); err != nil {
		return "", fmt.Errorf("could not create managed root: %w", err)
	}
	return resolved, nil
}

func (m *Manager) Prepare(ctx context.Context, root string, pr domain.PullRequest, source domain.GitSource, token, taskID string) (*Lease, error) {
	end, err := m.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer end()
	if err := validateSource(pr, source); err != nil {
		return nil, err
	}
	if len(token) > 8192 || strings.ContainsAny(token, "\r\n\x00") {
		return nil, errors.New("configured GitHub token is not a valid single-line credential")
	}
	root, err = managedRoot(root)
	if err != nil {
		return nil, err
	}
	key := recordKey(pr.Key(), taskID)
	r := m.get(key)
	if r.Entry != nil {
		lock, err := m.entryLock(ctx, r.Entry, false)
		if err != nil {
			return nil, err
		}
		defer lock.Close()
		// No lease holds this entry, so any recorded verification belongs to a dead holder.
		if err := quiescent(r); err != nil {
			if !m.retirable(ctx, r) {
				return nil, m.failure(key, r, err)
			}
			// Metadata-only retirement of an entry whose checkout is gone, then a fresh one.
			r.Entry, r.Verifying, r.ProcessGroup = nil, false, 0
			r.Workspace.Path, r.Workspace.ID, r.Workspace.Error = "", "", ""
			if err := m.put(key, r); err != nil {
				return nil, err
			}
		}
		if r.Entry != nil {
			m.mu.Lock()
			c := m.data.Caches[r.Entry.CacheID]
			m.mu.Unlock()
			matching := c.Root == root && c.URL == source.URL && r.Entry.Head == strings.ToLower(pr.HeadSHA) && r.Entry.Base == strings.ToLower(pr.BaseSHA)
			if matching {
				if err := m.inspectEntry(ctx, c, *r.Entry, false); err != nil {
					// Recover an interrupted add/removal through Finish's conservative
					// ownership checks before allocating a fresh checkout.
					matching = false
				} else if err := m.clean(ctx, *r.Entry, false); err != nil {
					return nil, m.failure(key, r, err)
				}
			}
			if matching {
				if err := m.fetch(ctx, c, source, pr, token); err != nil {
					return nil, m.failure(key, r, err)
				}
				// Transfer the exclusive inspection lock to the returned shared lease.
				if err := filelock.Downgrade(lock); err != nil {
					return nil, m.failure(key, r, err)
				}
				lease, err := m.lease(key, r, lock)
				if err != nil {
					return nil, err
				}
				// The deferred close owns a duplicate, not the lease's descriptor.
				return lease, nil
			}
			r.Workspace.Status, r.Workspace.Error = "cleanup_pending", ""
			if err := m.put(key, r); err != nil {
				return nil, err
			}
			if err := m.remove(ctx, c, *r.Entry, false); err != nil {
				return nil, m.failure(key, r, err)
			}
			r.Entry, r.Verifying, r.ProcessGroup = nil, false, 0
			r.Workspace.Path, r.Workspace.ID = "", ""
			if err := m.put(key, r); err != nil {
				return nil, err
			}
		}
	}
	r = record{PRKey: pr.Key(), TaskID: taskID, Workspace: domain.Workspace{Root: root, HeadSHA: strings.ToLower(pr.HeadSHA), Status: "preparing"}}
	if err := m.put(key, r); err != nil {
		return nil, err
	}
	c, err := m.repository(ctx, root, source.URL)
	if err != nil {
		return nil, m.failure(key, r, err)
	}
	if err := m.fetch(ctx, c, source, pr, token); err != nil {
		return nil, m.failure(key, r, err)
	}
	id, err := randomID()
	if err != nil {
		return nil, m.failure(key, r, err)
	}
	slot := filepath.Join(c.Namespace, "worktree-"+id)
	e := entry{ID: id, CacheID: c.ID, PRKey: pr.Key(), Slot: slot, Path: filepath.Join(slot, "tree"), Head: strings.ToLower(pr.HeadSHA), Base: strings.ToLower(pr.BaseSHA)}
	r.Entry, r.Workspace.Path = &e, e.Path
	r.Workspace.ID = e.ID
	if err := m.put(key, r); err != nil {
		return nil, err
	}
	if err := os.Mkdir(e.Slot, 0700); err != nil {
		return nil, m.failure(key, r, unsafeOwnership("exclusive checkout namespace could not be created"))
	}
	if err := exclusiveJSON(filepath.Join(e.Slot, markerName), m.marker(c, &e)); err != nil {
		return nil, m.failure(key, r, err)
	}
	if _, err := m.git(ctx, c.repo(), "", "worktree_add", "worktree", "add", "--detach", "--", e.Path, e.Head); err != nil {
		// Preserve both the durable intent and any partially registered checkout.
		// A later Prepare/Finish validates it before attempting exact-entry cleanup.
		return nil, m.failure(key, r, err)
	}
	if err := m.inspectEntry(ctx, c, e, true); err != nil {
		return nil, m.failure(key, r, err)
	}
	lock, err := m.entryLock(ctx, &e, false)
	if err != nil {
		return nil, m.failure(key, r, err)
	}
	defer lock.Close()
	if err := filelock.Downgrade(lock); err != nil {
		return nil, m.failure(key, r, err)
	}
	return m.lease(key, r, lock)
}

func (m *Manager) entryLock(ctx context.Context, e *entry, wait bool) (*os.File, error) {
	file, err := openLock(filepath.Join(m.private, "lease-"+e.ID+".lock"))
	if err != nil {
		return nil, err
	}
	if wait {
		err = lockContext(ctx, file)
	} else {
		err = filelock.TryLock(file, true)
	}
	if err != nil {
		file.Close()
		if errors.Is(err, filelock.ErrWouldBlock) {
			return nil, errWorkspaceInUse
		}
		return nil, err
	}
	return file, nil
}

func (m *Manager) lease(key string, r record, lock *os.File) (*Lease, error) {
	// The duplicate shares the lock; closing the temporary handle must not
	// explicitly unlock it while the lease remains alive.
	file, err := filelock.Dup(lock)
	if err != nil {
		return nil, err
	}
	r.Workspace.Status, r.Workspace.Error, r.Workspace.FinishedAt = "in_use", "", ""
	r.Workspace.CanDiscardChanges = false
	// Durable before any agent can start: a crash from here on is recoverable only
	// by proving the (possibly not yet recorded) agent group stopped.
	r.Verifying, r.ProcessGroup = true, 0
	if err := m.put(key, r); err != nil {
		file.Close()
		return nil, err
	}
	m.mu.Lock()
	m.active[r.Entry.ID]++
	m.mu.Unlock()
	return &Lease{Path: r.Entry.Path, manager: m, key: key, id: r.Entry.ID, lock: file}, nil
}

// RecordProcess durably binds the agent's process group to this lease before the
// agent speaks. Finish drops the operation gate while waiting for an entry lock.
func (l *Lease) RecordProcess(ctx context.Context, group int) error {
	if group <= 1 {
		return errors.New("invalid agent process group")
	}
	m := l.manager
	end, err := m.begin(ctx)
	if err != nil {
		return err
	}
	defer end()
	m.mu.Lock()
	r := m.data.Reviews[l.key]
	if l.released || r.Entry == nil || r.Entry.ID != l.id || !r.Verifying {
		m.mu.Unlock()
		return errors.New("workspace changed while the agent started")
	}
	if r.ProcessGroup != 0 && r.ProcessGroup != group {
		m.mu.Unlock()
		return errors.New("workspace already has a recorded agent process group")
	}
	r.ProcessGroup = group
	m.data.Reviews[l.key] = r
	m.mu.Unlock()
	return m.persist()
}

// Release is idempotent. Persist quiescence before dropping the entry lock so
// another manager cannot mistake a clean release for an unrecorded orphan.
// Failed persistence leaves the durable verifying proof intact and fails closed.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		m := l.manager
		defer func() {
			m.mu.Lock()
			l.released = true
			delete(m.active, l.id)
			m.mu.Unlock()
			unlock(l.lock)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		end, err := m.begin(ctx)
		if err != nil {
			return
		}
		defer end()
		r := m.get(l.key)
		if r.Entry == nil || r.Entry.ID != l.id {
			return
		}
		if r.ProcessGroup > 0 {
			stopped, err := processgroup.Stopped(r.ProcessGroup)
			if err != nil || !stopped {
				return
			}
		}
		r.Verifying, r.ProcessGroup = false, 0
		if r.Workspace.Status == "in_use" || (r.Workspace.Status == "cleanup_failed" && r.Workspace.Error == errUncleanVerification) {
			r.Workspace.Status, r.Workspace.Error = "ready", ""
		}
		_ = m.put(l.key, r)
	})
}

const errUncleanVerification = "A previous verification did not shut down cleanly. Retry cleanup or verify again once its agent process has stopped."

var errWorkspaceInUse = errors.New("workspace is in use; wait for verification to finish")

// retirable reports the one safe recovery for a verification whose agent was never
// recorded. It is metadata-only: the cache and owned slot validate (or the slot is
// conclusively absent), the slot holds nothing but its ownership marker, the checkout
// path is absent, and Git no longer registers it, so no agent can be executing in it.
// The caller then only drops the journal entry; nothing is ever deleted, so a path or
// registration that reappears afterwards cannot lose data. Any doubt refuses and the
// unknown-agent proof stays untouched.
func (m *Manager) retirable(ctx context.Context, r record) bool {
	if r.Entry == nil || !r.Verifying || r.ProcessGroup != 0 {
		return false
	}
	e := *r.Entry
	m.mu.Lock()
	c, ok := m.data.Caches[e.CacheID]
	m.mu.Unlock()
	if !ok || m.validateCache(ctx, c) != nil {
		return false
	}
	if _, err := os.Lstat(e.Slot); err == nil {
		if m.checkSlot(c, e) != nil {
			return false
		}
		children, err := os.ReadDir(e.Slot)
		if err != nil || len(children) != 1 || children[0].Name() != markerName {
			return false
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false
	}
	if _, err := os.Lstat(e.Path); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	registered, err := m.registered(ctx, c, e)
	return err == nil && !registered
}

// quiescent proves, with an exclusive entry lock held, that no recorded agent can still
// execute in this checkout. It only queries the process table and never signals a
// process: a reused PID can only make this refuse, never kill an unrelated process.
func quiescent(r record) error {
	if !r.Verifying {
		return nil
	}
	if r.ProcessGroup == 0 {
		return errors.New("an earlier verification ended before its agent process was recorded, so Skim cannot prove the agent stopped. Stop any agent still using this checkout, then remove both the checkout folder and its Git registration yourself (for example with `git worktree remove`); Skim will not remove it. Retry afterwards")
	}
	stopped, err := processgroup.Stopped(r.ProcessGroup)
	if err != nil {
		return fmt.Errorf("cannot inspect the earlier verification agent process group %d; cleanup is refused until it can be checked", r.ProcessGroup)
	}
	if !stopped {
		return fmt.Errorf("the earlier verification agent (process group %d) may still be running; stop it, then retry", r.ProcessGroup)
	}
	return nil
}

func (m *Manager) Finish(ctx context.Context, prKey string, discard bool, expectedID string) domain.Workspace {
	failed := func(err error) domain.Workspace {
		w := m.Status(prKey, "")
		w.Status, w.Error, w.CanDiscardChanges = "cleanup_failed", err.Error(), false
		return w
	}
	changed := errors.New("workspace changed since discard confirmation; refresh its status before discarding")
	end, err := m.begin(ctx)
	if err != nil {
		return failed(err)
	}
	defer func() { end() }()
	current := m.Status(prKey, "")
	if discard && (expectedID == "" || current.ID != expectedID) {
		return failed(changed)
	}
	attempted := map[string]string{}
	for {
		m.mu.Lock()
		keys := m.reviewKeys(prKey)
		journalErr := m.err
		m.mu.Unlock()
		if journalErr != nil {
			return failed(journalErr)
		}
		if len(keys) == 0 {
			keys = []string{recordKey(prKey, "")}
		}
		key := ""
		var r record
		for _, candidate := range keys {
			candidateRecord := m.get(candidate)
			id := ""
			if candidateRecord.Entry != nil {
				id = candidateRecord.Entry.ID
			}
			if previous, ok := attempted[candidate]; !ok || previous != id {
				key, r = candidate, candidateRecord
				attempted[key] = id
				break
			}
		}
		if key == "" {
			w := m.Status(prKey, "")
			if w.Count > 0 && w.Status != "cleanup_failed" {
				return failed(errors.New("workspace changed while cleanup waited; refresh its status and retry"))
			}
			return w
		}
		if r.PRKey == "" {
			r.PRKey = prKey
		}
		if r.Entry != nil {
			r.Workspace.Status, r.Workspace.Error = "cleanup_pending", ""
			r.Workspace.CanDiscardChanges = false
			if err := m.put(key, r); err != nil {
				return failed(err)
			}
			lock, err := m.entryLock(ctx, r.Entry, false)
			if errors.Is(err, errWorkspaceInUse) {
				// Waiting must not hold the journal gate: independent threads can
				// prepare, record their agents, and durably release their own leases.
				end()
				end = func() {}
				lock, err = m.entryLock(ctx, r.Entry, true)
				if err != nil {
					return failed(err)
				}
				end, err = m.begin(ctx)
				if err != nil {
					end = func() {}
					unlock(lock)
					return failed(err)
				}
				if discard && m.Status(prKey, "").ID != expectedID {
					unlock(lock)
					return failed(changed)
				}
			}
			if err != nil {
				m.cleanupFailure(key, r, err)
				continue
			}
			fresh := m.get(key)
			if fresh.Entry == nil || fresh.Entry.ID != r.Entry.ID {
				unlock(lock)
				return failed(errors.New("workspace changed while cleanup waited; refresh its status and retry"))
			}
			r = fresh
			if err := quiescent(r); err != nil {
				// Discard authorizes local changes, never an unproven agent.
				if !m.retirable(ctx, r) {
					m.cleanupFailure(key, r, err)
					unlock(lock)
					continue
				}
			} else {
				m.mu.Lock()
				c := m.data.Caches[r.Entry.CacheID]
				m.mu.Unlock()
				if err := m.remove(ctx, c, *r.Entry, discard); err != nil {
					m.cleanupFailure(key, r, err)
					unlock(lock)
					continue
				}
			}
			unlock(lock)
		}
		r.Verifying, r.ProcessGroup, r.Entry = false, 0, nil
		r.Workspace.Path, r.Workspace.Error, r.Workspace.ID = "", "", ""
		r.Workspace.Status, r.Workspace.CanDiscardChanges, r.Workspace.Count = "finished", false, 0
		if r.Workspace.FinishedAt == "" {
			r.Workspace.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if err := m.put(key, r); err != nil {
			return failed(err)
		}
		attempted[key] = ""
		// Our own successful deletion changes the set. Any later change while
		// waiting must still invalidate the caller's discard authorization.
		expectedID = m.Status(prKey, "").ID
	}
}

func (m *Manager) cleanupFailure(key string, r record, err error) domain.Workspace {
	r.Workspace.Status, r.Workspace.Error = "cleanup_failed", err.Error()
	r.Workspace.CanDiscardChanges = isLocalChanges(err)
	_ = m.put(key, r)
	return r.Workspace
}
