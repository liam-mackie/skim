package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"skim/internal/app"
	"skim/internal/domain"
	"skim/internal/provider"
	"skim/internal/store"
)

const credentialHTTPNewKey = "fixture-http-replacement-secret"

type credentialHTTPRequest struct {
	path string
	auth string
	body string
}

type credentialHTTPPeer struct {
	mu         sync.Mutex
	requests   []credentialHTTPRequest
	failKey    string
	reflectKey bool
}

func (p *credentialHTTPPeer) handler(control *analysisModelControl) http.Handler {
	var next http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		p.mu.Lock()
		fail, echo := p.failKey != "" && p.failKey == key, p.reflectKey
		p.mu.Unlock()
		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Rejected credential " + key})
			return
		}
		if echo && r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "Reflected credential " + key}, "finish_reason": "stop"}}})
			return
		}
		modelFixture(w, r)
	})
	if control != nil {
		next = control.wrap(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cannot read fixture request", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		p.mu.Lock()
		p.requests = append(p.requests, credentialHTTPRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(body)})
		p.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (p *credentialHTTPPeer) calls() []credentialHTTPRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]credentialHTTPRequest(nil), p.requests...)
}

// No Defaults or inherited provider environment is used. Every provider and
// GitHub request is confined to a local HTTP peer; all state is temporary.
func newCredentialHTTPHarness(t *testing.T, control *analysisModelControl) (*harness, *credentialHTTPPeer) {
	t.Helper()
	_, sha := workspace(t)
	f := &fixture{head: sha, reviews: []map[string]any{}}
	gh := httptest.NewServer(http.HandlerFunc(f.github))
	t.Cleanup(gh.Close)
	peer := &credentialHTTPPeer{}
	models := httptest.NewServer(peer.handler(control))
	t.Cleanup(models.Close)
	settings := domain.Settings{
		Providers: []domain.ModelProvider{
			{ID: "text", Name: "Review fixture", Kind: "openai", BaseURL: models.URL + "/v1", APIKeyEnv: "SKIM_CREDENTIAL_HTTP_TEXT"},
			{ID: "risk", Name: "Risk fixture", Kind: "typesafe", BaseURL: models.URL + "/v1", APIKeyEnv: "SKIM_CREDENTIAL_HTTP_RISK"},
			{ID: "chat", Name: "Chat fixture", Kind: "openai", BaseURL: models.URL + "/v1", APIKeyEnv: "SKIM_CREDENTIAL_HTTP_CHAT"},
		},
		Roles: domain.Roles{
			Review: domain.ModelRole{Provider: "text", Model: "fixture-text"},
			Jev:    domain.ModelRole{Provider: "risk", Model: "jev-latest"},
			Chat:   domain.ModelRole{Provider: "chat", Model: "fixture-chat"},
		},
	}
	for _, p := range settings.Providers {
		t.Setenv(p.APIKeyEnv, "")
	}
	// The secret-shaped directory lets persistence failures exercise redaction
	// of the submitted, not-yet-saved key in an actual OS error message.
	dataDir := filepath.Join(t.TempDir(), credentialHTTPNewKey)
	s, err := store.Open(dataDir, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	a := app.New(s)
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	a.Provider = func(domain.Settings) provider.Provider {
		p := provider.NewGitHub("fixture-token")
		p.BaseURL, p.Client = gh.URL, gh.Client()
		return p
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := New(a, Config{Address: server.Listener.Addr().String(), Web: os.DirFS(t.TempDir())})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	if control != nil {
		t.Cleanup(func() { control.release(nil, false, true) })
	}
	return &harness{base: server.URL, app: a, store: s, fixture: f, dataDir: dataDir}, peer
}

func credentialHTTPRaw(t *testing.T, h *harness, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, h.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data
}

func credentialHTTPJSON(t *testing.T, h *harness, method, path string, input any) (int, []byte) {
	t.Helper()
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
	}
	return credentialHTTPRaw(t, h, method, path, body)
}

func assertCredentialHTTPPublic(t *testing.T, body []byte, keys ...string) {
	t.Helper()
	for _, key := range keys {
		encoded, _ := json.Marshal(key)
		if key != "" && (bytes.Contains(body, []byte(key)) || bytes.Contains(body, encoded[1:len(encoded)-1])) {
			t.Fatal("HTTP response exposed a provider credential")
		}
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	var inspect func(any)
	inspect = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if key == "providerKeys" || key == "apiKey" || key == "credentialRevision" {
					t.Fatalf("HTTP response exposed private field %q", key)
				}
				inspect(child)
			}
		case []any:
			for _, child := range v {
				inspect(child)
			}
		}
	}
	inspect(value)
}

