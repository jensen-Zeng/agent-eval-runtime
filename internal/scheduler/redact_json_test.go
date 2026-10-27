package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCredentialRedactionPreservesJSON(t *testing.T) {
	original := []byte(`{"reason":"OPENAI_API_KEY=example-token","evaluation":{"log":"Bearer sk-aaaaaaaaaaaaaaaaaaaaaaaa"},"usage":{"total_tokens":30}}`)
	result := redactResultJSON(original)
	if !json.Valid(result) || strings.Contains(string(result), "example-token") || strings.Contains(string(result), "sk-aaaaaaaa") {
		t.Fatal(string(result))
	}
	var out map[string]any
	_ = json.Unmarshal(result, &out)
	if out["usage"].(map[string]any)["total_tokens"] != float64(30) {
		t.Fatal(out)
	}
}
