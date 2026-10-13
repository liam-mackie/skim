package acp

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxPermissionDetails = 16 << 10

type permissionToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	RawInput   json.RawMessage `json:"rawInput"`
	Content    json.RawMessage `json:"content"`
	Locations  json.RawMessage `json:"locations"`
}

func environmentRedactor() *strings.Replacer {
	values := make(map[string]bool)
	for _, variable := range os.Environ() {
		name, value, _ := strings.Cut(variable, "=")
		if value == "" {
			continue
		}
		name = strings.ToUpper(name)
		sensitive := false
		for _, fragment := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "AUTH", "COOKIE"} {
			if strings.Contains(name, fragment) {
				sensitive = true
				break
			}
		}
		// Long values are redacted regardless of the environment variable's
		// name; short values need a credential-like name to preserve readable
		// commands containing ordinary numbers and flags.
		if !sensitive && len(value) < 8 {
			continue
		}
		values[value] = true
		encoded, _ := json.Marshal(value)
		values[string(encoded[1:len(encoded)-1])] = true
	}
	ordered := make([]string, 0, len(values))
	for value := range values {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	pairs := make([]string, 0, 2*len(ordered))
	for _, value := range ordered {
		pairs = append(pairs, value, "[redacted]")
	}
	return strings.NewReplacer(pairs...)
}

func safeLabel(redactor *strings.Replacer, value string) string {
	value = redactor.Replace(value)
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
}

func permissionDetails(redactor *strings.Replacer, call permissionToolCall) string {
	var details strings.Builder
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"Input", call.RawInput}, {"Content", call.Content}, {"Locations", call.Locations},
	} {
		if len(field.raw) == 0 {
			continue
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(field.raw))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil || value == nil {
			continue
		}
		// Decode/re-encode first so alternate JSON escapes cannot bypass
		// credential matching. Redact before truncating, including credentials
		// crossing the display boundary.
		encoded, _ := json.Marshal(value)
		text := redactor.Replace(string(encoded))
		if details.Len() != 0 {
			details.WriteByte('\n')
		}
		details.WriteString(field.name)
		details.WriteString(": ")
		details.WriteString(text)
	}
	value := details.String()
	if len(value) <= maxPermissionDetails {
		return value
	}
	const suffix = "\n[truncated]"
	end := maxPermissionDetails - len(suffix)
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + suffix
}
