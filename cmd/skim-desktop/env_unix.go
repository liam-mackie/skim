//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const envProbeArg = "--skim-print-environment"

const (
	envBegin = "\n--skim-environment-begin--\n"
	envEnd   = "\n--skim-environment-end--\n"
)

// printEnvironment runs inside the user's login shell (see
// importLoginShellEnvironment) and reports the environment it was given.
func printEnvironment() {
	data, err := json.Marshal(os.Environ())
	if err != nil {
		os.Exit(1)
	}
	_, _ = os.Stdout.WriteString(envBegin + string(data) + envEnd)
}

// importLoginShellEnvironment gives a Finder/Dock/desktop-launcher start the
// environment a terminal start would have: PATH for git and ACP agents,
// GITHUB_TOKEN and provider/agent keys exported from shell profiles. Starts
// from a terminal already have it. Failure leaves the environment unchanged.
func importLoginShellEnvironment() {
	if os.Getenv("TERM") != "" {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
		if runtime.GOOS == "darwin" {
			shell = "/bin/zsh"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Interactive as well as login: many users export PATH and tokens from
	// .zshrc/.bashrc only. The executable path travels in the environment so
	// no shell-specific quoting is needed (works in sh, bash, zsh and fish).
	cmd := exec.CommandContext(ctx, shell, "-l", "-i", "-c", `"$SKIM_DESKTOP_EXE" `+envProbeArg)
	cmd.Env = append(os.Environ(), "SKIM_DESKTOP_EXE="+self)
	cmd.WaitDelay = time.Second // daemons started by shell profiles may keep stdout open
	output, err := cmd.Output()
	if err != nil {
		return
	}
	begin := bytes.LastIndex(output, []byte(envBegin))
	if begin < 0 {
		return
	}
	output = output[begin+len(envBegin):]
	end := bytes.Index(output, []byte(envEnd))
	if end < 0 {
		return
	}
	var shellEnv []string
	if json.Unmarshal(output[:end], &shellEnv) != nil {
		return
	}
	for key, value := range mergeEnvironment(os.Environ(), shellEnv) {
		_ = os.Setenv(key, value)
	}
}

// shellSession variables describe the probe shell itself, not the user's setup.
var shellSession = map[string]bool{"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true, "SKIM_DESKTOP_EXE": true}

// mergeEnvironment returns the variables to set. The launcher's own variables
// win, except PATH: a GUI launcher's PATH is the minimal system default.
func mergeEnvironment(current, shell []string) map[string]string {
	present := map[string]bool{}
	for _, kv := range current {
		if key, _, ok := strings.Cut(kv, "="); ok {
			present[key] = true
		}
	}
	updates := map[string]string{}
	for _, kv := range shell {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || shellSession[key] || (present[key] && key != "PATH") {
			continue
		}
		updates[key] = value
	}
	return updates
}
