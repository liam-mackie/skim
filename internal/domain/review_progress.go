package domain

// ReviewProgressUpdate is transient progress for the exact loaded PR snapshot.
// A nil Run means no review has run for this identity in this app process.
type ReviewProgressUpdate struct {
	PRIdentity
	Run *ReviewProgress `json:"run"`
}

// ReviewProgress tracks one agent review. Phase is preparing (checkout),
// reviewing (agent running), saving (validating its comments) or finished.
// Findings counts validated suggestions once the agent has replied; drafts are
// saved only when the review succeeds.
type ReviewProgress struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Phase      string `json:"phase"`
	Findings   int    `json:"findings"`
	Rejected   int    `json:"rejected"`
	StartedAt  string `json:"startedAt"`
	UpdatedAt  string `json:"updatedAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Error      string `json:"error,omitempty"`
}
