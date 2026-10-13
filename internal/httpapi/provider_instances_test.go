package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"skim/internal/domain"
	"skim/internal/model"
	"skim/internal/store"
)

// This local OpenAI-compatible peer accepts only concrete strict task schemas.
// It deliberately rejects the legacy json_object request with HTTP 400.
type fixtureSchema struct {
	Type                 string                   `json:"type"`
	Properties           map[string]fixtureSchema `json:"properties"`
	Items                *fixtureSchema           `json:"items"`
	Required             []string                 `json:"required"`
	AdditionalProperties *bool                    `json:"additionalProperties"`
	Enum                 []string                 `json:"enum"`
}

func fixtureObject(node fixtureSchema, fields map[string]string) bool {
	if node.Type != "object" || node.AdditionalProperties == nil || *node.AdditionalProperties || len(node.Properties) != len(fields) || len(node.Required) != len(fields) {
		return false
	}
	seen := map[string]bool{}
	for _, name := range node.Required {
		if _, ok := fields[name]; !ok || seen[name] {
			return false
		}
		seen[name] = true
	}
	for name, kind := range fields {
		if node.Properties[name].Type != kind {
			return false
		}
	}
	return true
}

func requireFixtureResponseFormat(raw json.RawMessage, name string) error {
	if name == "" {
		if len(raw) != 0 {
			return errors.New("text chat/style must omit response_format")
		}
		return nil
	}
	var format struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string        `json:"name"`
			Strict bool          `json:"strict"`
			Schema fixtureSchema `json:"schema"`
		} `json:"json_schema"`
	}
	if json.Unmarshal(raw, &format) != nil || format.Type != "json_schema" || format.JSONSchema.Name != name || !format.JSONSchema.Strict {
		return errors.New("response_format.type: Input should be json_schema with a concrete strict task schema")
	}
	root := "comments"
	rootFields := map[string]string{"comments": "array"}
	fields := map[string]string{"path": "string", "startLine": "integer", "endLine": "integer", "side": "string", "body": "string"}
	switch name {
	case "review_groups":
		root = "groups"
		rootFields = map[string]string{"groups": "array"}
		fields = map[string]string{"title": "string", "summary": "string", "paths": "array"}
	case "risk_factors":
		root = "factors"
		rootFields = map[string]string{"factors": "array"}
		fields = map[string]string{"text": "string", "side": "string", "startLine": "integer", "endLine": "integer"}
	}
	schema := format.JSONSchema.Schema
	if !fixtureObject(schema, rootFields) {
		return errors.New("structured output requires the exact closed root object")
	}
	item := schema.Properties[root].Items
	if item == nil || !fixtureObject(*item, fields) {
		return errors.New("structured output requires the exact closed item object")
	}
	if root == "groups" {
		paths := item.Properties["paths"]
		if paths.Items == nil || paths.Items.Type != "string" {
			return errors.New("group paths must be strings")
		}
	} else if !reflect.DeepEqual(item.Properties["side"].Enum, []string{"LEFT", "RIGHT"}) {
		return errors.New("comment side must be LEFT or RIGHT")
	}
	return nil
}

func TestHTTPNamedProvidersStructuredConsumerWorkflow(t *testing.T) {
	h, peer := newCredentialHTTPHarness(t, nil)
	settings := h.app.Settings()
	// Both text instances use the same protocol and endpoint, but independent keys.
	settings.Providers[0].Name = "  Gateway review  "
	settings.Providers[2].Name = "Personal chat"
	settings = credentialHTTPSettings(t, h, settings,
		domain.ProviderKeyUpdate{ProviderID: "text", APIKey: "fixture-review-key"},
		domain.ProviderKeyUpdate{ProviderID: "risk", APIKey: "fixture-risk-key"},
		domain.ProviderKeyUpdate{ProviderID: "chat", APIKey: "fixture-chat-key"})
	if settings.Providers[0].Name != "Gateway review" {
		t.Fatal("provider display name was not trimmed")
	}
	var state domain.State
	h.request(t, http.MethodPost, "/api/pull-request", map[string]string{"url": "https://github.com/acme/widgets/pull/7"}, &state)
	complete := h.awaitAnalysis(t, analysisTerminal)
	if complete.Analysis.Grouping.Status != "complete" || complete.Analysis.Risk.Status != "complete" || len(complete.Groups) != 1 || !reflect.DeepEqual(complete.Groups[0].Paths, []string{"src/calc_test.go", "src/calc.go"}) {
		t.Fatalf("strict-schema peer did not produce persisted reading order: %+v", complete)
	}
	before := h.app.State()
	calls := len(peer.calls())
	roles := settings.Roles
	revision := h.app.Settings().Providers[0].CredentialRevision
	settings.Providers[0].Name = "Renamed workspace"
	settings.Providers[1].Name = "Renamed risk"
	settings = credentialHTTPSettings(t, h, settings)
	after := h.app.State()
	if settings.Roles != roles || !reflect.DeepEqual(before.Analysis, after.Analysis) || !reflect.DeepEqual(before.Groups, after.Groups) || !reflect.DeepEqual(before.Files, after.Files) || len(peer.calls()) != calls || h.app.Settings().Providers[0].CredentialRevision != revision {
		t.Fatal("cosmetic rename changed roles, credential identity, generation or cached analysis")
	}
	var chat domain.ChatThread
	h.request(t, http.MethodPost, "/api/chat", map[string]string{"message": "Explain these changes"}, &chat)
	if len(chat.Messages) != 2 {
		t.Fatal("text chat failed without response_format")
	}
	var style struct {
		Style       string `json:"style"`
		SampleCount int    `json:"sampleCount"`
	}
	h.request(t, http.MethodPost, "/api/style/import", map[string]any{}, &style)
	if style.Style == "" || style.SampleCount != 2 {
		t.Fatal("text style import failed without response_format")
	}
	for _, call := range peer.calls() {
		if call.path != "/v1/chat/completions" {
			continue
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(call.body), &request); err != nil {
			t.Fatal(err)
		}
		want := "Bearer fixture-review-key"
		if request.Model == "fixture-chat" {
			want = "Bearer fixture-chat-key"
		}
		if call.auth != want {
			t.Fatal("same-kind named providers misrouted credentials")
		}
	}
	if err := h.app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(h.dataDir, model.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Settings().Providers[0].Name != "Renamed workspace" || reopened.Settings().Roles != roles || !reflect.DeepEqual(reopened.State().Groups, complete.Groups) {
		t.Fatal("restart lost named settings or successful grouping cache")
	}
	key, err := reopened.ResolveCredential(reopened.Settings().Providers[0])
	if err != nil || key != "fixture-review-key" {
		t.Fatal("rename/restart lost saved key")
	}
}

func TestHTTPEmptyProviderSettingsStayEmpty(t *testing.T) {
	h, _ := newCredentialHTTPHarness(t, nil)
	settings := credentialHTTPSettings(t, h, model.Defaults())
	if settings.Providers == nil || len(settings.Providers) != 0 || settings.Roles != (domain.Roles{}) {
		t.Fatal("empty settings API created provider rows or role defaults")
	}
	if err := h.app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(h.dataDir, httpFixtureSettings("http://127.0.0.1:1/v1"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.Settings().Providers) != 0 || reopened.Settings().Roles != (domain.Roles{}) {
		t.Fatal("restart repopulated intentionally empty providers or roles")
	}
}
