package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"keypool/internal/limits"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
)

func TestFromEnvThreeFormsAndDedupe(t *testing.T) {
	env := map[string]string{
		"GROQ_API_KEYS":      " alpha, beta\ngamma ",
		"GROQ_API_KEY":       "beta",
		"GROQ_API_KEY_10":    "ten",
		"GROQ_API_KEY_2":     "two",
		"OPENROUTER_API_KEY": "must-not-mix",
	}
	keys := keysFromEnv("groq", env)
	want := []string{"alpha", "beta", "gamma", "two", "ten"}
	if len(keys) != len(want) {
		t.Fatalf("got %d keys, want %d", len(keys), len(want))
	}
	for i, key := range keys {
		if got := key.Secret.Reveal(); got != want[i] {
			t.Fatalf("key[%d]=%q want %q", i, got, want[i])
		}
		if strings.Contains(key.Label, want[i]) && len(want[i]) > 8 {
			t.Fatalf("label reveals key: %q", key.Label)
		}
	}
}

func FuzzFromEnv(f *testing.F) {
	f.Add("one, two\nthree")
	f.Add("gsk_abcdefghijklmnopqrstuvwxyz")
	f.Fuzz(func(t *testing.T, raw string) {
		keys := keysFromEnv("GROQ", map[string]string{"GROQ_API_KEYS": raw, "GROQ_API_KEY_2": raw})
		seen := map[string]bool{}
		for _, key := range keys {
			value := key.Secret.Reveal()
			if value == "" || seen[value] {
				t.Fatalf("empty or duplicate key")
			}
			seen[value] = true
			if strings.Contains(key.Label, value) && len(value) > 8 {
				t.Fatalf("secret leaked in label")
			}
		}
	})
}

func TestFromEnvPrefixDoesNotMix(t *testing.T) {
	env := map[string]string{
		"GROQ_API_KEY":       "gsk_groq_key",
		"OPENROUTER_API_KEY": "sk-or-openrouter_key",
	}
	groq := keysFromEnv("GROQ", env)
	openrouter := keysFromEnv("OPENROUTER", env)
	if len(groq) != 1 || groq[0].Secret.Reveal() != "gsk_groq_key" {
		t.Fatalf("unexpected Groq keys: %d", len(groq))
	}
	if len(openrouter) != 1 || openrouter[0].Secret.Reveal() != "sk-or-openrouter_key" {
		t.Fatalf("unexpected OpenRouter keys: %d", len(openrouter))
	}
}

func TestRawModelLimitsAcceptsLegacyScalarTPM(t *testing.T) {
	var raw RawModelLimits
	if err := json.Unmarshal([]byte(`{"legacy-model":1800}`), &raw); err != nil {
		t.Fatal(err)
	}
	resolved := limits.Limits{PerModel: map[string]limits.ModelLimits(raw)}.Resolve("legacy-model")
	if resolved.TPM == nil || *resolved.TPM != 1800 {
		t.Fatalf("legacy scalar TPM not resolved: %+v", resolved)
	}
}

func TestRouteDefaultsAreJSONValues(t *testing.T) {
	raw := &Raw{
		APIKey:    "kp_test_key",
		Providers: map[string]ProviderRaw{"test": {Type: "openai_compatible", BaseURL: "https://example.test/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Keys: []sched.KeySpec{{ID: "k1", Secret: secret.New("provider-secret")}}}},
		Projects:  map[string]ProjectRaw{"p": {Project: "p", Keys: map[string]KeyRule{"test": {Include: Selector{KeyIDs: []string{"k1"}}}}, Routes: map[string]RouteRaw{"r": {Priority: 1, Chain: []string{"test:model"}, Defaults: map[string]any{"reasoning_effort": "medium", "temperature": 0.7}}}}},
	}
	snapshot, err := Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	project, ok := snapshot.Project("p")
	if !ok {
		t.Fatal("project missing")
	}
	defaults := project.Routes["r"].Defaults
	if !json.Valid(defaults["reasoning_effort"]) || string(defaults["reasoning_effort"]) != `"medium"` {
		t.Fatalf("reasoning_effort default is not valid JSON: %q", defaults["reasoning_effort"])
	}
	if !json.Valid(defaults["temperature"]) {
		t.Fatalf("temperature default is not valid JSON: %q", defaults["temperature"])
	}
}

func TestLoadFileJSONAndRelativeProjectFile(t *testing.T) {
	dir := t.TempDir()
	projectPath := filepath.Join(dir, "project.json")
	if err := os.WriteFile(projectPath, []byte(`{"project":"mathlab","description":"teste"}`), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "keypool.json")
	data := `{"listen":"127.0.0.1:8787","project_file":"project.json","providers":{"groq":{"type":"openai_compatible","base_url":"https://example.test/v1","env_prefix":"GROQ"}}}`
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadFile(configPath, map[string]string{"GROQ_API_KEY": "provider-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if raw.ProjectFile != "project.json" || raw.Projects["mathlab"].Description != "teste" {
		t.Fatalf("project_file não carregado: %+v", raw)
	}
	if len(raw.Providers["groq"].Keys) != 1 {
		t.Fatalf("chave de ambiente não importada: %+v", raw.Providers["groq"].Keys)
	}
	serialized, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "project_file") || strings.Contains(string(serialized), "provider-secret") {
		t.Fatalf("campos locais/segredos entraram na configuração persistível: %s", serialized)
	}
}

func TestLoadFileRejectsUnknownFieldsAndExtraDocuments(t *testing.T) {
	for _, content := range []string{`{"listen":"127.0.0.1:8787","unknown":true}`, `{"listen":"127.0.0.1:8787"} {}`} {
		path := filepath.Join(t.TempDir(), "invalid.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(path, map[string]string{}); err == nil {
			t.Fatalf("documento inválido aceito: %s", content)
		}
	}
}

func TestPolicySpillDefaultsOnAndExplicitFalseSurvivesJSON(t *testing.T) {
	if !(Policy{}).SpillEnabled() {
		t.Fatal("spill must default to true")
	}
	var explicit Policy
	if err := json.Unmarshal([]byte(`{"spill_on_saturation":false}`), &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.SpillEnabled() {
		t.Fatal("explicit false must disable spill")
	}
	data, err := json.Marshal(explicit)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Policy
	if err = json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.SpillEnabled() {
		t.Fatalf("explicit false was lost after JSON round-trip: %s", data)
	}
}

func TestWarmProviderOverridesCanDisableLayersWithoutChangingDefaults(t *testing.T) {
	disabled := false
	cfg, err := (WarmRaw{Providers: map[string]WarmProviderRaw{
		"openrouter": {L0Enabled: &disabled, L2Enabled: &disabled},
	}}).Build()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ForProvider("openrouter"); !got.Enabled || got.L0Enabled || !got.L1Enabled || got.L2Enabled {
		t.Fatalf("openrouter override=%+v", got)
	}
	if got := cfg.ForProvider("groq"); !got.Enabled || !got.L0Enabled || !got.L1Enabled || got.L2Enabled {
		t.Fatalf("unlisted provider should keep safe global defaults: %+v", got)
	}
}
