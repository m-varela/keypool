package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"keypool/internal/limits"
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
	node := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "legacy-model"},
		{Kind: yaml.ScalarNode, Value: "1800"},
	}}
	var raw RawModelLimits
	if err := raw.UnmarshalYAML(node); err != nil {
		t.Fatal(err)
	}
	resolved := limits.Limits{PerModel: map[string]limits.ModelLimits(raw)}.Resolve("legacy-model")
	if resolved.TPM == nil || *resolved.TPM != 1800 {
		t.Fatalf("legacy scalar TPM not resolved: %+v", resolved)
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
	no := false
	cfg, err := (WarmRaw{Providers: map[string]WarmProviderRaw{
		"openrouter": {L0Enabled: &no, L2Enabled: &no},
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
