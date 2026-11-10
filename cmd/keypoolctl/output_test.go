package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTakeJSONFlagBeforeAndAfterCommand(t *testing.T) {
	args, enabled := takeJSONFlag([]string{"--json", "key", "list", "--provider", "groq"})
	if !enabled || strings.Join(args, " ") != "key list --provider groq" {
		t.Fatalf("args=%v enabled=%v", args, enabled)
	}
	args, enabled = takeJSONFlag([]string{"status", "--json"})
	if !enabled || strings.Join(args, " ") != "status" {
		t.Fatalf("args=%v enabled=%v", args, enabled)
	}
}

func TestFormatAPIResponseDefaultsToTableAndOmitsSecrets(t *testing.T) {
	got, err := formatAPIResponse([]byte(`{"data":[{"id":"key-1","provider":"groq","secret":"do-not-print","enabled":true}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "provider") || !strings.Contains(got, "groq") {
		t.Fatalf("missing table columns/value: %s", got)
	}
	if strings.Contains(got, "do-not-print") || strings.Contains(got, "secret") {
		t.Fatalf("secret leaked into table output: %s", got)
	}
}

func TestFormatAPIResponseJSONPreservesStructuredOutput(t *testing.T) {
	got, err := formatAPIResponse([]byte(`{"data":[{"id":"p1","name":"groq"}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"data"`) || !strings.Contains(got, `"name": "groq"`) {
		t.Fatalf("not pretty JSON: %s", got)
	}
}

func TestFormatAPIResponsePreservesOneTimeSecret(t *testing.T) {
	got, err := formatAPIResponse([]byte(`{"api_key":"kp_one_time_secret"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "kp_one_time_secret") {
		t.Fatalf("one-time secret was hidden from its only response: %s", got)
	}
}

func TestFilterResponseRowsPreservesEnvelope(t *testing.T) {
	got, err := filterResponseRows([]byte(`{"count":3,"data":[{"id":"a","provider":"groq"},{"id":"b","provider":"openrouter"},{"id":"c","provider":"groq"}]}`), "provider", "groq")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Count int              `json:"count"`
		Data  []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Count != 3 || len(parsed.Data) != 2 || parsed.Data[0]["id"] != "a" || parsed.Data[1]["id"] != "c" {
		t.Fatalf("filtered=%s", got)
	}
}
