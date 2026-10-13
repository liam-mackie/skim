package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"skim/internal/domain"
)

func TestAssessFileRetainsTestScopeChoiceAndScores(t *testing.T) {
	for _, tc := range []struct {
		name, answer            string
		only                    bool
		confidence, probability float64
		invalid                 bool
	}{
		{"confident", `{"type":"choice","choice":"tests_only","confidence":0.98,"probabilities":{"tests_only":0.97,"other_or_uncertain":0.03}}`, true, 0.98, 0.97, false},
		{"uncertain-positive", `{"type":"choice","choice":"tests_only","confidence":0.76,"probabilities":{"tests_only":0.79,"other_or_uncertain":0.21}}`, true, 0.76, 0.79, false},
		{"mixed", `{"type":"choice","choice":"other_or_uncertain","confidence":0.99,"probabilities":{"tests_only":0.02,"other_or_uncertain":0.98}}`, false, 0.99, 0.02, false},
		{"missing-confidence", `{"type":"choice","choice":"tests_only","probabilities":{"tests_only":1,"other_or_uncertain":0}}`, false, 0, 0, true},
		{"missing-probability", `{"type":"choice","choice":"tests_only","confidence":1,"probabilities":{"tests_only":1}}`, false, 0, 0, true},
		{"unknown-choice", `{"type":"choice","choice":"test","confidence":1,"probabilities":{"tests_only":1,"other_or_uncertain":0}}`, false, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"answers":{"risk":{"type":"choice","choice":"medium","confidence":1,"probabilities":{"low":0,"medium":1,"high":0}},"test_scope":%s}}`, tc.answer)
			}))
			defer server.Close()
			risk, scope, err := New().AssessFile(context.Background(), modelFixtureSettings(server.URL), domain.File{Path: "ordinary-source.rs", PatchStatus: "available", Patch: "@@ -1 +1 @@\n-old\n+new"})
			if tc.invalid {
				if err == nil || risk != nil || scope != nil {
					t.Fatalf("invalid scope produced a partial assessment: %+v %+v %v", risk, scope, err)
				}
				return
			}
			if err != nil || risk == nil || scope == nil || risk.Level != "medium" || scope.OnlyTests != tc.only || scope.Confidence != tc.confidence || scope.Probability != tc.probability {
				t.Fatalf("assessment: %+v %+v %v", risk, scope, err)
			}
		})
	}
}
