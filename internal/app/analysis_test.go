package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"skim/internal/domain"
	"skim/internal/store"
)

func TestAnalysisErrorsRedactConfiguredCredentialsBeforePersistence(t *testing.T) {
	t.Setenv("SKIM_TEST_ANALYSIS_TOKEN", "github-test-secret")
	t.Setenv("SKIM_TEST_ANALYSIS_KEY", "model-test-secret")
	settings := domain.Settings{GitHubTokenEnv: "SKIM_TEST_ANALYSIS_TOKEN", Providers: []domain.ModelProvider{{ID: "analysis", Name: "Analysis", APIKeyEnv: "SKIM_TEST_ANALYSIS_KEY"}}}
	message := analysisError(settings, errors.New(`invalid grouping JSON: unknown field "model-test-secret"; github-test-secret`))
	if strings.Contains(message, "test-secret") || strings.Count(message, "[redacted]") != 2 || !strings.Contains(message, "invalid grouping JSON") {
		t.Fatalf("unsafe or unhelpful persisted diagnostic: %s", message)
	}
}

func TestRiskPolicyInvalidatesOnlyRiskAndReusesCurrentResults(t *testing.T) {
	settings := analysisSettingsFixture()
	for _, oldConfig := range []string{
		roleConfig(settings, settings.Roles.Jev), // Persisted before policy versioning.
		roleConfig(settings, settings.Roles.Jev) + ":previous-policy",
		roleConfig(settings, settings.Roles.Jev) + ":change-impact-v1",
	} {
		t.Run(oldConfig, func(t *testing.T) {
			pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, BaseSHA: "base", HeadSHA: "head"}
			record := store.Record{
				State: domain.State{
					PullRequest: &pr,
					Files:       []domain.File{{Path: "notes", PatchStatus: "available", Patch: "+explanation", Risk: &domain.Risk{Level: "high", Probability: 0.9}}},
					Groups:      []domain.Group{{ID: "unit", Title: "Reading order", Summary: "Explains the changed behavior.", Paths: []string{"notes"}}},
					Comments:    []domain.Comment{{ID: "draft", Body: "keep draft"}},
					Chats:       []domain.ChatThread{{ID: "chat", Messages: []domain.Message{{Role: "user", Content: "keep chat"}}}},
					Style:       "keep style",
					ReviewRun:   &domain.ReviewRun{HeadSHA: pr.HeadSHA, Report: "keep report", Rejected: []domain.RejectedComment{}},
					Analysis: &domain.Analysis{
						Generation: "old-generation",
						Risk:       domain.AnalysisTask{Status: "failed", Completed: 1, Total: 1, Error: "old error", FailedFiles: []domain.SkippedFile{{Path: "notes", Reason: "old failure"}}},
						Grouping:   domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1},
					},
				},
				Pending:        &store.Publication{Marker: "keep pending publication"},
				RiskConfig:     oldConfig,
				GroupingConfig: groupingRoleConfig(settings),
			}
			snapshot := analysisSnapshot(pr, record.State.Files)
			record.AnalysisSnapshot = snapshot
			before := record.State
			grouping := record.State.Analysis.Grouping
			pending := record.Pending
			a := &App{}
			a.prepareAnalysis(&record, settings, snapshot, false, false, false)
			if record.State.Files[0].Risk != nil || !reflect.DeepEqual(record.State.Analysis.Risk, domain.AnalysisTask{Status: "running", Total: 1}) {
				t.Fatalf("old risk/results were reused: %+v", record.State.Analysis.Risk)
			}
			if record.State.Analysis.Generation == "old-generation" || record.RiskConfig == oldConfig {
				t.Fatal("old run or policy still identifies the new risk analysis")
			}
			if record.AnalysisSnapshot != snapshot || record.GroupingConfig != groupingRoleConfig(settings) || !reflect.DeepEqual(record.State.Analysis.Grouping, grouping) || !reflect.DeepEqual(record.State.Groups, before.Groups) {
				t.Fatal("risk policy change invalidated grouping or PR evidence")
			}
			if !reflect.DeepEqual(record.State.Comments, before.Comments) || !reflect.DeepEqual(record.State.Chats, before.Chats) || record.State.Style != before.Style || !reflect.DeepEqual(record.State.ReviewRun, before.ReviewRun) || record.Pending != pending {
				t.Fatal("risk policy change altered user review state")
			}

			risk := &domain.Risk{Level: "low", Probability: 0.95}
			record.State.Files[0].Risk = risk
			record.State.Files[0].TestOnly = &domain.TestOnlyAssessment{OnlyTests: false, Confidence: 0.99, Probability: 0.01}
			record.State.Analysis.Risk = domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1}
			settings.Roles.Chat.Model = "unrelated-chat-change"
			settings.ACP.Command = "unrelated-agent-change"
			a.prepareAnalysis(&record, settings, snapshot, false, false, false)
			if record.State.Files[0].Risk != risk || record.State.Analysis.Risk.Status != "complete" || !reflect.DeepEqual(record.State.Analysis.Grouping, grouping) {
				t.Fatal("same-policy reload or unrelated settings discarded completed analysis")
			}
		})
	}
}

