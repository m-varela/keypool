package sched

import (
	"context"
	"encoding/json"
	"errors"
	"keypool/internal/clock"
	"keypool/internal/limits"
	"keypool/internal/secret"
	"strings"
	"sync"
	"testing"
	"time"
)

func ip(v int) *int { return &v }
func fakePool(scope Scope, ls limits.Limits, keys ...KeySpec) (*Pool, *clock.Fake) {
	c := clock.NewFake(time.Unix(1000, 0))
	p := NewPool(PoolConfig{Provider: "groq", Clock: c, Limits: ls, Scope: scope})
	p.SetKeys(keys)
	return p, c
}
func ks(id, label, group, key string) KeySpec {
	return KeySpec{ID: KeyID(id), Label: label, Group: group, Secret: secret.New(key)}
}
func TestRoundRobin(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "sk-one"), ks("2", "k2", "", "sk-two"))
	want := []KeyID{"1", "2", "1"}
	for i, w := range want {
		l, e := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
		if e != nil {
			t.Fatal(e)
		}
		if l.KeyID() != w {
			t.Fatalf("%d got %s", i, l.KeyID())
		}
		l.Release()
	}
}
func TestSuccessAdjustsReservation(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{TPM: ip(100)}}, ks("1", "k1", "", "k-secret"))
	l, e := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 60})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 60}); e == nil {
		t.Fatal("expected capacity")
	}
	l.Success(10, nil)
	if d, ok := p.EstimateWait(AcquireRequest{Model: "m", Tokens: 90}); !ok || d != 0 {
		t.Fatalf("estimate %v %v", d, ok)
	}
	_ = c
}
func TestTPMReservationWaitsWithFakeClock(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{TPM: ip(100)}}, ks("1", "k1", "", "k-secret"))
	l, e := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 60})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 60}); e == nil {
		t.Fatal("expected capacity")
	}
	l.Success(60, nil)
	waited, e := p.Acquire(context.Background(), AcquireRequest{Model: "m", Tokens: 60})
	if e != nil {
		t.Fatal(e)
	}
	defer waited.Release()
	if c.Elapsed() < time.Minute {
		t.Fatalf("fake clock advanced only %s", c.Elapsed())
	}
}
func TestRPMAndRPD(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{RPM: ip(1)}}, ks("1", "k1", "", "k-secret"))
	l, e := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if e != nil {
		t.Fatal(e)
	}
	l.Success(1, nil)
	if _, e = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); e == nil {
		t.Fatal("RPM not enforced")
	}
	if d, ok := p.EstimateWait(AcquireRequest{Model: "m", Tokens: 1}); !ok || d < time.Minute {
		t.Fatalf("%v %v", d, ok)
	}
}
func TestReleaseReturnsAllReservations(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{RPM: ip(1), TPM: ip(100)}}, ks("1", "k1", "", "k-secret"))
	l, e := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 50})
	if e != nil {
		t.Fatal(e)
	}
	l.Release()
	if _, e = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 50}); e != nil {
		t.Fatal(e)
	}
}
func TestProviderScopeSharesWindowsBetweenKeys(t *testing.T) {
	p, _ := fakePool(ScopeProvider, limits.Limits{Default: limits.ModelLimits{RPM: ip(1)}}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	l, e := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if e != nil {
		t.Fatal(e)
	}
	l.Success(1, nil)
	if _, e = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); e == nil {
		t.Fatal("scope did not share")
	}
}

