package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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

func TestBalanceGroupsChooseProviderWithLowerDailyQuotaUtilization(t *testing.T) {
	up := fakeupstream.New()
	defer up.Close()
	secretGroq := "groq-test-key-abcdefghijklmnopqrstuvwxyz"
	secretRouter := "router-test-key-abcdefghijklmnopqrstuvwxyz"
	raw := &config.Raw{
		APIKey: "kp_test", DefaultProject: "p",
		Providers: map[string]config.ProviderRaw{
			"groq":       {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, LimitScope: "provider", Limits: config.RawLimits{DefaultRPD: intPtr(100)}, Keys: []sched.KeySpec{{ID: "g", Secret: secret.New(secretGroq)}}},
			"openrouter": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, LimitScope: "provider", Limits: config.RawLimits{DefaultRPD: intPtr(20)}, Keys: []sched.KeySpec{{ID: "o", Secret: secret.New(secretRouter)}}},
		},
		Projects: map[string]config.ProjectRaw{"p": {
			Keys: map[string]config.KeyRule{
				"groq":       {Include: config.Selector{KeyIDs: []string{"g"}}},
				"openrouter": {Include: config.Selector{KeyIDs: []string{"o"}}},
			},
			Routes: map[string]config.RouteRaw{"simple": {
				// OpenRouter appears first, but has spent half its daily allowance.
				Chain:         []string{"openrouter:light-model", "groq:small-model"},
				BalanceGroups: [][]string{{"groq", "openrouter"}},
			}},
		}},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		lease, err := rt.Pools()["openrouter"].TryAcquire(sched.AcquireRequest{Model: "light-model", Tokens: 1})
		if err != nil {
			t.Fatal(err)
		}
		lease.Success(1, nil)
	}
	for i := 0; i < 20; i++ {
		lease, err := rt.Pools()["groq"].TryAcquire(sched.AcquireRequest{Model: "small-model", Tokens: 1})
		if err != nil {
			t.Fatal(err)
		}
		lease.Success(1, nil)
	}
	up.Enqueue("Bearer "+secretGroq, fakeupstream.JSON(http.StatusOK, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	payload := map[string]json.RawMessage{
		"model":    json.RawMessage(`"simple"`),
		"messages": json.RawMessage(`[{"role":"user","content":"summarize"}]`),
	}
	response, err := rt.Do(context.Background(), &Request{Project: "p", Model: "simple", Payload: payload, Timeout: time.Second * 5, TimeoutSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if response.Target != "groq:small-model" {
		t.Fatalf("expected lower-utilization Groq target, got %q", response.Target)
	}
}

func TestBalanceGroupsChooseLeastConsumedAcrossThreeProviders(t *testing.T) {
	up := fakeupstream.New()
	defer up.Close()
	geminiSecret := "gemini-test-key-abcdefghijklmnopqrstuvwxyz"
	groqSecret := "groq-test-key-abcdefghijklmnopqrstuvwxyz"
	openrouterSecret := "router-test-key-abcdefghijklmnopqrstuvwxyz"
	raw := &config.Raw{
		APIKey: "kp_test", DefaultProject: "p",
		Providers: map[string]config.ProviderRaw{
			"gemini":     {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, LimitScope: "provider", Limits: config.RawLimits{DefaultRPD: intPtr(20)}, Keys: []sched.KeySpec{{ID: "gm", Secret: secret.New(geminiSecret)}}},
			"groq":       {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, LimitScope: "provider", Limits: config.RawLimits{DefaultRPD: intPtr(100)}, Keys: []sched.KeySpec{{ID: "g", Secret: secret.New(groqSecret)}}},
			"openrouter": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, LimitScope: "provider", Limits: config.RawLimits{DefaultRPD: intPtr(50)}, Keys: []sched.KeySpec{{ID: "o", Secret: secret.New(openrouterSecret)}}},
		},
		Projects: map[string]config.ProjectRaw{"p": {
			Keys: map[string]config.KeyRule{
				"gemini":     {Include: config.Selector{KeyIDs: []string{"gm"}}},
				"groq":       {Include: config.Selector{KeyIDs: []string{"g"}}},
				"openrouter": {Include: config.Selector{KeyIDs: []string{"o"}}},
			},
			Routes: map[string]config.RouteRaw{"elaborate": {
				Chain:         []string{"gemini:flash", "groq:gpt-oss", "openrouter:nemotron:free"},
				BalanceGroups: [][]string{{"gemini", "groq", "openrouter"}},
			}},
		}},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// Gemini: 4/20 = 20%; Groq: 10/100 = 10%; OpenRouter: 15/50 = 30%.
	for _, item := range []struct {
		provider string
		model    string
		count    int
	}{{"gemini", "flash", 4}, {"groq", "gpt-oss", 10}, {"openrouter", "nemotron:free", 15}} {
		for i := 0; i < item.count; i++ {
			lease, err := rt.Pools()[item.provider].TryAcquire(sched.AcquireRequest{Model: item.model, Tokens: 1})
			if err != nil {
				t.Fatal(err)
			}
			lease.Success(1, nil)
		}
	}
	up.Enqueue("Bearer "+groqSecret, fakeupstream.JSON(http.StatusOK, `{"choices":[{"message":{"content":"balanced"},"finish_reason":"stop"}]}`))
	payload := map[string]json.RawMessage{
		"model":    json.RawMessage(`"elaborate"`),
		"messages": json.RawMessage(`[{"role":"user","content":"reason"}]`),
	}
	response, err := rt.Do(context.Background(), &Request{Project: "p", Model: "elaborate", Payload: payload, Timeout: 5 * time.Second, TimeoutSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if response.Target != "groq:gpt-oss" {
		t.Fatalf("expected least-consumed provider Groq, got %q", response.Target)
	}
}

func TestDailyQuotaExhaustionFallsBackAcrossPreferenceGroups(t *testing.T) {
	up := fakeupstream.New()
	defer up.Close()
	googleSecret := "google-test-key-abcdefghijklmnopqrstuvwxyz"
	groqSecret := "groq-test-key-abcdefghijklmnopqrstuvwxyz"
	openrouterSecret := "router-test-key-abcdefghijklmnopqrstuvwxyz"
	raw := &config.Raw{
		APIKey: "kp_test", DefaultProject: "p",
		Providers: map[string]config.ProviderRaw{
			"gemini":     {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, QuotaCooldown: "1h", LimitScope: "provider", Keys: []sched.KeySpec{{ID: "gm", Secret: secret.New(googleSecret)}}},
			"groq":       {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, LimitScope: "provider", Keys: []sched.KeySpec{{ID: "g", Secret: secret.New(groqSecret)}}},
			"openrouter": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, LimitScope: "provider", Keys: []sched.KeySpec{{ID: "o", Secret: secret.New(openrouterSecret)}}},
		},
		Projects: map[string]config.ProjectRaw{"p": {
			Keys: map[string]config.KeyRule{
				"gemini":     {Include: config.Selector{KeyIDs: []string{"gm"}}},
				"groq":       {Include: config.Selector{KeyIDs: []string{"g"}}},
				"openrouter": {Include: config.Selector{KeyIDs: []string{"o"}}},
			},
			Routes: map[string]config.RouteRaw{"deep": {
				Chain:         []string{"gemini:deep-model", "groq:reasoning-model", "openrouter:reasoning-model:free"},
				BalanceGroups: [][]string{{"gemini"}, {"groq", "openrouter"}},
			}},
		}},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	up.Enqueue("Bearer "+googleSecret, fakeupstream.JSON(http.StatusTooManyRequests, `{"error":{"message":"daily quota exhausted: requests per day"}}`))
	up.Enqueue("Bearer "+groqSecret, fakeupstream.JSON(http.StatusOK, `{"choices":[{"message":{"content":"fallback"},"finish_reason":"stop"}]}`))
	payload := map[string]json.RawMessage{
		"model":    json.RawMessage(`"deep"`),
		"messages": json.RawMessage(`[{"role":"user","content":"prove"}]`),
	}
	response, err := rt.Do(context.Background(), &Request{Project: "p", Model: "deep", Payload: payload, Timeout: 5 * time.Second, TimeoutSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if response.Target != "groq:reasoning-model" || !response.Fallback {
		t.Fatalf("expected cross-group Groq fallback after Google daily quota, got target=%q fallback=%v", response.Target, response.Fallback)
	}
}

func TestDailyQuotaExhaustionFallsBackWithinBalancedGroup(t *testing.T) {
	for _, first := range []string{"groq", "openrouter"} {
		t.Run(first+"_exhausted", func(t *testing.T) {
			up := fakeupstream.New()
			defer up.Close()
			groqSecret := "groq-test-key-abcdefghijklmnopqrstuvwxyz"
			openrouterSecret := "router-test-key-abcdefghijklmnopqrstuvwxyz"
			raw := &config.Raw{
				APIKey: "kp_test", DefaultProject: "p",
				Providers: map[string]config.ProviderRaw{
					"groq":       {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, QuotaCooldown: "24h", LimitScope: "provider", Keys: []sched.KeySpec{{ID: "g", Secret: secret.New(groqSecret)}}},
					"openrouter": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, QuotaCooldown: "24h", LimitScope: "provider", Keys: []sched.KeySpec{{ID: "o", Secret: secret.New(openrouterSecret)}}},
				},
				Projects: map[string]config.ProjectRaw{"p": {
					Keys: map[string]config.KeyRule{
						"groq":       {Include: config.Selector{KeyIDs: []string{"g"}}},
						"openrouter": {Include: config.Selector{KeyIDs: []string{"o"}}},
					},
					Routes: map[string]config.RouteRaw{"simple": {
						Chain:         []string{"groq:small-model", "openrouter:small-model:free"},
						BalanceGroups: [][]string{{"groq", "openrouter"}},
					}},
				}},
			}
			if first == "openrouter" {
				route := raw.Projects["p"].Routes["simple"]
				route.Chain = []string{"openrouter:small-model:free", "groq:small-model"}
				raw.Projects["p"].Routes["simple"] = route
			}
			snapshot, err := config.Build(raw)
			if err != nil {
				t.Fatal(err)
			}
			rt, err := New(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			fallback := "openrouter"
			if first == "openrouter" {
				fallback = "groq"
			}
			firstSecret, fallbackSecret := groqSecret, openrouterSecret
			firstTarget, fallbackTarget := "groq:small-model", "openrouter:small-model:free"
			if first == "openrouter" {
				firstSecret, fallbackSecret = openrouterSecret, groqSecret
				firstTarget, fallbackTarget = "openrouter:small-model:free", "groq:small-model"
			}
			up.Enqueue("Bearer "+firstSecret, fakeupstream.JSON(http.StatusTooManyRequests, `{"error":{"message":"daily quota exhausted: requests per day"}}`))
			up.Enqueue("Bearer "+fallbackSecret, fakeupstream.JSON(http.StatusOK, `{"choices":[{"message":{"content":"fallback"},"finish_reason":"stop"}]}`))
			payload := map[string]json.RawMessage{
				"model":    json.RawMessage(`"simple"`),
				"messages": json.RawMessage(`[{"role":"user","content":"summarize"}]`),
			}
			response, err := rt.Do(context.Background(), &Request{Project: "p", Model: "simple", Payload: payload, Timeout: 5 * time.Second, TimeoutSet: true})
			if err != nil {
				t.Fatal(err)
			}
			if response.Target != fallbackTarget || !response.Fallback {
				t.Fatalf("expected %s to fall back to %s (first target %s); got target=%s fallback=%v", first, fallback, firstTarget, response.Target, response.Fallback)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

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

func TestRouterTimeoutPreservesUpstreamDiagnostics(t *testing.T) {
	up := fakeupstream.New()
	defer up.Close()
	secretValue := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	raw := &config.Raw{
		APIKey: "kp_test", DefaultProject: "p",
		Providers: map[string]config.ProviderRaw{
			"groq": {Type: "openai_compatible", BaseURL: up.URL() + "/v1",
				Auth: provider.AuthSpec{Scheme: "bearer"}, Paths: provider.PathsSpec{Chat: "/chat/completions"},
				Keys: []sched.KeySpec{{ID: "k1", Secret: secret.New(secretValue)}}},
		},
		Projects: map[string]config.ProjectRaw{
			"p": {Project: "p", Keys: map[string]config.KeyRule{"groq": {Include: config.Selector{KeyIDs: []string{"k1"}}}},
				Routes: map[string]config.RouteRaw{"main": {Chain: []string{"groq:test-model"}}}},
		},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	up.Enqueue("Bearer "+secretValue, fakeupstream.Response{
		Status: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Chunks: [][]byte{[]byte(`{"choices":[]}`)}, ChunkDelay: 200 * time.Millisecond,
	})
	_, err = rt.Do(context.Background(), &Request{
		Project: "p", Model: "main", Payload: map[string]json.RawMessage{
			"model": json.RawMessage(`"main"`), "messages": json.RawMessage(`[]`),
		}, Timeout: 30 * time.Millisecond, TimeoutSet: true,
	})
	var failure *RouteFailure
	if !errors.As(err, &failure) {
		t.Fatalf("expected RouteFailure, got %T (%v)", err, err)
	}
	if failure.Code != "keypool_timeout" || failure.Phase != "upstream" {
		t.Fatalf("unexpected failure classification: %+v", failure)
	}
	if failure.UpstreamTarget != "groq:test-model" || failure.Attempts != 1 || failure.Fallback {
		t.Fatalf("upstream diagnostics lost or incorrect: %+v", failure)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline cause was not preserved: %v", err)
	}
	if strings.Contains(failure.Error(), secretValue) {
		t.Fatal("failure leaked upstream credential")
	}
}

func TestSpillContinuesAfterEachCandidateQueueTimeout(t *testing.T) {
	up := fakeupstream.New()
	defer up.Close()
	groqSecret := "groq-queue-test-key-abcdefghijklmnopqrstuvwxyz"
	routerSecret := "router-queue-test-key-abcdefghijklmnopqrstuvwxyz"
	raw := &config.Raw{
		APIKey: "kp_test", DefaultProject: "p",
		Providers: map[string]config.ProviderRaw{
			"groq":       {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Keys: []sched.KeySpec{{ID: "g", Secret: secret.New(groqSecret)}}},
			"openrouter": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Keys: []sched.KeySpec{{ID: "o", Secret: secret.New(routerSecret)}}},
		},
		Projects: map[string]config.ProjectRaw{"p": {
			Keys: map[string]config.KeyRule{
				"groq":       {Include: config.Selector{KeyIDs: []string{"g"}}},
				"openrouter": {Include: config.Selector{KeyIDs: []string{"o"}}},
			},
			Policy: config.Policy{SpillOnSaturation: true},
			Routes: map[string]config.RouteRaw{"main": {Chain: []string{"groq:small-model", "openrouter:free-model"}, BalanceGroups: [][]string{{"groq", "openrouter"}}}},
		}},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, setup := range []struct{ provider, model string }{{"groq", "small-model"}, {"openrouter", "free-model"}} {
		lease, acquireErr := rt.Pools()[setup.provider].TryAcquire(sched.AcquireRequest{Model: setup.model, Tokens: 1})
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		lease.RateLimited(100*time.Millisecond, true, nil)
	}
	_, err = rt.Do(context.Background(), &Request{
		Project: "p", Model: "main", Payload: map[string]json.RawMessage{"model": json.RawMessage(`"main"`), "messages": json.RawMessage(`[]`)},
		Timeout: time.Second, TimeoutSet: true, QueueTimeout: 10 * time.Millisecond, QueueTimeoutSet: true,
	})
	var failure *RouteFailure
	if !errors.As(err, &failure) {
		t.Fatalf("expected RouteFailure, got %T (%v)", err, err)
	}
	if failure.Code != "keypool_queue_timeout" || failure.Attempts != 0 || !failure.Fallback {
		t.Fatalf("queue fallback was not attempted across providers: %+v", failure)
	}
}

func TestSpillUsesPerUpstreamAttemptTimeoutBeforeFallback(t *testing.T) {
	up := fakeupstream.New()
	defer up.Close()
	groqSecret := "groq-slow-test-key-abcdefghijklmnopqrstuvwxyz"
	routerSecret := "router-fast-test-key-abcdefghijklmnopqrstuvwxyz"
	raw := &config.Raw{
		APIKey: "kp_test", DefaultProject: "p",
		Providers: map[string]config.ProviderRaw{
			"groq":       {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Keys: []sched.KeySpec{{ID: "g", Secret: secret.New(groqSecret)}}},
			"openrouter": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Keys: []sched.KeySpec{{ID: "o", Secret: secret.New(routerSecret)}}},
		},
		Projects: map[string]config.ProjectRaw{"p": {
			Keys: map[string]config.KeyRule{
				"groq":       {Include: config.Selector{KeyIDs: []string{"g"}}},
				"openrouter": {Include: config.Selector{KeyIDs: []string{"o"}}},
			},
			Policy: config.Policy{SpillOnSaturation: true, AttemptTimeout: "35ms"},
			Routes: map[string]config.RouteRaw{"main": {Chain: []string{"groq:slow-model", "openrouter:fast-model"}, BalanceGroups: [][]string{{"groq", "openrouter"}}}},
		}},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	up.Enqueue("Bearer "+groqSecret, fakeupstream.Response{Status: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Chunks: [][]byte{[]byte(`{"choices":[]}`)}, ChunkDelay: 200 * time.Millisecond})
	up.Enqueue("Bearer "+routerSecret, fakeupstream.JSON(http.StatusOK, `{"choices":[{"message":{"content":"fallback-ok"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`))
	resp, err := rt.Do(context.Background(), &Request{
		Project: "p", Model: "main", Payload: map[string]json.RawMessage{"model": json.RawMessage(`"main"`), "messages": json.RawMessage(`[]`)},
		Timeout: time.Second, TimeoutSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Target != "openrouter:fast-model" || !resp.Fallback || resp.Attempts != 2 {
		t.Fatalf("expected fallback after bounded first upstream attempt, got %+v", resp)
	}
}

func TestSpillFallsBackToQueuedProviderAfterImmediateUpstreamFailure(t *testing.T) {
	up := fakeupstream.New()
	defer up.Close()
	groqSecret := "groq-transient-test-key-abcdefghijklmnopqrstuvwxyz"
	routerSecret := "router-queued-test-key-abcdefghijklmnopqrstuvwxyz"
	raw := &config.Raw{
		APIKey: "kp_test", DefaultProject: "p",
		Providers: map[string]config.ProviderRaw{
			"groq":       {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Keys: []sched.KeySpec{{ID: "g", Secret: secret.New(groqSecret)}}},
			"openrouter": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Keys: []sched.KeySpec{{ID: "o", Secret: secret.New(routerSecret)}}},
		},
		Projects: map[string]config.ProjectRaw{"p": {
			Keys: map[string]config.KeyRule{
				"groq":       {Include: config.Selector{KeyIDs: []string{"g"}}},
				"openrouter": {Include: config.Selector{KeyIDs: []string{"o"}}},
			},
			Policy: config.Policy{SpillOnSaturation: true},
			Routes: map[string]config.RouteRaw{"main": {Chain: []string{"groq:transient-model", "openrouter:queued-model"}, BalanceGroups: [][]string{{"groq", "openrouter"}}}},
		}},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	queuedLease, err := rt.Pools()["openrouter"].TryAcquire(sched.AcquireRequest{Model: "queued-model", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	queuedLease.RateLimited(35*time.Millisecond, true, nil)
	up.Enqueue("Bearer "+groqSecret, fakeupstream.JSON(http.StatusServiceUnavailable, `{"error":{"message":"temporary upstream outage"}}`))
	up.Enqueue("Bearer "+routerSecret, fakeupstream.JSON(http.StatusOK, `{"choices":[{"message":{"content":"queued-fallback-ok"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`))
	resp, err := rt.Do(context.Background(), &Request{
		Project: "p", Model: "main", Payload: map[string]json.RawMessage{"model": json.RawMessage(`"main"`), "messages": json.RawMessage(`[]`)},
		Timeout: time.Second, TimeoutSet: true, QueueTimeout: 300 * time.Millisecond, QueueTimeoutSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Target != "openrouter:queued-model" || !resp.Fallback || resp.Attempts != 2 {
		t.Fatalf("expected queued fallback after transient first target, got %+v", resp)
	}
}
