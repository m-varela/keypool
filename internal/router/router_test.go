package router

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/testutil/fakeupstream"
)

func TestSpillWaitsForTargetWithLowestEstimatedDelay(t *testing.T) {
	up := fakeupstream.New()
	defer up.Close()
	fakeClock := clock.NewFake(time.Now())
	raw := &config.Raw{
		APIKey:         "kp_test",
		DefaultProject: "p",
		Providers: map[string]config.ProviderRaw{
			"groq": {
				Type:    "openai_compatible",
				BaseURL: up.URL() + "/v1",
				Auth:    provider.AuthSpec{Scheme: "bearer"},
				Paths:   provider.PathsSpec{Chat: "/chat/completions"},
				Keys:    []sched.KeySpec{{ID: "k1", Secret: secret.New("gsk_test_key_abcdefghijklmnopqrstuvwxyz")}},
			},
		},
		Projects: map[string]config.ProjectRaw{
			"p": {
				Project: "p",
				Keys:    map[string]config.KeyRule{"groq": {Include: config.Selector{KeyIDs: []string{"k1"}}}},
				Routes: map[string]config.RouteRaw{
					"best": {Priority: 5, Chain: []string{"groq:fast-model", "groq:slow-model"}},
				},
			},
		},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := NewWithClock(snapshot, fakeClock)
	if err != nil {
		t.Fatal(err)
	}
	pool := rt.Pools()["groq"]
	fastLease, err := pool.TryAcquire(sched.AcquireRequest{Model: "fast-model", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	fastLease.RateLimited(20*time.Second, true, nil)
	slowLease, err := pool.TryAcquire(sched.AcquireRequest{Model: "slow-model", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	slowLease.RateLimited(50*time.Second, true, nil)

	up.Enqueue("Bearer gsk_test_key_abcdefghijklmnopqrstuvwxyz", fakeupstream.JSON(200, `{"choices":[]}`))
	payload := map[string]json.RawMessage{
		"model":    json.RawMessage(`"best"`),
		"messages": json.RawMessage(`[]`),
	}
	resp, err := rt.Do(context.Background(), &Request{
		Project: "p", Model: "best", Payload: payload,
		Timeout: time.Minute, TimeoutSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Target != "groq:fast-model" {
		t.Fatalf("spill waited for wrong target: got %q, want groq:fast-model", resp.Target)
	}
	if got := fakeClock.Elapsed(); got < 20*time.Second || got >= 50*time.Second {
		t.Fatalf("waited %s; expected the shorter 20s cooldown, not 50s", got)
	}
}

func TestSnapshotApplyWaitsForActiveWarmRuntime(t *testing.T) {
	raw := &config.Raw{APIKey: "kp_test", Providers: map[string]config.ProviderRaw{
		"sample": {Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", Auth: provider.AuthSpec{Scheme: "bearer"}, Keys: []sched.KeySpec{{ID: "key-1", Secret: secret.New("secret")}}},
	}, Projects: map[string]config.ProjectRaw{
		"default": {Keys: map[string]config.KeyRule{"sample": {Include: config.Selector{KeyIDs: []string{"key-1"}}}}, Routes: map[string]config.RouteRaw{"main": {Chain: []string{"sample:model-a"}}}},
	}}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	_, release, ok := rt.AcquireWarmProvider("sample")
	if !ok {
		t.Fatal("failed to acquire warm runtime")
	}
	done := make(chan error, 1)
	go func() { done <- rt.SetSnapshot(snapshot) }()
	select {
	case err := <-done:
		t.Fatalf("snapshot replaced a runtime with an active warm probe: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SetSnapshot after warm probe: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot apply did not resume after warm probe release")
	}
}

func TestCredentialRotationResetsWarmHealthCatalogAndLatency(t *testing.T) {
	enabled := true
	raw := &config.Raw{APIKey: "kp_test", DefaultProject: "p", Providers: map[string]config.ProviderRaw{
		"sample": {Type: "openai_compatible", Enabled: &enabled, BaseURL: "http://127.0.0.1:1", Auth: provider.AuthSpec{Scheme: "bearer"}, Paths: provider.PathsSpec{Models: "/models"}, Keys: []sched.KeySpec{{ID: "key-1", Secret: secret.New("old-secret")}}},
	}, Projects: map[string]config.ProjectRaw{
		"p": {Keys: map[string]config.KeyRule{"sample": {Include: config.Selector{KeyIDs: []string{"key-1"}}}}, Routes: map[string]config.RouteRaw{"main": {Chain: []string{"sample:model-a"}}}},
	}}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	pool := rt.Pools()["sample"]
	now := time.Now()
	pool.RecordProbe("key-1", true, false, now, "ok")
	pool.SetCatalogAt("key-1", []string{"model-a"}, now)
	lease, err := pool.TryAcquire(sched.AcquireRequest{Model: "model-a", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	lease.RecordTTFB(250 * time.Millisecond)
	lease.Release()
	before := pool.Stats()[0].CredentialVersion

	rotated := *raw
	rotated.Providers = map[string]config.ProviderRaw{}
	for name, item := range raw.Providers {
		item.Keys = append([]sched.KeySpec(nil), item.Keys...)
		item.Keys[0].Secret = secret.New("new-secret")
		rotated.Providers[name] = item
	}
	next, err := config.Build(&rotated)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.SetSnapshot(next); err != nil {
		t.Fatal(err)
	}
	stats := rt.Pools()["sample"].Stats()[0]
	if stats.CredentialVersion <= before || stats.Validated || stats.CatalogKnown || stats.Health != "unknown" || !stats.LastOK.IsZero() || len(stats.TTFBEWMA) != 0 {
		t.Fatalf("credential rotation retained stale warm state: before=%d after=%+v", before, stats)
	}
}
