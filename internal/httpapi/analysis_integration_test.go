package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"skim/internal/domain"
)

// These gates delay real model HTTP responses, never application endpoints.
// Releasing a gate affects requests already waiting as well as future requests.
type analysisModelControl struct {
	mu                sync.Mutex
	riskGates         map[string]chan struct{}
	groupGate         chan struct{}
	riskStarted       map[string]int
	groupStarted      int
	foregroundGate    chan struct{}
	foregroundStarted int
	reviewGate        chan struct{}
	chatGate          chan struct{}
	reviewStarted     int
	chatStarted       int
	findingsGate      chan struct{}
	findingsStarted   int
	findingsContent   *string
	findingsFail      bool
	riskCancelled     int
	groupCancelled    int
	failRisk          map[string]bool
	invalidGrouping   bool
}

func newAnalysisModelControl() *analysisModelControl {
	return &analysisModelControl{
		riskGates:   map[string]chan struct{}{},
		riskStarted: map[string]int{},
		failRisk:    map[string]bool{},
	}
}

func (c *analysisModelControl) hold(paths []string, grouping bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, path := range paths {
		if c.riskGates[path] == nil {
			c.riskGates[path] = make(chan struct{})
		}
	}
	if grouping && c.groupGate == nil {
		c.groupGate = make(chan struct{})
	}
}

func (c *analysisModelControl) release(paths []string, grouping, all bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if all {
		paths = make([]string, 0, len(c.riskGates))
		for path := range c.riskGates {
			paths = append(paths, path)
		}
	}
	for _, path := range paths {
		if gate := c.riskGates[path]; gate != nil {
			close(gate)
			delete(c.riskGates, path)
		}
	}
	if (grouping || all) && c.groupGate != nil {
		close(c.groupGate)
		c.groupGate = nil
	}
	if all && c.foregroundGate != nil {
		close(c.foregroundGate)
		c.foregroundGate = nil
	}
	if all && c.reviewGate != nil {
		close(c.reviewGate)
		c.reviewGate = nil
	}
	if all && c.chatGate != nil {
		close(c.chatGate)
		c.chatGate = nil
	}
	if all && c.findingsGate != nil {
		close(c.findingsGate)
		c.findingsGate = nil
	}
}

func (c *analysisModelControl) mode(invalid bool, failed []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidGrouping = invalid
	c.failRisk = make(map[string]bool, len(failed))
	for _, path := range failed {
		c.failRisk[path] = true
	}
}

