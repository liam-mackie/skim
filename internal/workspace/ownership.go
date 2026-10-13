package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"skim/internal/filelock"
)

func (m *Manager) marker(c cache, e *entry) ownership {
	c.Initialized = false // This operational flag is not part of immutable ownership.
	return ownership{Owner: m.owner, Cache: c, Entry: e}
}

func (m *Manager) checkMarker(path string, c cache, e *entry) error {
	bytes, err := privateRead(path)
	if err != nil {
		return unsafeOwnership("ownership marker is missing or unsafe")
	}
	var got ownership
	if json.Unmarshal(bytes, &got) != nil {
		return unsafeOwnership("ownership marker is invalid")
	}
	want := m.marker(c, e)
	if got.Owner != want.Owner || got.Cache != want.Cache || (got.Entry == nil) != (want.Entry == nil) {
		return unsafeOwnership("journal and marker disagree")
	}
	if got.Entry != nil && *got.Entry != *want.Entry {
		return unsafeOwnership("checkout marker and journal disagree")
	}
	return nil
}

func (m *Manager) namespace(c cache) error {
	root, err := canonical(c.Root)
	if err != nil || root != c.Root {
		return unsafeOwnership("managed root was replaced or redirected")
	}
	if err := ownedDirectory(c.Namespace); err != nil {
		return unsafeOwnership("repository namespace was replaced or redirected")
	}
	return m.checkMarker(filepath.Join(c.Namespace, markerName), c, nil)
}

func (m *Manager) repository(ctx context.Context, root, sourceURL string) (cache, error) {
	m.mu.Lock()
	var found cache
	for _, c := range m.data.Caches {
		if c.Root == root && c.URL == sourceURL && c.Initialized {
			found = c
			break
		}
	}
	m.mu.Unlock()
	if found.ID != "" {
		return found, m.validateCache(ctx, found)
	}
	id, err := randomID()
	if err != nil {
		return cache{}, err
	}
	c := cache{ID: id, Root: root, Namespace: filepath.Join(root, ".skim-"+id), URL: sourceURL}
	m.mu.Lock()
	m.data.Caches[id] = c
	m.mu.Unlock()
	if err := m.persist(); err != nil {
		return cache{}, err
	}
	if err := os.Mkdir(c.Namespace, 0700); err != nil {
		return cache{}, unsafeOwnership("exclusive repository namespace could not be created")
	}
	if err := exclusiveJSON(filepath.Join(c.Namespace, markerName), m.marker(c, nil)); err != nil {
		return cache{}, err
	}
	if err := os.Mkdir(c.repo(), 0700); err != nil {
		return cache{}, err
	}
	if _, err := m.git(ctx, c.Namespace, "", "init", "init", "--bare", "--template=", "--", c.repo()); err != nil {
		return cache{}, err
	}
	// Replace init's platform-specific defaults with an exact, auditable config.
	// No remote, helper, include, hook, filter, or token is ever persisted.
	config := filepath.Join(c.repo(), "config")
	info, err := os.Lstat(config)
	if err != nil || !info.Mode().IsRegular() {
		return cache{}, unsafeOwnership("new bare repository config is invalid")
	}
	if err := os.WriteFile(config, []byte(bareConfig), 0600); err != nil {
		return cache{}, err
	}
	if err := m.validateCache(ctx, c); err != nil {
		return cache{}, err
	}
	c.Initialized = true
	m.mu.Lock()
	m.data.Caches[c.ID] = c
	m.mu.Unlock()
	if err := m.persist(); err != nil {
		return cache{}, err
	}
	return c, nil
}

func (m *Manager) validateCache(ctx context.Context, c cache) error {
	if err := m.namespace(c); err != nil {
		return err
	}
	if err := ownedDirectory(c.repo()); err != nil {
		return unsafeOwnership("bare repository path is not owned")
	}
	// A replaced objects/refs/worktrees directory must never redirect writes or
	// removals into another checkout. Git's own metadata contains no symlinks.
	if err := filepath.WalkDir(c.repo(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return unsafeOwnership("bare repository metadata contains a symlink")
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return unsafeOwnership("bare repository metadata contains a special file")
		}
		return nil
	}); err != nil {
		return err
	}
	config, err := os.ReadFile(filepath.Join(c.repo(), "config"))
	if err != nil || string(config) != bareConfig {
		return unsafeOwnership("bare repository configuration changed")
	}
	if _, err := os.Lstat(filepath.Join(c.repo(), "config.worktree")); !errors.Is(err, os.ErrNotExist) {
		return unsafeOwnership("unexpected worktree configuration")
	}
	bare, err := m.git(ctx, c.repo(), "", "inspect", "rev-parse", "--is-bare-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(bare) != "true" {
		return unsafeOwnership("cache is not a bare repository")
	}
	common, err := m.git(ctx, c.repo(), "", "inspect", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if !samePath(strings.TrimSpace(common), c.repo()) {
		return unsafeOwnership("bare repository identity changed")
	}
	return nil
}

