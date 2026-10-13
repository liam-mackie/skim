package model

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"skim/internal/domain"
)

func TestSettingsEmptyAndProtocolRoles(t *testing.T) {
	if err := Validate(domain.Settings{}); err != nil {
		t.Fatalf("empty setup rejected: %v", err)
	}
	s := modelFixtureSettings("https://example.test/v1")
	s.Roles.Chat = domain.ModelRole{}
	s.Roles.Review = domain.ModelRole{}
	if err := Validate(s); err != nil {
		t.Fatal(err)
	}
	s.Roles.Jev.Provider = "text"
	if err := Validate(s); err == nil {
		t.Fatal("generic chat model accepted as Jev")
	}
	s = modelFixtureSettings("https://example.test/v1")
	s.Providers[0].APIKeyEnv = "sk-secret-token"
	if err := Validate(s); err == nil {
		t.Fatal("secret literal accepted as environment name")
	}
	s = modelFixtureSettings("https://example.test/v1")
	s.Providers[0].BaseURL = "https://user:secret@example.org/v1"
	if err := Validate(s); err == nil {
		t.Fatal("credential-bearing URL accepted")
	}
}

func TestDefaultsLeaveModelConfigurationEmpty(t *testing.T) {
	for _, legacyEnvironment := range []bool{false, true} {
		name := "without-legacy-environment"
		if legacyEnvironment {
			name = "with-legacy-environment"
		}
		t.Run(name, func(t *testing.T) {
			for key, value := range map[string]string{
				"SKIM_OPENAI_BASE_URL":   "https://legacy-openai.example.test/v1",
				"TYPESAFE_BASE_URL":      "https://legacy-typesafe.example.test/v1",
				"SKIM_CHAT_MODEL":        "legacy-chat",
				"SKIM_REVIEW_MODEL":      "legacy-review",
				"TYPESAFE_DEFAULT_MODEL": "legacy-jev",
				"OPENAI_API_KEY":         "legacy-openai-key",
				"TYPESAFE_API_KEY":       "legacy-typesafe-key",
			} {
				if !legacyEnvironment {
					value = ""
				}
				t.Setenv(key, value)
			}
			settings := Defaults()
			if settings.Providers == nil || len(settings.Providers) != 0 {
				t.Fatalf("fresh settings must contain an empty provider array: %+v", settings.Providers)
			}
			if settings.Roles != (domain.Roles{}) {
				t.Fatalf("fresh settings seeded model roles: %+v", settings.Roles)
			}
		})
	}
}

func TestProviderNameValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		valid bool
	}{
		{name: "missing"},
		{name: "ASCII-whitespace", value: " \t\r\n"},
		{name: "Unicode-whitespace", value: "\u00a0\u2003"},
		{name: "too-long-ASCII", value: strings.Repeat("a", 101)},
		{name: "too-long-Unicode", value: strings.Repeat("界", 101)},
		{name: "ordinary", value: "Work account", valid: true},
		{name: "ASCII-boundary", value: strings.Repeat("a", 100), valid: true},
		{name: "Unicode-boundary", value: strings.Repeat("界", 100), valid: true},
		{name: "astral-Unicode-boundary", value: strings.Repeat("\U00010400", 100), valid: true},
		{name: "trimmed-boundary", value: " \t" + strings.Repeat("界", 100) + "\u2003", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := modelFixtureSettings("https://example.test/v1")
			settings.Providers[0].Name = tc.value
			err := Validate(settings)
			if (err == nil) != tc.valid {
				t.Fatalf("Validate name %q: got %v, want valid=%v", tc.value, err, tc.valid)
			}
		})
	}
}