func TestGroupingPolicyInvalidatesOnlyContextAndReusesCurrentResults(t *testing.T) {
	settings := analysisSettingsFixture()
	for _, test := range []struct {
		oldConfig string
		grouping  domain.AnalysisTask
	}{
		{roleConfig(settings, settings.Roles.Review), domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1}},
		{roleConfig(settings, settings.Roles.Review) + ":previous-policy", domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1}},
		{roleConfig(settings, settings.Roles.Review) + ":review-context-v1", domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1}},
		{roleConfig(settings, settings.Roles.Review) + ":review-context-v1", domain.AnalysisTask{Status: "failed", Total: 1, Error: "Recommended review units: invalid grouping summary at group 2: expected meaningful plain prose of at most 400 characters"}},
	} {
		t.Run(test.oldConfig+"/"+test.grouping.Status, func(t *testing.T) {
			pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, BaseSHA: "base", HeadSHA: "head"}
			risk := &domain.Risk{Level: "low", Probability: 0.95}
			riskTask := domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1}
			record := store.Record{
				State: domain.State{
					PullRequest: &pr,
					Files:       []domain.File{{Path: "notes", PatchStatus: "available", Patch: "+explanation", Risk: risk, TestOnly: &domain.TestOnlyAssessment{Confidence: 0.99, Probability: 0.01}}},
					Groups:      []domain.Group{{ID: "old", Title: "Old unit", Summary: "Old context.", Paths: []string{"notes"}}},
					Comments:    []domain.Comment{{ID: "draft", Body: "keep draft"}},
					Chats:       []domain.ChatThread{{ID: "chat", Messages: []domain.Message{{Role: "user", Content: "keep chat"}}}},
					Style:       "keep style",
					ReviewRun:   &domain.ReviewRun{HeadSHA: pr.HeadSHA, Report: "keep report", Rejected: []domain.RejectedComment{}},
					Analysis: &domain.Analysis{
						Generation: "old-generation",
						Risk:       riskTask,
						Grouping:   test.grouping,
					},
				},
				Pending:        &store.Publication{Marker: "keep pending publication"},
				RiskConfig:     riskRoleConfig(settings),
				GroupingConfig: test.oldConfig,
			}
			snapshot := analysisSnapshot(pr, record.State.Files)
			record.AnalysisSnapshot = snapshot
			before := record.State
			pending := record.Pending
			a := &App{}
			a.prepareAnalysis(&record, settings, snapshot, false, false, false)
			if len(record.State.Groups) != 0 || !reflect.DeepEqual(record.State.Analysis.Grouping, domain.AnalysisTask{Status: "running", Total: 1}) {
				t.Fatalf("old grouping context was reused: %+v", record.State)
			}
			if record.State.Analysis.Generation == "old-generation" || record.GroupingConfig != groupingRoleConfig(settings) {
				t.Fatal("old run or policy still identifies the new context request")
			}
			if record.State.Files[0].Risk != risk || !reflect.DeepEqual(record.State.Analysis.Risk, riskTask) || record.RiskConfig != riskRoleConfig(settings) || record.AnalysisSnapshot != snapshot {
				t.Fatal("grouping policy change discarded completed risk or PR evidence")
			}
			if !reflect.DeepEqual(record.State.Comments, before.Comments) || !reflect.DeepEqual(record.State.Chats, before.Chats) || record.State.Style != before.Style || !reflect.DeepEqual(record.State.ReviewRun, before.ReviewRun) || record.Pending != pending {
				t.Fatal("grouping policy change altered user review state")
			}

			record.State.Groups = []domain.Group{{ID: "unit", Title: "Notes", Summary: strings.Repeat("Updates the behavior explanation and the related contract documentation. ", 40), Paths: []string{"notes"}}}
			record.State.Analysis.Grouping = domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1}
			groups := record.State.Groups
			settings.Roles.Chat.Model = "unrelated-chat-change"
			settings.ACP.Command = "unrelated-agent-change"
			a.prepareAnalysis(&record, settings, snapshot, false, false, false)
			if !reflect.DeepEqual(record.State.Groups, groups) || record.State.Analysis.Grouping.Status != "complete" || record.State.Files[0].Risk != risk || !reflect.DeepEqual(record.State.Analysis.Risk, riskTask) {
				t.Fatal("same-policy reload or unrelated settings discarded completed context")
			}
		})
	}
}

