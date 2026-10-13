package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"skim/internal/domain"
	"skim/internal/filelock"
)

// Publication is written before a remote review is sent. An uncertain delivery
// stays pending across restarts and must be reconciled, never blindly retried.
type Publication struct {
	Marker     string   `json:"marker"`
	CommentIDs []string `json:"commentIds"`
	Event      string   `json:"event"`
	Body       string   `json:"body"`
	HeadSHA    string   `json:"headSha"`
}
type Record struct {
	State            domain.State `json:"state"`
	Pending          *Publication `json:"pending,omitempty"`
	AnalysisSnapshot string       `json:"analysisSnapshot,omitempty"`
	RiskConfig       string       `json:"riskConfig,omitempty"`
	GroupingConfig   string       `json:"groupingConfig,omitempty"`
}
type Data struct {
	Version      int                    `json:"version"`
	Revision     uint64                 `json:"revision"`
	Active       string                 `json:"active"`
	Settings     domain.Settings        `json:"settings"`
	Style        string                 `json:"style"`
	Reviews      map[string]Record      `json:"reviews"`
	ProviderKeys map[string]providerKey `json:"providerKeys,omitempty"`
	GitHubToken  string                 `json:"githubToken,omitempty"`
	storagePath  string
}
type Store struct {
	mu             sync.RWMutex
	path           string
	lock           *os.File
	data           Data
	commitRevision uint64
}

func Open(dir string, defaults domain.Settings) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("resolve data directory")
	}
	dir = absolute
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, fmt.Errorf("protect data directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = filelock.TryLock(lock, true); err != nil {
		lock.Close()
		return nil, errors.New("data directory is already used by another skim process")
	}
	s := &Store{path: filepath.Join(dir, "state.json"), lock: lock, data: Data{Version: 1, Settings: defaults, Reviews: map[string]Record{}}}
	bytes, err := os.ReadFile(s.path)
	migratedComments := false
	if err == nil {
		// Saved settings are authoritative, including deliberately empty providers/roles.
		s.data = Data{}
		if err = json.Unmarshal(bytes, &s.data); err != nil {
			s.Close()
			return nil, errors.New("saved state is invalid JSON")
		}
		if s.data.Version != 1 || s.data.Reviews == nil {
			s.Close()
			return nil, errors.New("unsupported or invalid saved state")
		}
		for key, r := range s.data.Reviews {
			for i, c := range r.State.Comments {
				switch c.Status {
				case "draft":
					c.Status = "suggested"
					if c.Source == "manual" {
						c.Status = "pending"
					}
					c.Revision++
				case "approved":
					c.Status = "pending"
					c.Revision++
				case "denied":
					c = domain.Comment{ID: c.ID, Revision: c.Revision + 1, Status: "deleted"}
				default:
					continue
				}
				r.State.Comments[i] = c
				migratedComments = true
			}
			if r.State.Analysis == nil {
				// Legacy Jev groups are not text-model review recommendations.
				r.State.Groups = []domain.Group{}
			} else {
				for _, task := range []*domain.AnalysisTask{&r.State.Analysis.Risk, &r.State.Analysis.Grouping} {
					if task.Status == "running" {
						task.Status = "cancelled"
						task.Error = "Analysis interrupted; reload the pull request to resume."
					}
				}
			}
			for i := range r.State.Chats {
				t := &r.State.Chats[i]
				if t.Verification != nil && t.Verification.Active() {
					t.Verification.Status = "failed"
					t.Verification.Error = "Verification interrupted by a server restart; submit a new turn to run it again."
					t.Verification.UpdatedAt = domain.Now()
					t.Verification.FinishedAt = t.Verification.UpdatedAt
					t.Revision++
					t.Verification.Revision = t.Revision
					migratedComments = true
				}
			}
			s.data.Reviews[key] = r
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Close()
		return nil, err
	}
	s.data.storagePath = s.path
	// Legacy workingDirectory was a user-owned checkout, not a managed root.
	// It is intentionally ignored by JSON decoding and is never adopted/deleted.
	if s.data.Settings.ACP.ManagedRoot == "" {
		s.data.Settings.ACP.ManagedRoot = filepath.Join(dir, "workspaces")
	}
	for i := range s.data.Settings.Providers {
		p := &s.data.Settings.Providers[i]
		if p.Name == "" {
			p.Name = p.ID
		}
	}
	if err == nil {
		if err := os.Chmod(s.path, 0600); err != nil {
			s.Close()
			return nil, fmt.Errorf("protect state file: %w", err)
		}
	}
	if migratedComments {
		if err := s.commitLocked(s.data); err != nil {
			s.Close()
			return nil, fmt.Errorf("persist comment migration: %w", err)
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.lock.Close() }

func (s *Store) Directory() string { return filepath.Dir(s.path) }

// Snapshot returns a detached snapshot; callers may mutate it before Commit.
func (s *Store) Snapshot() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := cloneData(s.data)
	d.Settings = d.runtimeSettings()
	return d
}

// State and Settings copy only the data needed by read-only HTTP routes.
func (s *Store) State() domain.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := domain.EmptyState()
	if r, ok := s.data.Reviews[s.data.Active]; ok {
		state = cloneState(r.State)
	}
	state.Revision = s.data.Revision
	state.Settings = s.data.runtimeSettings()
	state.Style = s.data.Style
	return state
}
func (s *Store) Settings() domain.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.runtimeSettings()
}

