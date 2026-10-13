package provider

import (
	"strings"
	"testing"

	"skim/internal/domain"
)

func TestGitSourceUsesBaseRepositoryPullRef(t *testing.T) {
	pr, err := ParseGitHubURL("https://github.com/Base-Owner/repo.name/pull/42/")
	if err != nil {
		t.Fatal(err)
	}
	pr.HeadRef = "fork-author:feature/branch"
	github := NewGitHub("unused-test-token")
	github.BaseURL = "http://127.0.0.1:1/arbitrary-api-fixture"
	source, err := github.GitSource(pr)
	if err != nil {
		t.Fatal(err)
	}
	if source.URL != "https://github.com/Base-Owner/repo.name.git" || source.HeadRef != "refs/pull/42/head" {
		t.Fatalf("incorrect source: %+v", source)
	}
}

func TestGitSourceRejectsInvalidMetadata(t *testing.T) {
	valid, err := ParseGitHubURL("https://github.com/owner/repo/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*domain.PullRequest){
		"provider":        func(pr *domain.PullRequest) { pr.Provider = "other" },
		"missing owner":   func(pr *domain.PullRequest) { pr.Owner = "" },
		"owner traversal": func(pr *domain.PullRequest) { pr.Owner = "../owner" },
		"repo traversal":  func(pr *domain.PullRequest) { pr.Repo = ".." },
		"repo injection":  func(pr *domain.PullRequest) { pr.Repo = "repo.git --upload-pack=bad" },
		"zero number":     func(pr *domain.PullRequest) { pr.Number = 0 },
		"negative number": func(pr *domain.PullRequest) { pr.Number = -7 },
		"owner mismatch":  func(pr *domain.PullRequest) { pr.Owner = "other" },
		"repo mismatch":   func(pr *domain.PullRequest) { pr.Repo = "other" },
		"number mismatch": func(pr *domain.PullRequest) { pr.Number = 8 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pr := valid
			mutate(&pr)
			if source, err := NewGitHub("").GitSource(pr); err == nil || source != (domain.GitSource{}) {
				t.Fatalf("accepted invalid metadata: %+v / %v", source, err)
			}
		})
	}
	for _, raw := range []string{
		"", "file:///private/repository", "git@github.com:owner/repo.git",
		"http://github.com/owner/repo/pull/7", "https://evil.invalid/owner/repo/pull/7",
		"https://github.com:443/owner/repo/pull/7", "https://secret@github.com/owner/repo/pull/7",
		"https://github.com/owner/repo/pull/7?token=secret", "https://github.com/owner/repo/pull/7#secret",
		"https://github.com/owner/repo/pull/07", "https://github.com/owner/repo/pull/7/files",
		"https://github.com/owner/%72epo/pull/7",
	} {
		pr := valid
		pr.URL = raw
		source, err := NewGitHub("").GitSource(pr)
		if err == nil || source != (domain.GitSource{}) {
			t.Fatalf("accepted invalid URL: %+v / %v", source, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("error exposed URL credentials")
		}
	}
}
