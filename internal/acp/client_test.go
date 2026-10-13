package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"skim/internal/domain"
)

const helperPrompt = "Prior user: explain the issue\nPrior assistant: examine the caller\nVerify the selected lines without editing files."

func checkout(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	gitTest(t, directory, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(directory, "tracked.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, directory, "add", "tracked.txt")
	gitTest(t, directory, "-c", "user.name=ACP Test", "-c", "user.email=acp@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "initial")
	return directory, strings.TrimSpace(gitTest(t, directory, "rev-parse", "HEAD"))
}

func gitTest(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-C", directory}, args...)...)
	cmd.Env = workspaceGitEnvironment()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git failed: %v: %s", err, output)
	}
	return string(output)
}

func helperSettings(t *testing.T, mode, marker string) domain.ACPSettings {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return domain.ACPSettings{
		Command: executable,
		Args:    []string{"-test.run=^TestACPHelperProcess$", "--", "skim-acp-helper", mode, marker},
	}
}

type runResult struct {
	text string
	err  error
}

func startRun(client *Client, ctx context.Context, settings domain.ACPSettings, directory, sha string) <-chan runResult {
	result := make(chan runResult, 1)
	go func() {
		text, err := client.Run(ctx, settings, directory, sha, helperPrompt, PermissionScope{}, nil)
		result <- runResult{text, err}
	}()
	return result
}

func awaitPermissions(t *testing.T, client *Client, count int, results ...<-chan runResult) []Permission {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		permissions := client.Permissions()
		if len(permissions) == count {
			return permissions
		}
		for _, result := range results {
			select {
			case value := <-result:
				t.Fatalf("run ended before permission: %v", value.err)
			default:
			}
		}
		select {
		case <-timer.C:
			t.Fatal("timed out waiting for agent permission")
		case <-ticker.C:
		}
	}
}

func awaitRun(t *testing.T, result <-chan runResult) runResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("ACP run leaked or failed to stop")
		return runResult{}
	}
}

func TestRunHandshakeAndInterleavedPermissions(t *testing.T) {
	directory, sha := checkout(t)
	client := New()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result := startRun(client, ctx, helperSettings(t, "permissions", ""), directory, sha)
	permissions := awaitPermissions(t, client, 2, result)
	for _, permission := range permissions {
		if permission.SessionID != "session-test" || permission.Title != "Inspect tracked file" {
			t.Fatalf("unexpected permission: %+v", permission)
		}
		if !strings.Contains(permission.Details, "git show HEAD:tracked.txt") || !strings.Contains(permission.Details, "tracked.txt") {
			t.Fatalf("missing tool details: %+v", permission)
		}
		if err := client.Resolve(permission.ID, "invalid"); err == nil {
			t.Fatal("invalid option accepted")
		}
	}
	// Polling returns owned copies; callers cannot mutate the live option set.
	permissions[0].Options[0].OptionID = "mutated"
	if err := client.Resolve(permissions[0].ID, "mutated"); err == nil {
		t.Fatal("permission snapshot mutated live options")
	}
	for _, permission := range permissions {
		if err := client.Resolve(permission.ID, "reject"); err != nil {
			t.Fatal(err)
		}
		if err := client.Resolve(permission.ID, "reject"); err == nil {
			t.Fatal("permission resolved twice")
		}
	}
	value := awaitRun(t, result)
	if value.err != nil {
		t.Fatal(value.err)
	}
	if value.text != "Checked the caller. No edit was made." {
		t.Fatalf("incorrect collected content: %q", value.text)
	}
	if len(client.Permissions()) != 0 {
		t.Fatal("permissions retained after success")
	}
}

func TestInvalidPermissionsAreRejectedWithoutApproval(t *testing.T) {
	directory, sha := checkout(t)
	client := New()
	text, err := client.Run(context.Background(), helperSettings(t, "invalid-permissions", ""), directory, sha, helperPrompt, PermissionScope{}, nil)
	if err != nil || text == "" {
		t.Fatalf("invalid permission protocol failed: %q / %v", text, err)
	}
	if len(client.Permissions()) != 0 {
		t.Fatal("invalid requests became pending permissions")
	}
}

