package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"skim/internal/domain"
	"skim/internal/store"
)

func TestCredentialHTTPAuthenticationAndReflectedRedaction(t *testing.T) {
	for _, source := range []string{"saved", "environment", "removed"} {
		for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
			name := source + "/success"
			if status != http.StatusOK {
				name = source + "/error"
			}
			t.Run(name, func(t *testing.T) {
				// Quotes, backslashes and HTML-sensitive characters exercise JSON escaping.
				const savedKey = `saved-"quoted"-\-<fixture>`
				const envKey = `env-"quoted"-\-<fixture>`
				t.Setenv("SKIM_MODEL_CREDENTIAL_TEST", "  "+envKey+"  ")
				wantKey := envKey
				if source == "saved" {
					wantKey = savedKey
				}
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
						t.Errorf("unexpected model request: %s %s", r.Method, r.URL.Path)
					}
					if r.Header.Get("Authorization") != "Bearer "+wantKey {
						t.Error("model used the wrong credential source")
					}
					var input struct {
						Model          string           `json:"model"`
						Messages       []domain.Message `json:"messages"`
						ResponseFormat json.RawMessage  `json:"response_format"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.Model != "fixture-model" || len(input.Messages) != 1 || input.Messages[0].Content != "hello" {
						t.Error("credential setup changed the model request")
					}
					if len(input.ResponseFormat) != 0 {
						t.Errorf("text request must omit response_format: %s", input.ResponseFormat)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					var response any
					if status == http.StatusOK {
						response = map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": "reflected " + wantKey + " safely"}}}}
					} else {
						response = map[string]string{"error": "reflected " + wantKey + " safely"}
					}
					if err := json.NewEncoder(w).Encode(response); err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()
				settings := domain.Settings{
					Providers: []domain.ModelProvider{{ID: "text", Name: "Text fixture", Kind: "openai", BaseURL: server.URL + "/v1", APIKeyEnv: "SKIM_MODEL_CREDENTIAL_TEST"}},
					Roles:     domain.Roles{Chat: domain.ModelRole{Provider: "text", Model: "fixture-model"}},
				}
				s, err := store.Open(t.TempDir(), settings)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if source != "environment" {
					committed, err := s.UpdateSettings(domain.SettingsUpdate{Settings: settings, ProviderKeys: []domain.ProviderKeyUpdate{{ProviderID: "text", APIKey: savedKey}}}, func(*store.Record, domain.Settings) {})
					if err != nil || !committed {
						t.Fatalf("save credential: %v", err)
					}
				}
				if source == "removed" {
					committed, err := s.UpdateSettings(domain.SettingsUpdate{Settings: s.Settings(), ProviderKeys: []domain.ProviderKeyUpdate{{ProviderID: "text", Remove: true}}}, func(*store.Record, domain.Settings) {})
					if err != nil || !committed {
						t.Fatalf("remove credential: %v", err)
					}
				}
				client := New()
				client.HTTP = server.Client()
				client.ResolveCredential = s.ResolveCredential
				settings = s.Settings()
				text, err := client.Chat(context.Background(), settings, settings.Roles.Chat, []domain.Message{{Role: "user", Content: "hello"}}, nil)
				if calls.Load() != 1 {
					t.Fatalf("expected one local model call, got %d", calls.Load())
				}
				if status == http.StatusOK {
					if err != nil || text != "reflected [redacted] safely" {
						t.Fatalf("successful reflection was not redacted: text=%q err=%v", text, err)
					}
				} else if err == nil || text != "" || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "[redacted]") || strings.Contains(err.Error(), "quoted") {
					t.Fatalf("unsafe or hidden provider error: text=%q err=%v", text, err)
				}
			})
		}
	}
}

func TestCredentialRevisionRequiresResolverAndRejectsStaleRequests(t *testing.T) {
	t.Setenv("SKIM_MODEL_CREDENTIAL_TEST", "environment-fallback-must-not-send")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	settings := domain.Settings{
		Providers: []domain.ModelProvider{{ID: "text", Name: "Text fixture", Kind: "openai", BaseURL: server.URL, APIKeyEnv: "SKIM_MODEL_CREDENTIAL_TEST"}},
		Roles:     domain.Roles{Chat: domain.ModelRole{Provider: "text", Model: "fixture-model"}},
	}
	s, err := store.Open(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if committed, err := s.UpdateSettings(domain.SettingsUpdate{Settings: settings, ProviderKeys: []domain.ProviderKeyUpdate{{ProviderID: "text", APIKey: "saved-fixture"}}}, func(*store.Record, domain.Settings) {}); err != nil || !committed {
		t.Fatalf("save credential: %v", err)
	}
	stale := s.Settings()
	client := New()
	client.HTTP = server.Client()
	if _, err := client.Chat(context.Background(), stale, stale.Roles.Chat, nil, nil); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("saved revision without resolver was not refused: %v", err)
	}
	if committed, err := s.UpdateSettings(domain.SettingsUpdate{Settings: s.Settings(), ProviderKeys: []domain.ProviderKeyUpdate{{ProviderID: "text", APIKey: "replacement-fixture"}}}, func(*store.Record, domain.Settings) {}); err != nil || !committed {
		t.Fatalf("replace credential: %v", err)
	}
	client.ResolveCredential = s.ResolveCredential
	if _, err := client.Chat(context.Background(), stale, stale.Roles.Chat, nil, nil); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale credential snapshot was not refused: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("unsafe requests reached the model: %d", calls.Load())
	}
}

func TestCredentialUnicodeEscapesAreRedactedAfterEveryJSONDecode(t *testing.T) {
	const key = "fixture-unicode-secret"
	var escaped strings.Builder
	for _, r := range key {
		fmt.Fprintf(&escaped, `\u%04x`, r)
	}
	nested := `{"comments":[{"body":"` + escaped.String() + `","integer":9007199254740993,"decimal":1.234567890123456789,"large":1e400}]}`
	for _, mode := range []string{"chat", "structured-review", "nested-string", "finish-reason", "http-error"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SKIM_MODEL_UNICODE_TEST", key)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+key {
					t.Error("wrong credential on local peer")
				}
				var input struct {
					ResponseFormat json.RawMessage `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if mode == "chat" {
					if len(input.ResponseFormat) != 0 {
						t.Errorf("text request must omit response_format: %s", input.ResponseFormat)
					}
				} else if !requireModelResponseSchema(t, input.ResponseFormat, "review_comments", expectedReviewSchema) {
					http.Error(w, "strict review_comments schema required", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch mode {
				case "chat":
					fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"reflected `+escaped.String()+`"}}]}`)
				case "finish-reason":
					fmt.Fprint(w, `{"choices":[{"finish_reason":"`+escaped.String()+`","message":{"content":"unused"}}]}`)
				case "http-error":
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, `{"error":"reflected `+escaped.String()+`"}`)
				default:
					content := nested
					if mode == "nested-string" {
						encoded, _ := json.Marshal(map[string]string{"nested": nested})
						content = string(encoded)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
				}
			}))
			defer peer.Close()
			settings := domain.Settings{
				Providers: []domain.ModelProvider{{ID: "text", Name: "Text fixture", BaseURL: peer.URL, APIKeyEnv: "SKIM_MODEL_UNICODE_TEST"}},
				Roles:     domain.Roles{Chat: domain.ModelRole{Provider: "text", Model: "fixture"}},
			}
			client := New()
			var schema *JSONSchema
			if mode != "chat" {
				schema = &ReviewCommentsSchema
			}
			text, err := client.Chat(context.Background(), settings, settings.Roles.Chat, nil, schema)
			if mode == "finish-reason" || mode == "http-error" {
				if err == nil || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), escaped.String()) || !strings.Contains(err.Error(), "[redacted]") {
					t.Fatalf("encoded credential survived error boundary: %v", err)
				}
				return
			}
			if err != nil || strings.Contains(text, key) || strings.Contains(text, escaped.String()) || !strings.Contains(text, "[redacted]") {
				t.Fatalf("encoded credential survived content boundary: text=%q err=%v", text, err)
			}
			if mode == "chat" {
				if text != "reflected [redacted]" {
					t.Fatalf("unexpected redacted chat: %q", text)
				}
				return
			}
			if mode == "nested-string" {
				var outer map[string]string
				if err := json.Unmarshal([]byte(text), &outer); err != nil {
					t.Fatal(err)
				}
				text = outer["nested"]
			}
			var result struct {
				Comments []struct {
					Body    string
					Integer json.Number
					Decimal json.Number
					Large   json.Number
				}
			}
			decoder := json.NewDecoder(strings.NewReader(text))
			decoder.UseNumber()
			if err := decoder.Decode(&result); err != nil {
				t.Fatal(err)
			}
			if len(result.Comments) != 1 || result.Comments[0].Body != "[redacted]" || result.Comments[0].Integer != "9007199254740993" || result.Comments[0].Decimal != "1.234567890123456789" || result.Comments[0].Large != "1e400" {
				t.Fatalf("structured content redaction changed non-secret numbers: %+v", result)
			}
		})
	}
}

func TestCredentialRedactionDoesNotRevisitRenamedJSONKeys(t *testing.T) {
	const original = `{"a":{"a":"value","count":9007199254740993}}`
	text := safeModelText(original, "a")
	var result map[string]map[string]any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || len(result["[redacted]"]) != 2 || result["[redacted]"]["count"] != json.Number("9007199254740993") || result["[redacted]"]["[redacted]"] != "v[redacted]lue" {
		t.Fatalf("redaction revisited inserted keys or changed unrelated numbers: %s", text)
	}
}