func (m *Manager) checkSlot(c cache, e entry) error {
	if e.CacheID != c.ID || e.Slot != filepath.Join(c.Namespace, "worktree-"+e.ID) || e.Path != filepath.Join(e.Slot, "tree") {
		return unsafeOwnership("checkout path is outside its recorded namespace")
	}
	if err := ownedDirectory(e.Slot); err != nil {
		return unsafeOwnership("checkout namespace was replaced or redirected")
	}
	return m.checkMarker(filepath.Join(e.Slot, markerName), c, &e)
}

type worktreeRecord struct {
	path, head     string
	detached, bare bool
}

func (m *Manager) registered(ctx context.Context, c cache, e entry) (bool, error) {
	text, err := m.git(ctx, c.repo(), "", "inspect", "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	count := 0
	var item worktreeRecord
	for _, field := range strings.Split(text, "\x00") {
		if field == "" {
			if samePath(item.path, e.Path) {
				count++
				if item.head != e.Head || !item.detached || item.bare {
					return false, unsafeOwnership("registered checkout HEAD or repository identity changed")
				}
			}
			item = worktreeRecord{}
			continue
		}
		switch {
		case strings.HasPrefix(field, "worktree "):
			item.path = strings.TrimPrefix(field, "worktree ")
		case strings.HasPrefix(field, "HEAD "):
			item.head = strings.TrimPrefix(field, "HEAD ")
		case field == "detached":
			item.detached = true
		case field == "bare":
			item.bare = true
		}
	}
	if count > 1 {
		return false, unsafeOwnership("checkout has duplicate registrations")
	}
	return count == 1, nil
}

func regularRead(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return "", unsafeOwnership("Git identity metadata is not a regular bounded file")
	}
	bytes, err := os.ReadFile(path)
	return string(bytes), err
}

// adminIdentity also works when an interrupted worktree add has registered the
// checkout but has not yet written the checkout's .git file or populated files.
func (m *Manager) adminIdentity(c cache, e entry) (string, error) {
	parent := filepath.Join(c.repo(), "worktrees")
	dirs, err := os.ReadDir(parent)
	if err != nil {
		return "", unsafeOwnership("worktree registration is missing")
	}
	found := ""
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		admin := filepath.Join(parent, dir.Name())
		backlink, err := regularRead(filepath.Join(admin, "gitdir"))
		if err != nil {
			return "", err
		}
		if !samePath(strings.TrimSpace(backlink), filepath.Join(e.Path, ".git")) {
			continue
		}
		if found != "" {
			return "", unsafeOwnership("ambiguous checkout registration")
		}
		head, err := regularRead(filepath.Join(admin, "HEAD"))
		if err != nil || strings.TrimSpace(head) != e.Head {
			return "", unsafeOwnership("registered detached HEAD changed")
		}
		common, err := regularRead(filepath.Join(admin, "commondir"))
		if err != nil || strings.TrimSpace(common) != "../.." {
			return "", unsafeOwnership("registered common directory changed")
		}
		if _, err := os.Lstat(filepath.Join(admin, "config.worktree")); !errors.Is(err, os.ErrNotExist) {
			return "", unsafeOwnership("unexpected per-worktree configuration")
		}
		found = admin
	}
	if found == "" {
		return "", unsafeOwnership("exact checkout registration is missing")
	}
	return found, nil
}