func TestJevTypedAnswerAndEvidenceBounds(t *testing.T) {
	for _, tc := range []struct{ name, answer string }{
		{"missing-confidence", `{"type":"choice","choice":"low","probabilities":{"low":1,"high":0}}`},
		{"missing-probability", `{"type":"choice","choice":"low","confidence":0.9,"probabilities":{"low":1}}`},
		{"unknown-choice", `{"type":"choice","choice":"safe","confidence":0.9,"probabilities":{"low":1,"high":0}}`},
		{"invalid-distribution", `{"type":"choice","choice":"low","confidence":0.9,"probabilities":{"low":0.9,"high":0.9}}`},
		{"out-of-range-confidence", `{"type":"choice","choice":"low","confidence":1.1,"probabilities":{"low":1,"high":0}}`},
		{"negative-probability", `{"type":"choice","choice":"low","confidence":0.9,"probabilities":{"low":1.1,"high":-0.1}}`},
		{"extra-probability", `{"type":"choice","choice":"low","confidence":0.9,"probabilities":{"low":1,"high":0,"other":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/systemone" {
					t.Errorf("wrong Jev protocol path: %s", r.URL.Path)
				}
				var input map[string]any
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if input["questions"] == nil || input["state"] == nil {
					t.Error("missing real System One fields")
				}
				_, _ = w.Write([]byte(`{"answers":{"risk":` + tc.answer + `}}`))
			}))
			defer server.Close()
			s := modelFixtureSettings(server.URL + "/v1")
			_, err := New().choose(context.Background(), s, "evidence", map[string]choiceQuestion{"risk": {Type: "choice", Criteria: map[string]string{"low": "localized", "high": "sensitive"}}})
			if err == nil {
				t.Fatal("invalid Jev answer accepted")
			}
		})
	}
	s := modelFixtureSettings("https://example.test/v1")
	_, err := New().choose(context.Background(), s, strings.Repeat("x", 96001), nil)
	if err == nil || !strings.Contains(err.Error(), "96 KB") {
		t.Fatalf("unbounded evidence not rejected locally: %v", err)
	}
}
func TestJevPreservesValidChoiceAtLowConfidenceAndTies(t *testing.T) {
	for _, answer := range []string{
		`{"type":"choice","choice":"high","confidence":0,"probabilities":{"low":0.33,"medium":0.33,"high":0.34}}`,
		`{"type":"choice","choice":"high","confidence":0,"probabilities":{"low":0.5,"medium":0,"high":0.5}}`,
	} {
		t.Run(answer, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"answers":{"risk":` + answer + `}}`))
			}))
			defer server.Close()
			answers, err := New().choose(context.Background(), modelFixtureSettings(server.URL), "protocol validation only", map[string]choiceQuestion{"risk": {Type: "choice", Criteria: risks}})
			if err != nil || answers["risk"].Choice != "high" || *answers["risk"].Confidence != 0 {
				t.Fatalf("valid typed choice was relabeled or rejected: %+v %v", answers, err)
			}
		})
	}
}

func TestModelErrorsRedactCredential(t *testing.T) {
	t.Setenv("SKIM_TEST_MODEL_KEY", "sensitive-test-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, strings.Repeat("x", 995)+"sensitive-test-key", http.StatusUnauthorized)
	}))
	defer server.Close()
	s := modelFixtureSettings(server.URL + "/v1")
	s.Providers[0].APIKeyEnv = "SKIM_TEST_MODEL_KEY"
	_, err := New().Chat(context.Background(), s, s.Roles.Chat, []domain.Message{{Role: "user", Content: "hello"}}, nil)
	if err == nil || strings.Contains(err.Error(), "sensi") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unsafe/invisible model error: %v", err)
	}
}

func modelFixtureSettings(url string) domain.Settings {
	return domain.Settings{
		Providers: []domain.ModelProvider{
			{ID: "text", Name: "Text fixture", Kind: "openai", BaseURL: url},
			{ID: "risk", Name: "Risk fixture", Kind: "typesafe", BaseURL: url},
		},
		Roles: domain.Roles{
			Chat:   domain.ModelRole{Provider: "text", Model: "chat-fixture"},
			Review: domain.ModelRole{Provider: "text", Model: "review-fixture"},
			Jev:    domain.ModelRole{Provider: "risk", Model: "risk-fixture"},
		},
	}
}

