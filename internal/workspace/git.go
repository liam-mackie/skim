package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"skim/internal/domain"
	"skim/internal/processgroup"
)

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var shaPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

// bareConfig is the exact, auditable cache configuration. Git for Windows' system
// config (deliberately ignored) and init's probing normally supply its platform
// values: NTFS has no executable bit, unprivileged users cannot create symlinks,
// and managed checkouts under %AppData% readily exceed MAX_PATH.
var bareConfig = func() string {
	if runtime.GOOS == "windows" {
		return "[core]\n\trepositoryformatversion = 0\n\tfilemode = false\n\tbare = true\n\tfsmonitor = false\n\tuntrackedCache = false\n\tsymlinks = false\n\tlongpaths = true\n"
	}
	return "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n\tfsmonitor = false\n\tuntrackedCache = false\n"
}()

func canonicalURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".git") {
		return false
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	return ownerPattern.MatchString(parts[0]) && repoPattern.MatchString(repo) && repo != "." && repo != ".." && value == "https://github.com/"+parts[0]+"/"+repo+".git"
}

func validateSource(pr domain.PullRequest, source domain.GitSource) error {
	if pr.Provider != "github" || pr.Number <= 0 || !ownerPattern.MatchString(pr.Owner) || !repoPattern.MatchString(pr.Repo) || pr.Repo == "." || pr.Repo == ".." || !shaPattern.MatchString(pr.HeadSHA) || !shaPattern.MatchString(pr.BaseSHA) {
		return errors.New("invalid GitHub pull-request metadata; reload the pull request")
	}
	if !canonicalURL(source.URL) || source.URL != "https://github.com/"+pr.Owner+"/"+pr.Repo+".git" || source.HeadRef != "refs/pull/"+strconv.Itoa(pr.Number)+"/head" {
		return errors.New("invalid Git source; expected the canonical GitHub base repository and pull-request head ref")
	}
	return nil
}

// GitError exposes a stable classification without exposing command arguments or credentials.
type GitError struct {
	Kind   string
	Detail string
}

func (e *GitError) Error() string { return "managed Git " + e.Kind + ": " + e.Detail }

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func (b *limitedBuffer) String() string { return b.buffer.String() }

func redactDiagnostic(text, token string) string {
	if token != "" {
		for _, secret := range []string{token, url.QueryEscape(token), url.PathEscape(token), base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token)), base64.StdEncoding.EncodeToString([]byte(token))} {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(text))
}

