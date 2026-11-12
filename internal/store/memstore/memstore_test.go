package memstore

import (
	"context"
	"encoding/json"
	"testing"

	"keypool/internal/config"
	"keypool/internal/domain"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/store"
)

func TestStoreLoadsKeysAndUpdatesThemWithConfig(t *testing.T) {
	first := secret.New("first-secret")
	st := New(&config.Raw{Providers: map[string]config.ProviderRaw{
		"groq": {Keys: []sched.KeySpec{{ID: "k1", Label: "first", Secret: first, Tags: []string{"example"}}}},
	}})
	keys, err := st.LoadKeys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("initial keys=%v err=%v", keys, err)
	}
	keys[0].Tags[0] = "mutated"
	keysAgain, err := st.LoadKeys(context.Background())
	if err != nil || len(keysAgain) != 1 || keysAgain[0].Tags[0] != "example" {
		t.Fatalf("LoadKeys exposed mutable tag storage: keys=%v err=%v", keysAgain, err)
	}
	second := secret.New("second-secret")
	if err = st.SaveConfig(context.Background(), &config.Raw{Providers: map[string]config.ProviderRaw{
		"openrouter": {Keys: []sched.KeySpec{{ID: "k2", Label: "second", Secret: second}}},
	}}); err != nil {
		t.Fatal(err)
	}
	keys, err = st.LoadKeys(context.Background())
	if err != nil || len(keys) != 1 || keys[0].Provider != "openrouter" || keys[0].ID != "k2" {
		t.Fatalf("updated keys=%v err=%v", keys, err)
	}
}

func TestStoreCopiesPoolSnapshotsAndUsage(t *testing.T) {
	st := New(nil)
	state := sched.PoolSnapshot{SavedAtMS: 123}
	if err := json.Unmarshal([]byte(`{"groups":{"team":{"bucket":{"Inflight":1}}}}`), &state); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePoolState(context.Background(), "groq", state); err != nil {
		t.Fatal(err)
	}
	state.Groups["team"] = nil
	loaded, err := st.LoadPoolState(context.Background(), "groq")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Groups["team"] == nil {
		t.Fatal("SavePoolState retained caller's mutable map")
	}
	usage := &domain.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}
	if err := st.AppendUsage(context.Background(), []domain.UsageEvent{{Usage: usage}}); err != nil {
		t.Fatal(err)
	}
	usage.TotalTokens = 999
	if got := st.usage.values()[0].Usage.TotalTokens; got != 5 {
		t.Fatalf("stored usage changed through caller pointer: %d", got)
	}
}

func TestStoreHonorsCanceledContext(t *testing.T) {
	st := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.LoadKeys(ctx); err == nil {
		t.Fatal("LoadKeys accepted canceled context")
	}
}

func TestSaveConfigPreservesDisabledKeys(t *testing.T) {
	raw := &config.Raw{Providers: map[string]config.ProviderRaw{"groq": {}}}
	st := New(raw)
	key := store.KeyRecord{Provider: "groq", ID: "k1", Label: "disabled", Secret: secret.New("gsk_disabled_key"), Enabled: false}
	if err := st.SaveKeys(context.Background(), "groq", []store.KeyRecord{key}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(context.Background(), &config.Raw{Providers: map[string]config.ProviderRaw{"groq": {}}}); err != nil {
		t.Fatal(err)
	}
	keys, err := st.LoadKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Enabled {
		t.Fatalf("disabled key lost or enabled: %+v", keys)
	}
}
