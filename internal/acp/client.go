// Package acp implements the ACP v1 newline-delimited JSON-RPC stdio client.
// Model selection and authentication are configured in the agent invocation,
// not through protocol model/configuration or interactive authentication APIs.
// Empty client capabilities disable client-provided filesystem/terminal methods;
// they do not sandbox the agent or prevent its own tools from editing files.
// Protocol references:
// https://agentclientprotocol.com/protocol/v1/initialization
// https://agentclientprotocol.com/protocol/v1/session-setup
// https://agentclientprotocol.com/protocol/v1/prompt-turn
// https://agentclientprotocol.com/protocol/v1/tool-calls
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"skim/internal/domain"
)

type outstandingPermission struct {
	id    string
	rpcID json.RawMessage
}

type turn struct {
	client    *Client
	transport *transport
	ctx       context.Context
	sessionID string
	scope     PermissionScope
	prompting bool
	decisions chan decision
	pending   map[string]outstandingPermission
	redactor  *strings.Replacer
	output    strings.Builder
}

// Run creates a fresh agent process/session and sends the caller's complete
// prompt, including any history. Verification is bounded to five minutes or
// the caller's earlier deadline. No permissions are automatically approved.
// ValidateInvocation checks the configured agent command without running it, so callers
// can refuse verification before creating any managed Git state.
func ValidateInvocation(settings domain.ACPSettings) error {
	command := strings.TrimSpace(settings.Command)
	if command == "" {
		return errors.New("configure an ACP agent command and arguments before verification")
	}
	// A relative path containing a separator resolves against the checkout at start
	// time. Windows accepts '/' as well as filepath.Separator.
	if filepath.IsAbs(command) || !strings.ContainsAny(command, "/"+string(filepath.Separator)) {
		if _, err := exec.LookPath(settings.Command); err != nil {
			return errors.New("cannot start ACP agent; check command, arguments, executable permissions, and PATH")
		}
	}
	return nil
}

// Run verifies one prompt. onStart, if not nil, is called with the agent's process
// group right after the process starts and before any protocol message is sent; its
// failure kills and drains the agent before Run returns. Run returns only after the
// whole process group has stopped.
func (c *Client) Run(ctx context.Context, settings domain.ACPSettings, directory, headSHA, prompt string, scope PermissionScope, onStart func(group int) error) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	if err := ValidateInvocation(settings); err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("ACP verification prompt must not be empty")
	}
	if len(prompt) > maxPromptBytes {
		return "", errors.New("ACP verification prompt exceeds the 2 MiB input limit")
	}
	if err := verifyWorkspace(ctx, directory, headSHA); err != nil {
		return "", fmt.Errorf("ACP workspace verification failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("ACP verification stopped: %w", err)
	}
	transport, err := startTransport(settings, directory, onStart)
	if err != nil {
		return "", err
	}
	t := &turn{
		client: c, transport: transport, ctx: ctx, scope: scope,
		decisions: make(chan decision, maxPendingPermissions),
		pending:   make(map[string]outstandingPermission),
		redactor:  environmentRedactor(),
	}
	result, err := t.run(directory, prompt)
	if ctx.Err() != nil {
		err = fmt.Errorf("ACP verification stopped: %w", ctx.Err())
	}
	if shutdownErr := t.shutdown(err != nil); err == nil {
		err = shutdownErr
	}
	if err != nil {
		if transport.stderr.bytes.Load() > 0 {
			err = fmt.Errorf("%w; agent stderr was omitted to protect credentials (check agent login and configuration locally)", err)
		}
		return "", err
	}
	return result, nil
}

