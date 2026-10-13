package acp

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPermissionDetailsRetainActualInputsAndRedactSecrets(t *testing.T) {
	secret := "credential-\"quoted\"-α-123456789"
	t.Setenv("SKIM_ACP_DETAIL_SECRET", secret)
	t.Setenv("SKIM_ACP_SHORT_TOKEN", "xy")
	input, err := json.Marshal(map[string]any{
		"command":  "git show HEAD:tracked.txt",
		"token":    secret,
		"short":    "xy",
		"sequence": json.Number("9007199254740993"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// An agent may use a different, still-valid JSON escape representation.
	input = []byte(strings.ReplaceAll(string(input), "credential", `\u0063redential`))
	call := permissionToolCall{
		RawInput:  input,
		Content:   json.RawMessage(`[{"type":"content","content":{"type":"text","text":"Inspect caller"}}]`),
		Locations: json.RawMessage(`[{"path":"tracked.txt","line":12}]`),
	}
	details := permissionDetails(environmentRedactor(), call)
	for _, required := range []string{"git show HEAD:tracked.txt", "Inspect caller", "tracked.txt", "9007199254740993", "[redacted]", "Input:", "Content:", "Locations:"} {
		if !strings.Contains(details, required) {
			t.Fatalf("missing %q in %q", required, details)
		}
	}
	if strings.Contains(details, "credential") || strings.Contains(details, `"xy"`) {
		t.Fatalf("credential escaped redaction: %q", details)
	}
}

func TestPermissionDetailsBoundAndOmitMissingData(t *testing.T) {
	redactor := strings.NewReplacer("a-secret-crossing-the-display-boundary", "[redacted]")
	if details := permissionDetails(redactor, permissionToolCall{RawInput: json.RawMessage("null")}); details != "" {
		t.Fatalf("invented details: %q", details)
	}
	input, _ := json.Marshal(map[string]string{"command": strings.Repeat("界", maxPermissionDetails) + "a-secret-crossing-the-display-boundary"})
	details := permissionDetails(redactor, permissionToolCall{RawInput: input})
	if len(details) > maxPermissionDetails || !utf8.ValidString(details) || !strings.HasSuffix(details, "[truncated]") {
		t.Fatal("permission details are not safely bounded")
	}
	if label := safeLabel(redactor, "a-secret-crossing-the-display-boundary\x1b\nrun"); label != "[redacted]  run" {
		t.Fatalf("unsafe label: %q", label)
	}
}

func TestDecodeEnvelopeRejectsMalformedProtocol(t *testing.T) {
	for _, input := range []string{
		`not-json`, `[]`, `null`, `{}`, `{"jsonrpc":"1.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":null,"result":{}}`,
		`{"jsonrpc":"2.0","id":true,"result":{}}`,
		`{"jsonrpc":"2.0","id":1}`, `{"jsonrpc":"2.0","result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"bad"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/update","result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{}} {}`,
	} {
		if _, err := decodeEnvelope([]byte(input)); err == nil {
			t.Errorf("accepted invalid envelope: %s", input)
		}
	}
	for _, input := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":"agent-request","method":"session/request_permission","params":{}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Authentication required"}}`,
	} {
		if _, err := decodeEnvelope([]byte(input)); err != nil {
			t.Errorf("rejected valid envelope: %s: %v", input, err)
		}
	}
}
