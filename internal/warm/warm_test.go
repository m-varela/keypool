package warm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/domain"
	"keypool/internal/provider"
	"keypool/internal/router"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/store/memstore"
)

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

func testRuntime(t *testing.T, baseURL string, handlerConfig func(*config.ProviderRaw), warmRaw config.WarmRaw) (*router.Router, *memstore.Store, config.WarmConfig) {
	t.Helper()
	providerRaw := config.ProviderRaw{
		Type: "openai_compatible", Enabled: boolPtr(true), BaseURL: baseURL,
		Auth: provider.AuthSpec{Scheme: "bearer"}, Timeout: "3s",
		Paths:  provider.PathsSpec{Chat: "/chat/completions", Models: "/models"},
		Probe:  provider.ProbeSpec{Kind: "key", Path: "/key"},
		Limits: config.RawLimits{DefaultTPM: intPtr(1000), DefaultRPM: intPtr(100), DefaultRPD: intPtr(500), DefaultTPD: intPtr(10000)},
		Keys:   []sched.KeySpec{{ID: "key-1", Label: "test-key", Secret: secret.New("test-secret")}},
	}
	if handlerConfig != nil {
		handlerConfig(&providerRaw)
	}
	raw := &config.Raw{
		APIKey:    "kp_test",
		Providers: map[string]config.ProviderRaw{"test": providerRaw},
		Projects: map[string]config.ProjectRaw{"default": {
			Keys:   map[string]config.KeyRule{"test": {Include: config.Selector{KeyIDs: []string{"key-1"}}}},
			Routes: map[string]config.RouteRaw{"default": {Chain: []string{"test:model-a"}}},
		}},
		DefaultProject: "default",
		Warm:           warmRaw,
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatalf("config.Build: %v", err)
	}
	rt, err := router.New(snapshot)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return rt, memstore.New(raw), snapshot.WarmConfig()
}