func credentialHTTPSettings(t *testing.T, h *harness, settings domain.Settings, updates ...domain.ProviderKeyUpdate) domain.Settings {
	t.Helper()
	status, body := credentialHTTPJSON(t, h, http.MethodPut, "/api/settings", domain.SettingsUpdate{Settings: settings, ProviderKeys: updates})
	if status != http.StatusOK {
		t.Fatalf("settings update: HTTP %d", status)
	}
	keys := make([]string, 0, len(updates))
	for _, update := range updates {
		if strings.TrimSpace(update.APIKey) != "" {
			keys = append(keys, strings.TrimSpace(update.APIKey))
		}
	}
	assertCredentialHTTPPublic(t, body, keys...)
	var result domain.Settings
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHTTPCredentialsWriteOnlyAndEnvironmentFallback(t *testing.T) {
	h, peer := newCredentialHTTPHarness(t, nil)
	const saved = "fixture-http-saved-secret"
	const environment = "fixture-http-environment-secret"
	var settings domain.Settings
	h.request(t, http.MethodGet, "/api/settings", nil, &settings)
	for _, p := range settings.Providers {
		if p.CredentialStatus != "missing" {
			t.Fatalf("unexpected initial credential status: %q", p.CredentialStatus)
		}
	}
	t.Setenv("SKIM_CREDENTIAL_HTTP_CHAT", environment)
	h.request(t, http.MethodGet, "/api/settings", nil, &settings)
	if settings.Providers[2].CredentialStatus != "environment" {
		t.Fatal("environment credential was not reported")
	}
	settings = credentialHTTPSettings(t, h, settings, domain.ProviderKeyUpdate{ProviderID: "chat", APIKey: "  " + saved + "  "})
	if settings.Providers[2].CredentialStatus != "saved" {
		t.Fatal("saved credential did not take precedence")
	}
	for _, path := range []string{"/api/settings", "/api/state"} {
		status, body := credentialHTTPJSON(t, h, http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Fatalf("read %s: HTTP %d", path, status)
		}
		assertCredentialHTTPPublic(t, body, saved, environment)
	}
	settings = credentialHTTPSettings(t, h, settings, domain.ProviderKeyUpdate{ProviderID: "chat", APIKey: " \t "})
	if settings.Providers[2].CredentialStatus != "saved" {
		t.Fatal("blank update removed the saved credential")
	}
	// Analysis intentionally lacks keys; foreground chat must still use its own
	// selected provider, without leaking credentials into the request payload.
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	for _, expected := range []string{saved, environment, ""} {
		if expected == environment {
			settings = credentialHTTPSettings(t, h, settings, domain.ProviderKeyUpdate{ProviderID: "chat", Remove: true})
			if settings.Providers[2].CredentialStatus != "environment" {
				t.Fatal("remove did not fall back to the environment")
			}
		} else if expected == "" {
			t.Setenv("SKIM_CREDENTIAL_HTTP_CHAT", "")
			settings = h.app.Settings()
			if settings.Providers[2].CredentialStatus != "missing" {
				t.Fatal("removed key unexpectedly remained available")
			}
		}
		before := len(peer.calls())
		status, body := credentialHTTPJSON(t, h, http.MethodPost, "/api/chat", map[string]string{"message": "Explain arithmetic."})
		assertCredentialHTTPPublic(t, body, saved, environment)
		calls := peer.calls()
		if expected == "" {
			if status != http.StatusBadRequest || len(calls) != before {
				t.Fatal("missing credential sent a model request")
			}
			continue
		}
		if status != http.StatusOK || len(calls) != before+1 || calls[before].auth != "Bearer "+expected {
			t.Fatal("chat did not use the selected credential")
		}
		if strings.Contains(calls[before].body, saved) || strings.Contains(calls[before].body, environment) {
			t.Fatal("credential entered the model prompt")
		}
	}
}

func TestHTTPCredentialsMalformedSettingsNeverEchoSubmittedKey(t *testing.T) {
	h, _ := newCredentialHTTPHarness(t, nil)
	const secret = "fixture-http-malformed-secret"
	before := h.app.Settings()
	valid, err := json.Marshal(domain.SettingsUpdate{Settings: before, ProviderKeys: []domain.ProviderKeyUpdate{{ProviderID: "chat", APIKey: secret}}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"unknown secret field":        []byte(`{"` + secret + `":true}`),
		"nested unknown secret field": []byte(`{"providerKeys":[{"providerId":"chat","apiKey":"` + secret + `","` + secret + `":true}]}`),
		"wrong field type":            []byte(`{"providerKeys":[{"providerId":"chat","apiKey":"` + secret + `","remove":"` + secret + `"}]}`),
		"truncated value":             append(append([]byte(nil), valid[:len(valid)-1]...), []byte(`,"`+secret+`":`)...),
		"multiple values":             append(append([]byte(nil), valid...), []byte(` {"`+secret+`":true}`)...),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			status, response := credentialHTTPRaw(t, h, http.MethodPut, "/api/settings", body)
			if status != http.StatusBadRequest {
				t.Fatalf("malformed settings accepted: HTTP %d", status)
			}
			assertCredentialHTTPPublic(t, response, secret)
			if !reflect.DeepEqual(h.app.Settings(), before) {
				t.Fatal("malformed update changed settings or credentials")
			}
		})
	}
}