func TestConcurrentRunsAndResolution(t *testing.T) {
	directory, sha := checkout(t)
	client := New()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	settings := helperSettings(t, "one-permission", "")
	first := startRun(client, ctx, settings, directory, sha)
	second := startRun(client, ctx, settings, directory, sha)
	permissions := awaitPermissions(t, client, 2, first, second)
	if permissions[0].ID == permissions[1].ID {
		t.Fatal("colliding public IDs across independent sessions")
	}
	for _, permission := range permissions {
		var wg sync.WaitGroup
		errors := make(chan error, 2)
		for range 2 {
			wg.Add(1)
			go func(id string) { defer wg.Done(); errors <- client.Resolve(id, "reject") }(permission.ID)
		}
		wg.Wait()
		one, two := <-errors, <-errors
		if (one == nil) == (two == nil) {
			t.Fatalf("expected exactly one winner: %v / %v", one, two)
		}
	}
	if value := awaitRun(t, first); value.err != nil {
		t.Fatal(value.err)
	}
	if value := awaitRun(t, second); value.err != nil {
		t.Fatal(value.err)
	}
	if len(client.Permissions()) != 0 {
		t.Fatal("permissions retained after concurrent runs")
	}
}

func TestCancellationCancelsPendingAndLatePermissions(t *testing.T) {
	directory, sha := checkout(t)
	marker := filepath.Join(t.TempDir(), "cancel; $(not-a-shell-command).json")
	client := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := startRun(client, ctx, helperSettings(t, "cancel", marker), directory, sha)
	permission := awaitPermissions(t, client, 1, result)[0]
	cancel()
	value := awaitRun(t, result)
	if !errors.Is(value.err, context.Canceled) || value.text != "" {
		t.Fatalf("cancellation returned %+v", value)
	}
	if len(client.Permissions()) != 0 {
		t.Fatal("cancelled permission remains visible")
	}
	if err := client.Resolve(permission.ID, "allow"); err == nil {
		t.Fatal("expired permission accepted")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal("agent did not acknowledge cancellation:", err)
	}
	if string(data) != "cancel notification and both cancelled outcomes received" {
		t.Fatalf("bad cancellation handshake: %s", data)
	}
}

func TestDeadlineStopsUncooperativeAgent(t *testing.T) {
	directory, sha := checkout(t)
	client := New()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := startRun(client, ctx, helperSettings(t, "ignore-cancel", ""), directory, sha)
	awaitPermissions(t, client, 1, result)
	value := awaitRun(t, result)
	if !errors.Is(value.err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", value.err)
	}
	if len(client.Permissions()) != 0 {
		t.Fatal("permission survived forced process cleanup")
	}
}

func TestProtocolFailures(t *testing.T) {
	directory, sha := checkout(t)
	for _, test := range []struct{ mode, contains string }{
		{"version", "protocol version 1"},
		{"auth", "requires authentication"},
		{"empty-session", "empty session ID"},
		{"eof", "exited before completing"},
		{"malformed", "invalid JSON-RPC"},
		{"unterminated", "unterminated"},
		{"oversize", "oversized"},
		{"response-cap", "text response limit"},
		{"wire-cap", "protocol output limit"},
		{"unexpected-id", "unexpected request ID"},
		{"empty", "without a text"},
		{"max_tokens", "token limit"},
		{"max_turn_requests", "request limit"},
		{"refusal", "refused"},
		{"cancelled", "cancelled"},
		{"unknown-stop", "unsupported stop reason"},
		{"nonzero", "exited unsuccessfully"},
		{"duplicate-permission", "reused an outstanding"},
		{"permission-limit", "64 pending permission requests"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client := New()
			text, err := client.Run(ctx, helperSettings(t, test.mode, ""), directory, sha, helperPrompt, PermissionScope{}, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) || text != "" {
				t.Fatalf("got %q / %v; want %q", text, err, test.contains)
			}
			if len(client.Permissions()) != 0 {
				t.Fatal("permission retained after failed run")
			}
		})
	}
}

func TestAgentDiagnosticsDoNotExposeCredentials(t *testing.T) {
	directory, sha := checkout(t)
	secret := "do-not-expose-test-credential-987654321"
	t.Setenv("SKIM_ACP_TEST_SECRET", secret)
	client := New()
	_, err := client.Run(context.Background(), helperSettings(t, "secret-error", ""), directory, sha, helperPrompt, PermissionScope{}, nil)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "stderr was omitted") {
		t.Fatalf("unsafe or missing diagnostic: %v", err)
	}
}

