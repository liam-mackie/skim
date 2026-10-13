package provider

import (
	"fmt"
	"strconv"

	"skim/internal/domain"
)

// GitSource fetches the base repository's pull ref, including for fork PRs.
// API fixture overrides and the contributor's branch never select a Git remote.
func (g *GitHub) GitSource(pr domain.PullRequest) (domain.GitSource, error) {
	if _, err := pullPath(pr); err != nil {
		return domain.GitSource{}, err
	}
	parsed, err := ParseGitHubURL(pr.URL)
	if err != nil || parsed.Owner != pr.Owner || parsed.Repo != pr.Repo || parsed.Number != pr.Number {
		return domain.GitSource{}, fmt.Errorf("invalid or mismatched GitHub pull-request URL")
	}
	return domain.GitSource{
		URL:     "https://github.com/" + pr.Owner + "/" + pr.Repo + ".git",
		HeadRef: "refs/pull/" + strconv.Itoa(pr.Number) + "/head",
	}, nil
}