func TestHTTPCredentialsRedactModelErrorsAndPersistedOutput(t *testing.T) {
	h, peer := newCredentialHTTPHarness(t, nil)
	// Quotes/backslashes exercise JSON-escaped reflection as well as plain text.
	const secret = "fixture-http-reflected-\"secret\\value"
	settings := credentialHTTPSettings(t, h, h.app.Settings(),
		domain.ProviderKeyUpdate{ProviderID: "text", APIKey: secret},
		domain.ProviderKeyUpdate{ProviderID: "risk", APIKey: secret},
		domain.ProviderKeyUpdate{ProviderID: "chat", APIKey: secret})
	peer.mu.Lock()
	peer.failKey = secret
	peer.mu.Unlock()
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	failed := h.awaitAnalysis(t, analysisTerminal)
	if failed.Analysis.Risk.Status != "failed" || failed.Analysis.Grouping.Status != "failed" || len(failed.Analysis.Risk.FailedFiles) != 2 {
		t.Fatal("reflected model errors did not reach analysis failure state")
	}
	for _, route := range []string{"/api/settings", "/api/state", "/api/analysis"} {
		status, body := credentialHTTPJSON(t, h, http.MethodGet, route, nil)
		if status != http.StatusOK {
			t.Fatalf("read %s: HTTP %d", route, status)
		}
		assertCredentialHTTPPublic(t, body, secret)
		if route != "/api/settings" && !bytes.Contains(body, []byte("[redacted]")) {
			t.Fatal("persisted analysis error did not retain the redaction marker")
		}
	}
	if status, body := credentialHTTPJSON(t, h, http.MethodPost, "/api/chat", map[string]string{"message": "Explain arithmetic."}); status != http.StatusBadRequest || !bytes.Contains(body, []byte("[redacted]")) {
		t.Fatal("model error did not report a redacted credential")
	} else {
		assertCredentialHTTPPublic(t, body, secret)
	}
	peer.mu.Lock()
	peer.failKey, peer.reflectKey = "", true
	peer.mu.Unlock()
	status, body := credentialHTTPJSON(t, h, http.MethodPost, "/api/chat", map[string]string{"message": "Reflect the fixture credential."})
	if status != http.StatusOK || !bytes.Contains(body, []byte("[redacted]")) {
		t.Fatal("successful reflected model output was not redacted")
	}
	assertCredentialHTTPPublic(t, body, secret)
	// Removing the private keys makes later read checks independent of the
	// current credential redactor: previously persisted output must be clean.
	credentialHTTPSettings(t, h, settings,
		domain.ProviderKeyUpdate{ProviderID: "text", Remove: true},
		domain.ProviderKeyUpdate{ProviderID: "risk", Remove: true},
		domain.ProviderKeyUpdate{ProviderID: "chat", Remove: true})
	h.awaitAnalysis(t, analysisTerminal)
	for _, route := range []string{"/api/settings", "/api/state", "/api/analysis"} {
		status, body := credentialHTTPJSON(t, h, http.MethodGet, route, nil)
		if status != http.StatusOK {
			t.Fatalf("read %s: HTTP %d", route, status)
		}
		assertCredentialHTTPPublic(t, body, secret)
	}
	persisted, err := os.ReadFile(filepath.Join(h.dataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(secret)
	if bytes.Contains(persisted, []byte(secret)) || bytes.Contains(persisted, encoded[1:len(encoded)-1]) {
		t.Fatal("removed credential remained in persisted model output")
	}
}

func TestHTTPCredentialsDestinationChangeRequiresExplicitDecision(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := "replace"
		if remove {
			name = "remove"
		}
		t.Run(name, func(t *testing.T) {
			h, oldPeer := newCredentialHTTPHarness(t, nil)
			const oldKey = "fixture-http-original-destination-secret"
			const newKey = "fixture-http-new-destination-secret"
			const envKey = "fixture-http-destination-environment-secret"
			t.Setenv("SKIM_CREDENTIAL_HTTP_CHAT", envKey)
			settings := credentialHTTPSettings(t, h, h.app.Settings(), domain.ProviderKeyUpdate{ProviderID: "chat", APIKey: oldKey})
			newPeer := &credentialHTTPPeer{}
			destination := httptest.NewServer(newPeer.handler(nil))
			t.Cleanup(destination.Close)
			changed := settings
			changed.Providers = append([]domain.ModelProvider(nil), settings.Providers...)
			changed.Providers[2].BaseURL = destination.URL + "/v1"
			for _, updates := range [][]domain.ProviderKeyUpdate{nil, {{ProviderID: "chat", APIKey: " "}}} {
				status, body := credentialHTTPJSON(t, h, http.MethodPut, "/api/settings", domain.SettingsUpdate{Settings: changed, ProviderKeys: updates})
				if status != http.StatusBadRequest {
					t.Fatal("destination changed without a credential decision")
				}
				assertCredentialHTTPPublic(t, body, oldKey, newKey, envKey)
				if h.app.Settings().Providers[2].BaseURL != settings.Providers[2].BaseURL || len(newPeer.calls()) != 0 {
					t.Fatal("rejected destination update became observable")
				}
			}
			var state domain.State
			h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
			h.awaitAnalysis(t, analysisTerminal)
			h.request(t, http.MethodPost, "/api/chat", map[string]string{"message": "Use original destination."}, nil)
			calls := oldPeer.calls()
			if len(calls) != 1 || calls[0].auth != "Bearer "+oldKey {
				t.Fatal("rejected destination update lost the original key/URL pair")
			}
			update := domain.ProviderKeyUpdate{ProviderID: "chat", APIKey: newKey}
			wantKey, wantStatus := newKey, "saved"
			if remove {
				update.APIKey, update.Remove = "", true
				wantKey, wantStatus = envKey, "environment"
			}
			changed = credentialHTTPSettings(t, h, changed, update)
			if changed.Providers[2].CredentialStatus != wantStatus {
				t.Fatal("explicit credential decision returned the wrong status")
			}
			h.request(t, http.MethodPost, "/api/chat", map[string]string{"message": "Use new destination."}, nil)
			calls = newPeer.calls()
			if len(calls) != 1 || calls[0].auth != "Bearer "+wantKey || len(oldPeer.calls()) != 1 {
				t.Fatal("new destination received the wrong credential")
			}
		})
	}
}

// Force the rename itself to fail, before the transaction commits. This is not
// the post-rename directory-sync uncertainty case, which has already committed.
func blockCredentialHTTPCommit(t *testing.T, h *harness) func() {
	t.Helper()
	path := filepath.Join(h.dataDir, "state.json")
	backup := filepath.Join(h.dataDir, "saved-state.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		_ = os.Rename(backup, path)
		t.Fatal(err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
			if err := os.Rename(backup, path); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}

func TestHTTPCredentialsPersistenceFailureKeepsOldPairAndRunningGeneration(t *testing.T) {
	control := newAnalysisModelControl()
	control.hold([]string{"src/calc.go"}, true)
	h, peer := newCredentialHTTPHarness(t, control)
	const oldKey = "fixture-http-persistence-old-secret"
	settings := credentialHTTPSettings(t, h, h.app.Settings(),
		domain.ProviderKeyUpdate{ProviderID: "risk", APIKey: oldKey},
		domain.ProviderKeyUpdate{ProviderID: "text", APIKey: oldKey})
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	waitCondition(t, func() bool {
		control.mu.Lock()
		defer control.mu.Unlock()
		return control.riskStarted["src/calc.go"] == 1 && control.groupStarted == 1
	})
	before := h.app.Analysis()
	persisted, err := os.ReadFile(filepath.Join(h.dataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	changed := settings
	changed.Providers = append([]domain.ModelProvider(nil), settings.Providers...)
	changed.Providers[1].BaseURL += "/replacement"
	restore := blockCredentialHTTPCommit(t, h)
	status, body := credentialHTTPJSON(t, h, http.MethodPut, "/api/settings", domain.SettingsUpdate{Settings: changed, ProviderKeys: []domain.ProviderKeyUpdate{{ProviderID: "risk", APIKey: credentialHTTPNewKey}}})
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("replace state")) || !bytes.Contains(body, []byte("[redacted]")) {
		t.Fatal("injected pre-commit persistence failure was not reported with redaction")
	}
	assertCredentialHTTPPublic(t, body, oldKey, credentialHTTPNewKey)
	if !reflect.DeepEqual(h.app.Analysis(), before) || h.app.Settings().Providers[1].BaseURL != settings.Providers[1].BaseURL || h.app.Settings().Providers[1].CredentialStatus != "saved" {
		t.Fatal("failed settings persistence changed the active generation or key/URL pair")
	}
	restore()
	after, err := os.ReadFile(filepath.Join(h.dataDir, "state.json"))
	if err != nil || !bytes.Equal(after, persisted) {
		t.Fatal("failed settings update changed the persisted snapshot")
	}
	control.release(nil, false, true)
	complete := h.awaitAnalysis(t, analysisTerminal)
	if complete.Analysis.Generation != before.Analysis.Generation || complete.Analysis.Risk.Status != "complete" || complete.Analysis.Grouping.Status != "complete" {
		t.Fatal("failed settings update prevented the original generation from finishing")
	}
	control.mu.Lock()
	cancelled := control.riskCancelled + control.groupCancelled
	riskFirst, riskSecond, groups := control.riskStarted["src/calc.go"], control.riskStarted["src/calc_test.go"], control.groupStarted
	control.mu.Unlock()
	if cancelled != 0 || riskFirst != 1 || riskSecond != 1 || groups != 1 {
		t.Fatal("failed settings update cancelled or restarted paid analysis")
	}
	for _, call := range peer.calls() {
		if call.auth != "Bearer "+oldKey || strings.Contains(call.path, "replacement") {
			t.Fatal("failed transaction altered the credential or destination used by running analysis")
		}
	}
}

func TestHTTPCredentialsActiveAnalysisRetriesOnlyRelevantRole(t *testing.T) {
	control := newAnalysisModelControl()
	control.hold([]string{"src/calc.go"}, false)
	h, peer := newCredentialHTTPHarness(t, control)
	const oldRisk = "fixture-http-analysis-old-risk"
	const newRisk = "fixture-http-analysis-new-risk"
	const textKey = "fixture-http-analysis-text-secret"
	const chatKey = "fixture-http-analysis-chat-secret"
	settings := credentialHTTPSettings(t, h, h.app.Settings(),
		domain.ProviderKeyUpdate{ProviderID: "risk", APIKey: oldRisk},
		domain.ProviderKeyUpdate{ProviderID: "text", APIKey: textKey})
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	partial := h.awaitAnalysis(t, func(u domain.AnalysisUpdate) bool {
		control.mu.Lock()
		defer control.mu.Unlock()
		return u.Analysis != nil && u.Analysis.Grouping.Status == "complete" && control.riskStarted["src/calc.go"] == 1
	})
	var manual domain.Comment
	h.request(t, http.MethodPost, "/api/comments", manualRequest(state.PullRequest, "RIGHT", 2, 2, "Keep this human-authored observation."), &manual)
	if manual.Source != "manual" {
		t.Fatal("manual comment provenance changed")
	}
	settings = credentialHTTPSettings(t, h, settings, domain.ProviderKeyUpdate{ProviderID: "chat", APIKey: chatKey})
	if h.app.Analysis().Analysis.Generation != partial.Analysis.Generation {
		t.Fatal("unrelated chat credential restarted analysis")
	}
	settings = credentialHTTPSettings(t, h, settings, domain.ProviderKeyUpdate{ProviderID: "risk", APIKey: " "})
	settings = credentialHTTPSettings(t, h, settings, domain.ProviderKeyUpdate{ProviderID: "risk", APIKey: oldRisk})
	if h.app.Analysis().Analysis.Generation != partial.Analysis.Generation {
		t.Fatal("blank or identical credential update restarted analysis")
	}
	credentialHTTPSettings(t, h, settings, domain.ProviderKeyUpdate{ProviderID: "risk", APIKey: newRisk})
	waitCondition(t, func() bool {
		control.mu.Lock()
		defer control.mu.Unlock()
		return control.riskStarted["src/calc.go"] == 2 && control.riskCancelled == 1
	})
	changed := h.app.Analysis()
	if changed.Analysis.Generation == partial.Analysis.Generation || changed.Analysis.Grouping.Status != "complete" || !reflect.DeepEqual(changed.Groups, partial.Groups) {
		t.Fatal("risk credential update failed to preserve successful unrelated grouping")
	}
	control.release(nil, false, true)
	complete := h.awaitAnalysis(t, analysisTerminal)
	if complete.Analysis.Risk.Status != "complete" || complete.Analysis.Grouping.Status != "complete" || !reflect.DeepEqual(complete.Groups, partial.Groups) {
		t.Fatal("replacement credential did not complete scoped analysis")
	}
	h.request(t, http.MethodGet, "/api/state", nil, &state)
	if len(state.Comments) != 1 || !reflect.DeepEqual(state.Comments[0], manual) {
		t.Fatal("credential change overwrote the manual comment")
	}
	control.mu.Lock()
	riskFirst, riskSecond, groups := control.riskStarted["src/calc.go"], control.riskStarted["src/calc_test.go"], control.groupStarted
	control.mu.Unlock()
	if riskFirst != 2 || riskSecond != 1 || groups != 1 {
		t.Fatal("credential replacement repeated unrelated successful work")
	}
	var riskKeys []string
	for _, call := range peer.calls() {
		if call.path == "/v1/systemone" {
			riskKeys = append(riskKeys, call.auth)
		}
		for _, key := range []string{oldRisk, newRisk, textKey, chatKey} {
			if strings.Contains(call.body, key) {
				t.Fatal("analysis prompt contained a provider key")
			}
		}
	}
	if !reflect.DeepEqual(riskKeys, []string{"Bearer " + oldRisk, "Bearer " + newRisk, "Bearer " + newRisk}) {
		t.Fatal("replacement generation did not exclusively use the replacement risk credential")
	}
	for _, route := range []string{"/api/settings", "/api/state", "/api/analysis"} {
		status, body := credentialHTTPJSON(t, h, http.MethodGet, route, nil)
		if status != http.StatusOK {
			t.Fatalf("read %s: HTTP %d", route, status)
		}
		assertCredentialHTTPPublic(t, body, oldRisk, newRisk, textKey, chatKey)
	}
	// The reverse direction also scopes invalidation: changing the text provider
	// reruns grouping but preserves already-successful risk classifications.
	const replacementText = "fixture-http-analysis-new-text"
	credentialHTTPSettings(t, h, h.app.Settings(), domain.ProviderKeyUpdate{ProviderID: "text", APIKey: replacementText})
	regrouped := h.awaitAnalysis(t, analysisTerminal)
	if regrouped.Analysis.Generation == complete.Analysis.Generation || regrouped.Analysis.Grouping.Status != "complete" || !reflect.DeepEqual(regrouped.Files, complete.Files) {
		t.Fatal("text credential update invalidated successful risk results")
	}
	control.mu.Lock()
	riskFirst, riskSecond, groups = control.riskStarted["src/calc.go"], control.riskStarted["src/calc_test.go"], control.groupStarted
	control.mu.Unlock()
	if riskFirst != 2 || riskSecond != 1 || groups != 2 {
		t.Fatal("text credential update reran risk or failed to rerun grouping")
	}
	calls := peer.calls()
	if calls[len(calls)-1].auth != "Bearer "+replacementText || calls[len(calls)-1].path != "/v1/chat/completions" {
		t.Fatal("grouping did not use the replacement text credential")
	}
	h.request(t, http.MethodGet, "/api/state", nil, &state)
	if len(state.Comments) != 1 || !reflect.DeepEqual(state.Comments[0], manual) {
		t.Fatal("regrouping overwrote the manual comment")
	}
}

func TestHTTPCredentialsUnicodeEscapedChatNeverReachesPublicState(t *testing.T) {
	h, _ := newCredentialHTTPHarness(t, nil)
	const secret = "fixture-http-unicode-secret"
	var escaped strings.Builder
	for _, r := range secret {
		fmt.Fprintf(&escaped, `\u%04x`, r)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("local peer received the wrong saved credential")
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var input struct {
			Messages []domain.Message `json:"messages"`
		}
		_ = json.Unmarshal(body, &input)
		if len(input.Messages) == 0 || strings.HasPrefix(input.Messages[0].Content, "Task: group") {
			modelFixture(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"Reflected `+escaped.String()+`"}}]}`)
	}))
	defer peer.Close()
	settings := h.app.Settings()
	updates := make([]domain.ProviderKeyUpdate, 0, len(settings.Providers))
	for i := range settings.Providers {
		settings.Providers[i].BaseURL = peer.URL + "/v1"
		updates = append(updates, domain.ProviderKeyUpdate{ProviderID: settings.Providers[i].ID, APIKey: secret})
	}
	credentialHTTPSettings(t, h, settings, updates...)
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	h.awaitAnalysis(t, analysisTerminal)
	status, body := credentialHTTPJSON(t, h, http.MethodPost, "/api/chat", map[string]string{"message": "Inspect the fixture"})
	if status != http.StatusOK || !bytes.Contains(body, []byte("[redacted]")) {
		t.Fatalf("redacted chat response unavailable: status=%d body=%s", status, body)
	}
	assertCredentialHTTPPublic(t, body, secret, escaped.String())
	for _, route := range []string{"/api/state", "/api/settings", "/api/analysis"} {
		status, body := credentialHTTPJSON(t, h, http.MethodGet, route, nil)
		if status != http.StatusOK {
			t.Fatalf("read %s: status=%d", route, status)
		}
		assertCredentialHTTPPublic(t, body, secret, escaped.String())
	}
}