func TestDailyQuotaUsageNormalizesRequestsAndTokens(t *testing.T) {
	p, _ := fakePool(ScopeProvider, limits.Limits{Default: limits.ModelLimits{RPD: ip(100), TPD: ip(1000)}}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	for i := 0; i < 20; i++ {
		lease, err := p.TryAcquire(AcquireRequest{Model: "model", Tokens: 10})
		if err != nil {
			t.Fatal(err)
		}
		lease.Success(10, nil)
	}
	usage := p.DailyQuotaUsage("model")
	if usage.RequestsUsed != 20 || usage.RequestsLimit != 100 || usage.TokensUsed != 200 || usage.TokensLimit != 1000 {
		t.Fatalf("unexpected normalized quota usage: %+v", usage)
	}
	if !usage.HasLimits || usage.Ratio != 0.2 {
		t.Fatalf("unexpected quota ratio: %+v", usage)
	}
}

func TestProviderScopeQuotaExhaustedBlocksAllKeys(t *testing.T) {
	p, _ := fakePool(ScopeProvider, limits.Limits{Default: limits.ModelLimits{RPD: ip(100)}}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	lease, err := p.TryAcquire(AcquireRequest{Model: "model", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	lease.QuotaExhausted()
	if wait, ok := p.EstimateWait(AcquireRequest{Model: "model", Tokens: 1}); !ok || wait <= 0 {
		t.Fatalf("provider-wide quota exhaustion was not applied to all keys: wait=%v ok=%v", wait, ok)
	}
}
func TestQuotaExhaustedUsesUpstreamDailyResetHint(t *testing.T) {
	p, c := fakePool(ScopeProvider, limits.Limits{Default: limits.ModelLimits{RPD: ip(100)}}, ks("1", "k1", "", "a"))
	p.cfg.QuotaCooldown = 24 * time.Hour
	lease, err := p.TryAcquire(AcquireRequest{Model: "model", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	reset := c.Now().Add(7 * time.Hour)
	hints := Hints{"rpd": {ResetAt: reset, ObservedAt: c.Now()}}
	lease.QuotaExhausted(&hints)
	wait, ok := p.EstimateWait(AcquireRequest{Model: "model", Tokens: 1})
	if !ok || wait != 7*time.Hour {
		t.Fatalf("upstream daily reset was not honored: wait=%v ok=%v", wait, ok)
	}
}

func TestAutoScopeMerge(t *testing.T) {
	p, c := fakePool(ScopeAuto, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	l, _ := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	l.RateLimited(30*time.Second, true, nil)
	c.Advance(time.Second)
	l2, _ := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1, Allow: func(id KeyID) bool { return id == "2" }})
	l2.RateLimited(30*time.Second, true, nil)
	p.mu.Lock()
	merged := p.merged
	p.mu.Unlock()
	if !merged {
		t.Fatal("not merged")
	}
}
func TestStatsNeverExposeKey(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "gsk_…last", "", "gsk_abcdefghijklmnopqrstuvwxyz"))
	b, e := json.Marshal(p.Stats())
	if e != nil {
		t.Fatal(e)
	}
	if string(b) == "" || contains(string(b), "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("leak: %s", b)
	}
}
func contains(s, sub string) bool { return len(sub) > 0 && strings.Contains(s, sub) }
func TestPriorityTicketOrder(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{RPM: ip(1)}}, ks("1", "k1", "", "a"))
	p.mu.Lock()
	p.waiters["m"] = []waiter{{ticket: 1, priority: 0}}
	if !p.outranked("m", 5, 0) {
		t.Fatal("urgent waiter must outrank")
	}
	p.mu.Unlock()
}
func TestSnapshotRestore(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{RPM: ip(2)}}, ks("1", "k1", "", "a"))
	l, _ := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	l.Success(1, nil)
	snap := p.Snapshot()
	p2, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{RPM: ip(2)}}, ks("1", "k1", "", "a"))
	p2.Restore(snap)
	if _, e := p2.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); e != nil {
		t.Fatal(e)
	}
	_ = c
}
func TestConcurrentAcquireIsRaceFree(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"), ks("3", "k3", "", "c"))
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, e := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
			if e == nil {
				l.Success(1, nil)
			} else {
				var capErr *ErrNoCapacity
				if !errors.As(e, &capErr) {
					t.Error(e)
				}
			}
		}()
	}
	wg.Wait()
}
func TestAcquireCanceled(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := p.Acquire(ctx, AcquireRequest{Model: "m"}); e == nil {
		t.Fatal("expected cancellation")
	}
}
func BenchmarkAcquireSuccess(b *testing.B) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l, e := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 10})
		if e != nil {
			b.Fatal(e)
		}
		l.Success(10, nil)
	}
}
func BenchmarkAcquireContended(b *testing.B) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"), ks("3", "k3", "", "c"))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l, e := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 10})
			if e == nil {
				l.Success(10, nil)
			}
		}
	})
}

