package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"skim/internal/app"
	"skim/internal/domain"
	"skim/internal/store"
)

func waitCondition(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatal("controlled operation did not reach its expected state")
		}
	}
}

func TestAnalysisCommitsDuringHeldForegroundModels(t *testing.T) {
	for _, route := range []string{"/api/chat", "/api/review", "/api/style/import"} {
		t.Run(route, func(t *testing.T) {
			control := newAnalysisModelControl()
			control.hold([]string{"src/calc.go", "src/calc_test.go"}, true)
			control.foregroundGate = make(chan struct{})
			h := newHarnessWithModels(t, control)
			t.Cleanup(func() { control.release(nil, false, true) })
			// The agent review is held on its permission request, not a model gate.
			if route == "/api/review" {
				h.agentReview(t, "", true)
			}
			var loaded domain.State
			h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &loaded)
			done := make(chan struct{})
			go func() {
				defer close(done)
				var input any = struct{}{}
				if route == "/api/chat" {
					input = map[string]string{"message": "Explain this change."}
				}
				h.request(t, "POST", route, input, nil)
			}()
			reviewPermission := ""
			if route == "/api/review" {
				reviewPermission = awaitReviewPermission(t, h)
			} else {
				waitCondition(t, func() bool {
					control.mu.Lock()
					defer control.mu.Unlock()
					return control.foregroundStarted > 0
				})
			}
			control.release([]string{"src/calc.go"}, true, false)
			partial := h.awaitAnalysis(t, func(u domain.AnalysisUpdate) bool {
				return u.Analysis != nil && u.Analysis.Risk.Completed == 1 && u.Analysis.Grouping.Status == "complete"
			})
			if partial.Files[0].Risk == nil || len(partial.Groups) == 0 {
				t.Fatal("held foreground request blocked analysis commits")
			}
			select {
			case <-done:
				t.Fatal("foreground operation unexpectedly completed before gate release")
			default:
			}
			control.release(nil, false, true)
			if reviewPermission != "" {
				h.request(t, "POST", "/api/permissions/"+reviewPermission, map[string]string{"optionId": "allow-once"}, nil)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("foreground did not finish after release")
			}
			complete := h.awaitAnalysis(t, analysisTerminal)
			var state domain.State
			h.request(t, "GET", "/api/state", nil, &state)
			if state.Files[0].Risk == nil || state.Files[1].Risk == nil || state.Files[0].TestOnly == nil || state.Files[1].TestOnly == nil || state.Analysis.Revision != complete.Analysis.Revision || len(state.Groups) == 0 {
				t.Fatal("foreground snapshot overwrote analysis completed during model request")
			}
			if route == "/api/chat" && len(state.Chats) != 1 || route == "/api/review" && len(state.Comments) != 2 || route == "/api/style/import" && state.Style == "" {
				t.Fatal("analysis commit overwrote foreground result")
			}
		})
	}
}

func TestAnalysisSwitchCancellationAndInterruptedResume(t *testing.T) {
	control := newAnalysisModelControl()
	control.hold([]string{"src/calc_test.go"}, true)
	h := newHarnessWithModels(t, control)
	var first domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &first)
	partial := h.awaitAnalysis(t, func(u domain.AnalysisUpdate) bool {
		control.mu.Lock()
		defer control.mu.Unlock()
		return u.Analysis != nil && u.Analysis.Risk.Completed == 1 && control.riskStarted["src/calc_test.go"] == 1 && control.groupStarted == 1
	})
	oldRecord := h.store.Snapshot().Reviews[partial.Analysis.Key]
	var second domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/8"}, &second)
	waitCondition(t, func() bool {
		control.mu.Lock()
		defer control.mu.Unlock()
		return control.riskCancelled == 1 && control.groupCancelled == 1
	})
	// Even a non-cooperative completion with a live context cannot commit to the
	// new active PR. This exercises the store guard independently of HTTP cancel.
	if err := h.store.UpdateAnalysis(context.Background(), *partial.Analysis, oldRecord.AnalysisSnapshot, func(r *store.Record) {
		r.State.Analysis.Grouping.Status = "complete"
		r.State.Groups = []domain.Group{{ID: "stale", Title: "Stale", Summary: "Stale summary.", Paths: []string{"src/calc.go"}}}
	}); err == nil {
		t.Fatal("stale completion from switched PR was accepted")
	}
	control.release(nil, false, true)
	secondComplete := h.awaitAnalysis(t, analysisTerminal)
	if secondComplete.Analysis.Key != second.PullRequest.Key() || secondComplete.Analysis.Generation == first.Analysis.Generation {
		t.Fatal("PR switch retained old run identity")
	}
	var resumed domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &resumed)
	finished := h.awaitAnalysis(t, analysisTerminal)
	if resumed.Files[0].Risk == nil || !riskProgressComplete(finished) || finished.Analysis.Generation == partial.Analysis.Generation {
		t.Fatal("interrupted run did not preserve successful risk and resume remaining work")
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.riskStarted["src/calc.go"] != 2 || control.riskStarted["src/calc_test.go"] != 3 || control.groupStarted != 3 {
		t.Fatalf("resume repeated successful per-file calls: risks=%v groups=%d", control.riskStarted, control.groupStarted)
	}
}

