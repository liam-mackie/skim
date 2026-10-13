package domain

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type PullRequest struct {
	Provider string `json:"provider"`
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url"`
	Author   string `json:"author"`
	BaseSHA  string `json:"baseSha"`
	HeadSHA  string `json:"headSha"`
	BaseRef  string `json:"baseRef"`
	HeadRef  string `json:"headRef"`
}

// GitSource is provider-validated clone metadata, never user-supplied Git flags.
type GitSource struct {
	URL     string
	HeadRef string
}

type Workspace struct {
	ID                string `json:"id,omitempty"`
	Root              string `json:"root"`
	Path              string `json:"path,omitempty"`
	HeadSHA           string `json:"headSha,omitempty"`
	Status            string `json:"status"`
	Error             string `json:"error,omitempty"`
	FinishedAt        string `json:"finishedAt,omitempty"`
	CanDiscardChanges bool   `json:"canDiscardChanges,omitempty"`
	Count             int    `json:"count,omitempty"`
}

type WorkspaceUpdate struct {
	PRIdentity
	Workspace Workspace `json:"workspace"`
}

// Risk is the displayed impact level: medium/high only when Jev assigns at least
// the flag threshold to that level or above. Probability is that supporting mass.
// Factors stays nil until explained; an empty list means no supporting evidence.
type Risk struct {
	Level        string       `json:"level"`
	Probability  float64      `json:"probability"`
	Factors      []RiskFactor `json:"factors"`
	FactorsError string       `json:"factorsError,omitempty"`
}

// RiskFactor is a review-model observation anchored to one side of one hunk.
type RiskFactor struct {
	Text      string `json:"text"`
	Side      string `json:"side"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
}

// TestOnlyAssessment records Jev's choice and its actual model scores.
type TestOnlyAssessment struct {
	OnlyTests   bool    `json:"onlyTests"`
	Confidence  float64 `json:"confidence"`
	Probability float64 `json:"probability"`
}
type File struct {
	Path         string              `json:"path"`
	PreviousPath string              `json:"previousPath,omitempty"`
	Status       string              `json:"status"`
	Additions    int                 `json:"additions"`
	Deletions    int                 `json:"deletions"`
	Patch        string              `json:"patch"`
	PatchStatus  string              `json:"patchStatus"`
	Language     string              `json:"language,omitempty"`
	Risk         *Risk               `json:"risk,omitempty"`
	TestOnly     *TestOnlyAssessment `json:"testOnly,omitempty"`
}
type Group struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Summary string   `json:"summary,omitempty"`
	Paths   []string `json:"paths"`
}

// ManualComment binds a human-authored draft to the exact displayed PR snapshot.
type ManualComment struct {
	PRKey     string `json:"prKey"`
	BaseSHA   string `json:"baseSha"`
	HeadSHA   string `json:"headSha"`
	Path      string `json:"path"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Side      string `json:"side"`
	Body      string `json:"body"`
}