func TestWorkspaceAndInvocationValidation(t *testing.T) {
	directory, sha := checkout(t)
	client := New()
	settings := helperSettings(t, "success", "")
	settings.ManagedRoot = t.TempDir() // The generated directory, not this root, is the runtime cwd.
	t.Run("generated directory", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "not-the-managed-checkout"))
		t.Setenv("GIT_WORK_TREE", t.TempDir())
		if text, err := client.Run(context.Background(), settings, directory, sha, helperPrompt, PermissionScope{}, nil); err != nil || text == "" {
			t.Fatalf("generated checkout was not used: %q / %v", text, err)
		}
	})
	t.Run("missing command", func(t *testing.T) {
		missing := settings
		missing.Command = ""
		if _, err := client.Run(context.Background(), missing, directory, sha, helperPrompt, PermissionScope{}, nil); err == nil || !strings.Contains(err.Error(), "configure") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		missing := settings
		missing.Command = filepath.Join(t.TempDir(), "not-installed")
		if _, err := client.Run(context.Background(), missing, directory, sha, helperPrompt, PermissionScope{}, nil); err == nil || !strings.Contains(err.Error(), "cannot start") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("relative directory", func(t *testing.T) {
		if _, err := client.Run(context.Background(), settings, ".", sha, helperPrompt, PermissionScope{}, nil); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("wrong head", func(t *testing.T) {
		if _, err := client.Run(context.Background(), settings, directory, strings.Repeat("0", 40), helperPrompt, PermissionScope{}, nil); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("untracked", func(t *testing.T) {
		path := filepath.Join(directory, "untracked.txt")
		if err := os.WriteFile(path, []byte("untracked"), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(path)
		if _, err := client.Run(context.Background(), settings, directory, sha, helperPrompt, PermissionScope{}, nil); err == nil || !strings.Contains(err.Error(), "tracked or untracked") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("tracked", func(t *testing.T) {
		path := filepath.Join(directory, "tracked.txt")
		if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(path, []byte("original\n"), 0600)
		if _, err := client.Run(context.Background(), settings, directory, sha, helperPrompt, PermissionScope{}, nil); err == nil || !strings.Contains(err.Error(), "tracked or untracked") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("git environment cannot redirect checkout", func(t *testing.T) {
		other, _ := checkout(t)
		gitTest(t, other, "-c", "user.name=ACP Test", "-c", "user.email=acp@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "--quiet", "-m", "different head")
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
		t.Setenv("GIT_WORK_TREE", other)
		if err := verifyWorkspace(context.Background(), directory, sha); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("prompt bounds", func(t *testing.T) {
		for _, prompt := range []string{" ", strings.Repeat("x", maxPromptBytes+1)} {
			if _, err := client.Run(context.Background(), settings, directory, sha, prompt, PermissionScope{}, nil); err == nil {
				t.Fatal("invalid prompt accepted")
			}
		}
	})
}

// This test becomes a real stdio agent only in a separate test process. os.Exit
// prevents the Go test runner's status text from corrupting the ACP wire stream.
func TestACPHelperProcess(t *testing.T) {
	index := -1
	for i, argument := range os.Args {
		if argument == "skim-acp-helper" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	if index+2 >= len(os.Args) {
		os.Exit(90)
	}
	runHelper(os.Args[index+1], os.Args[index+2])
	os.Exit(0)
}

type helperWire struct {
	reader  *bufio.Reader
	encoder *json.Encoder
}

func (h *helperWire) fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(91)
}

func (h *helperWire) read() envelope {
	line, err := h.reader.ReadBytes('\n')
	if err != nil {
		h.fail("read client message: %v", err)
	}
	var message envelope
	if json.Unmarshal(line, &message) != nil || message.JSONRPC != "2.0" {
		h.fail("invalid client message")
	}
	return message
}

func (h *helperWire) send(message any) {
	if err := h.encoder.Encode(message); err != nil {
		h.fail("write client message: %v", err)
	}
}

func (h *helperWire) response(id json.RawMessage, result any) {
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (h *helperWire) update(session, kind string, content any) {
	h.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": map[string]any{"sessionUpdate": kind, "content": content}}})
}

func (h *helperWire) permission(id any) {
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission", "params": map[string]any{
		"sessionId": "session-test", "toolCall": map[string]any{
			"toolCallId": "read-tracked", "title": "Inspect tracked file",
			"rawInput":  map[string]string{"command": "git show HEAD:tracked.txt"},
			"locations": []map[string]any{{"path": "tracked.txt", "line": 1}},
		},
		"options": []PermissionOption{{OptionID: "allow", Name: "Allow once", Kind: "allow_once"}, {OptionID: "reject", Name: "Reject once", Kind: "reject_once"}},
	}})
}

func runHelper(mode, marker string) {
	h := &helperWire{reader: bufio.NewReader(os.Stdin), encoder: json.NewEncoder(os.Stdout)}
	if os.Getenv("GIT_DIR") != "" || os.Getenv("GIT_WORK_TREE") != "" {
		h.fail("agent inherited a Git checkout override")
	}
	if mode == "descendant" {
		if err := os.WriteFile(marker+".ready", []byte("ready"), 0600); err != nil {
			os.Exit(91)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	initialize := h.read()
	var initParams struct {
		ProtocolVersion    int
		ClientCapabilities map[string]any
	}
	if initialize.Method != "initialize" || json.Unmarshal(initialize.Params, &initParams) != nil || initParams.ProtocolVersion != 1 || initParams.ClientCapabilities == nil || len(initParams.ClientCapabilities) != 0 {
		h.fail("incorrect initialize request")
	}
	version := 1
	if mode == "version" {
		version = 2
	}
	h.response(initialize.ID, map[string]any{"protocolVersion": version, "agentCapabilities": map[string]any{}, "authMethods": []any{map[string]string{"id": "login", "name": "Login"}}})
	if mode == "version" {
		io.Copy(io.Discard, h.reader)
		return
	}
	newSession := h.read()
	var sessionParams struct {
		CWD        string `json:"cwd"`
		MCPServers []any  `json:"mcpServers"`
	}
	if newSession.Method != "session/new" || json.Unmarshal(newSession.Params, &sessionParams) != nil || !filepath.IsAbs(sessionParams.CWD) || sessionParams.MCPServers == nil || len(sessionParams.MCPServers) != 0 {
		h.fail("incorrect session/new")
	}
	cwd, _ := os.Getwd()
	actualCWD, _ := filepath.EvalSymlinks(cwd)
	requestedCWD, _ := filepath.EvalSymlinks(sessionParams.CWD)
	if actualCWD != requestedCWD {
		h.fail("subprocess cwd does not match session cwd")
	}
	if mode == "auth" || mode == "secret-error" {
		message := "Authentication required"
		if mode == "secret-error" {
			message = os.Getenv("SKIM_ACP_TEST_SECRET")
			for range 10000 {
				fmt.Fprint(os.Stderr, message)
			}
		}
		h.send(map[string]any{"jsonrpc": "2.0", "id": newSession.ID, "error": map[string]any{"code": -32000, "message": message, "data": map[string]string{"credential": message}}})
		io.Copy(io.Discard, h.reader)
		return
	}
	if mode == "empty-session" {
		h.response(newSession.ID, map[string]string{"sessionId": ""})
		io.Copy(io.Discard, h.reader)
		return
	}
	h.response(newSession.ID, map[string]string{"sessionId": "session-test"})
	prompt := h.read()
	var promptParams struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string
			Text string
		}
	}
	if prompt.Method != "session/prompt" || json.Unmarshal(prompt.Params, &promptParams) != nil || promptParams.SessionID != "session-test" || len(promptParams.Prompt) != 1 || promptParams.Prompt[0].Type != "text" || promptParams.Prompt[0].Text != helperPrompt {
		h.fail("incorrect prompt or lost conversation history")
	}
	if strings.HasSuffix(mode, "-child") {
		executable, err := os.Executable()
		if err != nil {
			h.fail("cannot locate helper")
		}
		child := exec.Command(executable, "-test.run=^TestACPHelperProcess$", "--", "skim-acp-helper", "descendant", marker)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			h.fail("cannot start descendant")
		}
		// Ensure a real running descendant inherited the group's pipes before
		// allowing its parent to exit or acknowledge the test's cancellation.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(marker + ".ready"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				h.fail("descendant did not become ready")
			}
			time.Sleep(time.Millisecond)
		}
		if err := os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			h.fail("cannot record process group")
		}
	}
	switch mode {
	case "eof", "eof-child":
		return
	case "malformed":
		fmt.Fprintln(os.Stdout, "not JSON")
		io.Copy(io.Discard, h.reader)
		return
	case "unterminated":
		fmt.Fprint(os.Stdout, `{"jsonrpc":"2.0"}`)
		return
	case "oversize":
		fmt.Fprintln(os.Stdout, strings.Repeat("x", maxLineBytes+1))
		io.Copy(io.Discard, h.reader)
		return
	case "unexpected-id":
		h.response(json.RawMessage("999"), map[string]string{"stopReason": "end_turn"})
		io.Copy(io.Discard, h.reader)
		return
	case "response-cap", "wire-cap":
		kind, count := "agent_message_chunk", 10
		if mode == "wire-cap" {
			kind, count = "agent_thought_chunk", 70
		}
		chunk := map[string]string{"type": "text", "text": strings.Repeat("x", maxLineBytes/2)}
		for range count {
			h.update("session-test", kind, chunk)
		}
		io.Copy(io.Discard, h.reader)
		return
	case "permissions", "one-permission":
		count := 1
		if mode == "permissions" {
			count = 2
		}
		for i := range count {
			h.permission("permission-" + strconv.Itoa(i))
		}
		h.update("session-test", "agent_thought_chunk", map[string]string{"type": "text", "text": "private thought"})
		for range count {
			response := h.read()
			var result struct {
				Outcome struct{ Outcome, OptionID string }
			}
			if json.Unmarshal(response.Result, &result) != nil || result.Outcome.Outcome != "selected" || result.Outcome.OptionID != "reject" {
				h.fail("incorrect explicit permission outcome")
			}
		}
	case "invalid-permissions":
		for index, params := range []map[string]any{
			{"sessionId": "wrong-session", "toolCall": map[string]string{"toolCallId": "call"}, "options": []PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}},
			{"sessionId": "session-test", "toolCall": map[string]string{"toolCallId": "call"}, "options": []PermissionOption{}},
			{"sessionId": "session-test", "toolCall": map[string]string{"toolCallId": "call"}, "options": []PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "unknown"}}},
			{"sessionId": "session-test", "toolCall": map[string]string{"toolCallId": "call"}, "options": []PermissionOption{{OptionID: "same", Name: "Allow", Kind: "allow_once"}, {OptionID: "same", Name: "Reject", Kind: "reject_once"}}},
		} {
			h.send(map[string]any{"jsonrpc": "2.0", "id": index + 100, "method": "session/request_permission", "params": params})
			reply := h.read()
			if reply.Error == nil || reply.Error.Code != -32602 || string(reply.ID) != strconv.Itoa(index+100) {
				h.fail("invalid permission was not rejected")
			}
		}
	case "permission-limit":
		for index := range maxPendingPermissions + 1 {
			h.permission(index + 100)
		}
		io.Copy(io.Discard, h.reader)
		return
	case "duplicate-permission":
		h.permission("duplicate")
		h.permission("duplicate")
		io.Copy(io.Discard, h.reader)
		return
	case "cancel":
		h.permission("pending")
		notified, outcomes := false, 0
		for !notified || outcomes != 2 {
			message := h.read()
			if message.Method == "session/cancel" {
				if len(message.ID) != 0 {
					h.fail("cancel must be notification")
				}
				notified = true
				h.permission("late")
				continue
			}
			var result struct {
				Outcome struct {
					Outcome  string
					OptionID string
				}
			}
			if json.Unmarshal(message.Result, &result) != nil || result.Outcome.Outcome != "cancelled" || result.Outcome.OptionID != "" {
				h.fail("permission was not cancelled")
			}
			outcomes++
		}
		if err := os.WriteFile(marker, []byte("cancel notification and both cancelled outcomes received"), 0600); err != nil {
			h.fail("write cancellation marker: %v", err)
		}
		h.response(prompt.ID, map[string]string{"stopReason": "cancelled"})
		io.Copy(io.Discard, h.reader)
		return
	case "ignore-cancel", "ignore-cancel-child":
		h.permission("pending")
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode != "empty" {
		h.update("unrelated-session", "agent_message_chunk", map[string]string{"type": "text", "text": "wrong session"})
		h.update("session-test", "agent_thought_chunk", map[string]string{"type": "text", "text": "never include thoughts"})
		h.update("session-test", "user_message_chunk", map[string]string{"type": "text", "text": "never include user chunks"})
		h.update("session-test", "tool_call_update", []map[string]any{{"type": "content", "content": map[string]string{"type": "text", "text": "tool output"}}})
		h.update("session-test", "agent_message_chunk", map[string]string{"type": "image", "data": "ignored", "mimeType": "image/png"})
		h.update("session-test", "agent_message_chunk", map[string]string{"type": "text", "text": "Checked the caller. "})
		h.send(map[string]any{"jsonrpc": "2.0", "id": 71, "method": "fs/read_text_file", "params": map[string]string{"path": "tracked.txt"}})
		unsupported := h.read()
		if unsupported.Error == nil || unsupported.Error.Code != -32601 || string(unsupported.ID) != "71" {
			h.fail("unknown request did not return method-not-found")
		}
		h.update("session-test", "agent_message_chunk", map[string]string{"type": "text", "text": "No edit was made."})
	}
	stopReason := "end_turn"
	switch mode {
	case "max_tokens", "max_turn_requests", "refusal", "cancelled", "unknown-stop":
		stopReason = mode
	}
	h.response(prompt.ID, map[string]string{"stopReason": stopReason})
	if mode == "nonzero" {
		os.Exit(23)
	}
	if mode == "success-child" {
		return
	}
	io.Copy(io.Discard, h.reader)
}