func TestAnalysisReuseConfigurationAndExactSnapshot(t *testing.T) {
	control := newAnalysisModelControl()
	h := newHarnessWithModels(t, control)
	load := func() domain.State {
		var s domain.State
		h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &s)
		return s
	}
	load()
	first := h.awaitAnalysis(t, analysisTerminal)
	load()
	reused := h.awaitAnalysis(t, analysisTerminal)
	if reused.Files[0].Risk == nil || len(reused.Groups) == 0 {
		t.Fatal("same snapshot lost completed analysis")
	}
	control.mu.Lock()
	repeated := control.groupStarted != 1 || control.riskStarted["src/calc.go"] != 1 || control.riskStarted["src/calc_test.go"] != 1
	control.mu.Unlock()
	if repeated {
		t.Fatal("same snapshot repeated paid calls")
	}
	settings := h.app.Settings()
	settings.Roles.Review.Model = "different-text-model"
	h.request(t, "PUT", "/api/settings", settings, nil)
	changed := h.awaitAnalysis(t, analysisTerminal)
	if changed.Analysis.Generation == reused.Analysis.Generation || changed.Files[0].Risk == nil {
		t.Fatal("review config did not create scoped run preserving risks")
	}
	control.mu.Lock()
	riskReran := control.groupStarted != 2 || control.riskStarted["src/calc.go"] != 1
	control.mu.Unlock()
	if riskReran {
		t.Fatal("review config reran unaffected risk")
	}
	settings.Roles.Chat.Model = "different-chat-model"
	h.request(t, "PUT", "/api/settings", settings, nil)
	if h.app.Analysis().Analysis.Generation != changed.Analysis.Generation {
		t.Fatal("unrelated chat configuration reset analysis")
	}
	// Identical base/head refs are insufficient: complete evidence membership is
	// part of the saved signature, including files absent from a prior response.
	h.fixture.mu.Lock()
	h.fixture.mixed = true
	h.fixture.mu.Unlock()
	load()
	expanded := h.awaitAnalysis(t, analysisTerminal)
	if expanded.Analysis.HeadSHA != first.Analysis.HeadSHA || !riskProgressComplete(expanded) || len(expanded.Files) != 4 || len(expanded.Groups) != 2 || len(expanded.Analysis.Risk.SkippedFiles) != 2 {
		t.Fatal("same-ref changed file set did not invalidate analysis")
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.groupStarted != 3 || control.riskStarted["src/calc.go"] != 2 {
		t.Fatal("changed evidence did not schedule new model evidence")
	}
}

func TestAnalysisFailureAttemptsAndSettingsCancellation(t *testing.T) {
	control := newAnalysisModelControl()
	control.mode(true, []string{"src/calc_test.go"})
	h := newHarnessWithModels(t, control)
	var state domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	failed := h.awaitAnalysis(t, analysisTerminal)
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	retained := h.awaitAnalysis(t, analysisTerminal)
	if retained.Analysis.Risk.Status != "failed" || retained.Analysis.Grouping.Status != "failed" || retained.Files[0].Risk == nil {
		t.Fatal("reload discarded failure or successful partial result")
	}
	control.mu.Lock()
	retried := control.groupStarted != 1 || control.riskStarted["src/calc_test.go"] != 1
	control.mu.Unlock()
	if retried {
		t.Fatal("failed attempts silently retried on reload")
	}
	control.mode(false, nil)
	control.hold([]string{"src/calc.go"}, true)
	var refreshed domain.AnalysisUpdate
	h.request(t, "POST", "/api/analysis", struct{}{}, &refreshed)
	waitCondition(t, func() bool {
		control.mu.Lock()
		defer control.mu.Unlock()
		return control.riskStarted["src/calc.go"] == 2 && control.groupStarted == 2
	})
	oldRecord := h.store.Snapshot().Reviews[refreshed.Analysis.Key]
	settings := h.app.Settings()
	settings.Roles.Jev.Model = "different-jev"
	h.request(t, "PUT", "/api/settings", settings, nil)
	waitCondition(t, func() bool {
		control.mu.Lock()
		defer control.mu.Unlock()
		return control.riskCancelled > 0 && control.groupCancelled > 0
	})
	if err := h.store.UpdateAnalysis(context.Background(), *refreshed.Analysis, oldRecord.AnalysisSnapshot, func(r *store.Record) { r.State.Files[0].Risk = &domain.Risk{Level: "high"} }); err == nil {
		t.Fatal("same-head old generation completion was accepted")
	}
	control.release(nil, false, true)
	complete := h.awaitAnalysis(t, analysisTerminal)
	if complete.Analysis.Generation == refreshed.Analysis.Generation || complete.Analysis.Generation == failed.Analysis.Generation || complete.Analysis.Risk.Status != "complete" || complete.Analysis.Grouping.Status != "complete" {
		t.Fatal("settings replacement did not finish independent fresh work")
	}
}

