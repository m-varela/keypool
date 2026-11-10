package pgstore

import (
	"encoding/json"
	"strings"
	"testing"

	"keypool/internal/config"
	"keypool/internal/sched"
	"keypool/internal/secret"
)

func TestStableKeyUUID(t *testing.T) {
	first := stableKeyUUID("groq", "groq-1")
	if first != stableKeyUUID("groq", "groq-1") {
		t.Fatal("stable UUID changed across calls")
	}
	if !uuidLike(first) {
		t.Fatalf("not a UUID: %s", first)
	}
	if first == stableKeyUUID("openrouter", "groq-1") {
		t.Fatal("different providers collided")
	}
	if got := stableKeyUUID("groq", "550E8400-E29B-41D4-A716-446655440000"); got != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("UUID normalization: %s", got)
	}
	if first[14] != '8' {
		t.Fatalf("expected custom UUID version 8: %s", first)
	}
}

func TestPersistedConfigJSONExcludesSecrets(t *testing.T) {
	raw := config.Raw{APIKey: "kp_private_api_key", Providers: map[string]config.ProviderRaw{"groq": {Keys: []sched.KeySpec{{ID: "groq-1", Secret: secret.New("provider-secret-should-not-leak")}}}}}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, needle := range []string{"kp_private_api_key", "provider-secret-should-not-leak"} {
		if strings.Contains(text, needle) {
			t.Fatalf("serialized config leaked %q", needle)
		}
	}
}

func TestNormalizeSelectorResolvesCanonicalIDsWithoutMutatingInput(t *testing.T) {
	original := config.Selector{KeyIDs: []string{"old-alias", "direct-id"}, TagsAny: []string{"mathlab"}}
	canonical := normalizeSelector(original, map[string]string{"old-alias": "550e8400-e29b-41d4-a716-446655440000"}, "groq")
	if canonical.KeyIDs[0] != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("alias not canonicalized: %#v", canonical.KeyIDs)
	}
	if canonical.KeyIDs[1] != stableKeyUUID("groq", "direct-id") {
		t.Fatalf("unmapped key id not normalized: %#v", canonical.KeyIDs)
	}
	if original.KeyIDs[0] != "old-alias" {
		t.Fatalf("normalization mutated original selector: %#v", original.KeyIDs)
	}
}