func (m *Manager) git(ctx context.Context, directory, token, operation string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := m.validateGitHome(); err != nil {
		return "", err
	}
	options := []string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.attributesFile=" + os.DevNull,
		"-c", "credential.helper=",
		"-c", "credential.useHttpPath=true",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "http.followRedirects=false",
		"-c", "http.extraHeader=",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "submodule.recurse=false",
		"-c", "maintenance.auto=false",
		"-c", "gc.auto=0",
	}
	m.mu.Lock()
	allowFile := len(m.localRemotes) != 0
	m.mu.Unlock()
	if allowFile {
		options = append(options, "-c", "protocol.file.allow=always")
	}
	options = append(options, args...)
	cmd := exec.CommandContext(ctx, "git", options...)
	cmd.Dir = directory
	cmd.WaitDelay = time.Second
	// An allowlist removes inherited GIT_*, helper, SSH, proxy, tracing, and loader overrides.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + m.home, "XDG_CONFIG_HOME=" + m.home, "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_LITERAL_PATHSPECS=1",
	}
	if runtime.GOOS == "windows" {
		// Git for Windows needs the Windows directory (Winsock, Schannel/crypto DLLs
		// and MSYS runtime) and a temporary directory. User profile variables stay
		// out: HOME already points at the isolated Git home.
		for _, name := range []string{"SYSTEMROOT", "WINDIR", "TEMP", "TMP"} {
			if value, ok := os.LookupEnv(name); ok {
				cmd.Env = append(cmd.Env, name+"="+value)
			}
		}
	}
	if token != "" {
		dir, err := os.MkdirTemp(m.private, "askpass-")
		if err != nil {
			return "", errors.New("could not create private Git authentication helper")
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "askpass")
		script := "#!/bin/sh\ncase \"$1\" in\n*Username*) printf '%s\\n' 'x-access-token' ;;\n*Password*) printf '%s\\n' \"$SKIM_GIT_TOKEN\" ;;\n*) exit 1 ;;\nesac\n"
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			return "", errors.New("could not create private Git authentication helper")
		}
		// Askpass is an executable path, not shell source; quoting would become
		// part of the filename. Git passes the prompt as a separate argument.
		cmd.Env = append(cmd.Env, "GIT_ASKPASS="+path, "SKIM_GIT_TOKEN="+token)
	} else {
		cmd.Env = append(cmd.Env, "GIT_ASKPASS="+os.DevNull)
	}
	stdout := &limitedBuffer{limit: 8 << 20}
	stderr := &limitedBuffer{limit: 8192}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := processgroup.Run(cmd)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err == nil {
		if stdout.truncated {
			return "", &GitError{Kind: "output_limit", Detail: "Git output exceeded the safe inspection limit; checkout retained"}
		}
		return stdout.String(), nil
	}
	var executableError *exec.Error
	if errors.As(err, &executableError) || errors.Is(err, os.ErrNotExist) {
		return "", &GitError{Kind: "missing_executable", Detail: "Git is not installed or is not available on PATH"}
	}
	detail := redactDiagnostic(stderr.String(), token)
	if stderr.truncated {
		detail = "Git diagnostic exceeded the safe limit"
	}
	lower := strings.ToLower(detail)
	kind := operation + "_failed"
	switch {
	case strings.Contains(lower, "not a git repository"), strings.Contains(lower, "does not appear to be a git repository"):
		kind = "not_repository"
	case strings.Contains(lower, "authentication failed"), strings.Contains(lower, "could not read username"), strings.Contains(lower, "could not read password"), strings.Contains(lower, "invalid username"), strings.Contains(lower, "access denied"), strings.Contains(lower, "error: 403"), strings.Contains(lower, "error: 401"):
		kind = "authentication_failed"
	case strings.Contains(lower, "repository not found"):
		detail = "repository unavailable: it does not exist or the configured GitHub token lacks access"
	}
	if detail == "" {
		detail = "Git exited unsuccessfully"
	}
	return "", &GitError{Kind: kind, Detail: detail}
}

func (m *Manager) fetch(ctx context.Context, c cache, source domain.GitSource, pr domain.PullRequest, token string) error {
	if err := m.validateCache(ctx, c); err != nil {
		return err
	}
	m.mu.Lock()
	remote := m.localRemotes[source.URL]
	m.mu.Unlock()
	if remote == "" {
		remote = source.URL
	}
	ref := "refs/skim/pull/" + strconv.Itoa(pr.Number)
	if _, err := m.git(ctx, c.repo(), token, "fetch", "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", remote, "+"+source.HeadRef+":"+ref, "+"+strings.ToLower(pr.BaseSHA)+":refs/skim/base/"+strings.ToLower(pr.BaseSHA)); err != nil {
		return err
	}
	head, err := m.git(ctx, c.repo(), "", "inspect", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != strings.ToLower(pr.HeadSHA) {
		return errors.New("pull-request head changed since inspection; reload the pull request before verification")
	}
	base, err := m.git(ctx, c.repo(), "", "inspect", "rev-parse", "--verify", strings.ToLower(pr.BaseSHA)+"^{commit}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(base) != strings.ToLower(pr.BaseSHA) {
		return errors.New("fetched base does not match the inspected commit")
	}
	return nil
}

// SetLocalRemoteForTesting is an explicit, process-local test seam. It is never
// populated by environment variables, persisted settings, journals, or API input.
// Callers must supply their own private temporary bare repository.
func (m *Manager) SetLocalRemoteForTesting(canonicalSource, absoluteBarePath string) error {
	if !canonicalURL(canonicalSource) || !filepath.IsAbs(absoluteBarePath) {
		return errors.New("test remote requires a canonical GitHub URL and an absolute temporary bare-repository path")
	}
	path, err := canonical(absoluteBarePath)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return errors.New("test remote is not a directory")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.localRemotes[canonicalSource] = path
	return nil
}

func unsafeOwnership(reason string) error {
	return fmt.Errorf("workspace ownership check failed (%s); no user paths were removed; restore the owned metadata or move the retained files manually", reason)
}
