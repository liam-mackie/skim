package provider

import (
	"strings"
	"testing"

	"skim/internal/domain"
)

func TestValidateAnchor(t *testing.T) {
	patch := "@@ -10,3 +10,4 @@ function\n context\n-old\n+new\n+extra\n last\n@@ -30 +31 @@\n-before\n+after"
	file := domain.File{Path: "new.go", PreviousPath: "old.go", Additions: 3, Deletions: 2, Patch: patch, PatchStatus: "available"}
	cases := []struct {
		name, path, side string
		start, end       int
		valid            bool
	}{
		{"right multiline", "new.go", "RIGHT", 10, 13, true},
		{"left multiline", "new.go", "LEFT", 10, 12, true},
		{"second hunk right", "new.go", "RIGHT", 31, 31, true},
		{"second hunk left", "new.go", "LEFT", 30, 30, true},
		{"deleted line not on right", "new.go", "RIGHT", 30, 30, false},
		{"inserted line not on left", "new.go", "LEFT", 13, 13, false},
		{"cross hunks", "new.go", "RIGHT", 13, 31, false},
		{"missing path", "missing.go", "RIGHT", 10, 10, false},
		{"previous path", "old.go", "LEFT", 10, 10, false},
		{"invalid side", "new.go", "right", 10, 10, false},
		{"zero line", "new.go", "RIGHT", 0, 1, false},
		{"negative line", "new.go", "LEFT", -1, 1, false},
		{"reversed range", "new.go", "RIGHT", 12, 10, false},
		{"overflow endpoint", "new.go", "RIGHT", 10, int(^uint(0) >> 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAnchor([]domain.File{file}, tc.path, tc.start, tc.end, tc.side)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
	for _, status := range []string{"unavailable", "truncated", ""} {
		file.PatchStatus = status
		if err := ValidateAnchor([]domain.File{file}, file.Path, 10, 10, "RIGHT"); err == nil {
			t.Errorf("accepted patch status %q", status)
		}
	}
}

func TestPatchStatusAndMalformedHunks(t *testing.T) {
	cases := []struct {
		name, patch, status  string
		additions, deletions int
	}{
		{"omitted", "", "unavailable", 20, 10},
		{"binary omitted", "", "unavailable", 0, 0},
		{"complete", "@@ -1 +1 @@\n-a\n+b", "available", 1, 1},
		{"missing whole hunk", "@@ -1 +1 @@\n-a\n+b", "truncated", 2, 1},
		{"short hunk", "@@ -1,2 +1,2 @@\n-a\n+b", "truncated", 1, 1},
		{"excess content", "@@ -1 +1 @@\n-a\n+b\n+extra", "truncated", 2, 1},
		{"bad header", "@@ nonsense @@\n-a\n+b", "truncated", 1, 1},
		{"zero source", "@@ -0,0 +1,2 @@\n+a\n+b\n", "available", 2, 0},
		{"zero target", "@@ -1,2 +0,0 @@\n-a\n-b", "available", 0, 2},
		{"newline markers", "@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n\\ No newline at end of file", "available", 1, 1},
		{"arbitrary slash line", "@@ -1 +1 @@\n-a\n+b\n\\ arbitrary", "truncated", 1, 1},
		{"newline marker before content", "@@ -1 +1 @@\n\\ No newline at end of file\n-a\n+b", "truncated", 1, 1},
		{"negative header", "@@ --1 +1 @@\n-a\n+b", "truncated", 1, 1},
		{"huge header", "@@ -" + strings.Repeat("9", 50) + " +1 @@\n-a\n+b", "truncated", 1, 1},
		{"line zero", "@@ -0 +1 @@\n-a\n+b", "truncated", 1, 1},
		{"overlapping hunks", "@@ -1 +1 @@\n-a\n+b\n@@ -1 +1 @@\n-a\n+b", "truncated", 2, 2},
		{"empty interior line", "@@ -1 +1 @@\n-a\n\n+b", "truncated", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := patchStatus(tc.patch, tc.additions, tc.deletions); got != tc.status {
				t.Fatalf("got %q, want %q", got, tc.status)
			}
			if tc.status == "truncated" {
				file := domain.File{Path: "x", Patch: tc.patch, PatchStatus: "available", Additions: tc.additions, Deletions: tc.deletions}
				if err := ValidateAnchor([]domain.File{file}, "x", 1, 1, "RIGHT"); err == nil {
					t.Fatal("trusted a forged available status")
				}
			}
		})
	}
}

func TestAddedAndDeletedFileSides(t *testing.T) {
	added := domain.File{Path: "added", Patch: "@@ -0,0 +1,2 @@\n+a\n+b", Additions: 2, PatchStatus: "available"}
	deleted := domain.File{Path: "deleted", Patch: "@@ -1,2 +0,0 @@\n-a\n-b", Deletions: 2, PatchStatus: "available"}
	files := []domain.File{added, deleted}
	for _, tc := range []struct {
		path, side string
		valid      bool
	}{
		{"added", "RIGHT", true}, {"added", "LEFT", false}, {"deleted", "LEFT", true}, {"deleted", "RIGHT", false},
	} {
		if err := ValidateAnchor(files, tc.path, 1, 2, tc.side); (err == nil) != tc.valid {
			t.Errorf("%s %s: %v", tc.path, tc.side, err)
		}
	}
}