func (m *Manager) inspectEntry(ctx context.Context, c cache, e entry, clean bool) error {
	if err := m.validateCache(ctx, c); err != nil {
		return err
	}
	if err := m.checkSlot(c, e); err != nil {
		return err
	}
	info, err := os.Lstat(e.Path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return unsafeOwnership("checkout directory is missing or redirected")
	}
	resolved, err := canonical(e.Path)
	if err != nil || resolved != e.Path {
		return unsafeOwnership("checkout canonical location changed")
	}
	registered, err := m.registered(ctx, c, e)
	if err != nil {
		return err
	}
	if !registered {
		return unsafeOwnership("checkout is not registered in its owned repository")
	}
	admin, err := m.adminIdentity(c, e)
	if err != nil {
		return err
	}
	gitfile, err := regularRead(filepath.Join(e.Path, ".git"))
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(gitfile), "gitdir: ")
	if err != nil || !ok || !samePath(gitdir, admin) {
		return unsafeOwnership("checkout gitdir and journal disagree")
	}
	common, err := m.git(ctx, e.Path, "", "inspect", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if !samePath(strings.TrimSpace(common), c.repo()) {
		return unsafeOwnership("checkout common directory changed")
	}
	top, err := m.git(ctx, e.Path, "", "inspect", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if !samePath(strings.TrimSpace(top), e.Path) {
		return unsafeOwnership("checkout top-level directory changed")
	}
	head, err := m.git(ctx, e.Path, "", "inspect", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != e.Head {
		return unsafeOwnership("checkout HEAD changed")
	}
	if clean {
		return m.clean(ctx, e, true)
	}
	return nil
}

type localChangesError struct{ message string }

func (e *localChangesError) Error() string { return e.message }

func isLocalChanges(err error) bool {
	var changes *localChangesError
	return errors.As(err, &changes)
}

func (m *Manager) clean(ctx context.Context, e entry, includeIgnored bool) error {
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=all"}
	if includeIgnored {
		args = append(args, "--ignored=matching")
	}
	status, err := m.git(ctx, e.Path, "", "inspect", args...)
	if err != nil {
		return err
	}
	if status != "" {
		return &localChangesError{"workspace has local changes; preserve them manually or explicitly Finish with discard=true"}
	}
	// Do not execute Git inside submodules: any populated gitlink is retained.
	index, err := m.git(ctx, e.Path, "", "inspect", "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	for _, row := range strings.Split(index, "\x00") {
		if !strings.HasPrefix(row, "160000 ") {
			continue
		}
		_, path, ok := strings.Cut(row, "\t")
		// Index paths use '/'; IsLocal also rejects Windows volume, rooted and device names.
		if !ok || !filepath.IsLocal(filepath.FromSlash(path)) {
			return unsafeOwnership("invalid submodule path")
		}
		full := filepath.Join(e.Path, path)
		info, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return &localChangesError{"workspace contains a populated or changed submodule; preserve it or explicitly discard it"}
		}
		children, err := os.ReadDir(full)
		if err != nil {
			return err
		}
		if len(children) != 0 {
			return &localChangesError{"workspace contains a populated submodule; preserve it or explicitly discard it"}
		}
	}
	return nil
}

func (m *Manager) remove(ctx context.Context, c cache, e entry, discard bool) error {
	if err := m.validateCache(ctx, c); err != nil {
		return err
	}
	registered, err := m.registered(ctx, c, e)
	if err != nil {
		return err
	}
	// An intent journaled before mkdir has nothing to remove. No marker is
	// needed to conclude absence, but it is required before any filesystem work.
	if _, err := os.Lstat(e.Slot); errors.Is(err, os.ErrNotExist) && !registered {
		return nil
	}
	if err := m.checkSlot(c, e); err != nil {
		return err
	}
	info, statErr := os.Lstat(e.Path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if statErr == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return unsafeOwnership("checkout path is not the owned directory")
	}
	if registered {
		if _, err := m.adminIdentity(c, e); err != nil {
			return err
		}
		if statErr == nil {
			if _, err := os.Lstat(filepath.Join(e.Path, ".git")); errors.Is(err, os.ErrNotExist) {
				// Incomplete add: only explicit discard can remove populated files.
				children, err := os.ReadDir(e.Path)
				if err != nil {
					return err
				}
				if len(children) != 0 && !discard {
					return &localChangesError{"interrupted owned workspace contains files; preserve them or explicitly Finish with discard=true"}
				}
			} else if err := m.inspectEntry(ctx, c, e, !discard); err != nil {
				return err
			}
		}
		args := []string{"worktree", "remove"}
		if discard {
			args = append(args, "--force")
		}
		args = append(args, "--", e.Path)
		if _, err := m.git(ctx, c.repo(), "", "cleanup", args...); err != nil {
			return err
		}
	} else if statErr == nil {
		// No Git registration means there is no authority for recursive removal,
		// even with discard. Only a demonstrably empty allocated directory is safe.
		if err := os.Remove(e.Path); err != nil {
			return unsafeOwnership("unregistered checkout contains files; preserve and recover it manually")
		}
	}
	children, err := os.ReadDir(e.Slot)
	if err != nil {
		return err
	}
	if len(children) != 1 || children[0].Name() != markerName {
		return unsafeOwnership("checkout namespace contains unexpected files")
	}
	// Retain the tiny ownership tombstone. If the process stops after Git removes
	// the checkout but before the journal commit, restart can still prove that
	// the missing worktree is the same owned entry and complete idempotently.
	return filelock.SyncDir(e.Slot)
}
