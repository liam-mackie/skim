package acp

import (
	"errors"
	"sort"
	"sync"

	"skim/internal/domain"
)

// Permission is an agent request awaiting an explicit user decision.
type Permission struct {
	PermissionScope
	ID        string             `json:"id"`
	SessionID string             `json:"sessionId"`
	Title     string             `json:"title"`
	Details   string             `json:"details,omitempty"`
	Options   []PermissionOption `json:"options"`
}

// PermissionScope is captured by the caller for one turn, never inferred from
// agent-provided session identifiers or the currently selected pull request.
type PermissionScope struct {
	JobID    string `json:"jobId,omitempty"`
	ThreadID string `json:"threadId,omitempty"`
	PRKey    string `json:"prKey,omitempty"`
	PRURL    string `json:"prUrl,omitempty"`
	Path     string `json:"path,omitempty"`
	Question string `json:"question,omitempty"`
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// Client supports concurrent runs and permission polling/resolution.
// Use New or the zero value; do not copy a Client after first use.
type Client struct {
	mu      sync.Mutex
	pending map[string]*pendingPermission
}

type pendingPermission struct {
	permission Permission
	turn       *turn
	key        string
}

type decision struct {
	key      string
	optionID string
}

func New() *Client { return &Client{pending: make(map[string]*pendingPermission)} }

func (c *Client) Permissions() []Permission {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]Permission, 0, len(c.pending))
	for _, pending := range c.pending {
		if pending.turn.ctx.Err() != nil {
			continue
		}
		permission := pending.permission
		permission.Options = append([]PermissionOption(nil), permission.Options...)
		result = append(result, permission)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (c *Client) Resolve(id, optionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, ok := c.pending[id]
	if !ok {
		return errors.New("ACP permission is unknown or has expired; refresh outstanding permissions")
	}
	if pending.turn.ctx.Err() != nil {
		delete(c.pending, id)
		return errors.New("ACP permission has expired because verification ended")
	}
	valid := false
	for _, option := range pending.permission.Options {
		if option.OptionID == optionID {
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("invalid ACP permission option; choose one of the advertised option IDs")
	}
	// At most maxPendingPermissions decisions can be outstanding per turn.
	// Removing the entry makes resolution single-use, including concurrent calls.
	pending.turn.decisions <- decision{key: pending.key, optionID: optionID}
	delete(c.pending, id)
	return nil
}

func (c *Client) offer(t *turn, key, title, details string, options []PermissionOption) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = make(map[string]*pendingPermission)
	}
	id := domain.NewID()
	c.pending[id] = &pendingPermission{
		permission: Permission{PermissionScope: t.scope, ID: id, SessionID: t.sessionID, Title: title, Details: details, Options: options},
		turn:       t, key: key,
	}
	return id
}

func (c *Client) remove(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}
