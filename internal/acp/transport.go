package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"skim/internal/domain"
	"skim/internal/processgroup"
)

const (
	maxLineBytes          = 1 << 20
	maxWireBytes          = 32 << 20
	maxResponseBytes      = 4 << 20
	maxPromptBytes        = 2 << 20
	maxPendingPermissions = 64
	shutdownGrace         = time.Second
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type wireEvent struct {
	message envelope
	err     error
}

// Raw stderr and agent error messages can contain credentials, even transformed
// or split across writes. Retaining only a bounded byte count is deliberately
// stronger than attempting token-pattern redaction. No raw diagnostic is exposed.
type stderrSummary struct{ bytes atomic.Uint64 }

func (s *stderrSummary) Write(p []byte) (int, error) {
	if s.bytes.Load() < 64<<10 {
		n := s.bytes.Load() + uint64(len(p))
		if n > 64<<10 {
			n = 64 << 10
		}
		s.bytes.Store(n)
	}
	return len(p), nil
}

type transport struct {
	cmd         *exec.Cmd
	stdin       *os.File
	stdout      *os.File
	outbound    chan []byte
	events      chan wireEvent
	writeErrors chan error
	done        chan struct{}
	readerDone  chan struct{}
	writerDone  chan struct{}
	processDone chan struct{}
	processErr  error
	stderr      stderrSummary
}

func startTransport(settings domain.ACPSettings, directory string, onStart func(group int) error) (*transport, error) {
	input, stdin, err := os.Pipe()
	if err != nil {
		return nil, errors.New("cannot open ACP agent input pipe")
	}
	stdout, output, err := os.Pipe()
	if err != nil {
		input.Close()
		stdin.Close()
		return nil, errors.New("cannot open ACP agent output pipe")
	}
	t := &transport{
		cmd:   exec.Command(settings.Command, settings.Args...),
		stdin: stdin, stdout: stdout,
		outbound: make(chan []byte, maxPendingPermissions+8),
		events:   make(chan wireEvent, 8), writeErrors: make(chan error, 1),
		done: make(chan struct{}), readerDone: make(chan struct{}),
		writerDone: make(chan struct{}), processDone: make(chan struct{}),
	}
	t.cmd.Dir = directory
	// Agent-owned Git commands must resolve the same generated checkout that
	// passed verification, not a caller's inherited repository/index override.
	t.cmd.Env = workspaceGitEnvironment()
	t.cmd.Stdin = input
	t.cmd.Stdout = output
	t.cmd.Stderr = &t.stderr
	t.cmd.WaitDelay = shutdownGrace
	err = processgroup.Start(t.cmd)
	input.Close()
	output.Close()
	if err != nil {
		stdin.Close()
		stdout.Close()
		// exec errors include executable names, which may contain credentials.
		return nil, errors.New("cannot start ACP agent; check command, arguments, executable permissions, and PATH")
	}
	if onStart != nil {
		// The group is durable before the first protocol byte, so a crash can never
		// leave an unrecorded agent able to run tools in this checkout.
		if err := onStart(t.cmd.Process.Pid); err != nil {
			_ = processgroup.Kill(t.cmd)
			_ = t.cmd.Wait()
			processgroup.Wait(t.cmd)
			stdin.Close()
			stdout.Close()
			return nil, fmt.Errorf("cannot record ACP agent process; verification was not started: %w", err)
		}
	}
	go t.readLoop()
	go t.writeLoop()
	go func() {
		t.processErr = t.cmd.Wait()
		// A finished leader can leave descendants keeping protocol pipes open.
		// Stop those descendants now, not only after await sees EOF or times out.
		_ = processgroup.Kill(t.cmd)
		close(t.processDone)
	}()
	return t, nil
}

func (t *transport) emit(event wireEvent) {
	select {
	case t.events <- event:
	case <-t.done:
	}
}

func (t *transport) readLoop() {
	defer close(t.readerDone)
	defer close(t.events)
	scanner := bufio.NewScanner(t.stdout)
	scanner.Buffer(make([]byte, 4096), maxLineBytes+1)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			return index + 1, data[:index], nil
		}
		if atEOF && len(data) != 0 {
			return 0, nil, errors.New("unterminated ACP message")
		}
		return 0, nil, nil
	})
	total := 0
	for scanner.Scan() {
		total += len(scanner.Bytes()) + 1
		if total > maxWireBytes {
			t.emit(wireEvent{err: errors.New("ACP agent exceeded the total protocol output limit")})
			return
		}
		message, err := decodeEnvelope(scanner.Bytes())
		if err != nil {
			t.emit(wireEvent{err: err})
			return
		}
		t.emit(wireEvent{message: message})
		select {
		case <-t.done:
			return
		default:
		}
	}
	if scanner.Err() != nil {
		t.emit(wireEvent{err: errors.New("ACP agent produced malformed, oversized, or unterminated newline-delimited JSON; configure native ACP stdio mode")})
	} else {
		t.emit(wireEvent{err: io.EOF})
	}
}

