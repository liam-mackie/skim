package domain

// ReviewProgressUpdate is transient progress for the exact loaded PR snapshot.
// A nil Run means no review has run for this identity in this app process.
type ReviewProgressUpdate struct {
	PRIdentity
	Run *ReviewProgress `json:"run"`
}

// ReviewProgress counts validated prospective findings; drafts are saved only
// when the entire review succeeds, so running or failed findings are not drafts.
type ReviewProgress struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Phase       string `json:"phase"`
	CurrentPath string `json:"currentPath,omitempty"`
	Completed   int    `json:"completed"`
	Total       int    `json:"total"`
	Reviewed    int    `json:"reviewed"`
	Skipped     int    `json:"skipped"`
	Findings    int    `json:"findings"`
	StartedAt   string `json:"startedAt"`
	UpdatedAt   string `json:"updatedAt"`
	FinishedAt  string `json:"finishedAt,omitempty"`
	Error       string `json:"error,omitempty"`
}