func testServer(t *testing.T, keyStatus int, chatCounter *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
			return
		}
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
		case "/key":
			if keyStatus != http.StatusOK {
				w.Header().Set("Retry-After", "10")
				w.WriteHeader(keyStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"is_free_tier":false,"limit":500,"limit_remaining":400}}`))
		case "/chat/completions":
			if chatCounter != nil {
				chatCounter.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"p"}}],"usage":{"completion_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestRunPerformsL0AndL1AndPersistsCatalog(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	m := New(cfg, rt, repo)
	value, err := m.Run(context.Background(), "test", "key-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	results, ok := value.([]ProbeResult)
	if !ok || len(results) != 2 {
		t.Fatalf("unexpected run result: %#v", value)
	}
	if results[0].Layer != "L0" || !results[0].OK {
		t.Fatalf("L0 result: %+v", results[0])
	}
	if results[1].Layer != "L1" || !results[1].OK {
		t.Fatalf("L1 result: %+v", results[1])
	}
	if !rt.WarmProviders()[0].Pool.ModelAllowed("key-1", "model-a", false) {
		t.Fatal("listed model must be allowed")
	}
	if rt.WarmProviders()[0].Pool.ModelAllowed("key-1", "not-listed", false) {
		t.Fatal("unlisted fixed model must be rejected after catalog refresh")
	}
	entries, err := repo.LoadModelCatalog(context.Background())
	if err != nil || len(entries) != 1 || entries[0].Provider != "test" || len(entries[0].Models) != 2 {
		t.Fatalf("catalog persistence: %#v, %v", entries, err)
	}
	usage, err := repo.ReadUsage(context.Background(), time.Time{}, time.Time{}, "")
	if err != nil || len(usage) != 2 {
		t.Fatalf("warm usage: %d, %v", len(usage), err)
	}
	for _, event := range usage {
		if event.Source != "warmup" {
			t.Fatalf("usage source = %q", event.Source)
		}
	}
}

func TestL1InvalidCredentialDisablesKey(t *testing.T) {
	srv := testServer(t, http.StatusUnauthorized, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	m := New(cfg, rt, repo)
	_, err := m.ProbeKey(context.Background(), "key-1")
	if err == nil {
		t.Fatal("expected invalid credential error")
	}
	stats := rt.WarmProviders()[0].Pool.Stats()
	if len(stats) != 1 || !stats[0].Disabled || stats[0].Health != "invalid" {
		t.Fatalf("invalid credential state: %+v", stats)
	}
}

func TestL1RateLimitIsNeutralToCircuit(t *testing.T) {
	srv := testServer(t, http.StatusTooManyRequests, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	m := New(cfg, rt, repo)
	for i := 0; i < 5; i++ {
		_, err := m.ProbeKey(context.Background(), "key-1")
		if err == nil {
			t.Fatal("expected 429")
		}
	}
	stats := rt.WarmProviders()[0].Pool.Stats()
	if stats[0].Health == "open" || stats[0].Health == "invalid" || stats[0].Disabled {
		t.Fatalf("429 changed circuit/credential state: %+v", stats[0])
	}
}

func TestL2OneTokenBudgetAndUsageAccounting(t *testing.T) {
	var chats atomic.Int32
	srv := testServer(t, http.StatusOK, &chats)
	defer srv.Close()
	warmRaw := config.WarmRaw{Enabled: boolPtr(true), L2: config.WarmL2Raw{Enabled: true, Model: "model-a", DailyTokenBudget: 1, MinRPD: 10}}
	rt, repo, cfg := testRuntime(t, srv.URL, nil, warmRaw)
	m := New(cfg, rt, repo)
	if _, err := m.ProbeKey(context.Background(), "key-1"); err != nil {
		t.Fatalf("L1 validation: %v", err)
	}
	result, err := m.runLayer(context.Background(), "test", "key-1", layerL2, true)
	if err != nil || result == nil || !result.OK {
		t.Fatalf("L2 first token: %+v, %v", result, err)
	}
	_, err = m.runLayer(context.Background(), "test", "key-1", layerL2, true)
	if err == nil || !strings.Contains(err.Error(), "orçamento diário") {
		t.Fatalf("expected daily budget block, got %v", err)
	}
	if got := chats.Load(); got != 1 {
		t.Fatalf("chat calls = %d, want 1", got)
	}
	usage, err := repo.ReadUsage(context.Background(), time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range usage {
		if e.Alias == "warmup-L2" {
			found = true
			if e.Source != "warmup" || e.EstimatedTokens != 1 || e.Model != "model-a" {
				t.Fatalf("L2 usage: %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("L2 usage event not persisted")
	}
}

func TestRunDoesNotProbeBusyRealKey(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	lease, err := rt.WarmProviders()[0].Pool.TryAcquire(sched.AcquireRequest{Model: "model-a", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	m := New(cfg, rt, repo)
	_, err = m.ProbeKey(context.Background(), "key-1")
	if err == nil || !strings.Contains(err.Error(), "ocupada") {
		t.Fatalf("expected busy key, got %v", err)
	}
}

func TestL2DisabledByDefault(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{})
	m := New(cfg, rt, repo)
	if cfg.L2.Enabled {
		t.Fatal("L2 must be disabled by default")
	}
	_, err := m.runLayer(context.Background(), "test", "key-1", layerL2, true)
	if err == nil || !strings.Contains(err.Error(), "L2 desativada") {
		t.Fatalf("L2 unexpectedly active: %v", err)
	}
	usage, readErr := repo.ReadUsage(context.Background(), time.Time{}, time.Time{}, "")
	if readErr != nil || len(usage) != 0 {
		t.Fatalf("skipped L2 must not create token usage: rows=%d err=%v", len(usage), readErr)
	}
}

func TestAccountInfoFreeTierBlocksL2(t *testing.T) {
	var chatCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key" {
			_, _ = w.Write([]byte(`{"data":{"is_free_tier":true}}`))
			return
		}
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
			return
		}
		if r.URL.Path == "/chat/completions" {
			chatCount.Add(1)
			_, _ = w.Write([]byte(`{"usage":{"completion_tokens":1}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	warmRaw := config.WarmRaw{Enabled: boolPtr(true), L2: config.WarmL2Raw{Enabled: true, Model: "model-a", DailyTokenBudget: 3, MinRPD: 10}}
	rt, repo, cfg := testRuntime(t, srv.URL, nil, warmRaw)
	m := New(cfg, rt, repo)
	if _, err := m.ProbeKey(context.Background(), "key-1"); err != nil {
		t.Fatalf("L1: %v", err)
	}
	_, err := m.runLayer(context.Background(), "test", "key-1", layerL2, true)
	if err == nil || !strings.Contains(err.Error(), "conta free") {
		t.Fatalf("expected free-tier block, got %v", err)
	}
	if chatCount.Load() != 0 {
		t.Fatal("L2 sent a completion to a free-tier account")
	}
}

