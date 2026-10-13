//go:build unix

package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"skim/internal/processgroup"
)

func TestManagedGitStopsDescendantsBeforeReturning(t *testing.T) {
	for _, mode := range []string{"success", "pipe-holder", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newGitFixture(t)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			marker := filepath.Join(t.TempDir(), "process-group")
			wrapper := "#!/bin/sh\nexec " + shellQuote(executable) + " -test.run=^TestWorkspaceGitProcessHelper$ -- skim-workspace-process " + shellQuote(mode) + " " + shellQuote(marker) + "\n"
			mustWrite(t, filepath.Join(bin, "git"), wrapper)
			if err := os.Chmod(filepath.Join(bin, "git"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			t.Cleanup(func() {
				if data, err := os.ReadFile(marker); err == nil {
					if group, err := strconv.Atoi(string(data)); err == nil && group > 0 {
						_ = syscall.Kill(-group, syscall.SIGKILL)
					}
				}
			})
			result := make(chan error, 1)
			go func() {
				_, err := f.manager.git(ctx, f.manager.home, "", "fetch", "fetch")
				result <- err
			}()
			var group int
			deadline := time.After(5 * time.Second)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
		ready:
			for {
				select {
				case <-deadline:
					t.Fatal("Git helper descendant did not become ready")
				case <-ticker.C:
					if data, err := os.ReadFile(marker); err == nil {
						group, err = strconv.Atoi(string(data))
						if err == nil && group > 0 {
							break ready
						}
					}
				}
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-result:
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("Git cancellation was not retained: %v", err)
				}
				if mode == "success" && err != nil {
					t.Fatalf("successful parent was reported as failed: %v", err)
				}
				if mode == "pipe-holder" && err == nil {
					t.Fatal("incomplete inherited output drain was reported as successful")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Git subprocess cleanup did not complete")
			}
			if stopped, err := processgroup.Stopped(group); err != nil || !stopped {
				t.Fatalf("Git returned while a descendant could still write: %v", err)
			}
			before := mustRead(t, marker+".writes")
			time.Sleep(20 * time.Millisecond)
			if after := mustRead(t, marker+".writes"); after != before {
				t.Fatal("descendant wrote after Git runner released the operation")
			}
		})
	}
}

func TestWorkspaceGitProcessHelper(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "skim-workspace-process" {
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
	mode, marker := os.Args[index+1], os.Args[index+2]
	if mode == "child" {
		for sequence := 1; ; sequence++ {
			if err := os.WriteFile(marker+".writes", []byte(strconv.Itoa(sequence)), 0600); err != nil {
				os.Exit(91)
			}
			time.Sleep(time.Millisecond)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		os.Exit(92)
	}
	child := exec.Command(executable, "-test.run=^TestWorkspaceGitProcessHelper$", "--", "skim-workspace-process", "child", marker)
	if mode != "success" {
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
	}
	if err := child.Start(); err != nil {
		os.Exit(93)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(marker + ".writes"); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(94)
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(95)
	}
	if mode == "cancel" {
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(0)
}
