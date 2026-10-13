package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"skim/internal/domain"
	"skim/internal/model"
	"skim/internal/provider"
	"skim/internal/store"
)

// analysisRun owns immutable evidence. Only scoped store commits take a lock;
// neither model shares the request context or can hold up foreground actions.
type analysisRun struct {
	ctx                       context.Context
	cancel                    context.CancelFunc
	key, snapshot, generation string
	baseSHA, headSHA          string
}

func analysisSnapshot(pr domain.PullRequest, files []domain.File) string {
	h := sha256.New()
	e := json.NewEncoder(h)
	_ = e.Encode(pr)
	for _, file := range files {
		file.Risk = nil
		file.TestOnly = nil
		_ = e.Encode(file)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func roleConfig(settings domain.Settings, role domain.ModelRole) string {
	var selected domain.ModelProvider
	for _, p := range settings.Providers {
		if p.ID == role.Provider {
			selected = p
			break
		}
	}
	credential := selected.CredentialRevision
	if credential == "" && selected.APIKeyEnv != "" {
		// Environment values are process-local; only their digest enters cache identity.
		sum := sha256.Sum256([]byte(strings.TrimSpace(os.Getenv(selected.APIKeyEnv))))
		credential = hex.EncodeToString(sum[:])
	}
	// Keep the connection identity's original JSON shape so adding display names
	// neither invalidates saved successful results nor restarts in-flight analysis.
	type connection struct {
		ID        string `json:"id"`
		Kind      string `json:"kind,omitempty"`
		BaseURL   string `json:"baseUrl"`
		APIKeyEnv string `json:"apiKeyEnv"`
	}
	data, _ := json.Marshal(struct {
		Role       domain.ModelRole
		Provider   connection
		Credential string
	}{role, connection{selected.ID, selected.Kind, selected.BaseURL, selected.APIKeyEnv}, credential})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func riskRoleConfig(settings domain.Settings) string {
	return roleConfig(settings, settings.Roles.Jev) + ":" + model.RiskPolicyVersion
}

// jevConfigured reports whether risk analysis is enabled. Without a Jev role the
// risk task is "disabled": no labels, test-only collapse, or errors are produced.
func jevConfigured(settings domain.Settings) bool {
	return settings.Roles.Jev.Provider != "" && settings.Roles.Jev.Model != ""
}

// needsFactors reports whether a flagged medium/high file still lacks factors.
func needsFactors(risk *domain.Risk) bool {
	return risk != nil && (risk.Level == "medium" || risk.Level == "high") && risk.Factors == nil && risk.FactorsError == ""
}

func groupingRoleConfig(settings domain.Settings) string {
	return roleConfig(settings, settings.Roles.Review) + ":" + model.GroupingPolicyVersion
}

func analysisError(settings domain.Settings, err error) string {
	message := err.Error()
	redact := func(name string) {
		if value := os.Getenv(name); value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	redact(settings.GitHubTokenEnv)
	for _, provider := range settings.Providers {
		redact(provider.APIKeyEnv)
	}
	return message
}

// Callers hold the foreground gate. Cancel before taking the snapshot so every
// successful commit preceding cancellation is retained when a run is replaced.
func (a *App) cancelAnalysis() store.Data {
	if a.run != nil {
		a.run.cancel()
	}
	d := a.Store.Snapshot()
	if a.run == nil {
		return d
	}
	r, ok := d.Reviews[a.run.key]
	if ok && r.State.Analysis != nil && r.State.Analysis.Generation == a.run.generation {
		changed := false
		for _, task := range []*domain.AnalysisTask{&r.State.Analysis.Risk, &r.State.Analysis.Grouping} {
			if task.Status == "running" {
				task.Status = "cancelled"
				task.Error = "Analysis interrupted; reload the pull request to resume."
				changed = true
			}
		}
		if changed {
			r.State.Analysis.Revision++
			d.Reviews[a.run.key] = r
		}
	}
	a.run = nil
	return d
}

func (a *App) prepareAnalysis(r *store.Record, settings domain.Settings, snapshot string, fresh, forceRisk, forceGrouping bool) {
	riskConfig := riskRoleConfig(settings)
	groupingConfig := groupingRoleConfig(settings)
	fresh = fresh || r.State.Analysis == nil || r.AnalysisSnapshot != snapshot
	forceRisk = fresh || forceRisk || r.RiskConfig != riskConfig
	forceGrouping = fresh || forceGrouping || r.GroupingConfig != groupingConfig
	if !forceRisk && r.State.Analysis != nil && r.State.Analysis.Risk.Status == "complete" {
		for _, file := range r.State.Files {
			if file.PatchStatus == "available" && (file.Risk == nil || file.TestOnly == nil) {
				forceRisk = true
				break
			}
		}
	}
	if fresh {
		r.State.Analysis = &domain.Analysis{}
	}
	analysis := r.State.Analysis
	analysis.Key = r.State.PullRequest.Key()
	analysis.BaseSHA = r.State.PullRequest.BaseSHA
	analysis.HeadSHA = r.State.PullRequest.HeadSHA
	analysis.Generation = domain.NewID()
	analysis.Revision = 1
	switch {
	case !jevConfigured(settings):
		analysis.Risk = domain.AnalysisTask{Status: "disabled"}
		for i := range r.State.Files {
			r.State.Files[i].Risk = nil
			r.State.Files[i].TestOnly = nil
		}
	case forceRisk || analysis.Risk.Status == "disabled":
		analysis.Risk = domain.AnalysisTask{Status: "running", Total: len(r.State.Files)}
		for i := range r.State.Files {
			r.State.Files[i].Risk = nil
			r.State.Files[i].TestOnly = nil
		}
	case analysis.Risk.Status == "cancelled" || analysis.Risk.Status == "running" || slices.ContainsFunc(r.State.Files, func(f domain.File) bool { return needsFactors(f.Risk) }):
		analysis.Risk.Status = "running"
		analysis.Risk.Error = ""
	}
	if forceGrouping {
		analysis.Grouping = domain.AnalysisTask{Status: "running", Total: len(r.State.Files)}
		r.State.Groups = []domain.Group{}
	} else if analysis.Grouping.Status == "cancelled" || analysis.Grouping.Status == "running" {
		analysis.Grouping.Status = "running"
		analysis.Grouping.Error = ""
	}
	r.AnalysisSnapshot = snapshot
	r.RiskConfig = riskConfig
	r.GroupingConfig = groupingConfig
}

func (a *App) startAnalysis(r store.Record, settings domain.Settings) {
	analysis := r.State.Analysis
	if a.lifetime.Err() != nil || (analysis.Risk.Status != "running" && analysis.Grouping.Status != "running") {
		return
	}
	ctx, cancel := context.WithCancel(a.lifetime)
	run := &analysisRun{ctx: ctx, cancel: cancel, key: analysis.Key, snapshot: r.AnalysisSnapshot, generation: analysis.Generation, baseSHA: analysis.BaseSHA, headSHA: analysis.HeadSHA}
	a.run = run
	// Risk factors use the same review model as grouping; they wait for the guide.
	groupingDone := make(chan struct{})
	if analysis.Grouping.Status == "running" {
		a.jobs.Add(1)
		go func() {
			defer a.jobs.Done()
			defer close(groupingDone)
			plan, err := a.Models.PlanGroups(ctx, settings, *r.State.PullRequest, r.State.Files)
			_ = a.commitAnalysis(run, func(current *store.Record) {
				task := &current.State.Analysis.Grouping
				if err != nil {
					task.Status = "failed"
					task.Error = "Recommended review units: " + analysisError(settings, err)
					return
				}
				current.State.Groups = plan.Groups
				task.Status = "complete"
				task.Completed = task.Total
			})
		}()
	} else {
		close(groupingDone)
	}
	if analysis.Risk.Status == "running" {
		a.jobs.Add(1)
		go func() {
			defer a.jobs.Done()
			a.assessRisks(run, r, settings, groupingDone)
		}()
	}
}

func (a *App) assessRisks(run *analysisRun, initial store.Record, settings domain.Settings, groupingDone <-chan struct{}) {
	processed := make(map[string]bool, len(initial.State.Analysis.Risk.SkippedFiles)+len(initial.State.Analysis.Risk.FailedFiles))
	for _, f := range initial.State.Analysis.Risk.SkippedFiles {
		processed[f.Path] = true
	}
	for _, f := range initial.State.Analysis.Risk.FailedFiles {
		processed[f.Path] = true
	}
	risks := make([]*domain.Risk, len(initial.State.Files))
	for i, file := range initial.State.Files {
		if run.ctx.Err() != nil {
			return
		}
		if (file.Risk != nil && file.TestOnly != nil) || processed[file.Path] {
			risks[i] = file.Risk
			continue
		}
		var risk *domain.Risk
		var testOnly *domain.TestOnlyAssessment
		var err error
		if file.PatchStatus == "available" {
			risk, testOnly, err = a.Models.AssessFile(run.ctx, settings, file)
		}
		if commitErr := a.commitAnalysis(run, func(current *store.Record) {
			task := &current.State.Analysis.Risk
			task.Completed++
			switch {
			case file.PatchStatus != "available":
				task.SkippedFiles = append(task.SkippedFiles, domain.SkippedFile{Path: file.Path, Reason: "Patch " + file.PatchStatus + "; risk was not assessed."})
			case err != nil:
				task.FailedFiles = append(task.FailedFiles, domain.SkippedFile{Path: file.Path, Reason: analysisError(settings, err)})
			default:
				current.State.Files[i].Risk = risk
				current.State.Files[i].TestOnly = testOnly
			}
		}); commitErr != nil {
			return
		}
		risks[i] = risk
	}
	if !a.explainRisks(run, initial.State.Files, risks, settings, groupingDone) {
		return
	}
	_ = a.commitAnalysis(run, func(current *store.Record) {
		task := &current.State.Analysis.Risk
		task.Status = "complete"
		task.Completed = task.Total
		if len(task.FailedFiles) != 0 {
			task.Status = "failed"
			task.Error = fmt.Sprintf("Jev file assessment failed for %d file(s): %s", len(task.FailedFiles), task.FailedFiles[0].Reason)
		}
	})
}

// explainRisks attaches review-model factors to each flagged file once grouping
// has settled. A factor failure is recorded on that file and never drops its level.
// Progress counts every file plus every flagged file. It reports false when the run ended.
func (a *App) explainRisks(run *analysisRun, files []domain.File, risks []*domain.Risk, settings domain.Settings, groupingDone <-chan struct{}) bool {
	select {
	case <-groupingDone:
	case <-run.ctx.Done():
		return false
	}
	var flagged, pending []int
	for i, risk := range risks {
		if risk != nil && (risk.Level == "medium" || risk.Level == "high") {
			flagged = append(flagged, i)
			if needsFactors(risk) {
				pending = append(pending, i)
			}
		}
	}
	if a.commitAnalysis(run, func(current *store.Record) {
		task := &current.State.Analysis.Risk
		task.Total = len(files) + len(flagged)
		task.Completed = task.Total - len(pending)
	}) != nil {
		return false
	}
	for _, i := range pending {
		if run.ctx.Err() != nil {
			return false
		}
		file := files[i]
		explained := *risks[i]
		factors, err := a.Models.ExplainRisk(run.ctx, settings, file, explained.Level)
		for _, factor := range factors {
			if err == nil {
				if anchorErr := provider.ValidateAnchor([]domain.File{file}, file.Path, factor.StartLine, factor.EndLine, factor.Side); anchorErr != nil {
					err = fmt.Errorf("model cited lines outside the patch: %w", anchorErr)
				}
			}
		}
		if run.ctx.Err() != nil {
			return false
		}
		if err != nil {
			explained.FactorsError = analysisError(settings, err)
		} else {
			explained.Factors = factors
		}
		if a.commitAnalysis(run, func(current *store.Record) {
			current.State.Analysis.Risk.Completed++
			current.State.Files[i].Risk = &explained
		}) != nil {
			return false
		}
	}
	return true
}

func (a *App) commitAnalysis(run *analysisRun, update func(*store.Record)) error {
	identity := domain.Analysis{Key: run.key, BaseSHA: run.baseSHA, HeadSHA: run.headSHA, Generation: run.generation}
	err := a.Store.UpdateAnalysis(run.ctx, identity, run.snapshot, update)
	if err != nil {
		run.cancel()
	}
	return err
}

func (a *App) Analysis() domain.AnalysisUpdate { return a.Store.Analysis() }

func (a *App) RefreshAnalysis(ctx context.Context) (domain.AnalysisUpdate, error) {
	if err := a.lock(ctx); err != nil {
		return domain.AnalysisUpdate{}, err
	}
	defer a.unlock()
	if a.lifetime.Err() != nil {
		return domain.AnalysisUpdate{}, errors.New("application is closed")
	}
	d := a.cancelAnalysis()
	r, err := active(d)
	if err != nil {
		return domain.AnalysisUpdate{}, err
	}
	a.prepareAnalysis(&r, d.Settings, analysisSnapshot(*r.State.PullRequest, r.State.Files), false, true, true)
	if err = a.save(d, r); err != nil {
		return domain.AnalysisUpdate{}, err
	}
	a.startAnalysis(r, d.Settings)
	return a.Analysis(), nil
}

// Close cancels server-owned work, persists interruption status, and waits for
// every worker before the caller closes the store. Repeated calls are harmless.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		a.stop()
		_ = a.lock(context.Background())
		d := a.cancelAnalysis()
		a.closeErr = a.Store.Commit(d)
		a.unlock()
		a.jobs.Wait()
	})
	return a.closeErr
}