func (t *turn) run(directory, prompt string) (string, error) {
	if err := t.transport.request(t.ctx, 1, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	}); err != nil {
		return "", err
	}
	result, err := t.await(1, "initialize")
	if err != nil {
		return "", err
	}
	var initialization struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if json.Unmarshal(result, &initialization) != nil || initialization.ProtocolVersion != 1 {
		return "", errors.New("ACP agent did not negotiate protocol version 1; use a compatible ACP agent")
	}
	if err := t.transport.request(t.ctx, 2, "session/new", map[string]any{
		"cwd": directory, "mcpServers": []any{},
	}); err != nil {
		return "", err
	}
	result, err = t.await(2, "session/new")
	if err != nil {
		return "", err
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(result, &session) != nil || strings.TrimSpace(session.SessionID) == "" {
		return "", errors.New("ACP agent returned an invalid or empty session ID")
	}
	t.sessionID = session.SessionID
	t.prompting = true
	if err := t.transport.request(t.ctx, 3, "session/prompt", map[string]any{
		"sessionId": t.sessionID,
		"prompt":    []map[string]string{{"type": "text", "text": prompt}},
	}); err != nil {
		return "", err
	}
	result, err = t.await(3, "session/prompt")
	if err != nil {
		return "", err
	}
	t.prompting = false
	var completed struct {
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(result, &completed) != nil {
		return "", errors.New("ACP agent returned an invalid prompt result")
	}
	if completed.StopReason != "end_turn" {
		switch completed.StopReason {
		case "max_tokens":
			return "", errors.New("ACP verification stopped at the agent token limit; adjust the agent configuration")
		case "max_turn_requests":
			return "", errors.New("ACP verification stopped at the agent request limit; adjust the agent configuration")
		case "refusal":
			return "", errors.New("ACP agent refused the verification request")
		case "cancelled":
			return "", errors.New("ACP agent cancelled verification before completion")
		default:
			return "", errors.New("ACP agent returned a missing or unsupported stop reason")
		}
	}
	if len(t.pending) != 0 {
		return "", errors.New("ACP agent ended verification with unresolved permission requests")
	}
	if strings.TrimSpace(t.output.String()) == "" {
		return "", errors.New("ACP agent completed without a text verification response")
	}
	return t.output.String(), nil
}

func (t *turn) await(id int, method string) (json.RawMessage, error) {
	key := "n:" + strconv.Itoa(id)
	for {
		if err := t.ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-t.ctx.Done():
			return nil, t.ctx.Err()
		case err := <-t.transport.writeErrors:
			return nil, err
		case decision := <-t.decisions:
			pending, ok := t.pending[decision.key]
			if !ok {
				continue
			}
			if err := t.permissionResult(t.ctx, pending.rpcID, decision.optionID); err != nil {
				return nil, err
			}
			delete(t.pending, decision.key)
		case event, ok := <-t.transport.events:
			if !ok || errors.Is(event.err, io.EOF) {
				return nil, errors.New("ACP agent exited before completing verification; check native ACP stdio mode and agent authentication")
			}
			if event.err != nil {
				return nil, event.err
			}
			message := event.message
			if message.Method != "" {
				if err := t.handle(message); err != nil {
					return nil, err
				}
				continue
			}
			responseKey, _ := requestKey(message.ID)
			if responseKey != key {
				return nil, errors.New("ACP agent returned a response for an unexpected request ID")
			}
			if message.Error != nil {
				return nil, agentError(method, message.Error)
			}
			return message.Result, nil
		}
	}
}

func agentError(method string, response *rpcError) error {
	// Neither message nor data is surfaced: agents may embed auth headers,
	// environment values, arguments, or remote response bodies in these fields.
	if response.Code == -32000 {
		return errors.New("ACP agent requires authentication; log in with the configured agent locally or configure its required credential environment variables")
	}
	return fmt.Errorf("ACP %s failed with JSON-RPC error %d; check agent authentication, arguments, and configuration locally", method, response.Code)
}

func (t *turn) handle(message envelope) error {
	if len(message.ID) != 0 {
		if message.Method == "session/request_permission" {
			return t.requestPermission(message)
		}
		return t.transport.reject(t.ctx, message.ID, -32601, "Method not found")
	}
	if message.Method != "session/update" {
		return nil
	}
	var params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
		} `json:"update"`
	}
	if json.Unmarshal(message.Params, &params) != nil || params.SessionID == "" || params.Update.SessionUpdate == "" {
		return errors.New("ACP agent sent an invalid session/update notification")
	}
	if params.SessionID != t.sessionID || !t.prompting || params.Update.SessionUpdate != "agent_message_chunk" {
		return nil
	}
	var content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(params.Update.Content, &content) != nil || content.Type == "" {
		return errors.New("ACP agent sent an invalid message content block")
	}
	if content.Type != "text" {
		return nil
	}
	if len(content.Text) > maxResponseBytes-t.output.Len() {
		return errors.New("ACP agent exceeded the 4 MiB text response limit")
	}
	t.output.WriteString(content.Text)
	return nil
}