func TestAssessFileIsolatesCompleteFileEvidence(t *testing.T) {
	calls := 0
	file := domain.File{Path: "service.go", Status: "modified", PatchStatus: "available", Patch: "@@ -1 +1 @@\n-old\n+new", Risk: &domain.Risk{Level: "high"}, TestOnly: &domain.TestOnlyAssessment{OnlyTests: true, Confidence: 1, Probability: 1}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/systemone" {
			t.Errorf("wrong protocol: %s", r.URL.Path)
		}
		var input struct {
			Model     string                     `json:"model"`
			State     map[string]json.RawMessage `json:"state"`
			Questions map[string]choiceQuestion  `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Model != "risk-fixture" || len(input.Questions) != 2 || input.Questions["risk"].Type != "choice" || input.Questions["test_scope"].Type != "choice" {
			t.Errorf("not a combined typed Jev request: %+v", input)
		}
		var evidence domain.File
		if len(input.State) != 1 || json.Unmarshal(input.State["file"], &evidence) != nil {
			t.Errorf("risk evidence is not isolated to one file: %+v", input.State)
			return
		}
		expected := file
		expected.Risk = nil
		expected.TestOnly = nil
		if !reflect.DeepEqual(evidence, expected) {
			t.Errorf("risk evidence changed: %+v", evidence)
		}
		_, _ = w.Write([]byte(`{"answers":{"risk":{"type":"choice","choice":"medium","confidence":0.8,"probabilities":{"low":0.1,"medium":0.8,"high":0.1}},"test_scope":{"type":"choice","choice":"other_or_uncertain","confidence":0.98,"probabilities":{"tests_only":0.02,"other_or_uncertain":0.98}}}}`))
	}))
	defer server.Close()
	settings := modelFixtureSettings(server.URL)
	risk, testOnly, err := New().AssessFile(context.Background(), settings, file)
	if err != nil || risk == nil || risk.Level != "medium" || testOnly == nil || testOnly.OnlyTests || testOnly.Probability != 0.02 || testOnly.Confidence != 0.98 {
		t.Fatalf("combined assessment not preserved: %+v %+v, %v", risk, testOnly, err)
	}
	if file.Risk == nil || file.Risk.Level != "high" {
		t.Fatal("risk assessment mutated caller evidence")
	}
	for _, status := range []string{"unavailable", "truncated", ""} {
		incomplete := file
		incomplete.PatchStatus = status
		if risk, testOnly, err := New().AssessFile(context.Background(), settings, incomplete); err == nil || risk != nil || testOnly != nil {
			t.Fatalf("manufactured assessment for %q: %+v %+v %v", status, risk, testOnly, err)
		}
	}
	file.Patch = ""
	if risk, testOnly, err := New().AssessFile(context.Background(), settings, file); err == nil || risk != nil || testOnly != nil {
		t.Fatal("empty patch accepted")
	}
	if calls != 1 {
		t.Fatalf("expected one risk call, got %d", calls)
	}
}

func TestFlaggedRiskRequiresConfidentMassAtOrAboveLevel(t *testing.T) {
	for _, test := range []struct {
		name        string
		p           map[string]float64
		level       string
		probability float64
	}{
		{"confident high", map[string]float64{"low": 0.05, "medium": 0.15, "high": 0.8}, "high", 0.8},
		{"high and medium mass flags medium", map[string]float64{"low": 0.2, "medium": 0.3, "high": 0.5}, "medium", 0.8},
		{"hesitant medium argmax stays low", map[string]float64{"low": 0.3, "medium": 0.45, "high": 0.25}, "low", 0.3},
		{"just below threshold", map[string]float64{"low": 0.21, "medium": 0.59, "high": 0.2}, "low", 0.21},
	} {
		t.Run(test.name, func(t *testing.T) {
			level, probability := flaggedRisk(test.p)
			if level != test.level || math.Abs(probability-test.probability) > 1e-9 {
				t.Fatalf("got %s %.3f, want %s %.3f", level, probability, test.level, test.probability)
			}
		})
	}
}

func TestExplainRiskValidatesFactors(t *testing.T) {
	file := domain.File{Path: "auth.go", Status: "modified", Additions: 1, Deletions: 1, PatchStatus: "available", Patch: "@@ -1 +1 @@\n-check()\n+skip()", Risk: &domain.Risk{Level: "high"}}
	for _, test := range []struct {
		name    string
		content string
		want    []domain.RiskFactor
	}{
		{"valid", `{"factors":[{"text":"  Authorization check is removed; confirm callers still enforce it. ","side":"RIGHT","startLine":1,"endLine":1}]}`, []domain.RiskFactor{{Text: "Authorization check is removed; confirm callers still enforce it.", Side: "RIGHT", StartLine: 1, EndLine: 1}}},
		{"no supporting evidence", `{"factors":[]}`, []domain.RiskFactor{}},
		{"too many", `{"factors":[` + strings.Repeat(`{"text":"Check this.","side":"RIGHT","startLine":1,"endLine":1},`, 3) + `{"text":"Check this.","side":"RIGHT","startLine":1,"endLine":1}]}`, nil},
		{"markup", `{"factors":[{"text":"<b>Check</b> this.","side":"RIGHT","startLine":1,"endLine":1}]}`, nil},
		{"null list", `{"factors":null}`, nil},
		{"extra field", `{"factors":[],"level":"high"}`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Model    string           `json:"model"`
					Messages []domain.Message `json:"messages"`
				}
				_ = json.NewDecoder(r.Body).Decode(&input)
				if input.Model != "review-fixture" || len(input.Messages) != 2 || strings.Contains(input.Messages[1].Content, `"risk":`) || !strings.Contains(input.Messages[1].Content, `"flaggedLevel":"high"`) {
					t.Errorf("factors request did not isolate the flagged file for the review model: %+v", input)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": test.content}}}})
			}))
			defer server.Close()
			factors, err := New().ExplainRisk(context.Background(), modelFixtureSettings(server.URL), file, "high")
			if test.want == nil {
				if err == nil || factors != nil {
					t.Fatalf("invalid factors accepted: %+v", factors)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(factors, test.want) {
				t.Fatalf("got %+v, %v", factors, err)
			}
		})
	}
	if factors, err := New().ExplainRisk(context.Background(), modelFixtureSettings("http://127.0.0.1:1"), file, "low"); err == nil || factors != nil {
		t.Fatal("low risk must not be explained")
	}
}

func TestPlanGroupsFullEvidenceAndReadingOrder(t *testing.T) {
	pr := domain.PullRequest{Provider: "github", Owner: "owner", Repo: "repo", Number: 42, Title: "Wire storage", Body: "Share storage contract", URL: "https://example.test/pr/42", Author: "reader", BaseSHA: "base", HeadSHA: "head", BaseRef: "main", HeadRef: "storage"}
	files := []domain.File{
		{Path: "storage_test.go", Status: "added", Additions: 3, PatchStatus: "available", Patch: "+test Storage contract", Language: "Go", Risk: &domain.Risk{Level: "high"}},
		{Path: "storage.go", PreviousPath: "old.go", Status: "renamed", Additions: 4, Deletions: 2, PatchStatus: "available", Patch: "+implement Storage\n" + strings.Repeat("complete evidence\n", 8000), Language: "Go"},
		{Path: "api.go", Status: "modified", PatchStatus: "available", Patch: "+interface Storage"},
		{Path: "caller.go", Status: "modified", PatchStatus: "available", Patch: "+use Storage"},
		{Path: "asset.bin", Status: "added", PatchStatus: "unavailable"},
		{Path: "generated.go", Status: "modified", PatchStatus: "truncated", Patch: "+partial evidence"},
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/chat/completions" {
			t.Errorf("wrong grouping protocol: %s", r.URL.Path)
		}
		var input struct {
			Model          string           `json:"model"`
			Messages       []domain.Message `json:"messages"`
			ResponseFormat json.RawMessage  `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if !requireModelResponseSchema(t, input.ResponseFormat, "review_groups", expectedGroupsSchema) {
			http.Error(w, "strict review_groups schema required", http.StatusBadRequest)
			return
		}
		if input.Model != "review-fixture" || len(input.Messages) != 2 {
			t.Errorf("wrong review request: %+v", input)
			return
		}
		var evidence struct {
			PullRequest domain.PullRequest `json:"pullRequest"`
			Files       []domain.File      `json:"files"`
		}
		if err := json.Unmarshal([]byte(input.Messages[1].Content), &evidence); err != nil {
			t.Error(err)
		}
		expected := append([]domain.File(nil), files...)
		for i := range expected {
			expected[i].Risk = nil
		}
		if evidence.PullRequest != pr || !reflect.DeepEqual(evidence.Files, expected) || strings.Contains(input.Messages[1].Content, `"risk"`) {
			t.Errorf("incomplete or risk-bearing grouping evidence: %+v", evidence)
		}
		content := `{"groups":[{"title":"Storage contract","summary":"Defines the shared storage interface.","paths":["api.go"]},{"title":"Storage integration","summary":"Connects the storage implementation, caller, and contract test.","paths":["storage.go","caller.go","storage_test.go"]},{"title":"Generated assets","summary":"Changes generated and binary assets; their full behavior is not visible in the supplied patches.","paths":["generated.go","asset.bin"]}]}`
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
	}))
	defer server.Close()
	plan, err := New().PlanGroups(context.Background(), modelFixtureSettings(server.URL), pr, files)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Group{
		{ID: "group-1", Title: "Storage contract", Summary: "Defines the shared storage interface.", Paths: []string{"api.go"}},
		{ID: "group-2", Title: "Storage integration", Summary: "Connects the storage implementation, caller, and contract test.", Paths: []string{"storage.go", "caller.go", "storage_test.go"}},
		{ID: "group-3", Title: "Generated assets", Summary: "Changes generated and binary assets; their full behavior is not visible in the supplied patches.", Paths: []string{"generated.go", "asset.bin"}},
	}
	if !reflect.DeepEqual(plan.Groups, want) || calls != 1 {
		t.Fatalf("model context or reading order changed or extra calls: %+v, calls=%d", plan, calls)
	}
	if files[0].Risk == nil {
		t.Fatal("grouping mutated input risk")
	}
}

