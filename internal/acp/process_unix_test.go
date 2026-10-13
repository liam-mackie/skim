//go:build unix

package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"skim/internal/domain"
	"skim/internal/processgroup"
)

func TestRunWaitsForDescendants(t *testing.T) {
	for _, mode := range []string{"success-child", "eof-child", "ignore-cancel-child"} {
		t.Run(mode, func(t *testing.T) {
			directory, sha := checkout(t)
			marker := filepath.Join(t.TempDir(), "process-group")
			client := New()
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			result := startRun(client, ctx, helperSettings(t, mode, marker), directory, sha)
			if mode == "ignore-cancel-child" {
				awaitPermissions(t, client, 1, result)
				cancel()
			}
			value := awaitRun(t, result)
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal("helper did not start its descendant:", err)
			}
			group, err := strconv.Atoi(string(data))
			if err != nil || group <= 0 {
				t.Fatal("invalid helper process group")
			}
			defer syscall.Kill(-group, syscall.SIGKILL)
			stopped, err := processgroup.Stopped(group)
			if err != nil || !stopped {
				t.Fatalf("Run returned while its process group was still active: %v", err)
			}
			switch mode {
			case "success-child":
				if value.err != nil || value.text == "" {
					t.Fatalf("successful parent with descendant: %+v", value)
				}
			case "eof-child":
				if value.err == nil || !strings.Contains(value.err.Error(), "before completing") {
					t.Fatalf("parent exit was not detected: %+v", value)
				}
			case "ignore-cancel-child":
				if !errors.Is(value.err, context.Canceled) {
					t.Fatalf("cancellation did not survive cleanup: %+v", value)
				}
			}
		})
	}
}

func TestRunReportsLiveGroupBeforeProtocolAndDrainsOnCallbackFailure(t *testing.T) {
	directory, sha := checkout(t)
	var group int
	text, err := New().Run(context.Background(), helperSettings(t, "success", ""), directory, sha, helperPrompt, PermissionScope{}, func(g int) error {
		group = g
		stopped, err := processgroup.Stopped(g)
		if err != nil || stopped {
			t.Errorf("callback group %d was not live: stopped=%v err=%v", g, stopped, err)
		}
		return nil
	})
	if err != nil || text == "" || group <= 1 {
		t.Fatalf("callback run: %q %v group=%d", text, err, group)
	}
	if stopped, err := processgroup.Stopped(group); err != nil || !stopped {
		t.Fatalf("Run returned with its group active: %v", err)
	}

	// The descendant helper never reads stdin; only the callback failure can end it.
	failure := errors.New("journal unavailable")
	group = 0
	_, err = New().Run(context.Background(), helperSettings(t, "descendant", filepath.Join(t.TempDir(), "m")), directory, sha, helperPrompt, PermissionScope{}, func(g int) error {
		group = g
		return failure
	})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "verification was not started") {
		t.Fatalf("callback failure was not reported: %v", err)
	}
	defer syscall.Kill(-group, syscall.SIGKILL)
	if stopped, err := processgroup.Stopped(group); err != nil || !stopped {
		t.Fatalf("agent survived callback failure: %v", err)
	}
}

func TestValidateInvocationNeverExecutes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInvocation(domain.ACPSettings{Command: script}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("preflight executed the agent")
	}
	for _, command := range []string{"", "  ", filepath.Join(t.TempDir(), "absent"), "skim-no-such-agent-binary"} {
		if err := ValidateInvocation(domain.ACPSettings{Command: command}); err == nil {
			t.Fatalf("%q was accepted", command)
		}
	}
}