func (t *turn) requestPermission(message envelope) error {
	var params struct {
		SessionID string             `json:"sessionId"`
		ToolCall  permissionToolCall `json:"toolCall"`
		Options   []PermissionOption `json:"options"`
	}
	if json.Unmarshal(message.Params, &params) != nil || params.SessionID != t.sessionID || !t.prompting || params.ToolCall.ToolCallID == "" || len(params.Options) == 0 || len(params.Options) > 64 {
		return t.transport.reject(t.ctx, message.ID, -32602, "Invalid permission request or session")
	}
	options := make(map[string]bool, len(params.Options))
	for _, option := range params.Options {
		if option.OptionID == "" || strings.TrimSpace(option.Name) == "" || options[option.OptionID] {
			return t.transport.reject(t.ctx, message.ID, -32602, "Invalid permission options")
		}
		switch option.Kind {
		case "allow_once", "allow_always", "reject_once", "reject_always":
		default:
			return t.transport.reject(t.ctx, message.ID, -32602, "Invalid permission option kind")
		}
		options[option.OptionID] = true
	}
	key, _ := requestKey(message.ID)
	if _, duplicate := t.pending[key]; duplicate {
		return errors.New("ACP agent reused an outstanding permission request ID")
	}
	if len(t.pending) >= maxPendingPermissions {
		_ = t.permissionResult(t.ctx, message.ID, "")
		return errors.New("ACP agent exceeded the limit of 64 pending permission requests")
	}
	title := params.ToolCall.Title
	if strings.TrimSpace(title) == "" {
		title = params.ToolCall.ToolCallID
	}
	for index := range params.Options {
		params.Options[index].Name = safeLabel(t.redactor, params.Options[index].Name)
	}
	id := t.client.offer(t, key, safeLabel(t.redactor, title), permissionDetails(t.redactor, params.ToolCall), params.Options)
	t.pending[key] = outstandingPermission{id: id, rpcID: message.ID}
	return nil
}

func (t *turn) permissionResult(ctx context.Context, id json.RawMessage, optionID string) error {
	outcome := map[string]string{"outcome": "cancelled"}
	if optionID != "" {
		outcome = map[string]string{"outcome": "selected", "optionId": optionID}
	}
	return t.transport.result(ctx, id, map[string]any{"outcome": outcome})
}

func (t *turn) shutdown(cancelled bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if cancelled && t.sessionID != "" {
		_ = t.transport.send(ctx, map[string]any{
			"jsonrpc": "2.0", "method": "session/cancel",
			"params": map[string]string{"sessionId": t.sessionID},
		})
	}
	for key, pending := range t.pending {
		t.client.remove(pending.id)
		_ = t.permissionResult(ctx, pending.rpcID, "")
		delete(t.pending, key)
	}
	if cancelled && t.prompting {
		// Continue consuming interleaved notifications until cancellation is
		// acknowledged. Late permission requests are cancelled, never approved.
		for {
			select {
			case <-ctx.Done():
				goto closed
			case <-t.transport.writeErrors:
				goto closed
			case event, ok := <-t.transport.events:
				if !ok || event.err != nil {
					goto closed
				}
				message := event.message
				if len(message.ID) == 0 {
					continue
				}
				if message.Method == "session/request_permission" {
					_ = t.permissionResult(ctx, message.ID, "")
				} else if message.Method != "" {
					_ = t.transport.reject(ctx, message.ID, -32601, "Method not found")
				} else if key, _ := requestKey(message.ID); key == "n:3" {
					goto closed
				}
			}
		}
	}
closed:
	return t.transport.close(ctx.Done())
}