func TestTPDReservationProtectsDailyQuota(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{TPD: ip(100)}}, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 40})
	if err != nil {
		t.Fatal(err)
	}
	l.Success(40, nil)
	if d, ok := p.EstimateWait(AcquireRequest{Model: "m", Tokens: 40, Reserve: 0}); !ok || d != 0 {
		t.Fatalf("high priority should use reserve: %v %v", d, ok)
	}
	if d, ok := p.EstimateWait(AcquireRequest{Model: "m", Tokens: 40, Reserve: .25}); !ok || d <= 0 {
		t.Fatalf("low priority should wait: %v %v", d, ok)
	}
}

func TestLeaseWatchdogCompletesLeakedLease(t *testing.T) {
	c := clock.NewFake(time.Unix(1000, 0))
	p := NewPool(PoolConfig{Clock: c, LeaseTTL: time.Second})
	p.SetKeys([]KeySpec{ks("1", "k1", "", "a")})
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(2 * time.Second)
	_ = p.Stats()
	l.Success(1, nil) // watchdog já concluiu; chamada tardia deve ser no-op.
	if p.Stats()[0].Inflight != 0 {
		t.Fatal("lease watchdog deixou inflight pendente")
	}
}

func TestHintsBlockUntilReset(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	h := Hints{"tpm": {Remaining: 0, HasRemaining: true, ResetAt: c.Now().Add(time.Minute), ObservedAt: c.Now()}}
	l.Success(10, &h)
	if d, ok := p.EstimateWait(AcquireRequest{Model: "m", Tokens: 10}); !ok || d < time.Minute {
		t.Fatalf("hint not applied: %v %v", d, ok)
	}
}

func TestCircuitBreakerHalfOpenAllowsOneProbe(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	for i := 0; i < 3; i++ {
		l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		l.Failed()
		c.Advance(31 * time.Second)
	}
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); err == nil {
		t.Fatal("half-open circuit admitted a second probe")
	}
	l.Success(1, nil)
	if p.Stats()[0].Health != "healthy" {
		t.Fatal("circuit did not close after success")
	}
}

func TestConcurrencyLimitReleasesOnCompletion(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{Conc: ip(1)}}, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); err == nil {
		t.Fatal("CONC limit not enforced")
	}
	l.Release()
	if _, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestFreeBucketSharedBetweenFreeModels(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Rules: []limits.Rule{
		{Match: "model-a:free", Bucket: "free", Free: true, ModelLimits: limits.ModelLimits{RPM: ip(1)}},
		{Match: "model-b:free", Bucket: "free", Free: true, ModelLimits: limits.ModelLimits{RPM: ip(1)}},
		{Match: "model-c:free", Bucket: "free", Free: true, ModelLimits: limits.ModelLimits{RPM: ip(1)}},
	}}, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "model-a:free", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.Success(1, nil)
	if _, err = p.TryAcquire(AcquireRequest{Model: "model-b:free", Tokens: 1}); err == nil {
		t.Fatal("free models did not share their bucket")
	}
	if _, err = p.TryAcquire(AcquireRequest{Model: "model-c:free", Tokens: 1}); err == nil {
		t.Fatal("third free model should hit the shared bucket")
	}
	paid, err := p.TryAcquire(AcquireRequest{Model: "paid-model", Tokens: 1})
	if err != nil {
		t.Fatalf("paid model unexpectedly shares free bucket: %v", err)
	}
	paid.Release()
}

func TestRateLimitCooldownIsPerBucket(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "one", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.RateLimited(30*time.Second, true, nil)
	if _, err = p.TryAcquire(AcquireRequest{Model: "one", Tokens: 1}); err == nil {
		t.Fatal("same bucket should be cooling down")
	}
	if _, err = p.TryAcquire(AcquireRequest{Model: "two", Tokens: 1}); err != nil {
		t.Fatalf("different bucket unexpectedly blocked: %v", err)
	}
	c.Advance(31 * time.Second)
}

