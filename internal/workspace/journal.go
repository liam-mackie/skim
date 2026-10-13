package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"skim/internal/domain"
	"skim/internal/filelock"
)

const journalVersion = 2
const markerName = "ownership.json"

type journal struct {
	Version int               `json:"version"`
	Owner   string            `json:"owner"`
	Caches  map[string]cache  `json:"caches"`
	Reviews map[string]record `json:"reviews"`
}

type cache struct {
	ID          string `json:"id"`
	Root        string `json:"root"`
	Namespace   string `json:"namespace"`
	URL         string `json:"url"`
	Initialized bool   `json:"initialized"`
}

func (c cache) repo() string { return filepath.Join(c.Namespace, "repository.git") }

type entry struct {
	ID      string `json:"id"`
	CacheID string `json:"cacheId"`
	PRKey   string `json:"prKey"`
	Slot    string `json:"slot"`
	Path    string `json:"path"`
	Head    string `json:"head"`
	Base    string `json:"base"`
}

type record struct {
	PRKey     string           `json:"prKey"`
	TaskID    string           `json:"taskId,omitempty"`
	Workspace domain.Workspace `json:"workspace"`
	Entry     *entry           `json:"entry,omitempty"`
	// Verifying is set durably before an agent may start and cleared only after its
	// process group has drained. ProcessGroup (0 = not yet recorded) is bound to this
	// record's Entry: a replaced checkout is a new record, never inheriting a group.
	Verifying    bool `json:"verifying,omitempty"`
	ProcessGroup int  `json:"processGroup,omitempty"`
}

func recordKey(prKey, taskID string) string {
	if taskID == "" {
		return prKey
	}
	return prKey + "\x00" + taskID
}

type ownership struct {
	Owner string `json:"owner"`
	Cache cache  `json:"cache"`
	Entry *entry `json:"entry,omitempty"`
}

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// private reports whether owner-only permission bits protect a path. Windows
// reports 0777/0666 regardless of ACLs, so the bit check cannot be applied there
// and Windows relies on location instead: Skim's private data lives under the
// per-user profile (%AppData%), whose inherited ACL grants access only to the
// user, SYSTEM and Administrators. Namespaces under a configured managed root
// inherit that root's ACL, so on shared Windows hosts the root must be private.
func private(info os.FileInfo) bool {
	return runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0
}

// samePath reports whether a path printed by Git names expected. Git for Windows
// prints forward slashes, and Windows path names are case-insensitive.
func samePath(reported, expected string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.FromSlash(reported), expected)
	}
	return reported == expected
}

func ownedDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !private(info) {
		return errors.New("owned directory is missing, replaced, or not private")
	}
	resolved, err := canonical(path)
	if err != nil || resolved != path {
		return errors.New("owned directory has changed canonical location")
	}
	return nil
}

func privateRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !private(info) || info.Size() > 8<<20 {
		return nil, errors.New("private workspace metadata is unsafe")
	}
	return os.ReadFile(path)
}

// atomicWrite never follows the destination. Its parent must already be owned.
func atomicWrite(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWriteBytes(path, data)
}

func atomicWriteBytes(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".journal-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return filelock.SyncDir(filepath.Dir(path))
}

func exclusiveJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = filelock.SyncDir(filepath.Dir(path))
	}
	return err
}

