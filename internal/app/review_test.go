package app

import (
	"reflect"
	"strings"
	"testing"

	"skim/internal/domain"
)

func TestParseReviewComments(t *testing.T) {
	finding := proposedComment{Path: "a.go", StartLine: 1, EndLine: 1, Side: "RIGHT", Body: "Finding"}
	suggestion := proposedComment{Path: "a.go", StartLine: 1, EndLine: 1, Side: "RIGHT", Body: "Keep the old value.\n\n```suggestion\nold\n```"}
	for _, tc := range []struct {
		name   string
		report string
		want   []proposedComment
		err    string
	}{
		{name: "empty comments", report: "Nothing found.\n```json\n{\"comments\":[]}\n```", want: []proposedComment{}},
		{name: "last block wins", report: "Draft:\n```json\n{\"comments\":[]}\n```\nFinal:\n```json\n{\"comments\":[{\"path\":\"a.go\",\"startLine\":1,\"endLine\":1,\"side\":\"RIGHT\",\"body\":\"Finding\"}]}\n```", want: []proposedComment{finding}},
		{name: "suggestion fence inside a body", report: "Report.\n```json\n{\"comments\":[{\"path\":\"a.go\",\"startLine\":1,\"endLine\":1,\"side\":\"RIGHT\",\"body\":\"Keep the old value.\\n\\n```suggestion\\nold\\n```\"}]}\n```", want: []proposedComment{suggestion}},
		{name: "trailing prose after the fence", report: "```json\n{\"comments\":[{\"path\":\"a.go\",\"startLine\":1,\"endLine\":1,\"side\":\"RIGHT\",\"body\":\"Finding\"}]}\n```\nThat is all; no checks were run.", want: []proposedComment{finding}},
		{name: "missing block", report: "I reviewed the change and found nothing.", err: "no comments JSON block"},
		{name: "invalid JSON", report: "```json\n{\"comments\":[\n```", err: "invalid"},
		{name: "comments missing", report: "```json\n{\"findings\":[]}\n```", err: "invalid"},
		{name: "null comments", report: "```json\n{\"comments\":null}\n```", err: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseReviewComments(tc.report)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || got != nil {
					t.Fatalf("got %+v, %v; want error containing %q", got, err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestReviewCommentProblem(t *testing.T) {
	files := []domain.File{
		{Path: "a.go", Additions: 1, Deletions: 1, PatchStatus: "available", Patch: "@@ -1,2 +1,2 @@\n ctx\n-old\n+new"},
		{Path: "large.go", Additions: 900, Deletions: 0, PatchStatus: "truncated", Patch: "@@ -0,0 +1 @@\n+partial"},
	}
	comment := func(path string, line int, side, body string) domain.Comment {
		return domain.Comment{Path: path, StartLine: line, EndLine: line, Side: side, Body: body}
	}
	for _, tc := range []struct {
		name    string
		comment domain.Comment
		problem string
	}{
		{name: "added line", comment: comment("a.go", 2, "RIGHT", "Finding")},
		{name: "removed line", comment: comment("a.go", 2, "LEFT", "Finding")},
		{name: "right suggestion", comment: comment("a.go", 2, "RIGHT", "Keep it.\n```suggestion\nold\n```")},
		{name: "unknown path", comment: comment("other.go", 1, "RIGHT", "Finding"), problem: "path is not in the pull request"},
		{name: "incomplete patch", comment: comment("large.go", 1, "RIGHT", "Finding"), problem: "complete available patch"},
		{name: "outside hunk", comment: comment("a.go", 3, "RIGHT", "Finding"), problem: "within one diff hunk"},
		{name: "invalid side", comment: comment("a.go", 2, "BOTH", "Finding"), problem: "LEFT or RIGHT"},
		{name: "blank body", comment: comment("a.go", 2, "RIGHT", " \n "), problem: "1–60000 bytes"},
		{name: "oversized body", comment: comment("a.go", 2, "RIGHT", strings.Repeat("x", 60001)), problem: "1–60000 bytes"},
		{name: "left suggestion", comment: comment("a.go", 2, "LEFT", "Restore it.\n```suggestion\nold\n```"), problem: "RIGHT-side replacement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewCommentProblem(files, tc.comment)
			if tc.problem == "" && got != "" || tc.problem != "" && !strings.Contains(got, tc.problem) {
				t.Fatalf("problem %q; want %q", got, tc.problem)
			}
		})
	}
}