func TestProbeResultDoesNotExposeSecret(t *testing.T) {
	message := redactForKey("upstream rejected test-secret", secret.New("test-secret"))
	if strings.Contains(message, "test-secret") {
		t.Fatalf("credential leaked: %s", message)
	}
}

func TestReadinessTimeoutWarningClearsAfterFirstPass(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	m := NewWithClock(cfg, rt, repo, clock.Real())
	m.mu.Lock()
	m.ready = true
	m.readyReason = "ready_timeout expirou antes da primeira passada L1"
	m.firstPending = map[string]bool{}
	m.mu.Unlock()
	m.checkFirstPass()
	status := m.Status().(Status)
	if !status.Ready || status.ReadyReason != "primeira passada L1 concluída" {
		t.Fatalf("readiness did not clear timeout warning: %+v", status)
	}
}

func TestStatusIncludesLearnedRateHintsAndLatency(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	p := rt.WarmProviders()[0].Pool
	observedAt := time.Now().Add(-time.Minute)
	hint := sched.Hints{"rpm": {Remaining: 7, HasRemaining: true, ObservedAt: observedAt}}
	p.ApplyHints("key-1", p.HintBuckets(nil)[0], hint)
	lease, err := p.TryAcquire(sched.AcquireRequest{Model: "model-a", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	lease.RecordTTFB(45 * time.Millisecond)
	lease.Success(1, nil)
	m := New(cfg, rt, repo)
	value := m.Status().(Status)
	if len(value.Keys) != 1 {
		t.Fatalf("unexpected key status: %+v", value.Keys)
	}
	key := value.Keys[0]
	if key.Hints == nil || !key.Hints[p.HintBuckets(nil)[0]]["rpm"].HasRemaining {
		t.Fatalf("learned hints missing from status: %+v", key.Hints)
	}
	if key.TTFBEWMA["model-a"] != 45*time.Millisecond {
		t.Fatalf("TTFB EWMA missing: %+v", key.TTFBEWMA)
	}
	if !key.HintObservedAt.Equal(observedAt) {
		t.Fatalf("hint age timestamp = %s, want %s", key.HintObservedAt, observedAt)
	}
}

func TestL2BlocksLowRPDByDefault(t *testing.T) {
	var chatCount atomic.Int32
	srv := testServer(t, http.StatusOK, &chatCount)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, func(raw *config.ProviderRaw) {
		raw.Limits.DefaultRPD = intPtr(50)
	}, config.WarmRaw{Enabled: boolPtr(true), L2: config.WarmL2Raw{Enabled: true, Model: "model-a", DailyTokenBudget: 4}})
	m := New(cfg, rt, repo)
	if _, err := m.ProbeKey(context.Background(), "key-1"); err != nil {
		t.Fatalf("L1 validation: %v", err)
	}
	_, err := m.runLayer(context.Background(), "test", "key-1", layerL2, true)
	if err == nil || !strings.Contains(err.Error(), "RPD baixo") {
		t.Fatalf("expected low RPD block, got %v", err)
	}
	if got := chatCount.Load(); got != 0 {
		t.Fatalf("low RPD must not send a completion, requests=%d", got)
	}
	usage, readErr := repo.ReadUsage(context.Background(), time.Time{}, time.Time{}, "")
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, event := range usage {
		if event.Alias == "warmup-L2" {
			t.Fatalf("blocked L2 must not create token usage: %+v", event)
		}
	}
}

func TestL0HonorsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Retry-After", "75")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.URL.Path == "/key" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	m := New(cfg, rt, repo)
	result, err := m.runLayer(context.Background(), "test", "key-1", layerL0, true)
	if err == nil || result == nil || !result.RetryKnown || result.RetryAfter < 75*time.Second {
		t.Fatalf("L0 retry-after not captured: result=%+v err=%v", result, err)
	}
	interval := m.nextInterval(cfg, layerL0, result, err, "test", "key-1")
	if interval < 75*time.Second {
		t.Fatalf("L0 scheduled before Retry-After: %s", interval)
	}
}