func TestProviderScopeRateLimitCoolsEveryKey(t *testing.T) {
	p, _ := fakePool(ScopeProvider, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.RateLimited(30*time.Second, true, nil)
	if _, err = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); err == nil {
		t.Fatal("provider scope did not cool all keys")
	}
}

func TestQuotaExhaustedOnFreeBlocksKeyTotally(t *testing.T) {
	ls := limits.Limits{Rules: []limits.Rule{{Match: "free-model", Free: true, Bucket: "free"}}}
	p, c := fakePool(ScopeKey, ls, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "free-model", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.QuotaExhausted()
	if _, err = p.TryAcquire(AcquireRequest{Model: "paid-model", Tokens: 1}); err == nil {
		t.Fatal("free quota exhaustion should block the whole key")
	}
	c.Advance(time.Hour + time.Second)
	p, _ = fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	l, err = p.TryAcquire(AcquireRequest{Model: "paid-model", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.QuotaExhausted()
	if _, err = p.TryAcquire(AcquireRequest{Model: "other-model", Tokens: 1}); err != nil {
		t.Fatalf("paid quota should be bucket scoped: %v", err)
	}
}

func TestEstimateWaitDoesNotReserveOrClaimHalfOpenCircuit(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	for i := 0; i < 3; i++ {
		l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
		if err != nil {
			t.Fatal(err)
		}
		l.Failed()
		c.Advance(31 * time.Second)
	}
	// Expired open circuit can be estimated without consuming its single probe.
	if d, ok := p.EstimateWait(AcquireRequest{Model: "m", Tokens: 1}); !ok || d != 0 {
		t.Fatalf("estimate=%v %v", d, ok)
	}
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); err == nil {
		t.Fatal("estimate claimed no probe, second real request must be rejected")
	}
	l.Success(1, nil)
}

func TestLeastUsedPrefersLessUsedKey(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	p.cfg.Strategy = LeastUsed
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 30})
	if err != nil {
		t.Fatal(err)
	}
	l.Success(30, nil)
	l, err = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if l.KeyID() != "2" {
		t.Fatalf("least-used chose %s", l.KeyID())
	}
	l.Release()
}

func TestRateLimitCooldownSwitchesKey(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	first, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.KeyID() != "1" {
		t.Fatalf("first key = %s", first.KeyID())
	}
	first.RateLimited(30*time.Second, true, nil)
	next, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if next.KeyID() != "2" {
		t.Fatalf("cooldown did not switch key: %s", next.KeyID())
	}
}

func TestAuthErrorDisablesKey(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	first, err := p.TryAcquire(AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	first.AuthFailed()
	second, err := p.TryAcquire(AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if second.KeyID() != "2" {
		t.Fatalf("second key = %s", second.KeyID())
	}
	second.AuthFailed()
	if _, err = p.TryAcquire(AcquireRequest{Model: "m"}); !errors.Is(err, ErrAllDisabled) {
		t.Fatalf("expected ErrAllDisabled, got %v", err)
	}
}

func TestTPMIsPerModel(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{
		Default:  limits.ModelLimits{TPM: ip(100)},
		PerModel: map[string]limits.ModelLimits{"special": {TPM: ip(50)}},
	}, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "special", Tokens: 40})
	if err != nil {
		t.Fatal(err)
	}
	l.Success(40, nil)
	other, err := p.TryAcquire(AcquireRequest{Model: "regular", Tokens: 80})
	if err != nil {
		t.Fatalf("another model should have its own bucket: %v", err)
	}
	other.Release()
	if _, err = p.TryAcquire(AcquireRequest{Model: "special", Tokens: 20}); err == nil {
		t.Fatal("special model TPM should remain enforced")
	}
}