func TestPlanGroupsRejectsInvalidResponsesAtomically(t *testing.T) {
	for _, content := range []string{
		`{"groups":[{"title":"","summary":"Updates related files.","paths":["a","b"]}]}`,
		`{"groups":[{"title":"  ","summary":"Updates related files.","paths":["a","b"]}]}`,
		`{"groups":[{"title":"Two\nlines","summary":"Updates related files.","paths":["a","b"]}]}`,
		`{"groups":[{"title":"` + strings.Repeat("x", 101) + `","summary":"Updates related files.","paths":["a","b"]}]}`,
		`{"groups":[{"title":"Unit","summary":"Updates related files.","paths":["a","unknown","b"]}]}`,
		`{"groups":[{"title":"Unit","summary":"Updates related files.","paths":["a","a","b"]}]}`,
		`{"groups":[{"title":"First","summary":"Updates one file.","paths":["a"]},{"title":"Second","summary":"Updates related files.","paths":["a","b"]}]}`,
		`{"groups":[{"title":"Unit","summary":"Updates related files.","paths":["a"]}]}`,
		`{"groups":[{"title":"Unit","summary":"Updates related files.","paths":[]}]}`,
		`{"groups":[{"title":"Unit","summary":"Updates related files.","paths":["a","b"],"reason":"prose"}]}`,
		`{"groups":[{"title":"Unit","summary":"Updates related files.","paths":["a","b"]}],"summary":"prose"}`,
		`{"introduction":"A PR-wide overview is no longer part of the contract.","groups":[{"title":"Unit","summary":"Updates related files.","paths":["a","b"]}]}`,
		`{"groups":[{"title":"Unit","summary":"Updates related files.","paths":["a","b"]}]} {}`,
		`{"groups":[{"title":17,"summary":"Updates related files.","paths":["a","b"]}]}`,
		`{"groups":null}`,
		`null`,
		`not JSON`,
	} {
		t.Run(content, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
			}))
			defer server.Close()
			plan, err := New().PlanGroups(context.Background(), modelFixtureSettings(server.URL), domain.PullRequest{}, []domain.File{{Path: "a"}, {Path: "b"}})
			if err == nil || plan.Groups != nil {
				t.Fatalf("invalid response partially accepted: %+v, %v", plan, err)
			}
		})
	}
}

