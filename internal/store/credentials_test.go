package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"skim/internal/domain"
)

func credentialStoreFixture(t *testing.T) (*Store, string) {
	t.Helper()
	t.Setenv("SKIM_STORE_CREDENTIAL_TEST", "environment-fixture")
	dir := t.TempDir()
	s, err := Open(dir, domain.Settings{Providers: []domain.ModelProvider{
		{ID: "first", Name: "First account", Kind: "openai", BaseURL: "https://first.invalid/v1", APIKeyEnv: "SKIM_STORE_CREDENTIAL_TEST"},
		{ID: "second", Name: "Second account", Kind: "typesafe", BaseURL: "https://second.invalid/v1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func saveCredentialSettings(t *testing.T, s *Store, settings domain.Settings, updates ...domain.ProviderKeyUpdate) {
	t.Helper()
	committed, err := s.UpdateSettings(domain.SettingsUpdate{Settings: settings, ProviderKeys: updates}, func(*Record, domain.Settings) {})
	if err != nil || !committed {
		t.Fatalf("settings transaction: committed=%v err=%v", committed, err)
	}
}

func TestSavedGitHubTokenOverridesEnvironmentAndStaysPrivate(t *testing.T) {
	t.Setenv("SKIM_STORE_GITHUB_TEST", "environment-github-token")
	dir := t.TempDir()
	s, err := Open(dir, domain.Settings{GitHubTokenEnv: "SKIM_STORE_GITHUB_TEST"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	save := func(update domain.GitHubTokenUpdate) error {
		_, err := s.UpdateSettings(domain.SettingsUpdate{Settings: s.Settings(), GitHubToken: update}, func(*Record, domain.Settings) {})
		return err
	}
	if s.GitHubToken() != "environment-github-token" || s.Settings().GitHubTokenStatus != "environment" {
		t.Fatal("environment token not used before a token is saved")
	}
	if err := save(domain.GitHubTokenUpdate{Token: "  saved-github-token  "}); err != nil {
		t.Fatal(err)
	}
	if s.GitHubToken() != "saved-github-token" || s.Settings().GitHubTokenStatus != "saved" {
		t.Fatal("saved token did not take precedence over the environment")
	}
	if err := save(domain.GitHubTokenUpdate{Token: " \t "}); err != nil || s.GitHubToken() != "saved-github-token" {
		t.Fatalf("blank update changed the saved token: %v", err)
	}
	public, _ := json.Marshal(s.Settings())
	if strings.Contains(string(public), "saved-github-token") {
		t.Fatal("public settings exposed the saved token")
	}
	if !strings.Contains(s.Redact("GitHub said saved-github-token"), "[redacted]") {
		t.Fatal("saved token not redacted from diagnostics")
	}
	for _, invalid := range []domain.GitHubTokenUpdate{{Token: "x", Remove: true}, {Token: "two\nlines"}} {
		if err := save(invalid); err == nil || s.GitHubToken() != "saved-github-token" {
			t.Fatalf("invalid update %+v was accepted", invalid)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(dir, domain.Settings{}); err != nil {
		t.Fatal(err)
	}
	if s.GitHubToken() != "saved-github-token" {
		t.Fatal("saved token did not survive restart")
	}
	if err := save(domain.GitHubTokenUpdate{Remove: true}); err != nil {
		t.Fatal(err)
	}
	if s.GitHubToken() != "environment-github-token" || s.Settings().GitHubTokenStatus != "environment" {
		t.Fatal("removal did not fall back to the environment token")
	}
	t.Setenv("SKIM_STORE_GITHUB_TEST", "")
	if s.GitHubToken() != "" || s.Settings().GitHubTokenStatus != "missing" {
		t.Fatal("no token reported as available")
	}
}

func TestSettingsSaveCommitsStyleAtomically(t *testing.T) {
	s, _ := credentialStoreFixture(t)
	style := "Be brief."
	settings := s.Settings()
	settings.ACP.Command = "agent"
	if _, err := s.UpdateSettings(domain.SettingsUpdate{Settings: settings, Style: &style}, func(*Record, domain.Settings) {}); err != nil {
		t.Fatal(err)
	}
	if s.State().Style != style || s.Settings().ACP.Command != "agent" {
		t.Fatal("style and settings were not saved together")
	}
	if _, err := s.UpdateSettings(domain.SettingsUpdate{Settings: s.Settings()}, func(*Record, domain.Settings) {}); err != nil || s.State().Style != style {
		t.Fatalf("omitted style changed the saved style: %v", err)
	}
	before := s.Snapshot()
	oversized := strings.Repeat("x", 32001)
	settings.ACP.Command = "other"
	if _, err := s.UpdateSettings(domain.SettingsUpdate{Settings: settings, Style: &oversized}, func(*Record, domain.Settings) {}); err == nil || !reflect.DeepEqual(s.Snapshot(), before) {
		t.Fatal("oversized style was not rejected with the whole save")
	}
}

func TestCredentialsPersistPrivatelyAndReopen(t *testing.T) {
	s, dir := credentialStoreFixture(t)
	saveCredentialSettings(t, s, s.Settings(), domain.ProviderKeyUpdate{ProviderID: "first", APIKey: "saved-fixture-key"})
	before := s.Settings()
	if before.Providers[0].CredentialStatus != "saved" || before.Providers[0].CredentialRevision == "" {
		t.Fatal("saved credential lacks status or revision")
	}
	for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "state.json"): 0600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s permissions = %o, want %o", path, info.Mode().Perm(), mode)
		}
	}
	for _, public := range []any{s.Settings(), s.State(), s.Snapshot().Settings} {
		data, err := json.Marshal(public)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "saved-fixture-key") || strings.Contains(string(data), before.Providers[0].CredentialRevision) {
			t.Fatal("public JSON exposed a credential or its private revision")
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk struct {
		Settings     json.RawMessage        `json:"settings"`
		ProviderKeys map[string]providerKey `json:"providerKeys"`
	}
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.ProviderKeys["first"].Key != "saved-fixture-key" || strings.Contains(string(disk.Settings), "saved-fixture-key") {
		t.Fatal("credential was not persisted exclusively in private root storage")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening must also repair overly permissive existing storage.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "state.json"), 0644); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	p := reopened.Settings().Providers[0]
	key, err := reopened.ResolveCredential(p)
	if err != nil || key != "saved-fixture-key" || p.CredentialRevision != before.Providers[0].CredentialRevision || p.CredentialStatus != "saved" {
		t.Fatalf("reopen lost credential identity/status: %v", err)
	}
	for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "state.json"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("reopen did not protect %s: %v", path, err)
		}
	}
}

func TestCredentialsKeepReplaceRemoveAndProviderIsolation(t *testing.T) {
	s, _ := credentialStoreFixture(t)
	if p := s.Settings().Providers[0]; p.CredentialStatus != "environment" || p.CredentialRevision != "" {
		t.Fatal("environment credential status missing")
	}
	saveCredentialSettings(t, s, s.Settings(), domain.ProviderKeyUpdate{ProviderID: "first", APIKey: "first-key"}, domain.ProviderKeyUpdate{ProviderID: "second", APIKey: "second-key"})
	original := s.Snapshot().ProviderKeys
	for _, updates := range [][]domain.ProviderKeyUpdate{nil, {{ProviderID: "first", APIKey: " \t "}}, {{ProviderID: "first", APIKey: " first-key "}}} {
		saveCredentialSettings(t, s, s.Settings(), updates...)
		if !reflect.DeepEqual(s.Snapshot().ProviderKeys, original) {
			t.Fatal("blank, omitted, or unchanged key modified credential identity")
		}
	}
	saveCredentialSettings(t, s, s.Settings(), domain.ProviderKeyUpdate{ProviderID: "first", APIKey: "replacement-key"})
	keys := s.Snapshot().ProviderKeys
	if keys["first"].Key != "replacement-key" || keys["first"].Revision == original["first"].Revision || keys["second"] != original["second"] {
		t.Fatal("replacement did not isolate the selected provider")
	}
	saveCredentialSettings(t, s, s.Settings(), domain.ProviderKeyUpdate{ProviderID: "first", Remove: true})
	p := s.Settings().Providers[0]
	if p.CredentialStatus != "environment" || p.CredentialRevision != "" {
		t.Fatal("explicit removal did not restore environment fallback")
	}
	if key, err := s.ResolveCredential(p); err != nil || key != "" {
		t.Fatal("removed saved credential still resolves")
	}
	t.Setenv("SKIM_STORE_CREDENTIAL_TEST", "")
	if s.Settings().Providers[0].CredentialStatus != "missing" {
		t.Fatal("empty environment did not produce missing credential status")
	}
	settings := s.Settings()
	settings.Providers = settings.Providers[:1]
	saveCredentialSettings(t, s, settings)
	if len(s.Snapshot().ProviderKeys) != 0 {
		t.Fatal("deleted provider retained its private credential")
	}
}

func TestCredentialsRejectStaleSnapshotsAndDestinationChanges(t *testing.T) {
	s, _ := credentialStoreFixture(t)
	saveCredentialSettings(t, s, s.Settings(), domain.ProviderKeyUpdate{ProviderID: "first", APIKey: "first-key"})
	old := s.Settings().Providers[0]
	for _, field := range []string{"url", "kind"} {
		t.Run(field, func(t *testing.T) {
			settings := s.Settings()
			if field == "url" {
				settings.Providers[0].BaseURL = "https://changed.invalid/v1"
			} else {
				settings.Providers[0].Kind = "typesafe"
			}
			if _, err := s.ResolveCredential(settings.Providers[0]); err == nil {
				t.Fatal("credential resolved for a different destination")
			}
			before := s.Snapshot()
			committed, err := s.UpdateSettings(domain.SettingsUpdate{Settings: settings}, func(*Record, domain.Settings) {})
			if committed || err == nil || !reflect.DeepEqual(s.Snapshot(), before) {
				t.Fatal("destination change without explicit key decision was not rejected atomically")
			}
		})
	}
	settings := s.Settings()
	settings.Providers[0].BaseURL += "/"
	saveCredentialSettings(t, s, settings)
	if key, err := s.ResolveCredential(old); err != nil || key != "first-key" {
		t.Fatal("equivalent trailing slash invalidated credential")
	}
	settings = s.Settings()
	settings.Providers[0].BaseURL = "https://changed.invalid/v1"
	saveCredentialSettings(t, s, settings, domain.ProviderKeyUpdate{ProviderID: "first", APIKey: "first-key"})
	if _, err := s.ResolveCredential(old); err == nil {
		t.Fatal("stale snapshot resolved after destination replacement")
	}
	current := s.Settings().Providers[0]
	if current.CredentialRevision == old.CredentialRevision {
		t.Fatal("destination replacement retained old revision")
	}
	if key, err := s.ResolveCredential(current); err != nil || key != "first-key" {
		t.Fatal("replacement credential did not resolve at new destination")
	}
	saveCredentialSettings(t, s, s.Settings(), domain.ProviderKeyUpdate{ProviderID: "first", Remove: true})
	if _, err := s.ResolveCredential(current); err == nil {
		t.Fatal("removed credential resolved through stale snapshot")
	}
}

func TestCredentialsFailedPersistLeavesSettingsKeysAndAnalysisUnchanged(t *testing.T) {
	s, dir := credentialStoreFixture(t)
	saveCredentialSettings(t, s, s.Settings(), domain.ProviderKeyUpdate{ProviderID: "first", APIKey: "original-key"})
	d := s.Snapshot()
	d.Active = "review"
	d.Reviews[d.Active] = Record{State: domain.State{PullRequest: &domain.PullRequest{HeadSHA: "head"}, Analysis: &domain.Analysis{Generation: "original"}}}
	if err := s.Commit(d); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	// Keep the real state intact while deterministically forcing rename failure.
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	originalPath := s.path
	s.path = blocked
	defer func() { s.path = originalPath }()
	settings := s.Settings()
	settings.Providers[0].BaseURL = "https://replacement.invalid/v1"
	prepared := false
	committed, err := s.UpdateSettings(domain.SettingsUpdate{Settings: settings, ProviderKeys: []domain.ProviderKeyUpdate{{ProviderID: "first", APIKey: "replacement-key"}}}, func(r *Record, settings domain.Settings) {
		prepared = true
		r.State.Analysis.Generation = "replacement"
		if settings.Providers[0].CredentialStatus != "saved" {
			t.Error("analysis preparation did not receive candidate credentials")
		}
	})
	if err == nil || committed || !prepared {
		t.Fatalf("failed persistence result: committed=%v prepared=%v err=%v", committed, prepared, err)
	}
	if !reflect.DeepEqual(s.Snapshot(), before) {
		t.Fatal("failed persist changed settings, credentials, or analysis in memory")
	}
	key, err := s.ResolveCredential(before.Settings.Providers[0])
	if err != nil || key != "original-key" {
		t.Fatal("failed replacement revoked original credential")
	}
	data, err := os.ReadFile(originalPath)
	if err != nil || strings.Contains(string(data), "replacement-key") || !strings.Contains(string(data), "original-key") {
		t.Fatalf("failed replacement changed persisted credential: %v", err)
	}
}

func TestCredentialsInvalidUpdatesAreAtomic(t *testing.T) {
	s, _ := credentialStoreFixture(t)
	saveCredentialSettings(t, s, s.Settings(), domain.ProviderKeyUpdate{ProviderID: "first", APIKey: "saved-key"})
	before := s.Snapshot()
	for _, tc := range []struct {
		name    string
		updates []domain.ProviderKeyUpdate
	}{
		{"unknown-provider", []domain.ProviderKeyUpdate{{ProviderID: "unknown", APIKey: "new-key"}}},
		{"duplicate-provider", []domain.ProviderKeyUpdate{{ProviderID: "first", APIKey: "new-key"}, {ProviderID: "first", Remove: true}}},
		{"replace-and-remove", []domain.ProviderKeyUpdate{{ProviderID: "first", APIKey: "new-key", Remove: true}}},
		{"embedded-newline", []domain.ProviderKeyUpdate{{ProviderID: "first", APIKey: "new\nkey"}}},
		{"embedded-null", []domain.ProviderKeyUpdate{{ProviderID: "first", APIKey: "new\x00key"}}},
		{"oversized", []domain.ProviderKeyUpdate{{ProviderID: "first", APIKey: strings.Repeat("x", 8193)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			committed, err := s.UpdateSettings(domain.SettingsUpdate{Settings: s.Settings(), ProviderKeys: tc.updates}, func(*Record, domain.Settings) {
				t.Error("invalid credential transaction reached analysis preparation")
			})
			if committed || err == nil || !reflect.DeepEqual(s.Snapshot(), before) {
				t.Fatal("invalid credential update was not rejected atomically")
			}
		})
	}
	if got := s.Redact("saved-key and environment-fixture"); got != "[redacted] and [redacted]" {
		t.Fatalf("store did not redact both credential sources: %q", got)
	}
}

func TestNamedSameKindInstancesRetainIDBoundRolesAndKeysOnRename(t *testing.T) {
	s, dir := credentialStoreFixture(t)
	settings := s.Settings()
	settings.Providers[1].Kind = "openai"
	settings.Providers[1].BaseURL = settings.Providers[0].BaseURL
	settings.Roles = domain.Roles{
		Chat:   domain.ModelRole{Provider: "first", Model: "chat-model"},
		Review: domain.ModelRole{Provider: "second", Model: "review-model"},
	}
	saveCredentialSettings(t, s, settings,
		domain.ProviderKeyUpdate{ProviderID: "first", APIKey: "first-account-key"},
		domain.ProviderKeyUpdate{ProviderID: "second", APIKey: "second-account-key"})
	before := s.Settings()
	keys := s.Snapshot().ProviderKeys
	settings = s.Settings()
	settings.Providers[0].Name = "Renamed first account"
	settings.Providers[1].Name = "Renamed second account"
	// Reordering must not bind credentials or roles to a table position.
	settings.Providers[0], settings.Providers[1] = settings.Providers[1], settings.Providers[0]
	saveCredentialSettings(t, s, settings)
	assertIdentity := func(s *Store) {
		t.Helper()
		got := s.Settings()
		if got.Roles != before.Roles || !reflect.DeepEqual(s.Snapshot().ProviderKeys, keys) {
			t.Fatal("rename changed stable role bindings or private key identity")
		}
		for _, p := range got.Providers {
			if p.Name != "Renamed "+p.ID+" account" || p.CredentialStatus != "saved" || p.CredentialRevision != keys[p.ID].Revision {
				t.Fatalf("provider identity was not retained: %+v", p)
			}
			if key, err := s.ResolveCredential(p); err != nil || key != p.ID+"-account-key" {
				t.Fatalf("wrong credential for %s: %v", p.ID, err)
			}
		}
		for _, p := range before.Providers {
			if key, err := s.ResolveCredential(p); err != nil || key != p.ID+"-account-key" {
				t.Fatalf("display rename revoked existing snapshot for %s: %v", p.ID, err)
			}
		}
	}
	assertIdentity(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, domain.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertIdentity(reopened)
}