func TestRiskIsDisabledWithoutJevAndResumesWhenConfigured(t *testing.T) {
	settings := analysisSettingsFixture()
	settings.Roles.Jev = domain.ModelRole{}
	pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, BaseSHA: "base", HeadSHA: "head"}
	files := []domain.File{{Path: "a.go", PatchStatus: "available", Patch: "+change", Risk: &domain.Risk{Level: "high", Probability: 0.9}, TestOnly: &domain.TestOnlyAssessment{OnlyTests: true, Confidence: 0.99, Probability: 0.99}}}
	record := store.Record{State: domain.State{PullRequest: &pr, Files: files}}
	snapshot := analysisSnapshot(pr, files)
	a := &App{}
	a.prepareAnalysis(&record, settings, snapshot, true, false, false)
	if record.State.Analysis.Risk.Status != "disabled" || record.State.Files[0].Risk != nil || record.State.Files[0].TestOnly != nil {
		t.Fatalf("unconfigured Jev still produced risk state: %+v %+v", record.State.Analysis.Risk, record.State.Files[0])
	}
	if record.State.Analysis.Grouping.Status != "running" {
		t.Fatal("disabled risk blocked grouping")
	}
	a.prepareAnalysis(&record, settings, snapshot, false, true, false)
	if record.State.Analysis.Risk.Status != "disabled" {
		t.Fatal("explicit refresh ran risk without Jev")
	}
	settings.Roles.Jev = analysisSettingsFixture().Roles.Jev
	a.prepareAnalysis(&record, settings, snapshot, false, false, false)
	if !reflect.DeepEqual(record.State.Analysis.Risk, domain.AnalysisTask{Status: "running", Total: 1}) {
		t.Fatalf("configuring Jev did not start risk analysis: %+v", record.State.Analysis.Risk)
	}
}

func TestUnexplainedFlaggedRiskResumesWithoutReassessment(t *testing.T) {
	settings := analysisSettingsFixture()
	pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, BaseSHA: "base", HeadSHA: "head"}
	flagged := &domain.Risk{Level: "medium", Probability: 0.85}
	files := []domain.File{{Path: "a.go", PatchStatus: "available", Patch: "+change", Risk: flagged, TestOnly: &domain.TestOnlyAssessment{Confidence: 0.99, Probability: 0.01}}}
	snapshot := analysisSnapshot(pr, files)
	record := store.Record{
		State:            domain.State{PullRequest: &pr, Files: files, Analysis: &domain.Analysis{Risk: domain.AnalysisTask{Status: "cancelled", Completed: 1, Total: 2}, Grouping: domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1}}},
		AnalysisSnapshot: snapshot, RiskConfig: riskRoleConfig(settings), GroupingConfig: groupingRoleConfig(settings),
	}
	a := &App{}
	a.prepareAnalysis(&record, settings, snapshot, false, false, false)
	if record.State.Analysis.Risk.Status != "running" || record.State.Files[0].Risk != flagged {
		t.Fatalf("interrupted factor pass did not resume with the saved level: %+v", record.State.Analysis.Risk)
	}
	explained := *flagged
	explained.Factors = []domain.RiskFactor{}
	record.State.Files[0].Risk = &explained
	record.State.Analysis.Risk.Status = "complete"
	a.prepareAnalysis(&record, settings, snapshot, false, false, false)
	if record.State.Analysis.Risk.Status != "complete" {
		t.Fatal("explained file (no supporting factors) was queued again")
	}
}