func TestPersistedL2BudgetIsProviderScoped(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true), L2: config.WarmL2Raw{Enabled: true, Model: "model-a", DailyTokenBudget: 10, MinRPD: 10}})
	_ = rt
	at := time.Now().UTC()
	rows := []domain.UsageEvent{
		{At: at, Provider: "other", KeyID: "key-1", Alias: "warmup-L2", Source: "warmup", EstimatedTokens: 8},
		{At: at, Provider: "test", KeyID: "key-1", Alias: "warmup-L2", Source: "warmup", EstimatedTokens: 2},
	}
	if err := repo.AppendUsage(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	m := New(cfg, rt, repo)
	got, err := m.loadPersistedL2Budget(context.Background(), "test", "key-1", at)
	if err != nil || got != 2 {
		t.Fatalf("provider-scoped budget=%d, err=%v; want 2", got, err)
	}
}

func TestWarmReadinessTimeoutUsesInjectedClock(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true), ReadyTimeout: "3s"})
	fake := clock.NewFake(time.Unix(1000, 0))
	m := NewWithClock(cfg, rt, repo, fake)
	m.mu.Lock()
	m.firstPending["test/key-1"] = true
	m.mu.Unlock()
	done := make(chan struct{})
	m.wg.Add(1)
	go func() { m.readinessTimeout(context.Background(), cfg.ReadyTimeout, 0); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fake-clock readiness timeout blocked")
	}
	if fake.Elapsed() != 3*time.Second {
		t.Fatalf("fake clock elapsed=%s, want 3s", fake.Elapsed())
	}
	status := m.Status().(Status)
	if !status.Ready || !strings.Contains(status.ReadyReason, "ready_timeout") {
		t.Fatalf("readiness timeout did not publish warning: %+v", status)
	}
}

func TestFirstPassRemainsPendingWhenKeyIsAlreadyBeingProbed(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	m := New(cfg, rt, repo)
	m.mu.Lock()
	m.firstPending["test/key-1"] = true
	m.statuses["test/key-1"] = &KeyStatus{Provider: "test", KeyID: "key-1"}
	m.mu.Unlock()
	release, ok := rt.WarmProviders()[0].Pool.BeginProbe("key-1", 0, false, true)
	if !ok {
		t.Fatal("failed to reserve key for competing probe")
	}
	defer release()
	m.executeScheduled(context.Background(), job{provider: "test", key: "key-1", kind: layerL1})
	m.mu.Lock()
	pending := m.firstPending["test/key-1"]
	_, requeued := m.scheduled[jobID{provider: "test", key: "key-1", kind: layerL1}]
	m.mu.Unlock()
	if !pending || !requeued {
		t.Fatalf("busy first L1 was lost: pending=%v requeued=%v", pending, requeued)
	}
}

func TestL2DoesNotSpendBudgetBeforeTransportWrites(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	warmRaw := config.WarmRaw{Enabled: boolPtr(true), L2: config.WarmL2Raw{Enabled: true, Model: "model-a", DailyTokenBudget: 2, MinRPD: 10}}
	rt, repo, cfg := testRuntime(t, srv.URL, nil, warmRaw)
	providers := rt.WarmProviders()
	providers[0].Client.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed before request write")
	})
	m := New(cfg, rt, repo)
	result, err := m.runLayer(context.Background(), "test", "key-1", layerL2, true)
	if err == nil || result == nil || result.TokenAttempted {
		t.Fatalf("failed pre-write transport counted as token attempt: result=%+v err=%v", result, err)
	}
	m.mu.Lock()
	budget := m.daily["test/key-1"]
	m.mu.Unlock()
	if budget.tokens != 0 {
		t.Fatalf("pre-write failure spent daily budget: %+v", budget)
	}
	usage, readErr := repo.ReadUsage(context.Background(), time.Time{}, time.Time{}, "")
	if readErr != nil || len(usage) != 0 {
		t.Fatalf("pre-write failure created L2 usage: rows=%d err=%v", len(usage), readErr)
	}
}

