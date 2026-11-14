package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"keypool/internal/clock"
	"keypool/internal/provider"
	"keypool/internal/secret"
)

func TestAdaptPayload(t *testing.T) {
	s := &provider.Spec{Capabilities: provider.CapabilitySpec{MaxTokensField: "max_completion_tokens", ReasoningStyle: "top_level", ReasoningEffortModels: []string{"openai/gpt-oss-*"}}}
	p := map[string]json.RawMessage{"model": json.RawMessage(`"openai/gpt-oss-120b"`), "max_tokens": json.RawMessage("100"), "temperature": json.RawMessage("null"), "reasoning_effort": json.RawMessage(`"high"`)}
	out := AdaptPayload(s, "openai/gpt-oss-120b", p)
	if _, ok := out["max_tokens"]; ok {
		t.Fatal("max_tokens not renamed")
	}
	if string(out["max_completion_tokens"]) != "100" {
		t.Fatal("missing max_completion_tokens")
	}
	if _, ok := out["temperature"]; ok {
		t.Fatal("null not removed")
	}
	if _, ok := out["reasoning_effort"]; !ok {
		t.Fatal("reasoning dropped for supported model")
	}
}
func TestReasoningDroppedOnUnsupportedModel(t *testing.T) {
	s := &provider.Spec{Capabilities: provider.CapabilitySpec{ReasoningStyle: "nested", ReasoningEffortModels: []string{"openai/gpt-oss-*"}}}
	p := map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"high"`)}
	if _, ok := AdaptPayload(s, "qwen/qwen3", p)["reasoning_effort"]; ok {
		t.Fatal("reasoning effort should be discarded")
	}
}
func TestNestedReasoningPassesForFrontierOpenRouterModels(t *testing.T) {
	s := &provider.Spec{Capabilities: provider.CapabilitySpec{
		MaxTokensField: "max_tokens", ReasoningStyle: "nested",
		ReasoningEffortModels: []string{
			"google/gemini-4-*", "anthropic/claude-fable-*", "openai/gpt-6-*",
		},
	}}
	for _, model := range []string{
		"google/gemini-4-carbon", "anthropic/claude-fable-5.2", "openai/gpt-6-astra",
	} {
		p := map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"high"`)}
		out := AdaptPayload(s, model, p)
		if _, ok := out["reasoning_effort"]; ok {
			t.Fatalf("%s: top-level reasoning_effort should be nested", model)
		}
		var reasoning map[string]string
		if err := json.Unmarshal(out["reasoning"], &reasoning); err != nil {
			t.Fatalf("%s: invalid reasoning payload: %v", model, err)
		}
		if reasoning["effort"] != "high" {
			t.Fatalf("%s: reasoning effort = %q, want high", model, reasoning["effort"])
		}
	}
}

func TestBuildRequestAuthAndRedactionBoundary(t *testing.T) {
	s := &provider.Spec{Name: "test", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1234/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Paths: provider.PathsSpec{Chat: "/chat/completions"}}
	req, e := (Adapter{}).BuildRequest(context.Background(), s, secret.New("gsk_abcdefghijklmnopqrstuvwxyz"), &provider.Canonical{Payload: map[string]json.RawMessage{"model": json.RawMessage(`"m"`)}})
	if e != nil {
		t.Fatal(e)
	}
	if req.Header.Get("Authorization") != "Bearer gsk_abcdefghijklmnopqrstuvwxyz" {
		t.Fatal("missing auth")
	}
	if strings.Contains(req.URL.String(), "gsk_") {
		t.Fatal("secret in URL")
	}
}

func TestHintsRequireRemainingBeforeBlocking(t *testing.T) {
	now := time.Unix(1000, 0)
	h := http.Header{"X-Ratelimit-Reset-Tokens": []string{"1m"}}
	hints := (Adapter{}).Hints(h, now)
	if (*hints)["tpm"].HasRemaining {
		t.Fatal("reset-only header must not invent remaining=0")
	}
}

func TestClassifyUsesInjectedClock(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ad := Adapter{Clock: clock.NewFake(base)}
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"X-Ratelimit-Reset": []string{"1700000030"}},
	}
	got := ad.Classify(resp, nil, nil)
	if !got.RetryKnown || got.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, known=%v; want 30s, true", got.RetryAfter, got.RetryKnown)
	}
}

func TestProviderWideResetAndRetryHeadersApplyToAllDimensions(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	spec := &provider.Spec{RateHeaders: map[string]provider.RateHeaderSpec{
		"reset":       {Header: "X-RateLimit-Reset", Format: "epoch"},
		"retry_after": {Header: "Retry-After", Format: "seconds"},
	}}
	resetAt := now.Add(2 * time.Minute)
	h := http.Header{}
	h.Set("X-RateLimit-Reset", "1700000120")
	h.Set("Retry-After", "30")
	got := (Adapter{}).HintsFor(spec, h, now)
	for _, dim := range []string{"tpm", "rpm", "rpd", "tpd"} {
		if (*got)[dim].ResetAt.Before(now.Add(2 * time.Minute)) {
			t.Fatalf("%s reset=%v, want >= %v", dim, (*got)[dim].ResetAt, resetAt)
		}
		if (*got)[dim].ObservedAt.IsZero() {
			t.Fatalf("%s hint missing observation timestamp", dim)
		}
	}
}

func TestOpenRouterFreeReasoningModelsAreAdapted(t *testing.T) {
	preset, err := os.ReadFile("../presets/openrouter.json")
	if err != nil {
		t.Fatalf("read OpenRouter preset: %v", err)
	}
	var spec provider.Spec
	if err := json.Unmarshal(preset, &spec); err != nil {
		t.Fatalf("decode OpenRouter preset: %v", err)
	}
	if spec.Capabilities.ReasoningStyle != "nested" {
		t.Fatalf("expected nested reasoning adapter, got %q", spec.Capabilities.ReasoningStyle)
	}
	for _, model := range []string{
		"nvidia/nemotron-3-ultra-550b-a55b:free",
		"nvidia/nemotron-3.5-super-vl:free",
		"baidu/ernie-5.5-flash-preview:free",
	} {
		out := AdaptPayload(&spec, model, map[string]json.RawMessage{
			"reasoning_effort": json.RawMessage(`"high"`),
		})
		if _, ok := out["reasoning_effort"]; ok {
			t.Fatalf("%s: reasoning_effort should be nested", model)
		}
		var reasoning map[string]string
		if err := json.Unmarshal(out["reasoning"], &reasoning); err != nil {
			t.Fatalf("%s: invalid nested reasoning: %v", model, err)
		}
		if reasoning["effort"] != "high" {
			t.Fatalf("%s: expected reasoning.effort=high, got %#v", model, reasoning)
		}
	}
}