func analysisSettingsFixture() domain.Settings {
	return domain.Settings{
		Providers: []domain.ModelProvider{
			{ID: "text", Name: "Text account", Kind: "openai", BaseURL: "https://text.invalid/v1"},
			{ID: "risk", Name: "Risk account", Kind: "typesafe", BaseURL: "https://risk.invalid/v1"},
		},
		Roles: domain.Roles{
			Chat:   domain.ModelRole{Provider: "text", Model: "chat-model"},
			Review: domain.ModelRole{Provider: "text", Model: "review-model"},
			Jev:    domain.ModelRole{Provider: "risk", Model: "risk-model"},
		},
	}
}

func TestAnalysisRoleCacheIdentityIgnoresProviderRename(t *testing.T) {
	for _, role := range []string{"risk", "grouping"} {
		t.Run(role, func(t *testing.T) {
			settings := analysisSettingsFixture()
			cacheKey := func(s domain.Settings) string {
				if role == "risk" {
					return riskRoleConfig(s)
				}
				return groupingRoleConfig(s)
			}
			before := cacheKey(settings)
			for i := range settings.Providers {
				settings.Providers[i].Name = "Renamed " + settings.Providers[i].Name
			}
			if cacheKey(settings) != before {
				t.Fatal("display-only rename changed analysis cache identity")
			}
			if role == "risk" {
				settings.Roles.Jev.Model = "different-risk-model"
			} else {
				settings.Roles.Review.Model = "different-review-model"
			}
			if cacheKey(settings) == before {
				t.Fatal("model change failed to invalidate analysis cache")
			}
		})
	}
}

