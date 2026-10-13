package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"skim/internal/processgroup"
)

func verifyWorkspace(ctx context.Context, directory, headSHA string) error {
	if !filepath.IsAbs(directory) {
		return errors.New("ACP workspace must be an absolute path to the managed PR checkout")
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return errors.New("ACP workspace is not an accessible directory; prepare the managed PR checkout again")
	}
	if !validSHA(headSHA) {
		return errors.New("ACP verification requires the full PR head commit SHA; reload the pull request")
	}
	head, err := workspaceGit(ctx, directory, 128, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != headSHA {
		return errors.New("ACP workspace HEAD does not match the PR head; prepare the exact PR head commit again")
	}
	status, err := workspaceGit(ctx, directory, 1, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("ACP workspace has tracked or untracked changes; finish or discard the managed checkout before preparing it again")
	}
	return nil
}

func validSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

type limitedCapture struct {
	data  []byte
	limit int
}

func (c *limitedCapture) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := c.limit - len(c.data); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		c.data = append(c.data, p...)
	}
	return n, nil
}

func workspaceGit(ctx context.Context, directory string, limit int, args ...string) (string, error) {
	base := []string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.attributesFile=" + os.DevNull, "-C", directory}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	// Inherited Git overrides must not make us verify a different repository,
	// index, or configuration than the directory sent to the agent.
	cmd.Env = workspaceGitEnvironment()
	capture := limitedCapture{limit: limit}
	diagnostic := limitedCapture{limit: 4096}
	cmd.Stdout = &capture
	cmd.Stderr = &diagnostic
	if err := processgroup.Run(cmd); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Never surface raw Git output: repository config, paths and remotes
		// can contain credentials, including values unknown to our redactor.
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git is not installed or is not on PATH; install git to prepare an ACP workspace")
		}
		if strings.Contains(string(diagnostic.data), "not a git repository") {
			return "", errors.New("ACP workspace is not a Git repository; prepare the managed checkout again")
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("git %s failed while inspecting the ACP workspace (exit status %d); diagnostic output omitted to protect credentials", args[0], exit.ExitCode())
		}
		return "", errors.New("cannot execute git to inspect the ACP workspace; check executable permissions and workspace access")
	}
	return string(capture.data), nil
}

func workspaceGitEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+5)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if runtime.GOOS == "windows" {
			// Windows variable names are case-insensitive: Git_Dir is GIT_DIR.
			name = strings.ToUpper(name)
		}
		if !strings.HasPrefix(name, "GIT_") && name != "LC_ALL" {
			environment = append(environment, variable)
		}
	}
	// Global/system filters, includes and hooks must not run while checking a
	// managed checkout. Repository-local config belongs to the workspace manager.
	return append(environment, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_ATTR_NOSYSTEM=1", "LC_ALL=C")
}