var invalidEnvelope = errors.New("ACP agent produced an invalid JSON-RPC 2.0 message; configure native ACP stdio mode")

func decodeEnvelope(line []byte) (envelope, error) {
	var message envelope
	if json.Unmarshal(line, &message) != nil || message.JSONRPC != "2.0" {
		return message, invalidEnvelope
	}
	if len(message.ID) != 0 {
		if _, err := requestKey(message.ID); err != nil {
			return message, invalidEnvelope
		}
	}
	if message.Method != "" {
		if len(message.Result) != 0 || message.Error != nil {
			return message, invalidEnvelope
		}
	} else if len(message.ID) == 0 || (len(message.Result) != 0) == (message.Error != nil) {
		return message, invalidEnvelope
	}
	return message, nil
}

func requestKey(id json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "", errors.New("invalid JSON-RPC ID")
	}
	switch value := value.(type) {
	case string:
		return "s:" + value, nil
	case json.Number:
		return "n:" + value.String(), nil
	default:
		return "", errors.New("invalid JSON-RPC ID")
	}
}

func (t *transport) writeLoop() {
	defer close(t.writerDone)
	defer t.stdin.Close()
	for data := range t.outbound {
		if _, err := t.stdin.Write(data); err != nil {
			select {
			case t.writeErrors <- errors.New("cannot write to ACP agent; the agent closed its input"):
			default:
			}
			return
		}
	}
}

func (t *transport) send(ctx context.Context, message any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(message)
	if err != nil {
		return errors.New("cannot encode ACP request")
	}
	data = append(data, '\n')
	select {
	case t.outbound <- data:
		return nil
	case err := <-t.writeErrors:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *transport) request(ctx context.Context, id int, method string, params any) error {
	return t.send(ctx, struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
}

func (t *transport) result(ctx context.Context, id json.RawMessage, result any) error {
	return t.send(ctx, struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, result})
}

func (t *transport) reject(ctx context.Context, id json.RawMessage, code int, message string) error {
	return t.send(ctx, envelope{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (t *transport) close(deadline <-chan struct{}) error {
	close(t.outbound)
	var exitErr error
	select {
	case <-t.processDone:
		if t.processErr != nil && !errors.Is(t.processErr, exec.ErrWaitDelay) {
			exitErr = errors.New("ACP agent exited unsuccessfully; check agent authentication and configuration locally")
		}
	case <-deadline:
	}
	// Also terminate descendants holding pipes or executing agent-owned tools.
	_ = processgroup.Kill(t.cmd)
	close(t.done)
	t.stdin.Close()
	t.stdout.Close()
	<-t.writerDone
	<-t.readerDone
	<-t.processDone
	processgroup.Wait(t.cmd)
	return exitErr
}