// PullRequest returns metadata without copying the active review's diffs.
func (s *Store) PullRequest() *domain.PullRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.data.Reviews[s.data.Active]
	if !ok || r.State.PullRequest == nil {
		return nil
	}
	pr := *r.State.PullRequest
	return &pr
}

// Workspace copies only active identity and completion metadata for polling.
func (s *Store) Workspace() domain.WorkspaceUpdate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := domain.WorkspaceUpdate{}
	if r, ok := s.data.Reviews[s.data.Active]; ok && r.State.PullRequest != nil {
		pr := r.State.PullRequest
		result.PRIdentity = domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}
		result.Workspace = r.State.Workspace
	}
	result.Workspace.Root = s.data.Settings.ACP.ManagedRoot
	return result
}

// Analysis copies only the polling resource, not patches or editable review work.
func (s *Store) Analysis() domain.AnalysisUpdate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := domain.AnalysisUpdate{Files: []domain.FileRisk{}, Groups: []domain.Group{}}
	if r, ok := s.data.Reviews[s.data.Active]; ok {
		result.Analysis = cloneAnalysis(r.State.Analysis)
		result.Files = make([]domain.FileRisk, len(r.State.Files))
		for i, f := range r.State.Files {
			result.Files[i].Path = f.Path
			result.Files[i].Risk = cloneRisk(f.Risk)
			if f.TestOnly != nil {
				testOnly := *f.TestOnly
				result.Files[i].TestOnly = &testOnly
			}
		}
		result.Groups = cloneGroups(r.State.Groups)
	}
	return result
}
func cloneRisk(r *domain.Risk) *domain.Risk {
	if r == nil {
		return nil
	}
	risk := *r
	risk.Factors = slices.Clone(r.Factors)
	return &risk
}
func cloneAnalysis(a *domain.Analysis) *domain.Analysis {
	if a == nil {
		return nil
	}
	result := *a
	result.Risk.SkippedFiles = slices.Clone(a.Risk.SkippedFiles)
	result.Risk.FailedFiles = slices.Clone(a.Risk.FailedFiles)
	result.Grouping.SkippedFiles = slices.Clone(a.Grouping.SkippedFiles)
	result.Grouping.FailedFiles = slices.Clone(a.Grouping.FailedFiles)
	return &result
}
func cloneGroups(groups []domain.Group) []domain.Group {
	result := slices.Clone(groups)
	for i := range result {
		result[i].Paths = slices.Clone(result[i].Paths)
	}
	return result
}
func cloneSettings(s domain.Settings) domain.Settings {
	s.Providers = slices.Clone(s.Providers)
	s.ACP.Args = slices.Clone(s.ACP.Args)
	return s
}
func cloneState(s domain.State) domain.State {
	if s.PullRequest != nil {
		p := *s.PullRequest
		s.PullRequest = &p
	}
	s.Files = slices.Clone(s.Files)
	for i := range s.Files {
		s.Files[i].Risk = cloneRisk(s.Files[i].Risk)
		if s.Files[i].TestOnly != nil {
			testOnly := *s.Files[i].TestOnly
			s.Files[i].TestOnly = &testOnly
		}
	}
	s.Groups = cloneGroups(s.Groups)
	s.Analysis = cloneAnalysis(s.Analysis)
	if s.FileReview != nil {
		review := *s.FileReview
		review.ReviewedPaths = slices.Clone(review.ReviewedPaths)
		s.FileReview = &review
	}
	s.Comments = slices.Clone(s.Comments)
	s.Chats = slices.Clone(s.Chats)
	for i := range s.Chats {
		s.Chats[i].Messages = slices.Clone(s.Chats[i].Messages)
		if s.Chats[i].Verification != nil {
			job := *s.Chats[i].Verification
			if job.Workspace != nil {
				workspace := *job.Workspace
				job.Workspace = &workspace
			}
			s.Chats[i].Verification = &job
		}
	}
	s.Settings = cloneSettings(s.Settings)
	if s.ReviewRun != nil {
		run := *s.ReviewRun
		run.Rejected = slices.Clone(run.Rejected)
		s.ReviewRun = &run
	}
	return s
}
func cloneData(d Data) Data {
	result := d
	result.Settings = cloneSettings(d.Settings)
	result.ProviderKeys = maps.Clone(d.ProviderKeys)
	result.Reviews = make(map[string]Record, len(d.Reviews))
	for key, r := range d.Reviews {
		r.State = cloneState(r.State)
		if r.Pending != nil {
			p := *r.Pending
			p.CommentIDs = slices.Clone(p.CommentIDs)
			r.Pending = &p
		}
		result.Reviews[key] = r
	}
	return result
}
func (s *Store) Commit(d Data) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Foreground actions may have waited on an external model since Snapshot.
	// Carry forward newer, independently committed analysis for that exact run.
	for key, r := range d.Reviews {
		current, ok := s.data.Reviews[key]
		if !ok || r.State.Analysis == nil || current.State.Analysis == nil || r.AnalysisSnapshot != current.AnalysisSnapshot || r.State.Analysis.Generation != current.State.Analysis.Generation || r.State.Analysis.Revision >= current.State.Analysis.Revision {
			continue
		}
		r.State.Analysis = current.State.Analysis
		r.State.Groups = current.State.Groups
		for i := range r.State.Files {
			r.State.Files[i].Risk = current.State.Files[i].Risk
			r.State.Files[i].TestOnly = current.State.Files[i].TestOnly
		}
		d.Reviews[key] = r
	}
	return s.commitLocked(d)
}