func TestRPDUses24hWindow(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{RPD: ip(2)}}, ks("1", "k1", "", "a"))
	for i := 0; i < 2; i++ {
		l, err := p.TryAcquire(AcquireRequest{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		l.Success(1, nil)
	}
	if _, err := p.TryAcquire(AcquireRequest{Model: "m"}); err == nil {
		t.Fatal("third request should exceed RPD")
	}
	wait, ok := p.EstimateWait(AcquireRequest{Model: "m"})
	if !ok || wait < 23*time.Hour {
		t.Fatalf("RPD wait = %v, possible=%v", wait, ok)
	}
}

func TestRPMEnforcedAndWaits(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{RPM: ip(2)}}, ks("1", "k1", "", "a"))
	for i := 0; i < 2; i++ {
		l, err := p.TryAcquire(AcquireRequest{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		l.Success(1, nil)
	}
	wait, ok := p.EstimateWait(AcquireRequest{Model: "m"})
	if !ok || wait < 59*time.Second {
		t.Fatalf("RPM wait = %v, possible=%v", wait, ok)
	}
	l, err := p.Acquire(context.Background(), AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if c.Elapsed() < time.Minute {
		t.Fatalf("fake clock advanced only %s", c.Elapsed())
	}
}

func TestLimitOneIsEnforced(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{RPM: ip(1)}}, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	l.Success(1, nil)
	if _, err = p.TryAcquire(AcquireRequest{Model: "m"}); err == nil {
		t.Fatal("RPM=1 must reject a second request in the same window")
	}
	waited, err := p.Acquire(context.Background(), AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer waited.Release()
	if c.Elapsed() < time.Minute {
		t.Fatalf("limit=1 wait advanced only %s", c.Elapsed())
	}
}

func TestEstimateWaitDoesNotReserve(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{TPM: ip(1000)}}, ks("1", "k1", "", "a"))
	for i := 0; i < 5; i++ {
		if wait, ok := p.EstimateWait(AcquireRequest{Model: "m", Tokens: 100}); !ok || wait != 0 {
			t.Fatalf("estimate %d = %v, %v", i, wait, ok)
		}
	}
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 900})
	if err != nil {
		t.Fatalf("estimate reserved capacity unexpectedly: %v", err)
	}
	l.Release()
	if _, ok := p.EstimateWait(AcquireRequest{Model: "m", Tokens: 1000}); ok {
		t.Fatal("request equal to TPM should be impossible")
	}
}

func TestEstimateWaitImpossibleWhenAllDisabled(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	l.AuthFailed()
	if _, ok := p.EstimateWait(AcquireRequest{Model: "m"}); ok {
		t.Fatal("disabled pool should be impossible")
	}
}

func TestTwoPoolsInParallelAreIndependent(t *testing.T) {
	groq, _ := fakePool(ScopeKey, limits.Limits{}, ks("g1", "groq-key", "", "gsk_test"))
	openrouter, _ := fakePool(ScopeKey, limits.Limits{}, ks("o1", "or-key", "", "sk-or-test"))
	g, err := groq.TryAcquire(AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := openrouter.TryAcquire(AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Release()
	defer o.Release()
	if g.KeyID() != "g1" || o.KeyID() != "o1" {
		t.Fatalf("pools interfered: groq=%s openrouter=%s", g.KeyID(), o.KeyID())
	}
}

func TestAutoScopeMergeUnifiesWindows(t *testing.T) {
	p, c := fakePool(ScopeAuto, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	first, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 10, Allow: func(id KeyID) bool { return id == "1" }})
	if err != nil {
		t.Fatal(err)
	}
	first.RateLimited(30*time.Second, true, nil)
	c.Advance(time.Second)
	second, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 20, Allow: func(id KeyID) bool { return id == "2" }})
	if err != nil {
		t.Fatal(err)
	}
	second.RateLimited(30*time.Second, true, nil)
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.merged {
		t.Fatal("scope was not merged")
	}
	left := p.windows(p.byID["1"], "m")
	right := p.windows(p.byID["2"], "m")
	if left != right {
		t.Fatal("merged keys do not share the same windows")
	}
	if got := left.tpm.Used(c.Now()); got != 30 {
		t.Fatalf("merged TPM usage = %d, want 30", got)
	}
}