func (c *analysisModelControl) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var input struct {
			State    json.RawMessage  `json:"state"`
			Messages []domain.Message `json:"messages"`
		}
		if err := json.Unmarshal(body, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var gate <-chan struct{}
		var fail, invalid bool
		var riskRequest, groupRequest bool
		var findingsContent *string
		if r.URL.Path == "/v1/systemone" {
			riskRequest = true
			var state struct {
				File domain.File `json:"file"`
			}
			if err := json.Unmarshal(input.State, &state); err != nil || state.File.Path == "" {
				http.Error(w, "missing per-file risk state", http.StatusBadRequest)
				return
			}
			c.mu.Lock()
			c.riskStarted[state.File.Path]++
			gate = c.riskGates[state.File.Path]
			fail = c.failRisk[state.File.Path]
			c.mu.Unlock()
		} else if len(input.Messages) > 0 && strings.HasPrefix(input.Messages[0].Content, "Task: group") {
			groupRequest = true
			c.mu.Lock()
			c.groupStarted++
			gate = c.groupGate
			invalid = c.invalidGrouping
			c.mu.Unlock()
		} else if len(input.Messages) > 0 && strings.HasPrefix(input.Messages[0].Content, "Task: a separate classifier") {
			// Risk factors are background analysis, never gated foreground work.
		} else {
			c.mu.Lock()
			c.foregroundStarted++
			gate = c.foregroundGate
			if len(input.Messages) > 0 && strings.Contains(input.Messages[0].Content, "Review this changed file") {
				c.reviewStarted++
				if c.reviewGate != nil {
					gate = c.reviewGate
				}
			}
			if len(input.Messages) > 0 && strings.Contains(input.Messages[0].Content, "Extract only concise, actionable") {
				c.findingsStarted++
				if c.findingsGate != nil {
					gate = c.findingsGate
				}
				findingsContent, fail = c.findingsContent, c.findingsFail
			}
			if len(input.Messages) > 0 && strings.Contains(input.Messages[0].Content, "Help the user locate") {
				c.chatStarted++
				if c.chatGate != nil {
					gate = c.chatGate
				}
			}
			c.mu.Unlock()
		}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				c.mu.Lock()
				if riskRequest {
					c.riskCancelled++
				}
				if groupRequest {
					c.groupCancelled++
				}
				c.mu.Unlock()
				return
			}
		}
		if fail {
			http.Error(w, "fixture risk service unavailable", http.StatusBadGateway)
			return
		}
		if findingsContent != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message":       map[string]string{"role": "assistant", "content": *findingsContent},
				"finish_reason": "stop",
			}}})
			return
		}
		if invalid {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message":       map[string]string{"role": "assistant", "content": `{"groups":[{"title":"Invalid membership","summary":"Changes related implementation.","paths":["src/calc.go","src/calc.go"]}]}`},
				"finish_reason": "stop",
			}}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *harness) awaitAnalysis(t *testing.T, ready func(domain.AnalysisUpdate) bool) domain.AnalysisUpdate {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var update domain.AnalysisUpdate
	for {
		h.request(t, http.MethodGet, "/api/analysis", nil, &update)
		if ready(update) {
			return update
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("analysis did not reach expected observable state: %+v", update)
		}
	}
}

func analysisTerminal(update domain.AnalysisUpdate) bool {
	return update.Analysis != nil && update.Analysis.Risk.Status != "running" && update.Analysis.Grouping.Status != "running"
}

// riskProgressComplete reports whether every file was assessed and every
// flagged (medium/high) file was explained: progress counts both passes.
func riskProgressComplete(update domain.AnalysisUpdate) bool {
	flagged := 0
	for _, file := range update.Files {
		if file.Risk != nil && file.Risk.Level != "low" {
			flagged++
		}
	}
	task := update.Analysis.Risk
	return task.Total == len(update.Files)+flagged && task.Completed == task.Total
}

