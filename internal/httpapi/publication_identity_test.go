package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"skim/internal/domain"
	"skim/internal/provider"
	"skim/internal/store"
)

func TestPublishRequiresDisplayedSnapshotBeforeProviderOrReconciliation(t *testing.T) {
	h := newHarness(t)
	var original domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &original)
	// A second window has switched the shared backend while the first overview remains open.
	var current domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/8"}, &current)
	h.awaitAnalysis(t, analysisTerminal)
	var providerCalls atomic.Int32
	previous := h.app.Provider
	h.app.Provider = func(settings domain.Settings) provider.Provider {
		providerCalls.Add(1)
		return previous(settings)
	}
	for _, reconciling := range []bool{false, true} {
		if reconciling {
			d := h.store.Snapshot()
			r := d.Reviews[d.Active]
			r.Pending = &store.Publication{Marker: "keep-this-uncertain-review", Event: "APPROVE", HeadSHA: current.PullRequest.HeadSHA, CommentIDs: []string{}}
			d.Reviews[d.Active] = r
			if err := h.store.Commit(d); err != nil {
				t.Fatal(err)
			}
		}
		for _, mode := range []string{"missing-all", "missing-key", "missing-base", "missing-head", "old-pr", "old-base", "old-head"} {
			payload := map[string]string{"event": "APPROVE", "prKey": current.PullRequest.Key(), "baseSha": current.PullRequest.BaseSHA, "headSha": current.PullRequest.HeadSHA}
			switch mode {
			case "missing-all":
				delete(payload, "prKey")
				delete(payload, "baseSha")
				delete(payload, "headSha")
			case "missing-key":
				delete(payload, "prKey")
			case "missing-base":
				delete(payload, "baseSha")
			case "missing-head":
				delete(payload, "headSha")
			case "old-pr":
				payload["prKey"] = original.PullRequest.Key()
			case "old-base":
				payload["baseSha"] = "stale-base"
			case "old-head":
				payload["headSha"] = "stale-head"
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately bypass h.request's normal fixture identity injection.
			response, err := http.Post(h.base+"/api/publish", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			result, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s reconciliation=%v: got %d: %s", mode, reconciling, response.StatusCode, result)
			}
		}
		if providerCalls.Load() != 0 {
			t.Fatal("stale or missing identity reached the remote provider")
		}
		d := h.store.Snapshot()
		pending := d.Reviews[d.Active].Pending
		if reconciling && (pending == nil || pending.Marker != "keep-this-uncertain-review") {
			t.Fatal("stale publication changed its reconciliation journal")
		}
		if !reconciling && pending != nil {
			t.Fatal("rejected publication created a journal entry")
		}
	}
	h.fixture.mu.Lock()
	defer h.fixture.mu.Unlock()
	if h.fixture.posts != 0 {
		t.Fatal("stale overview published a review")
	}
}