func TestPlanGroupsRejectsMalformedSummaryAtomically(t *testing.T) {
	for _, field := range []string{"summary"} {
		for _, test := range []struct {
			name  string
			value any
			omit  bool
		}{
			{name: "missing", omit: true},
			{name: "null"},
			{name: "empty", value: ""},
			{name: "whitespace", value: " \n\u2003"},
			{name: "punctuation", value: "..."},
			{name: "number", value: 42},
			{name: "object", value: map[string]string{"text": "Unexpected object."}},
			{name: "html", value: "<p>Changes shared behavior.</p>"},
			{name: "html comment", value: "<!-- hidden -->Changes shared behavior."},
			{name: "control", value: "Changes\u0000shared behavior."},
			{name: "hidden formatting", value: "Changes\u202eshared behavior."},
		} {
			t.Run(field+"/"+test.name, func(t *testing.T) {
				group := map[string]any{"title": "Unit", "summary": "Changes related files.", "paths": []string{"a", "b"}}
				response := map[string]any{"groups": []any{group}}
				target := group
				if test.omit {
					delete(target, field)
				} else {
					target[field] = test.value
				}
				content, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": string(content)}}}})
				}))
				defer server.Close()
				plan, err := New().PlanGroups(context.Background(), modelFixtureSettings(server.URL), domain.PullRequest{}, []domain.File{{Path: "a"}, {Path: "b"}})
				if err == nil || plan.Groups != nil {
					t.Fatalf("malformed context partially accepted: %+v, %v", plan, err)
				}
			})
		}
	}
}