func TestHTTPAnalysisProgressAndScopedWrites(t *testing.T) {
	control := newAnalysisModelControl()
	control.hold([]string{"src/calc.go", "src/calc_test.go"}, true)
	h := newHarnessWithModels(t, control)
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	if state.PullRequest == nil || len(state.Files) != 2 || state.Files[0].Patch == "" || state.Analysis == nil || state.Analysis.Risk.Status != "running" || state.Analysis.Grouping.Status != "running" {
		t.Fatalf("load did not return usable state while models were held: %+v", state)
	}
	generation := state.Analysis.Generation
	h.request(t, http.MethodPut, "/api/style", map[string]string{"style": "Keep this foreground edit."}, nil)
	h.request(t, http.MethodGet, "/api/state", nil, &state)
	if state.Style != "Keep this foreground edit." || state.Analysis.Generation != generation {
		t.Fatal("foreground write was blocked or reset analysis")
	}
	// Review and chat use the real text peer and remain request-owned operations.
	h.request(t, http.MethodPost, "/api/review", struct{}{}, &state)
	if len(state.Comments) != 2 {
		t.Fatalf("review did not remain usable: %+v", state.Comments)
	}
	commentID := state.Comments[0].ID
	h.request(t, http.MethodPatch, "/api/comments/"+commentID, map[string]string{"body": "Retain the edited comment."}, nil)
	var chat domain.ChatThread
	h.request(t, http.MethodPost, "/api/chat", map[string]string{"message": "Explain the change."}, &chat)
	var settings domain.Settings
	h.request(t, http.MethodGet, "/api/settings", nil, &settings)
	settings.ACP = domain.ACPSettings{}
	h.request(t, http.MethodPut, "/api/settings", settings, nil)

	control.release([]string{"src/calc.go"}, false, false)
	partial := h.awaitAnalysis(t, func(u domain.AnalysisUpdate) bool {
		control.mu.Lock()
		laterStarted := control.riskStarted["src/calc_test.go"] > 0
		control.mu.Unlock()
		return laterStarted && u.Analysis != nil && u.Analysis.Risk.Completed == 1
	})
	if partial.Analysis.Risk.Status != "running" || partial.Analysis.Grouping.Status != "running" || partial.Analysis.Risk.Total != 2 || partial.Analysis.Generation != generation {
		t.Fatalf("first risk was not committed independently: %+v", partial)
	}
	if len(partial.Files) != 2 || partial.Files[0].Path != "src/calc.go" || partial.Files[0].Risk == nil || partial.Files[0].TestOnly == nil || partial.Files[1].Risk != nil || partial.Files[1].TestOnly != nil {
		t.Fatalf("unexpected progressive badges: %+v", partial.Files)
	}
	var scoped map[string]json.RawMessage
	h.request(t, http.MethodGet, "/api/analysis", nil, &scoped)
	if len(scoped) != 3 || scoped["analysis"] == nil || scoped["files"] == nil || scoped["groups"] == nil {
		t.Fatalf("poll response leaked state fields: %v", scoped)
	}
	var files []map[string]json.RawMessage
	if err := json.Unmarshal(scoped["files"], &files); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		for key := range file {
			if key != "path" && key != "risk" && key != "testOnly" {
				t.Fatalf("poll response leaked file field %q", key)
			}
		}
	}
	var failure map[string]string
	if status := h.request(t, http.MethodPost, "/api/classify", struct{}{}, &failure); status != http.StatusNotFound {
		t.Fatalf("obsolete classify route still exists: %d", status)
	}
	control.release(nil, false, true)
	complete := h.awaitAnalysis(t, analysisTerminal)
	if complete.Analysis.Risk.Status != "complete" || complete.Analysis.Grouping.Status != "complete" || complete.Analysis.Revision <= partial.Analysis.Revision {
		t.Fatalf("analysis did not finish progressively: %+v", complete)
	}
	h.request(t, http.MethodGet, "/api/state", nil, &state)
	if state.Style != "Keep this foreground edit." || state.Comments[0].ID != commentID || state.Comments[0].Body != "Retain the edited comment." || len(state.Chats) != 1 || state.Chats[0].ID != chat.ID || state.Settings.ACP.Command != "" || len(state.Settings.ACP.Args) != 0 || state.Settings.ACP.ManagedRoot != h.dataDir+"/workspaces" {
		t.Fatal("background commits overwrote foreground work")
	}
}

