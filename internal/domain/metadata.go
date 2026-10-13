package domain

// InboxFilter narrows remote search without changing authentication or active work.
type InboxFilter struct {
	Username string
	Repo     string
}

type InboxUser struct {
	Login string `json:"login"`
	URL   string `json:"url"`
}

type ReviewSummary struct {
	PRKey     string `json:"prKey"`
	Provider  string `json:"provider"`
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Author    string `json:"author"`
	UpdatedAt string `json:"updatedAt"`
	Draft     bool   `json:"draft"`
	Requested bool   `json:"requested"`
	Assigned  bool   `json:"assigned"`
}

type ReviewInbox struct {
	User     InboxUser       `json:"user"`
	Username string          `json:"username"`
	Repo     string          `json:"repo"`
	Reviews  []ReviewSummary `json:"reviews"`
	Complete bool            `json:"complete"`
	Warnings []string        `json:"warnings"`
}

type PRIdentity struct {
	PRKey   string `json:"prKey"`
	BaseSHA string `json:"baseSha"`
	HeadSHA string `json:"headSha"`
}

func (p PRIdentity) Matches(pr *PullRequest) bool {
	return pr != nil && p.PRKey != "" && p.BaseSHA != "" && p.HeadSHA != "" &&
		p.PRKey == pr.Key() && p.BaseSHA == pr.BaseSHA && p.HeadSHA == pr.HeadSHA
}

type Commit struct {
	SHA         string `json:"sha"`
	Message     string `json:"message"`
	AuthorName  string `json:"authorName"`
	AuthorLogin string `json:"authorLogin"`
	AuthoredAt  string `json:"authoredAt"`
	URL         string `json:"url"`
}

type CommitList struct {
	PRIdentity
	Commits  []Commit `json:"commits"`
	Total    int      `json:"total"`
	Complete bool     `json:"complete"`
	Warnings []string `json:"warnings"`
}