func TestAnalysisCloseCancelsAndRejectsLateCompletion(t *testing.T) {
	control := newAnalysisModelControl()
	control.hold([]string{"src/calc.go"}, true)
	h := newHarnessWithModels(t, control)
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	waitCondition(t, func() bool {
		control.mu.Lock()
		defer control.mu.Unlock()
		return control.riskStarted["src/calc.go"] == 1 && control.groupStarted == 1
	})
	if err := h.app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Close(); err != nil {
		t.Fatal(err)
	}
	update := h.app.Analysis()
	if update.Analysis.Risk.Status != "cancelled" || update.Analysis.Grouping.Status != "cancelled" {
		t.Fatal("shutdown did not expose interrupted analysis")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := h.store.Snapshot().Reviews[update.Analysis.Key]
	if err := h.store.UpdateAnalysis(ctx, *update.Analysis, r.AnalysisSnapshot, func(r *store.Record) { r.State.Files[0].Risk = &domain.Risk{Level: "high"} }); err == nil {
		t.Fatal("cancelled operation committed after shutdown")
	}
}

func TestAnalysisRestartReusesCompleteAndResumesInterrupted(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "complete"
		if interrupted {
			name = "interrupted"
		}
		t.Run(name, func(t *testing.T) {
			control := newAnalysisModelControl()
			if interrupted {
				control.hold([]string{"src/calc_test.go"}, true)
			}
			h := newHarnessWithModels(t, control)
			var state domain.State
			h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
			if interrupted {
				h.awaitAnalysis(t, func(u domain.AnalysisUpdate) bool {
					control.mu.Lock()
					defer control.mu.Unlock()
					return u.Analysis.Risk.Completed == 1 && control.riskStarted["src/calc_test.go"] == 1 && control.groupStarted == 1
				})
			} else {
				h.awaitAnalysis(t, analysisTerminal)
			}
			if err := h.app.Close(); err != nil {
				t.Fatal(err)
			}
			if err := h.store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(h.dataDir, domain.Settings{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			restarted := app.New(reopened)
			defer restarted.Close()
			restarted.Provider = h.app.Provider
			control.release(nil, false, true)
			loaded, err := restarted.Load(context.Background(), "https://github.com/acme/widgets/pull/7")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Files[0].Risk == nil {
				t.Fatal("restart discarded a saved per-file decision")
			}
			waitCondition(t, func() bool { return analysisTerminal(restarted.Analysis()) })
			complete := restarted.Analysis()
			if complete.Analysis.Risk.Status != "complete" || complete.Analysis.Grouping.Status != "complete" || !riskProgressComplete(complete) {
				t.Fatalf("restart did not reach complete analysis: %+v", complete)
			}
			control.mu.Lock()
			firstCalls, laterCalls, groupCalls := control.riskStarted["src/calc.go"], control.riskStarted["src/calc_test.go"], control.groupStarted
			control.mu.Unlock()
			expected := 1
			if interrupted {
				expected = 2
			}
			if firstCalls != 1 || laterCalls != expected || groupCalls != expected {
				t.Fatalf("restart repeated completed work or missed interrupted calls: %d %d %d", firstCalls, laterCalls, groupCalls)
			}
		})
	}
}

func TestRiskContinuesDuringPendingACPPermission(t *testing.T) {
	control := newAnalysisModelControl()
	control.hold([]string{"src/calc.go", "src/calc_test.go"}, true)
	h := newHarnessWithModels(t, control)
	t.Cleanup(func() { control.release(nil, false, true) })
	var loaded domain.State
	h.request(t, "POST", "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &loaded)
	done := startVerification(t, h, context.Background())
	waitCondition(t, func() bool { return len(h.app.Agent.Permissions()) != 0 })
	permission := h.app.Agent.Permissions()[0]
	control.release([]string{"src/calc.go"}, true, false)
	partial := h.awaitAnalysis(t, func(u domain.AnalysisUpdate) bool {
		return u.Analysis.Risk.Completed == 1 && u.Analysis.Grouping.Status == "complete"
	})
	if partial.Files[0].Risk == nil || len(h.app.Agent.Permissions()) != 1 {
		t.Fatal("ACP approval wait prevented progressive analysis")
	}
	h.request(t, "POST", "/api/permissions/"+permission.ID, map[string]string{"optionId": "allow-once"}, nil)
	control.release(nil, false, true)
	result := awaitStateOperation(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}
	h.awaitAnalysis(t, analysisTerminal)
	if len(result.state.Comments) != 1 || result.state.Comments[0].Source != "verification" || h.app.State().Files[0].Risk == nil {
		t.Fatal("ACP and analysis results did not survive independent commits")
	}
}
