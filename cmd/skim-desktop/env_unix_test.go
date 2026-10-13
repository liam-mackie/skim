//go:build !windows

package main

import (
	"reflect"
	"testing"
)

func TestMergeEnvironmentPrecedence(t *testing.T) {
	launcher := []string{"PATH=/usr/bin:/bin", "HOME=/Users/me", "GITHUB_TOKEN=launcher"}
	shell := []string{
		"PATH=/opt/homebrew/bin:/usr/bin:/bin", // GUI PATH is minimal: shell PATH wins
		"HOME=/elsewhere",                      // launcher values otherwise win
		"GITHUB_TOKEN=shell",
		"ANTHROPIC_API_KEY=k=v",                                               // values may contain '='
		"PWD=/tmp", "SHLVL=2", "_=/usr/bin/env", "SKIM_DESKTOP_EXE=/app/skim", // probe-shell state
	}
	got := mergeEnvironment(launcher, shell)
	want := map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin:/bin", "ANTHROPIC_API_KEY": "k=v"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnvironment = %v, want %v", got, want)
	}
}
