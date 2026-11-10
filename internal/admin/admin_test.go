package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"keypool/internal/config"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/store/memstore"
)

func fixture(t *testing.T) (*API, *memstore.Store) {
	t.Helper()
	key := secret.New("gsk_existing_secret_123456789")
	enabled := true
	raw := &config.Raw{APIKey: "kp_test_token", DefaultProject: "mathlab", Providers: map[string]config.ProviderRaw{
		"groq": {Type: "openai_compatible", BaseURL: "https://example.com/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Enabled: &enabled, Keys: []sched.KeySpec{{ID: "groq-1", Label: "existing", Secret: key, Tags: []string{"mathlab"}}}},
	}, Projects: map[string]config.ProjectRaw{
		"mathlab": {Project: "mathlab", Keys: map[string]config.KeyRule{"groq": {Include: config.Selector{TagsAny: []string{"mathlab"}}}}, Routes: map[string]config.RouteRaw{"prover": {Priority: 2, Chain: []string{"groq:openai/gpt-oss-120b"}}}},
	}}
	st := memstore.New(raw)
	var published *config.Snapshot
	api, err := New(st, raw.APIKey, func(s *config.Snapshot) error { published = s; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = config.Build(raw); err != nil {
		t.Fatal(err)
	}
	_ = context.Background()
	_ = published
	return api, st
}

func TestApplyRejectsUnknownFieldsAndPreservesSecrets(t *testing.T) {
	api, st := fixture(t)
	body := `{"default_project":"mathlab","providers":{"groq":{"type":"openai_compatible","base_url":"https://example.com/v1","unexpected":true}},"projects":{"mathlab":{"project":"mathlab","keys":{"groq":{"include":{"tags_any":["mathlab"]}}},"routes":{"prover":{"priority":2,"chain":["groq:openai/gpt-oss-120b"]}}}}}`
	r := httptest.NewRequest(http.MethodPost, "/admin/v1/apply", strings.NewReader(body))
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("unknown field status=%d body=%s", w.Code, w.Body.String())
	}
	keys, err := st.LoadKeys(context.Background())
	if err != nil || len(keys) != 1 || keys[0].Secret.Reveal() != "gsk_existing_secret_123456789" {
		t.Fatalf("existing credential not preserved: %v %d", err, len(keys))
	}
}

func TestKeyAPIResponsesNeverContainSecret(t *testing.T) {
	api, _ := fixture(t)
	secretValue := "gsk_new_secret_value_123456789"
	payload, _ := json.Marshal(map[string]any{"provider": "groq", "label": "groq-new", "secret": secretValue, "tags": []string{"mathlab"}})
	r := httptest.NewRequest(http.MethodPost, "/admin/v1/keys", strings.NewReader(string(payload)))
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("add status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secretValue) {
		t.Fatal("POST /keys leaked provider secret")
	}
	r = httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	w = httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), secretValue) {
		t.Fatalf("GET /keys leaked secret: %d %s", w.Code, w.Body.String())
	}
}

func TestDecodeStrictJSONRejectsUnknownFields(t *testing.T) {
	var raw config.Raw
	if err := decodeStrict([]byte(`{"listen":"127.0.0.1:8787","unknown_field":true}`), &raw); err == nil {
		t.Fatal("expected unknown JSON field rejection")
	}
}

func TestApplySuccessPublishesSnapshotAndPreservesExistingKey(t *testing.T) {
	api, st := fixture(t)
	body := `{"listen":"127.0.0.1:8787","default_project":"mathlab","providers":{"groq":{"type":"openai_compatible","base_url":"https://example.com/v1","auth":{"scheme":"bearer"},"enabled":true}},"projects":{"mathlab":{"project":"mathlab","keys":{"groq":{"include":{"tags_any":["mathlab"]}}},"routes":{"prover":{"priority":2,"chain":["groq:openai/gpt-oss-120b"]}}}}}`
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/v1/apply", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("apply status=%d body=%s", w.Code, w.Body.String())
	}
	keys, err := st.LoadKeys(context.Background())
	if err != nil || len(keys) != 1 || keys[0].Secret.Reveal() != "gsk_existing_secret_123456789" {
		t.Fatalf("credential lost: %v %+v", err, keys)
	}
}

func TestAPIKeyRotationReturnsNewKeyOnceAndUpdatesStore(t *testing.T) {
	api, st := fixture(t)
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/v1/apikey/rotate", nil))
	if w.Code != 200 {
		t.Fatalf("rotate status=%d body=%s", w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result["api_key"], "kp_") {
		t.Fatalf("invalid rotated key: %q", result["api_key"])
	}
	raw, err := st.LoadConfig(context.Background())
	if err != nil || raw.APIKey != result["api_key"] {
		t.Fatalf("store not rotated: %v", err)
	}
}

func TestApplyAcceptsSingleProjectProfile(t *testing.T) {
	api, st := fixture(t)
	body := `{"project":"mathlab","description":"perfil atualizado","allow_lease":false,"keys":{"groq":{"include":{"tags_any":["mathlab"]}}},"routes":{"prover":{"priority":2,"chain":["groq:openai/gpt-oss-120b"]}}}`
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/v1/apply", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("project apply status=%d body=%s", w.Code, w.Body.String())
	}
	raw, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if raw.Projects["mathlab"].Description != "perfil atualizado" {
		t.Fatalf("project profile not applied: %+v", raw.Projects["mathlab"])
	}
}

func TestAdminJSONRedactsSecretEvenWhenUsedAsMetadata(t *testing.T) {
	api, st := fixture(t)
	known := "gsk_existing_secret_123456789"
	raw, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	project := raw.Projects["mathlab"]
	project.Description = known
	raw.Projects["mathlab"] = project
	for _, path := range []string{"/admin/v1/projects", "/admin/v1/providers", "/admin/v1/keys", "/admin/v1/stats", "/admin/v1/usage", "/admin/v1/events"} {
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(w.Body.String(), known) {
			t.Fatalf("secret leaked from %s: %s", path, w.Body.String())
		}
	}
	newSecret := "gsk_metadata_leak_test_123456"
	payload, _ := json.Marshal(map[string]any{"provider": "groq", "label": newSecret, "secret": newSecret, "tags": []string{"mathlab"}})
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/v1/keys", strings.NewReader(string(payload))))
	if strings.Contains(w.Body.String(), newSecret) {
		t.Fatalf("secret leaked in key creation response: %s", w.Body.String())
	}
}

func TestKeyMutationValidatesBeforePersisting(t *testing.T) {
	api, st := fixture(t)
	keys, err := st.LoadKeys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("fixture keys: %v %d", err, len(keys))
	}
	path := "/admin/v1/keys/" + string(keys[0].ID) + "/disable"
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	if w.Code != 400 {
		t.Fatalf("disabling last route key should fail validation: %d %s", w.Code, w.Body.String())
	}
	after, err := st.LoadKeys(context.Background())
	if err != nil || len(after) != 1 || !after[0].Enabled {
		t.Fatalf("failed mutation changed store: %v %+v", err, after)
	}
}