// UpdateAnalysis serializes only the scoped commit, independently of foreground
// model/ACP work. Snapshot and generation must still identify the active review.
func (s *Store) UpdateAnalysis(ctx context.Context, identity domain.Analysis, snapshot string, update func(*Record)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	r, ok := s.data.Reviews[identity.Key]
	if !ok || s.data.Active != identity.Key || r.AnalysisSnapshot != snapshot || r.State.PullRequest == nil || r.State.PullRequest.BaseSHA != identity.BaseSHA || r.State.PullRequest.HeadSHA != identity.HeadSHA || r.State.Analysis == nil || r.State.Analysis.Generation != identity.Generation {
		return context.Canceled
	}
	d := s.data
	d.Reviews = maps.Clone(s.data.Reviews)
	r.State.Analysis = cloneAnalysis(r.State.Analysis)
	r.State.Files = slices.Clone(r.State.Files)
	update(&r)
	r.State.Analysis.Revision++
	d.Reviews[identity.Key] = r
	if err := s.commitLocked(d); err != nil {
		// Disk failure cannot itself be persisted. Preserve the last committed
		// results, but expose a terminal in-memory error instead of polling forever.
		current := s.data.Reviews[identity.Key]
		current.State.Analysis = cloneAnalysis(current.State.Analysis)
		for _, task := range []*domain.AnalysisTask{&current.State.Analysis.Risk, &current.State.Analysis.Grouping} {
			if task.Status == "running" {
				task.Status = "failed"
				task.Error = "Could not persist analysis: " + err.Error()
			}
		}
		current.State.Analysis.Revision++
		s.data.Reviews[identity.Key] = current
		return err
	}
	return nil
}

func (s *Store) commitLocked(d Data) error {
	d.Revision = s.data.Revision + 1
	b, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return fmt.Errorf("create state file: %w", err)
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(temp, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	// Rename has committed the new snapshot. Keep memory consistent even if the
	// directory fsync fails, and report durability uncertainty to the caller.
	s.data = cloneData(d)
	s.commitRevision++
	if err = filelock.SyncDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("state replaced but directory sync failed: %w", err)
	}
	return nil
}
func (d Data) State() domain.State {
	s := domain.EmptyState()
	if r, ok := d.Reviews[d.Active]; ok {
		s = r.State
	}
	s.Revision = d.Revision
	s.Settings = d.runtimeSettings()
	s.Style = d.Style
	return s
}