func TestHalfOpenProbeIsNotClaimedWhenOtherLimitsBlock(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{PerModel: map[string]limits.ModelLimits{"m": {RPM: ip(1)}}}, ks("1", "k1", "", "a"))
	l, err := p.TryAcquire(AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	l.Success(1, nil)
	for i, advance := range []time.Duration{3 * time.Second, 5 * time.Second, 9 * time.Second} {
		f, e := p.TryAcquire(AcquireRequest{Model: "other"})
		if e != nil {
			t.Fatalf("failure %d: %v", i, e)
		}
		f.Failed()
		c.Advance(advance)
	}
	// O circuito abriu, mas o bucket m ainda esta limitado por RPM. A estimativa
	// e uma tentativa bloqueada nao podem consumir a unica vaga de meia-abertura.
	c.Advance(31 * time.Second)
	if _, ok := p.EstimateWait(AcquireRequest{Model: "m"}); !ok {
		t.Fatal("estimate should remain possible after circuit reset")
	}
	if _, err := p.TryAcquire(AcquireRequest{Model: "m"}); err == nil {
		t.Fatal("limited bucket unexpectedly allowed a request")
	}
	// A primeira chamada elegivel em outro bucket adquire a sonda; a segunda e recusada.
	probe, err := p.TryAcquire(AcquireRequest{Model: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.TryAcquire(AcquireRequest{Model: "other"}); err == nil {
		t.Fatal("second half-open probe unexpectedly admitted")
	}
	probe.Success(1, nil)
}

func TestQuotaExhaustedOnPaidKeepsFreeUsable(t *testing.T) {
	ls := limits.Limits{Rules: []limits.Rule{{Match: "free-model", Free: true, Bucket: "free"}}}
	p, _ := fakePool(ScopeKey, ls, ks("1", "k1", "", "a"))
	paid, err := p.TryAcquire(AcquireRequest{Model: "paid-model"})
	if err != nil {
		t.Fatal(err)
	}
	paid.QuotaExhausted()
	free, err := p.TryAcquire(AcquireRequest{Model: "free-model"})
	if err != nil {
		t.Fatalf("paid quota exhaustion blocked free bucket: %v", err)
	}
	free.Release()
}

func TestGroupScopeSharesWindows(t *testing.T) {
	p, _ := fakePool(ScopeGroup, limits.Limits{Default: limits.ModelLimits{RPM: ip(1)}}, ks("1", "k1", "org-a", "a"), ks("2", "k2", "org-a", "b"))
	first, err := p.TryAcquire(AcquireRequest{Model: "m", Allow: func(id KeyID) bool { return id == "1" }})
	if err != nil {
		t.Fatal(err)
	}
	first.Success(1, nil)
	if _, err = p.TryAcquire(AcquireRequest{Model: "m", Allow: func(id KeyID) bool { return id == "2" }}); err == nil {
		t.Fatal("keys in same account group did not share RPM")
	}
}

func TestSetKeysPreservesStateByID(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{RPM: ip(1)}}, ks("1", "k1", "", "a"))
	first, err := p.TryAcquire(AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	first.Success(1, nil)
	p.SetKeys([]KeySpec{ks("1", "k1-renamed", "", "a")})
	if _, err = p.TryAcquire(AcquireRequest{Model: "m"}); err == nil {
		t.Fatal("SetKeys did not preserve the window for an unchanged KeyID/secret")
	}
}

func TestHeadroomStrategyPrefersMoreCapacity(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{Default: limits.ModelLimits{TPM: ip(100)}}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	p.cfg.Strategy = Headroom
	used, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 80, Allow: func(id KeyID) bool { return id == "1" }})
	if err != nil {
		t.Fatal(err)
	}
	used.Success(80, nil)
	p.mu.Lock()
	p.cursors["m"] = 0
	p.mu.Unlock()
	chosen, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer chosen.Release()
	if chosen.KeyID() != "2" {
		t.Fatalf("headroom chose %s, want key 2", chosen.KeyID())
	}
}