func TestHTTPAnalysisIndependentFailuresAndGroupOrder(t *testing.T) {
	for _, riskFailure := range []bool{true, false} {
		name := "grouping-fails"
		if riskFailure {
			name = "risk-fails"
		}
		t.Run(name, func(t *testing.T) {
			control := newAnalysisModelControl()
			if riskFailure {
				control.mode(false, []string{"src/calc_test.go"})
			} else {
				control.mode(true, nil)
			}
			h := newHarnessWithModels(t, control)
			h.fixture.mu.Lock()
			h.fixture.mixed = true
			h.fixture.mu.Unlock()
			var state domain.State
			h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
			update := h.awaitAnalysis(t, analysisTerminal)
			if !riskProgressComplete(update) || update.Analysis.Risk.Total <= 4 || len(update.Analysis.Risk.SkippedFiles) != 2 || update.Files[0].Risk == nil || update.Files[2].Risk != nil || update.Files[3].Risk != nil {
				t.Fatalf("risk scope or partial success lost: %+v", update)
			}
			if f := update.Files[0].Risk; f.Level != "medium" || len(f.Factors) != 1 || f.Factors[0].Side != "RIGHT" || f.Factors[0].StartLine != 2 || f.FactorsError != "" {
				t.Fatalf("flagged file did not receive validated factors: %+v", f)
			}
			if riskFailure {
				if update.Analysis.Risk.Status != "failed" || len(update.Analysis.Risk.FailedFiles) != 1 || update.Analysis.Risk.FailedFiles[0].Path != "src/calc_test.go" || update.Analysis.Grouping.Status != "complete" || update.Analysis.Grouping.Completed != update.Analysis.Grouping.Total {
					t.Fatalf("risk failure blocked grouping: %+v", update.Analysis)
				}
				if len(update.Groups) != 2 || !reflect.DeepEqual(update.Groups[0].Paths, []string{"generated.txt", "assets/logo.png"}) || !reflect.DeepEqual(update.Groups[1].Paths, []string{"src/calc_test.go", "src/calc.go"}) || update.Groups[0].Title != "Change unit 1" || update.Groups[1].Title != "Change unit 2" || update.Groups[0].ID == "" || update.Groups[0].ID == update.Groups[1].ID {
					t.Fatalf("model group/path order or membership was changed: %+v", update.Groups)
				}
			} else if update.Analysis.Grouping.Status != "failed" || update.Analysis.Grouping.Error == "" || len(update.Groups) != 0 || update.Analysis.Risk.Status != "complete" || update.Files[1].Risk == nil {
				t.Fatalf("invalid grouping was accepted or blocked risks: %+v", update)
			}
			control.mu.Lock()
			calls := len(control.riskStarted)
			groupCalls := control.groupStarted
			control.mu.Unlock()
			if calls != 2 || groupCalls != 1 {
				t.Fatalf("unexpected model scope: risk paths=%d grouping calls=%d", calls, groupCalls)
			}
		})
	}
}

func TestHTTPAnalysisRefreshGeneration(t *testing.T) {
	control := newAnalysisModelControl()
	h := newHarnessWithModels(t, control)
	var empty domain.AnalysisUpdate
	h.request(t, http.MethodGet, "/api/analysis", nil, &empty)
	if empty.Analysis != nil || empty.Files == nil || empty.Groups == nil || len(empty.Files) != 0 || len(empty.Groups) != 0 {
		t.Fatalf("unexpected analysis without PR: %+v", empty)
	}
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	first := h.awaitAnalysis(t, analysisTerminal)
	control.hold([]string{"src/calc.go", "src/calc_test.go"}, true)
	var refreshed domain.AnalysisUpdate
	h.request(t, http.MethodPost, "/api/analysis", struct{}{}, &refreshed)
	if refreshed.Analysis == nil || refreshed.Analysis.Generation == "" || refreshed.Analysis.Generation == first.Analysis.Generation || refreshed.Analysis.Key != first.Analysis.Key || refreshed.Analysis.BaseSHA != first.Analysis.BaseSHA || refreshed.Analysis.HeadSHA != first.Analysis.HeadSHA || len(refreshed.Groups) != 0 || refreshed.Analysis.Risk.Status != "running" || refreshed.Analysis.Grouping.Status != "running" {
		t.Fatalf("refresh did not immediately return a new scoped generation: %+v", refreshed)
	}
	control.release(nil, false, true)
	complete := h.awaitAnalysis(t, analysisTerminal)
	if complete.Analysis.Generation != refreshed.Analysis.Generation || complete.Analysis.Risk.Status != "complete" || complete.Analysis.Grouping.Status != "complete" {
		t.Fatalf("refresh result has wrong generation: %+v", complete)
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.riskStarted["src/calc.go"] != 2 || control.riskStarted["src/calc_test.go"] != 2 || control.groupStarted != 2 {
		t.Fatalf("refresh did not rerun both jobs: risks=%v groups=%d", control.riskStarted, control.groupStarted)
	}
}
