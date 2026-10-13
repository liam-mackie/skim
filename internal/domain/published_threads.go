package domain

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PublishedComment is remote discussion, never a local review draft. IDs stay
// decimal strings because GitHub IDs can exceed JavaScript's integer precision.
type PublishedComment struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	URL       string `json:"url"`
}

type ReviewThread struct {
	ID                  string             `json:"id"`
	Path                string             `json:"path"`
	Side                string             `json:"side"`
	StartLine           int                `json:"startLine,omitempty"`
	EndLine             int                `json:"endLine,omitempty"`
	Outdated            bool               `json:"outdated"`
	CanReply            bool               `json:"canReply"`
	ReplyDisabledReason string             `json:"replyDisabledReason,omitempty"`
	DiffHunk            string             `json:"diffHunk,omitempty"`
	Comments            []PublishedComment `json:"comments"`
}

type ReviewThreads struct {
	PRIdentity
	Threads  []ReviewThread `json:"threads"`
	Warnings []string       `json:"warnings"`
}

// Attachments maps GitHub image asset UUIDs to pre-signed URLs that expire
// after about five minutes. Clients fetch them again when an image fails.
type Attachments struct {
	PRIdentity
	URLs map[string]string `json:"urls"`
}

type ReviewReply struct {
	PRIdentity
	ThreadID string `json:"threadId"`
	Body     string `json:"body"`
}

// Validate checks write fields without trimming or changing the composed text.
// The application separately binds PRIdentity to the active loaded snapshot.
func (r ReviewReply) Validate() error {
	id, err := strconv.ParseInt(r.ThreadID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != r.ThreadID {
		return fmt.Errorf("reply thread ID must be a positive canonical decimal integer")
	}
	if len(r.Body) > 60000 || strings.TrimSpace(r.Body) == "" || !utf8.ValidString(r.Body) || strings.ContainsRune(r.Body, 0) {
		return fmt.Errorf("reply body must contain 1–60000 bytes of valid UTF-8 text without NUL characters")
	}
	return nil
}