type Comment struct {
	ID        string `json:"id"`
	Revision  uint64 `json:"revision"`
	Path      string `json:"path"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Side      string `json:"side"`
	Body      string `json:"body"`
	Status    string `json:"status"`
	Source    string `json:"source"` // review, verification, or manual
	ThreadID  string `json:"threadId,omitempty"`
	CreatedAt string `json:"createdAt"`
	HeadSHA   string `json:"headSha"`
	Stale     bool   `json:"stale,omitempty"`
}

// Deleted comments retain only their identity and clock, so delayed snapshots
// cannot resurrect deleted content and persisted tombstones contain no evidence.
func (c Comment) MarshalJSON() ([]byte, error) {
	if c.Status == "deleted" {
		return json.Marshal(struct {
			ID       string `json:"id"`
			Revision uint64 `json:"revision"`
			Status   string `json:"status"`
		}{c.ID, c.Revision, c.Status})
	}
	type plain Comment
	return json.Marshal(plain(c))
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type ChatThread struct {
	ID           string           `json:"id"`
	Revision     uint64           `json:"revision"`
	Kind         string           `json:"kind"`
	Path         string           `json:"path,omitempty"`
	StartLine    int              `json:"startLine,omitempty"`
	EndLine      int              `json:"endLine,omitempty"`
	Side         string           `json:"side,omitempty"`
	Messages     []Message        `json:"messages"`
	HeadSHA      string           `json:"headSha"`
	Stale        bool             `json:"stale,omitempty"`
	Verification *VerificationJob `json:"verification,omitempty"`
}

// VerificationJob is the most recent server-owned turn of a verification thread.
// The thread retains the complete conversation, including earlier job reports.
type VerificationJob struct {
	ID               string     `json:"id"`
	Revision         uint64     `json:"revision"`
	ThreadID         string     `json:"threadId"`
	PRKey            string     `json:"prKey"`
	PRURL            string     `json:"prUrl"`
	BaseSHA          string     `json:"baseSha"`
	HeadSHA          string     `json:"headSha"`
	Path             string     `json:"path"`
	StartLine        int        `json:"startLine"`
	EndLine          int        `json:"endLine"`
	Side             string     `json:"side"`
	Question         string     `json:"question"`
	Status           string     `json:"status"`
	StartedAt        string     `json:"startedAt"`
	UpdatedAt        string     `json:"updatedAt"`
	FinishedAt       string     `json:"finishedAt,omitempty"`
	Error            string     `json:"error,omitempty"`
	SuggestionsError string     `json:"suggestionsError,omitempty"`
	Workspace        *Workspace `json:"workspace,omitempty"`
	// Cleared hides a finished job from Activity; its thread and report remain.
	Cleared bool `json:"cleared,omitempty"`
}

func (j VerificationJob) Active() bool {
	switch j.Status {
	case "queued", "preparing", "running", "summarizing", "cancelling":
		return true
	default:
		return false
	}
}

type VerificationAccepted struct {
	Job    VerificationJob `json:"job"`
	Thread ChatThread      `json:"thread"`
}

type VerificationUpdate struct {
	PRIdentity
	Threads  []ChatThread      `json:"threads"`
	Comments []Comment         `json:"comments"`
	Jobs     []VerificationJob `json:"jobs"`
}
type ModelProvider struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Kind             string `json:"kind,omitempty"`
	BaseURL          string `json:"baseUrl"`
	APIKeyEnv        string `json:"apiKeyEnv"`
	CredentialStatus string `json:"credentialStatus,omitempty"`
	// CredentialRevision identifies a private saved key without carrying its value.
	CredentialRevision string `json:"-"`
}
type ModelRole struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type Roles struct {
	Chat   ModelRole `json:"chat"`
	Review ModelRole `json:"review"`
	Jev    ModelRole `json:"jev"`
}
type ACPSettings struct {
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	ManagedRoot string   `json:"managedRoot"`
}
type Settings struct {
	GitHubTokenEnv string `json:"githubTokenEnv"`
	// GitHubTokenStatus is recomputed by the server: saved, environment or missing.
	GitHubTokenStatus string          `json:"githubTokenStatus,omitempty"`
	Providers         []ModelProvider `json:"providers"`
	Roles             Roles           `json:"roles"`
	ACP               ACPSettings     `json:"acp"`
}

// ProviderKeyUpdate is accepted only by the settings write endpoint.
type ProviderKeyUpdate struct {
	ProviderID string `json:"providerId"`
	APIKey     string `json:"apiKey,omitempty"`
	Remove     bool   `json:"remove,omitempty"`
}

// GitHubTokenUpdate is accepted only by the settings write endpoint. A blank
// token keeps the saved one; Remove falls back to githubTokenEnv.
type GitHubTokenUpdate struct {
	Token  string `json:"token,omitempty"`
	Remove bool   `json:"remove,omitempty"`
}

// SettingsUpdate is one settings save: settings, write-only secrets, and the
// writing style commit together.
type SettingsUpdate struct {
	Settings
	ProviderKeys []ProviderKeyUpdate `json:"providerKeys,omitempty"`
	GitHubToken  GitHubTokenUpdate   `json:"githubToken"`
	// Style replaces the writing style when present.
	Style *string `json:"style,omitempty"`
}
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ReviewRun is the last agent review of a PR: the agent's full report and any
// comments it proposed that could not be anchored to the diff.
type ReviewRun struct {
	HeadSHA   string            `json:"headSha"`
	CreatedAt string            `json:"createdAt"`
	Report    string            `json:"report"`
	Rejected  []RejectedComment `json:"rejected"`
	// Error is set when the report held no readable comments; the report is kept.
	Error string `json:"error,omitempty"`
}

// RejectedComment is an agent comment that failed diff validation.
type RejectedComment struct {
	Path      string `json:"path"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Side      string `json:"side"`
	Body      string `json:"body"`
	Reason    string `json:"reason"`
}
type AnalysisTask struct {
	Status       string        `json:"status"`
	Completed    int           `json:"completed"`
	Total        int           `json:"total"`
	Error        string        `json:"error,omitempty"`
	SkippedFiles []SkippedFile `json:"skippedFiles,omitempty"`
	FailedFiles  []SkippedFile `json:"failedFiles,omitempty"`
}
type Analysis struct {
	Key        string       `json:"key"`
	BaseSHA    string       `json:"baseSha"`
	HeadSHA    string       `json:"headSha"`
	Generation string       `json:"generation"`
	Revision   uint64       `json:"revision"`
	Risk       AnalysisTask `json:"risk"`
	Grouping   AnalysisTask `json:"grouping"`
}
type FileRisk struct {
	Path     string              `json:"path"`
	Risk     *Risk               `json:"risk,omitempty"`
	TestOnly *TestOnlyAssessment `json:"testOnly,omitempty"`
}
type AnalysisUpdate struct {
	Analysis *Analysis  `json:"analysis"`
	Files    []FileRisk `json:"files"`
	Groups   []Group    `json:"groups"`
}

// FileReview is local reading progress, independent of AI coverage or publication.
type FileReview struct {
	Revision      uint64   `json:"revision"`
	ReviewedPaths []string `json:"reviewedPaths"`
}

type FileReviewUpdate struct {
	PRIdentity
	FileReview FileReview `json:"fileReview"`
}

type State struct {
	Revision    uint64       `json:"revision"`
	PullRequest *PullRequest `json:"pullRequest"`
	Files       []File       `json:"files"`
	Groups      []Group      `json:"groups"`
	Comments    []Comment    `json:"comments"`
	Chats       []ChatThread `json:"chats"`
	Style       string       `json:"style"`
	Settings    Settings     `json:"settings"`
	ReviewRun   *ReviewRun   `json:"reviewRun,omitempty"`
	FileReview  *FileReview  `json:"fileReview,omitempty"`
	Analysis    *Analysis    `json:"analysis,omitempty"`
	Workspace   Workspace    `json:"workspace"`
}

func EmptyState() State {
	return State{Files: []File{}, Groups: []Group{}, Comments: []Comment{}, Chats: []ChatThread{}}
}
func (p PullRequest) Key() string {
	return fmt.Sprintf("%s/%s/%s/%d", p.Provider, p.Owner, p.Repo, p.Number)
}
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Now() string { return time.Now().UTC().Format(time.RFC3339) }