func TestLatencyStrategyPrefersFasterOfTwoChoices(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"), ks("2", "k2", "", "b"))
	p.cfg.Strategy = Latency
	slow, err := p.TryAcquire(AcquireRequest{Model: "m", Allow: func(id KeyID) bool { return id == "1" }})
	if err != nil {
		t.Fatal(err)
	}
	slow.RecordTTFB(200 * time.Millisecond)
	c.Advance(100 * time.Millisecond) // total latency menor, mas TTFB maior
	slow.Success(1, nil)
	fast, err := p.TryAcquire(AcquireRequest{Model: "m", Allow: func(id KeyID) bool { return id == "2" }})
	if err != nil {
		t.Fatal(err)
	}
	fast.RecordTTFB(100 * time.Millisecond)
	c.Advance(300 * time.Millisecond)
	fast.Success(1, nil)
	chosen, err := p.TryAcquire(AcquireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer chosen.Release()
	if chosen.KeyID() != "2" {
		t.Fatalf("latency strategy chose %s, want faster key 2", chosen.KeyID())
	}
}

func TestForbiddenBlocksOnlyModelThenInvalidatesAfterSecondModel(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	first, err := p.TryAcquire(AcquireRequest{Model: "model-a", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	first.Forbidden()
	if _, err = p.TryAcquire(AcquireRequest{Model: "model-a", Tokens: 1}); err == nil {
		t.Fatal("403 should block the same key/model pair")
	}
	second, err := p.TryAcquire(AcquireRequest{Model: "model-b", Tokens: 1})
	if err != nil {
		t.Fatalf("one forbidden model must not disable the key: %v", err)
	}
	second.Forbidden()
	if _, err = p.TryAcquire(AcquireRequest{Model: "model-c", Tokens: 1}); err == nil {
		t.Fatal("two forbidden models should invalidate the key")
	}
	c.Advance(2 * time.Hour)
}

func TestLongRateLimitMarksSharedScopeAtGroupLevel(t *testing.T) {
	p, _ := fakePool(ScopeGroup, limits.Limits{}, ks("1", "k1", "acct", "a"), ks("2", "k2", "acct", "b"))
	l, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.RateLimited(3*time.Minute, true, nil)
	until, ok := p.LongLimited(AcquireRequest{Model: "m", Tokens: 1})
	if !ok || until.IsZero() {
		t.Fatalf("shared group was not marked long-limited: %s %v", until, ok)
	}
	if _, err = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); err == nil {
		t.Fatal("shared group must keep all keys in cooldown")
	}
}

func TestRateLimitedHalfOpenDoesNotLeaveProbing(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	for i := 0; i < 3; i++ {
		lease, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		lease.Failed()
		c.Advance(31 * time.Second)
	}
	lease, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stats()[0].Health; got != "probing" {
		t.Fatalf("half-open health=%q, want probing", got)
	}
	lease.RateLimited(0, false, nil)
	if got := p.Stats()[0].Health; got != "healthy" {
		t.Fatalf("429 left circuit in %q, want healthy", got)
	}
	if _, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); err == nil {
		t.Fatal("rate-limit cooldown should still prevent immediate real traffic")
	}
}

func TestWarmProbeReservationBlocksRealTraffic(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	before, _, _, _, _ := p.KeyActivity("1")
	release, ok := p.BeginProbe("1", time.Second, false, false)
	if !ok {
		t.Fatal("idle key should be reservable")
	}
	if _, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); err == nil {
		t.Fatal("real traffic acquired a key during a warm probe")
	} else {
		var capacity *ErrNoCapacity
		if !errors.As(err, &capacity) {
			t.Fatalf("unexpected acquire error: %v", err)
		}
	}
	warmLease, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 0, Warm: true, Allow: func(id KeyID) bool { return id == "1" }})
	if err != nil {
		t.Fatalf("warm accounting lease should be allowed: %v", err)
	}
	warmLease.MarkWarm()
	warmLease.Success(0, nil)
	after, _, _, _, _ := p.KeyActivity("1")
	if !after.Equal(before) {
		t.Fatalf("warm lease changed real activity timestamp: before=%v after=%v", before, after)
	}
	release()
	if _, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1}); err != nil {
		t.Fatalf("real traffic did not resume after warm probe: %v", err)
	}
}