func TestPlanGroupsPreservesLongContext(t *testing.T) {
	summary := strings.Repeat("The storage integration adapts the implementation and caller to the shared contract. Its contract test captures the expected interaction without providing evidence of a passing run. The related files form one change because the caller relies on the same interface implemented by storage. This description separates the stated goal from the behavior visible in the patches. Unicode context such as 関係 remains intact.\n\n", 8) + "This final summary detail must not be truncated."
	want := []domain.Group{
		{ID: "group-1", Title: "Storage contract", Summary: "Defines the shared storage interface.", Paths: []string{"api.go"}},
		{ID: "group-2", Title: "Storage integration", Summary: summary, Paths: []string{"storage.go", "caller.go", "storage_test.go"}},
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		content, _ := json.Marshal(map[string]any{"groups": []any{
			map[string]any{"title": want[0].Title, "summary": want[0].Summary, "paths": want[0].Paths},
			map[string]any{"title": want[1].Title, "summary": want[1].Summary, "paths": want[1].Paths},
		}})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": string(content)}}}})
	}))
	defer server.Close()
	files := []domain.File{{Path: "api.go"}, {Path: "storage.go"}, {Path: "caller.go"}, {Path: "storage_test.go"}}
	plan, err := New().PlanGroups(context.Background(), modelFixtureSettings(server.URL), domain.PullRequest{}, files)
	if err != nil || !reflect.DeepEqual(plan.Groups, want) || calls != 1 {
		t.Fatalf("long context was rejected, altered, or retried: %+v, calls=%d, error=%v", plan, calls, err)
	}
}

func TestAnalysisModelConfigurationAndLocalBounds(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		t.Error("local rejection contacted provider")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := New()
	file := domain.File{Path: "a", PatchStatus: "available", Patch: "+change"}
	if plan, err := client.PlanGroups(context.Background(), domain.Settings{}, domain.PullRequest{}, nil); err != nil || plan.Groups == nil || len(plan.Groups) != 0 {
		t.Fatalf("empty files must need no configuration and return factual context: %+v %v", plan, err)
	}
	settings := modelFixtureSettings(server.URL)
	noReview := settings
	noReview.Roles.Review = domain.ModelRole{}
	if plan, err := client.PlanGroups(context.Background(), noReview, domain.PullRequest{}, []domain.File{file}); err == nil || plan.Groups != nil {
		t.Fatal("missing review configuration fabricated context")
	}
	noRisk := settings
	noRisk.Roles.Jev = domain.ModelRole{}
	if risk, testOnly, err := client.AssessFile(context.Background(), noRisk, file); err == nil || risk != nil || testOnly != nil {
		t.Fatal("missing Jev configuration fabricated risk")
	}
	file.Patch = strings.Repeat("x", 4<<20)
	if plan, err := client.PlanGroups(context.Background(), settings, domain.PullRequest{}, []domain.File{file}); err == nil || plan.Groups != nil || !strings.Contains(err.Error(), "silently truncated") {
		t.Fatalf("grouping context not rejected without truncation: %+v %v", plan, err)
	}
	if risk, testOnly, err := client.AssessFile(context.Background(), settings, file); err == nil || risk != nil || testOnly != nil || !strings.Contains(err.Error(), "silently truncated") {
		t.Fatalf("risk context not rejected without truncation: %+v %v", risk, err)
	}
	file.Patch = "+small"
	if plan, err := client.PlanGroups(context.Background(), settings, domain.PullRequest{Body: strings.Repeat("x", 4<<20)}, []domain.File{file}); err == nil || plan.Groups != nil || !strings.Contains(err.Error(), "silently truncated") {
		t.Fatalf("PR metadata bypassed context bound: %+v %v", plan, err)
	}
	if calls != 0 {
		t.Fatalf("unexpected paid calls: %d", calls)
	}
}