func (m *Manager) initialize(privateDataDir string) error {
	if strings.TrimSpace(privateDataDir) == "" {
		return errors.New("private workspace data directory is required")
	}
	if err := os.MkdirAll(privateDataDir, 0700); err != nil {
		return err
	}
	parent, err := canonical(privateDataDir)
	if err != nil {
		return err
	}
	m.private = filepath.Join(parent, "managed-workspaces")
	created := false
	if err := os.Mkdir(m.private, 0700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := ownedDirectory(m.private); err != nil {
		return err
	}
	if created {
		id, err := randomID()
		if err != nil {
			return err
		}
		m.data = journal{Version: journalVersion, Owner: id, Caches: map[string]cache{}, Reviews: map[string]record{}}
		if err := exclusiveJSON(filepath.Join(m.private, markerName), id); err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(m.private, "journal.json"), m.data); err != nil {
			return err
		}
	}
	owner, err := privateRead(filepath.Join(m.private, markerName))
	if err != nil {
		return errors.New("workspace journal directory is not owned; choose a different private data directory")
	}
	if err := json.Unmarshal(owner, &m.owner); err != nil || len(m.owner) != 32 {
		return errors.New("workspace journal ownership marker is invalid")
	}
	if err := m.load(); err != nil {
		return err
	}
	m.home = filepath.Join(m.private, "git-home")
	if err := os.Mkdir(m.home, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return ownedDirectory(m.home)
}

func (m *Manager) privateIdentity() error {
	if err := ownedDirectory(m.private); err != nil {
		return err
	}
	marker, err := privateRead(filepath.Join(m.private, markerName))
	if err != nil {
		return err
	}
	var owner string
	if json.Unmarshal(marker, &owner) != nil || owner != m.owner {
		return errors.New("workspace journal ownership changed")
	}
	return nil
}

// Git must not discover credentials in an old or externally populated home.
// The helper and journal live outside this deliberately empty directory.
func (m *Manager) validateGitHome() error {
	if err := m.privateIdentity(); err != nil {
		return err
	}
	if err := ownedDirectory(m.home); err != nil {
		return err
	}
	children, err := os.ReadDir(m.home)
	if err != nil {
		return err
	}
	if len(children) != 0 {
		return errors.New("isolated Git home contains unexpected files; preserve them outside the private Git home before retrying")
	}
	return nil
}

func (m *Manager) load() error {
	if err := m.privateIdentity(); err != nil {
		return err
	}
	bytes, err := privateRead(filepath.Join(m.private, "journal.json"))
	if err != nil {
		return err
	}
	var data journal
	if err := json.Unmarshal(bytes, &data); err != nil {
		return errors.New("workspace journal is unreadable; preserve it for recovery")
	}
	if (data.Version != 1 && data.Version != journalVersion) || data.Owner != m.owner || data.Caches == nil || data.Reviews == nil {
		return errors.New("workspace journal identity is invalid; preserve it for recovery")
	}
	for id, c := range data.Caches {
		if id != c.ID || !validID(id) || !filepath.IsAbs(c.Root) || filepath.Clean(c.Root) != c.Root || c.Namespace != filepath.Join(c.Root, ".skim-"+id) || !canonicalURL(c.URL) {
			return errors.New("workspace journal contains an unsafe repository entry")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := make(map[string]bool)
	for key, r := range data.Reviews {
		// Version one stored only a default checkout at the canonical PR key.
		// Keep that key and immutable entry/marker identity during migration.
		if data.Version == 1 {
			if r.PRKey != "" || r.TaskID != "" {
				return errors.New("workspace journal contains an unsafe legacy review entry")
			}
			r.PRKey = key
		}
		if r.PRKey == "" || strings.ContainsRune(r.PRKey, '\x00') || key != recordKey(r.PRKey, r.TaskID) {
			return errors.New("workspace journal contains an unsafe review identity")
		}
		if r.ProcessGroup < 0 || r.ProcessGroup == 1 || (r.ProcessGroup > 0 && (!r.Verifying || r.Entry == nil)) || (r.Verifying && r.Entry == nil) {
			return errors.New("workspace journal contains an unsafe verification record")
		}
		if e := r.Entry; e != nil {
			c, ok := data.Caches[e.CacheID]
			if !ok || !validID(e.ID) || entries[e.ID] || e.PRKey != r.PRKey || e.Slot != filepath.Join(c.Namespace, "worktree-"+e.ID) || e.Path != filepath.Join(e.Slot, "tree") || !shaPattern.MatchString(e.Head) || !shaPattern.MatchString(e.Base) || r.Workspace.Root != c.Root || r.Workspace.Path != e.Path || r.Workspace.HeadSHA != e.Head || r.Workspace.ID != e.ID {
				return errors.New("workspace journal contains an unsafe checkout entry")
			}
			entries[e.ID] = true
			r.Workspace.Count = 1
			// A live lease in this process owns its record in memory; the disk
			// copy may be older, but its identity must still validate first.
			if cur, ok := m.data.Reviews[key]; ok && cur.Entry != nil && cur.Entry.ID == e.ID && m.active[e.ID] > 0 {
				data.Reviews[key] = cur
				continue
			}
			if r.Workspace.Status == "in_use" && m.active[e.ID] == 0 {
				// An in-use checkout without a clean Release is never blindly ready:
				// quiescence is proven (or refused) by the next Prepare/Finish.
				r.Verifying = true
				r.Workspace.Status, r.Workspace.Error = "cleanup_failed", errUncleanVerification
			}
			if r.Workspace.Status == "preparing" {
				r.Workspace.Status = "cleanup_pending"
				r.Workspace.Error = "workspace preparation was interrupted; retry verification or Finish to recover the owned checkout"
			}
		}
		data.Reviews[key] = r
	}
	data.Version = journalVersion
	m.data = data
	return nil
}

func validID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}

// All production mutations hold the operation gate and journal file lock, so
// persistence cannot reorder snapshots. Polling need not wait for the fsync.
func (m *Manager) persist() error {
	m.mu.Lock()
	bytes, err := json.Marshal(m.data)
	m.mu.Unlock()
	if err == nil {
		if err = m.privateIdentity(); err == nil {
			err = atomicWriteBytes(filepath.Join(m.private, "journal.json"), bytes)
		}
	}
	if err != nil {
		m.mu.Lock()
		m.err = fmt.Errorf("workspace journal could not be saved; no further workspace changes are safe: %w", err)
		m.mu.Unlock()
	}
	return err
}

func openLock(path string) (*os.File, error) {
	file, err := filelock.OpenNoFollow(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !private(info) {
		file.Close()
		return nil, errors.New("workspace lock is unsafe")
	}
	return file, nil
}

// lockContext polls for an exclusive lock until it is acquired or ctx ends.
func lockContext(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := filelock.TryLock(file, true)
		if err == nil {
			return nil
		}
		if !errors.Is(err, filelock.ErrWouldBlock) {
			return err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func unlock(file *os.File) { _ = filelock.Unlock(file); _ = file.Close() }

func (m *Manager) begin(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.gate:
	}
	end := func() { m.gate <- struct{}{} }
	m.mu.Lock()
	err := m.err
	m.mu.Unlock()
	if err != nil {
		end()
		return nil, err
	}
	if err := m.privateIdentity(); err != nil {
		end()
		return nil, err
	}
	file, err := openLock(filepath.Join(m.private, "journal.lock"))
	if err != nil {
		end()
		return nil, err
	}
	if err := lockContext(ctx, file); err != nil {
		file.Close()
		end()
		return nil, err
	}
	if err := m.load(); err != nil {
		unlock(file)
		end()
		return nil, err
	}
	return func() { unlock(file); end() }, nil
}