func TestWarmProbeReservationCannotBypassRealInflight(t *testing.T) {
	p, _ := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "a"))
	lease, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if release, ok := p.BeginProbe("1", time.Second, false, true); ok {
		release()
		t.Fatal("forced warm probe bypassed real inflight traffic")
	}
}

func TestHealthDegradesBeforeCircuitOpensAndBackoffIsExponential(t *testing.T) {
	p, clock := fakePool(ScopeKey, limits.Limits{}, ks("1", "k1", "", "secret"))
	for i := 1; i <= 2; i++ {
		lease, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
		if err != nil {
			t.Fatal(err)
		}
		lease.Failed()
		if got := p.Stats()[0].Health; got != "degraded" {
			t.Fatalf("after failure %d health=%q, want degraded", i, got)
		}
		clock.Advance(time.Duration(1<<uint(i-1))*time.Second + time.Second)
	}
	lease, err := p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	lease.Failed()
	for attempt, want := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 480 * time.Second, 10 * time.Minute, 10 * time.Minute} {
		got := p.ProbeRetryAfter("1", time.Second)
		if got != want {
			t.Fatalf("circuit backoff attempt %d=%s, want %s", attempt+1, got, want)
		}
		clock.Advance(got + time.Second)
		lease, err = p.TryAcquire(AcquireRequest{Model: "m", Tokens: 1})
		if err != nil {
			t.Fatalf("half-open attempt %d: %v", attempt+1, err)
		}
		lease.Failed()
	}
}

func TestModelAllowedExpiresFixedCatalogButKeepsValidatedDynamicAlias(t *testing.T) {
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	p := NewPool(PoolConfig{Provider: "groq", Clock: c, CatalogTTL: time.Hour, Limits: limits.Limits{}})
	p.SetKeys([]KeySpec{ks("catalog-key", "catalog", "", "secret")})
	p.SetCatalogAt("catalog-key", []string{"model-a"}, c.Now())
	p.RecordProbe("catalog-key", true, false, c.Now(), "ok")
	if !p.ModelAllowed("catalog-key", "model-a", false) {
		t.Fatal("fresh catalog should allow listed model")
	}
	c.Advance(time.Hour + time.Second)
	if p.ModelAllowed("catalog-key", "model-a", false) {
		t.Fatal("expired catalog must not authorize a fixed model")
	}
	if !p.ModelAllowed("catalog-key", "dynamic-alias", true) {
		t.Fatal("validated key should continue allowing dynamic aliases after catalog expiry")
	}
}

func TestRestoreStartupInvalidatesCredentialDerivedStateButKeepsUsageWindows(t *testing.T) {
	p, c := fakePool(ScopeKey, limits.Limits{}, ks("startup-key", "startup", "", "secret"))
	p.SetCatalogAt("startup-key", []string{"model-a"}, c.Now())
	p.RecordProbe("startup-key", true, false, c.Now(), "ok")
	p.RecordConnectionProbe("startup-key", true, c.Now(), "reused")
	before := p.Stats()[0]
	if !before.Validated || !before.CatalogKnown || before.Health != "healthy" {
		t.Fatalf("fixture did not establish credential state: %+v", before)
	}
	snapshot := p.Snapshot()

	restarted, _ := fakePool(ScopeKey, limits.Limits{}, ks("startup-key", "startup", "", "secret"))
	restarted.RestoreStartup(snapshot)
	after := restarted.Stats()[0]
	if after.Validated || after.CatalogKnown || after.CatalogModels != 0 || after.Health != "unknown" || !after.LastOK.IsZero() {
		t.Fatalf("startup retained stale credential-derived state: %+v", after)
	}
	if len(after.TTFBEWMA) != 0 || len(after.TotalEWMA) != 0 {
		t.Fatalf("startup retained credential-derived latency metrics: %+v", after)
	}
	if after.UsedTotal != before.UsedTotal {
		t.Fatalf("usage history lost across startup: before=%d after=%d", before.UsedTotal, after.UsedTotal)
	}
}