// These expectations are independent of the schemas supplied by production callers.
const expectedGroupsSchema = `{
	"type":"object",
	"properties":{"groups":{
		"type":"array",
		"items":{
			"type":"object",
			"properties":{
				"title":{"type":"string"},
				"summary":{"type":"string"},
				"paths":{"type":"array","items":{"type":"string"}}
			},
			"required":["title","summary","paths"],
			"additionalProperties":false
		}
	}},
	"required":["groups"],
	"additionalProperties":false
}`

const expectedReviewSchema = `{
	"type":"object",
	"properties":{"comments":{
		"type":"array",
		"items":{
			"type":"object",
			"properties":{
				"path":{"type":"string"},
				"startLine":{"type":"integer"},
				"endLine":{"type":"integer"},
				"side":{"type":"string","enum":["LEFT","RIGHT"]},
				"body":{"type":"string"}
			},
			"required":["path","startLine","endLine","side","body"],
			"additionalProperties":false
		}
	}},
	"required":["comments"],
	"additionalProperties":false
}`

func requireModelResponseSchema(t *testing.T, raw json.RawMessage, name, schema string) bool {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal(raw, &actual); err != nil {
		t.Errorf("missing or invalid response_format: %s (%v)", raw, err)
		return false
	}
	if err := json.Unmarshal([]byte(`{"type":"json_schema","json_schema":{"name":"`+name+`","strict":true,"schema":`+schema+`}}`), &expected); err != nil {
		t.Errorf("invalid test schema: %v", err)
		return false
	}
	normalizeSchemaRequired(actual)
	normalizeSchemaRequired(expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("response_format does not match strict %s contract: %s", name, raw)
		return false
	}
	return true
}

func normalizeSchemaRequired(value any) {
	switch value := value.(type) {
	case map[string]any:
		if required, ok := value["required"].([]any); ok {
			sort.Slice(required, func(i, j int) bool {
				left, _ := required[i].(string)
				right, _ := required[j].(string)
				return left < right
			})
		}
		for _, child := range value {
			normalizeSchemaRequired(child)
		}
	case []any:
		for _, child := range value {
			normalizeSchemaRequired(child)
		}
	}
}

func TestChatResponseFormatContract(t *testing.T) {
	for _, structured := range []bool{false, true} {
		name := "text"
		if structured {
			name = "review-comments"
		}
		t.Run(name, func(t *testing.T) {
			content := "ordinary text"
			if structured {
				content = `{"comments":[{"path":"file.go","startLine":2,"endLine":3,"side":"RIGHT","body":"Review detail"}]}`
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
					t.Errorf("unexpected chat request: %s %s", r.Method, r.URL.Path)
				}
				var input struct {
					Model          string           `json:"model"`
					Messages       []domain.Message `json:"messages"`
					ResponseFormat json.RawMessage  `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if input.Model != "chat-fixture" || !reflect.DeepEqual(input.Messages, []domain.Message{{Role: "user", Content: "hello"}}) {
					t.Errorf("schema mode changed chat input: %+v", input)
				}
				if structured {
					if !requireModelResponseSchema(t, input.ResponseFormat, "review_comments", expectedReviewSchema) {
						http.Error(w, "strict review_comments schema required", http.StatusBadRequest)
						return
					}
				} else if len(input.ResponseFormat) != 0 {
					t.Errorf("text requests must omit response_format, got %s", input.ResponseFormat)
					http.Error(w, "unexpected response_format", http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
			}))
			defer server.Close()
			settings := modelFixtureSettings(server.URL)
			var schema *JSONSchema
			if structured {
				schema = &ReviewCommentsSchema
			}
			text, err := New().Chat(context.Background(), settings, settings.Roles.Chat, []domain.Message{{Role: "user", Content: "hello"}}, schema)
			if err != nil || text != content || calls != 1 {
				t.Fatalf("chat contract failed: text=%q err=%v calls=%d", text, err, calls)
			}
		})
	}
}
