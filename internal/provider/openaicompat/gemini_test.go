package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"keypool/internal/provider"
	"keypool/internal/secret"
)

func TestGeminiPresetAndModelsContract(t *testing.T) {
	spec, err := provider.LoadPreset("gemini.json")
	if err != nil {
		t.Fatalf("load Gemini preset: %v", err)
	}
	if spec.Type != "openai_compatible" || spec.BaseURL != "https://generativelanguage.googleapis.com/v1beta/openai" {
		t.Fatalf("unexpected Gemini preset: %#v", spec)
	}
	if spec.Auth.Scheme != "bearer" || spec.Paths.Chat != "/chat/completions" || spec.Paths.Models != "/models" || spec.Probe.Kind != "models" {
		t.Fatalf("unexpected Gemini protocol configuration: %#v", spec)
	}
	if spec.LimitScope != "provider" {
		t.Fatalf("same-project keys must share local provider limits, got %q", spec.LimitScope)
	}

	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if r.Method != http.MethodGet {
			t.Errorf("models request method = %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gemini-model-a"},{"id":"gemini-model-a"},{"id":"gemini-model-b"}]}`))
	}))
	defer server.Close()
	spec.BaseURL = server.URL
	models, err := (Adapter{}).ListModels(context.Background(), server.Client(), spec, secret.New("AIza-test-secret"))
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if gotAuth != "Bearer AIza-test-secret" || gotPath != "/models" {
		t.Fatalf("unexpected request auth/path: %q %q", gotAuth, gotPath)
	}
	if strings.Join(models, ",") != "gemini-model-a,gemini-model-b" {
		t.Fatalf("unexpected deduplicated models: %#v", models)
	}
}

func TestGeminiChatUsesBearerAndNoQuerySecret(t *testing.T) {
	spec, err := provider.LoadPreset("gemini.json")
	if err != nil {
		t.Fatal(err)
	}
	spec.BaseURL = "https://example.invalid/v1beta/openai/"
	payload := map[string]json.RawMessage{
		"model":            json.RawMessage(`"gemini-model"`),
		"messages":         json.RawMessage(`[ {"role":"user","content":"hello"} ]`),
		"reasoning_effort": json.RawMessage(`"high"`),
	}
	req, err := (Adapter{}).BuildRequest(context.Background(), spec, secret.New("AIza-test-secret"), &provider.Canonical{Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.URL.Path != "/v1beta/openai/chat/completions" {
		t.Fatalf("unexpected Gemini chat endpoint: %s %s", req.Method, req.URL.String())
	}
	if req.Header.Get("Authorization") != "Bearer AIza-test-secret" {
		t.Fatal("Gemini Bearer authorization missing")
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatal("Gemini request must use application/json")
	}
	if req.URL.RawQuery != "" || strings.Contains(req.URL.String(), "AIza-test-secret") {
		t.Fatal("Gemini key leaked into URL")
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatal("unverified reasoning_effort must not be forwarded")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "AIza-test-secret") {
		t.Fatal("Gemini key leaked into request body")
	}
	payload["model"] = json.RawMessage(`"gemini-3.8-flash"`)
	req, err = (Adapter{}).BuildRequest(context.Background(), spec, secret.New("AIza-test-secret"), &provider.Canonical{Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if string(body["reasoning_effort"]) != `"high"` {
		t.Fatal("reasoning_effort should pass through for verified Gemini 3 models")
	}
	payload["model"] = json.RawMessage(`"gemini-4-carbon"`)
	req, err = (Adapter{}).BuildRequest(context.Background(), spec, secret.New("AIza-test-secret"), &provider.Canonical{Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if string(body["reasoning_effort"]) != `"high"` {
		t.Fatal("reasoning_effort should pass through for verified Gemini 4 models")
	}
}