func TestSettingsRenamePreservesAnalysisGenerationCachesRolesAndCredentials(t *testing.T) {
	s, err := store.Open(t.TempDir(), analysisSettingsFixture())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// No providers can be called: this App has no model client, and cached tasks
	// are complete. A rename must not schedule replacement analysis.
	a := &App{Store: s, gate: make(chan struct{}, 1), lifetime: context.Background()}
	if _, err := a.SetSettings(context.Background(), domain.SettingsUpdate{Settings: s.Settings(), ProviderKeys: []domain.ProviderKeyUpdate{{ProviderID: "text", APIKey: "text-fixture-key"}, {ProviderID: "risk", APIKey: "risk-fixture-key"}}}); err != nil {
		t.Fatal(err)
	}
	settings := s.Settings()
	d := s.Snapshot()
	pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 1, BaseSHA: "base", HeadSHA: "head"}
	d.Active = pr.Key()
	d.Reviews[d.Active] = store.Record{
		State: domain.State{
			PullRequest: &pr,
			Files:       []domain.File{{Path: "a.go", Risk: &domain.Risk{Level: "low", Probability: 0.95}}},
			Groups:      []domain.Group{{ID: "unit", Title: "Cached grouping", Summary: "Connects the changed behavior.", Paths: []string{"a.go"}}},
			Analysis: &domain.Analysis{
				Generation: "existing-generation",
				Revision:   7,
				Risk:       domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1},
				Grouping:   domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1},
			},
		},
		AnalysisSnapshot: "existing-snapshot",
		RiskConfig:       riskRoleConfig(settings),
		GroupingConfig:   groupingRoleConfig(settings),
	}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	settings.Providers[0].Name = "  Renamed text  "
	settings.Providers[1].Name = "  Renamed risk  "
	got, err := a.SetSettings(context.Background(), domain.SettingsUpdate{Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	after := s.Snapshot()
	if !reflect.DeepEqual(after.Reviews, before.Reviews) || a.run != nil {
		t.Fatal("rename changed cached results, analysis generation, or scheduled work")
	}
	if got.Roles != before.Settings.Roles || !reflect.DeepEqual(after.ProviderKeys, before.ProviderKeys) {
		t.Fatal("rename changed role bindings or credential identity")
	}
	for i, p := range got.Providers {
		if p.Name != []string{"Renamed text", "Renamed risk"}[i] {
			t.Fatalf("name was not trimmed: %q", p.Name)
		}
		if p.ID != before.Settings.Providers[i].ID || p.CredentialRevision != before.Settings.Providers[i].CredentialRevision {
			t.Fatal("rename changed stable provider identity")
		}
		if key, err := s.ResolveCredential(p); err != nil || key != p.ID+"-fixture-key" {
			t.Fatalf("rename lost %s credential: %v", p.ID, err)
		}
	}
}

func TestPairedJevMetadataDoesNotChangeEvidenceOrReviewedProgress(t *testing.T) {
	settings := analysisSettingsFixture()
	pr := domain.PullRequest{Provider: "github", Owner: "team", Repo: "repo", Number: 1, BaseSHA: "base", HeadSHA: "head"}
	file := domain.File{Path: "src/code.rs", PatchStatus: "available", Patch: "@@ -1 +1 @@\n-old\n+new"}
	snapshot := analysisSnapshot(pr, []domain.File{file})
	file.Risk = &domain.Risk{Level: "low"}
	file.TestOnly = &domain.TestOnlyAssessment{OnlyTests: true, Confidence: 0.99, Probability: 0.99}
	if analysisSnapshot(pr, []domain.File{file}) != snapshot {
		t.Fatal("model judgments changed source evidence identity")
	}
	record := store.Record{
		State: domain.State{PullRequest: &pr, Files: []domain.File{file}, Groups: []domain.Group{{ID: "g", Title: "Group", Paths: []string{file.Path}}},
			FileReview: &domain.FileReview{Revision: 3, ReviewedPaths: []string{file.Path}},
			Analysis:   &domain.Analysis{Risk: domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1}, Grouping: domain.AnalysisTask{Status: "complete", Completed: 1, Total: 1}}},
		AnalysisSnapshot: snapshot, RiskConfig: riskRoleConfig(settings), GroupingConfig: groupingRoleConfig(settings),
	}
	a := &App{}
	a.prepareAnalysis(&record, settings, snapshot, false, false, false)
	if record.State.Files[0].TestOnly != file.TestOnly || record.State.Files[0].Risk != file.Risk {
		t.Fatal("current paired results were not reused")
	}
	record.State.Files[0].TestOnly = nil
	a.prepareAnalysis(&record, settings, snapshot, false, false, false)
	if record.State.Analysis.Risk.Status != "running" || record.State.Files[0].Risk != nil || record.State.Files[0].TestOnly != nil {
		t.Fatal("risk-only result was mistaken for a complete paired assessment")
	}
	if record.State.Analysis.Grouping.Status != "complete" || record.State.FileReview.Revision != 3 || len(record.State.FileReview.ReviewedPaths) != 1 {
		t.Fatal("assessment refresh discarded grouping or human review progress")
	}
	s, err := store.Open(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record.State.Files[0] = file
	data := s.Snapshot()
	data.Active = pr.Key()
	data.Reviews[pr.Key()] = record
	if err := s.Commit(data); err != nil {
		t.Fatal(err)
	}
	state := s.State()
	state.Files[0].TestOnly.Probability = 0
	poll := s.Analysis()
	poll.Files[0].TestOnly.Confidence = 0
	if s.State().Files[0].TestOnly.Probability != 0.99 || s.Analysis().Files[0].TestOnly.Confidence != 0.99 {
		t.Fatal("assessment snapshots shared mutable model scores")
	}
}