func TestL2RequiresSuccessfulL1Validation(t *testing.T) {
	var chats atomic.Int32
	srv := testServer(t, http.StatusOK, &chats)
	defer srv.Close()
	warmRaw := config.WarmRaw{Enabled: boolPtr(true), L2: config.WarmL2Raw{Enabled: true, Model: "model-a", DailyTokenBudget: 2, MinRPD: 10}}
	rt, repo, cfg := testRuntime(t, srv.URL, nil, warmRaw)
	m := New(cfg, rt, repo)
	_, err := m.runLayer(context.Background(), "test", "key-1", layerL2, true)
	if err == nil || !strings.Contains(err.Error(), "validada por L1") {
		t.Fatalf("L2 should require a validated credential, got %v", err)
	}
	if chats.Load() != 0 {
		t.Fatalf("L2 made %d completions before L1", chats.Load())
	}
}

func TestL2BlocksLowLearnedRPDRemaining(t *testing.T) {
	var chats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining-Requests", "20")
		switch r.URL.Path {
		case "/key":
			_, _ = w.Write([]byte(`{"data":{"is_free_tier":false}}`))
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/chat/completions":
			chats.Add(1)
			_, _ = w.Write([]byte(`{"usage":{"completion_tokens":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	warmRaw := config.WarmRaw{Enabled: boolPtr(true), L2: config.WarmL2Raw{Enabled: true, Model: "model-a", DailyTokenBudget: 2, MinRPD: 100}}
	rt, repo, cfg := testRuntime(t, srv.URL, func(raw *config.ProviderRaw) {
		raw.RateHeaders = map[string]provider.RateHeaderSpec{
			"remaining_requests": {Header: "X-RateLimit-Remaining-Requests", Dimension: "rpd", Format: "integer"},
		}
	}, warmRaw)
	m := New(cfg, rt, repo)
	if _, err := m.ProbeKey(context.Background(), "key-1"); err != nil {
		t.Fatalf("L1 validation: %v", err)
	}
	if _, ok := rt.WarmProviders()[0].Pool.HintSnapshot("key-1")[""]["rpd"]; !ok {
		t.Fatal("provider-wide rate-limit hint was not retained in the global bucket")
	}
	_, err := m.runLayer(context.Background(), "test", "key-1", layerL2, true)
	if err == nil || !strings.Contains(err.Error(), "RPD baixo") {
		t.Fatalf("expected learned low-RPD block, got %v", err)
	}
	if chats.Load() != 0 {
		t.Fatalf("low learned RPD sent %d completions", chats.Load())
	}
}

func TestInvalidCredentialStopsL0AndL2Rescheduling(t *testing.T) {
	srv := testServer(t, http.StatusUnauthorized, nil)
	defer srv.Close()
	warmRaw := config.WarmRaw{Enabled: boolPtr(true), L2: config.WarmL2Raw{Enabled: true, Model: "model-a", DailyTokenBudget: 1, MinRPD: 10}}
	rt, repo, cfg := testRuntime(t, srv.URL, nil, warmRaw)
	m := New(cfg, rt, repo)
	m.syncTargets(true)
	m.mu.Lock()
	delete(m.scheduled, jobID{provider: "test", key: "key-1", kind: layerL1})
	m.compactQueueLocked()
	m.mu.Unlock()
	m.executeScheduled(context.Background(), job{provider: "test", key: "key-1", kind: layerL1})
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.scheduled {
		if id.provider == "test" && id.key == "key-1" && (id.kind == layerL0 || id.kind == layerL2) {
			t.Fatalf("invalid key retained non-L1 scheduled job: %+v", id)
		}
	}
	if at, ok := m.scheduled[jobID{provider: "test", key: "key-1", kind: layerL1}]; !ok || !at.After(m.clk.Now()) {
		t.Fatalf("invalid key should retain only delayed L1 recheck: %v %v", at, ok)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestReadinessTimeoutFromPreviousGenerationIsIgnored(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true), ReadyTimeout: "3s"})
	fake := clock.NewFake(time.Unix(2000, 0))
	m := NewWithClock(cfg, rt, repo, fake)
	m.mu.Lock()
	m.readinessEpoch = 2
	m.firstPending["test/key-1"] = true
	m.ready = false
	m.mu.Unlock()
	done := make(chan struct{})
	m.wg.Add(1)
	go func() { m.readinessTimeout(context.Background(), cfg.ReadyTimeout, 1); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fake-clock stale readiness timer blocked")
	}
	status := m.Status().(Status)
	if status.Ready || status.ReadyReason != "" {
		t.Fatalf("stale timeout changed readiness generation: %+v", status)
	}
}

func TestManualProbesRespectGlobalWorkerBudget(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true), Workers: 1})
	m := New(cfg, rt, repo)
	m.mu.Lock()
	m.activeWorkers = 1 // simulate the only worker being occupied by scheduled work
	m.mu.Unlock()
	if _, err := m.ProbeKey(context.Background(), "key-1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("manual key probe must share worker limit, got %v", err)
	}
	if _, err := m.Run(context.Background(), "test", "key-1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("manual warm run must share worker limit, got %v", err)
	}
}

func TestStartupInvalidatesPersistedCatalogBeforeCurrentCredentialProbe(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	if err := repo.SaveModelCatalog(context.Background(), "test", "key-1", []string{"stale-model"}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	m := New(cfg, rt, repo)
	m.syncTargets(true)
	entries, err := repo.LoadModelCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stale persisted catalog survived startup: %+v", entries)
	}
	if rt.WarmProviders()[0].Pool.ModelAllowed("key-1", "stale-model", false) {
		t.Fatal("fixed model was authorized by an unverified startup catalog")
	}
}

func TestStartupWithL1DisabledDoesNotStrandFixedModelsOnEmptyCatalog(t *testing.T) {
	srv := testServer(t, http.StatusOK, nil)
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true), L1Enabled: boolPtr(false)})
	m := New(cfg, rt, repo)
	m.syncTargets(true)
	if !rt.WarmProviders()[0].Pool.ModelAllowed("key-1", "model-a", false) {
		t.Fatal("disabling L1 must not strand fixed-model routing behind an empty catalog")
	}
}

func TestSuccessfulL1WithUnavailableCatalogBlocksFixedModelsButKeepsDynamicAliases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/key":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"is_free_tier":false,"limit":500,"limit_remaining":400}}`))
		case "/models":
			http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rt, repo, cfg := testRuntime(t, srv.URL, nil, config.WarmRaw{Enabled: boolPtr(true)})
	_, err := New(cfg, rt, repo).ProbeKey(context.Background(), "key-1")
	if err != nil {
		t.Fatalf("L1 credential validation should succeed despite catalog failure: %v", err)
	}
	pool := rt.WarmProviders()[0].Pool
	if pool.ModelAllowed("key-1", "fixed-model", false) {
		t.Fatal("fixed model was allowed without an explicit catalog entry")
	}
	if !pool.ModelAllowed("key-1", "dynamic-alias", true) {
		t.Fatal("validated credential should still allow a dynamic alias")
	}
	if models, known := pool.Catalog("key-1"); !known || len(models) != 0 {
		t.Fatalf("empty authoritative catalog was not recorded: models=%v known=%v", models, known)
	}
	entries, err := repo.LoadModelCatalog(context.Background())
	if err != nil || len(entries) != 1 || len(entries[0].Models) != 0 {
		t.Fatalf("empty catalog persistence: %+v, %v", entries, err)
	}
}
