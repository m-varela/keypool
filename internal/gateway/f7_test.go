package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func f7GatewayConfig(t *testing.T, upURL string, allowLease bool, budget, maxInflight int, maxWait ...string) (*config.Snapshot, *router.Router) {
	t.Helper()
	llmKey := sched.KeySpec{ID: "llm-key", Label: "llm-main", Secret: secret.New("sk-llm-test-abcdefghijklmnopqrstuvwxyz"), Tags: []string{"proj"}}
	genericKey := sched.KeySpec{ID: "generic-key", Label: "search-main", Secret: secret.New("generic-secret-abcdefghijklmnopqrstuvwxyz"), Tags: []string{"proj"}}
	policy := config.Policy{DailyTokenBudget: budget, MaxInflight: maxInflight}
	if len(maxWait) > 0 {
		policy.MaxWait = maxWait[0]
	}
	raw := &config.Raw{
		APIKey: "kp_f7_test", DefaultProject: "project-a",
		Providers: map[string]config.ProviderRaw{
			"llm":    {Type: "openai_compatible", BaseURL: upURL, Auth: provider.AuthSpec{Scheme: "bearer"}, Paths: provider.PathsSpec{Chat: "/chat", Models: "/models"}, Probe: provider.ProbeSpec{Kind: "none"}, Keys: []sched.KeySpec{llmKey}},
			"search": {Type: "generic_http", BaseURL: upURL + "/base", Auth: provider.AuthSpec{Scheme: "bearer"}, Probe: provider.ProbeSpec{Kind: "none"}, Keys: []sched.KeySpec{genericKey}},
		},
		Projects: map[string]config.ProjectRaw{
			"project-a": {
				Project: "project-a", AllowLease: allowLease,
				Policy: policy,
				Keys: map[string]config.KeyRule{
					"llm":    {Include: config.Selector{KeyIDs: []string{"llm-key"}}},
					"search": {Include: config.Selector{KeyIDs: []string{"generic-key"}}},
				},
				Routes: map[string]config.RouteRaw{"chat": {Priority: 2, Chain: []string{"llm:model-a"}}},
			},
		},
	}
	snap, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := router.New(snap)
	if err != nil {
		t.Fatal(err)
	}
	return snap, rt
}

func f7HTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/base/v1/search":
			if r.Method != http.MethodPost {
				t.Errorf("passthrough method=%s", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer generic-secret-abcdefghijklmnopqrstuvwxyz" {
				t.Errorf("upstream auth=%q", got)
			}
			if r.Header.Get("X-Keypool-Project") != "" {
				t.Errorf("internal project header leaked to upstream")
			}
			if r.Header.Get("X-Private-Hop") != "" {
				t.Errorf("Connection-nominated hop header leaked to upstream")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"q":"abc"}` {
				t.Errorf("passthrough body=%s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.URL.Path == "/chat":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7},"choices":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func f7Request(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kp_f7_test")
	req.Header.Set("X-Keypool-Project", "project-a")
	return req
}

func TestF7LeaseCreateReportAuditsAndSettlesBudget(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	snap, rt := f7GatewayConfig(t, up.URL, true, 1000, 0)
	g := New(snap, rt)
	var events []sched.Event
	g.SetEventSink(func(e sched.Event) { events = append(events, e) })
	var usage []domain.UsageEvent
	g.SetUsageSink(func(e domain.UsageEvent) { usage = append(usage, e) })
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, err := http.DefaultClient.Do(f7Request(t, http.MethodPost, srv.URL+"/v1/leases", `{"target":"llm:model-a","est_tokens":100,"priority":2,"timeout_ms":500,"ttl_s":30}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status=%d body=%s", resp.StatusCode, b)
	}
	var created struct {
		LeaseID    string `json:"lease_id"`
		Secret     string `json:"secret"`
		AuthHeader string `json:"auth_header"`
		Expires    string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.LeaseID, "ls_") || created.Secret != "sk-llm-test-abcdefghijklmnopqrstuvwxyz" || created.AuthHeader != "Authorization" || created.Expires == "" {
		t.Fatalf("unexpected lease response: %+v", created)
	}
	used, reserved := rt.ProjectDailyUsage("project-a")
	if used != 0 || reserved != 100 {
		t.Fatalf("before report used=%d reserved=%d", used, reserved)
	}
	report := f7Request(t, http.MethodPost, srv.URL+"/v1/leases/"+created.LeaseID+"/report", `{"outcome":"success","tokens":12,"rate_headers":{"retry-after":"30"}}`)
	rr, err := http.DefaultClient.Do(report)
	if err != nil {
		t.Fatal(err)
	}
	defer rr.Body.Close()
	if rr.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(rr.Body)
		t.Fatalf("report status=%d body=%s", rr.StatusCode, b)
	}
	used, reserved = rt.ProjectDailyUsage("project-a")
	if used != 12 || reserved != 0 {
		t.Fatalf("after report used=%d reserved=%d", used, reserved)
	}
	if len(events) != 2 || events[0].Kind != "lease.created" || events[1].Kind != "lease.reported" {
		t.Fatalf("events=%+v", events)
	}
	if len(usage) != 1 || usage[0].Source != "lease" || usage[0].ActualTokens != 12 {
		t.Fatalf("usage=%+v", usage)
	}
	again, err := http.DefaultClient.Do(f7Request(t, http.MethodPost, srv.URL+"/v1/leases/"+created.LeaseID+"/report", `{"outcome":"success","tokens":12}`))
	if err != nil {
		t.Fatal(err)
	}
	defer again.Body.Close()
	if again.StatusCode != http.StatusNotFound {
		t.Fatalf("duplicate report status=%d", again.StatusCode)
	}
}

func TestF7ProjectMaxWaitAlsoBoundsInflightSlot(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	_, rt := f7GatewayConfig(t, up.URL, true, 1000, 1, "25ms")
	first, err := rt.AcquireLease(context.Background(), "project-a", "llm:model-a", 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Complete("cancel", 0, nil, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err = rt.AcquireLease(ctx, "project-a", "llm:model-a", 10, 2)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire error=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("max_wait was not applied to max_inflight admission: %v", elapsed)
	}
}

func TestF7LeaseRequiresAllowLease(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	snap, rt := f7GatewayConfig(t, up.URL, false, 1000, 0)
	srv := httptest.NewServer(New(snap, rt).Handler())
	defer srv.Close()
	resp, err := http.DefaultClient.Do(f7Request(t, http.MethodPost, srv.URL+"/v1/leases", `{"target":"llm:model-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
}

func TestF7LeaseWatchdogChargesEstimateAndReleasesInflight(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	snap, rt := f7GatewayConfig(t, up.URL, true, 1000, 1)
	fake := clock.NewFake(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	g := NewWithClock(snap, rt, fake)
	eventCh := make(chan sched.Event, 4)
	g.SetEventSink(func(e sched.Event) { eventCh <- e })
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, err := http.DefaultClient.Do(f7Request(t, http.MethodPost, srv.URL+"/v1/leases", `{"target":"llm:model-a","est_tokens":25,"ttl_s":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var created struct {
		ID string `json:"lease_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	fake.Advance(time.Second)
	events := make([]sched.Event, 0, 2)
	for len(events) < 2 {
		select {
		case event := <-eventCh:
			events = append(events, event)
		case <-time.After(time.Second): // watchdog real apenas limita o tempo de falha do teste
			t.Fatalf("watchdog events not delivered: %+v", events)
		}
	}
	g.leaseMu.Lock()
	remaining := len(g.leases)
	g.leaseMu.Unlock()
	if remaining != 0 {
		t.Fatal("lease TTL watchdog did not expire")
	}
	used, reserved := rt.ProjectDailyUsage("project-a")
	if used != 25 || reserved != 0 {
		t.Fatalf("watchdog usage=%d reserved=%d", used, reserved)
	}
	if events[0].Kind != "lease.created" || events[1].Kind != "lease.leaked" {
		t.Fatalf("events=%+v", events)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	next, err := rt.AcquireLease(ctx, "project-a", "llm:model-a", 10, 2)
	if err != nil {
		t.Fatalf("project inflight slot not released: %v", err)
	}
	next.Complete("cancel", 0, nil, fake.Now())
}

func TestF7CloseLeasesFinalizesOutstandingReservations(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	snap, rt := f7GatewayConfig(t, up.URL, true, 1000, 1)
	g := New(snap, rt)
	var usage []domain.UsageEvent
	g.SetUsageSink(func(e domain.UsageEvent) { usage = append(usage, e) })
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, err := http.DefaultClient.Do(f7Request(t, http.MethodPost, srv.URL+"/v1/leases", `{"target":"llm:model-a","est_tokens":31,"ttl_s":120}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("lease create status=%d body=%s", resp.StatusCode, b)
	}
	g.CloseLeases()
	if used, reserved := rt.ProjectDailyUsage("project-a"); used != 31 || reserved != 0 {
		t.Fatalf("shutdown did not settle reservation: used=%d reserved=%d", used, reserved)
	}
	if len(usage) != 1 || usage[0].Outcome != "leaked" || usage[0].ActualTokens != 31 {
		t.Fatalf("shutdown usage audit=%+v", usage)
	}
	g.leaseMu.Lock()
	remaining := len(g.leases)
	g.leaseMu.Unlock()
	if remaining != 0 {
		t.Fatalf("leases remain after close: %d", remaining)
	}
}

func TestF7GenericHTTPPassthroughInjectsProviderCredential(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	// Lease permission is deliberately false: passthrough does not expose the secret.
	snap, rt := f7GatewayConfig(t, up.URL, false, 100, 1)
	g := New(snap, rt)
	usageCh := make(chan domain.UsageEvent, 1)
	g.SetUsageSink(func(e domain.UsageEvent) { usageCh <- e })
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	req := f7Request(t, http.MethodPost, srv.URL+"/p/search/v1/search?q=abc", `{"q":"abc"}`)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "X-Private-Hop")
	req.Header.Set("X-Private-Hop", "must-not-forward")
	rr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rr.Body.Close()
	b, _ := io.ReadAll(rr.Body)
	if rr.StatusCode != http.StatusCreated || string(b) != `{"ok":true}` {
		t.Fatalf("status=%d body=%s", rr.StatusCode, b)
	}
	if rr.Header.Get("X-Keypool-Provider") != "search" || rr.Header.Get("X-Keypool-Key") != "search-main" {
		t.Fatalf("missing passthrough metadata: %v", rr.Header)
	}
	select {
	case event := <-usageCh:
		if event.Source != "passthrough" || event.ActualTokens != 1 {
			t.Fatalf("usage=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("passthrough usage audit was not delivered")
	}
	used, reserved := rt.ProjectDailyUsage("project-a")
	if used != 1 || reserved != 0 {
		t.Fatalf("daily budget usage=%d reserved=%d", used, reserved)
	}
	stats := rt.Pools()["search"].Stats()
	if len(stats) != 1 || stats[0].TTFBEWMA["/v1/search"] <= 0 {
		t.Fatalf("passthrough did not feed per-model TTFB: %+v", stats)
	}
}

func TestF7GenericProviderIsNotAChatRoute(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	raw := &config.Raw{APIKey: "kp", Providers: map[string]config.ProviderRaw{"generic": {Type: "generic_http", BaseURL: up.URL, Keys: []sched.KeySpec{{ID: "k", Secret: secret.New("generic-key-abcdefghijklmnopqrstuvwxyz")}}}}, Projects: map[string]config.ProjectRaw{"p": {Project: "p", Keys: map[string]config.KeyRule{"generic": {Include: config.Selector{KeyIDs: []string{"k"}}}}, Routes: map[string]config.RouteRaw{"a": {Chain: []string{"generic:/v1/search"}}}}}}
	if _, err := config.Build(raw); err == nil {
		t.Fatal("generic_http must not be accepted as an OpenAI chat route")
	}
}

func TestF7PassthroughRejectsTraversal(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	_, rt := f7GatewayConfig(t, up.URL, false, 0, 0)
	in, _ := http.NewRequest(http.MethodGet, "http://keypool.invalid/", nil)
	if _, err := rt.Passthrough(context.Background(), "project-a", "search", "../admin", in, 2); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestF7PassthroughClientErrorStillCountsConsumedRequest(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	_, rt := f7GatewayConfig(t, up.URL, false, 0, 0)
	in, _ := http.NewRequest(http.MethodGet, "http://keypool.invalid/v1/missing", nil)
	response, err := rt.Passthrough(context.Background(), "project-a", "search", "v1/missing", in, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Close()
	if response.Response.StatusCode != http.StatusNotFound {
		t.Fatalf("upstream status=%d, want 404", response.Response.StatusCode)
	}
	response.Finish("not_found", 1, time.Now())
	stats := rt.Pools()["search"].Stats()
	if len(stats) != 1 || stats[0].UsedTotal != 1 {
		t.Fatalf("upstream 404 should count as a real request: %+v", stats)
	}
}

func TestF7LeaseReportRateLimitDoesNotFailCircuit(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	_, rt := f7GatewayConfig(t, up.URL, true, 1000, 0)
	grant, err := rt.AcquireLease(context.Background(), "project-a", "llm:model-a", 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{"Retry-After": []string{"30"}}
	grant.Complete("rate_limited", 0, h, time.Now())
	stats := rt.Pools()["llm"].Stats()
	if len(stats) != 1 || stats[0].Health == "degraded" || stats[0].Health == "invalid" {
		t.Fatalf("429 changed key health: %+v", stats)
	}
}

func TestF7DailyBudgetReservesAndSettles(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	_, rt := f7GatewayConfig(t, up.URL, true, 10, 0)
	first, err := rt.AcquireLease(context.Background(), "project-a", "llm:model-a", 6, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rt.AcquireLease(context.Background(), "project-a", "llm:model-a", 5, 2); err == nil {
		t.Fatal("second lease should exceed reserved daily budget")
	}
	first.Complete("success", 4, nil, time.Now())
	used, reserved := rt.ProjectDailyUsage("project-a")
	if used != 4 || reserved != 0 {
		t.Fatalf("used=%d reserved=%d", used, reserved)
	}
	last, err := rt.AcquireLease(context.Background(), "project-a", "llm:model-a", 6, 2)
	if err != nil {
		t.Fatalf("budget should allow exact remaining tokens: %v", err)
	}
	last.Complete("success", 6, nil, time.Now())
	used, reserved = rt.ProjectDailyUsage("project-a")
	if used != 10 || reserved != 0 {
		t.Fatalf("final used=%d reserved=%d", used, reserved)
	}
}

func TestF7PassthroughRequiresProjectKeyPermission(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	llmKey := sched.KeySpec{ID: "llm-key", Secret: secret.New("sk-llm-test-abcdefghijklmnopqrstuvwxyz")}
	searchKey := sched.KeySpec{ID: "search-key", Secret: secret.New("search-test-secret-abcdefghijklmnopqrstuvwxyz")}
	raw := &config.Raw{
		APIKey: "kp_f7_test", DefaultProject: "project-a",
		Providers: map[string]config.ProviderRaw{
			"llm":    {Type: "openai_compatible", BaseURL: up.URL, Auth: provider.AuthSpec{Scheme: "bearer"}, Paths: provider.PathsSpec{Chat: "/chat", Models: "/models"}, Probe: provider.ProbeSpec{Kind: "none"}, Keys: []sched.KeySpec{llmKey}},
			"search": {Type: "generic_http", BaseURL: up.URL, Auth: provider.AuthSpec{Scheme: "bearer"}, Probe: provider.ProbeSpec{Kind: "none"}, Keys: []sched.KeySpec{searchKey}},
		},
		Projects: map[string]config.ProjectRaw{"project-a": {Project: "project-a", Keys: map[string]config.KeyRule{
			"llm":    {Include: config.Selector{KeyIDs: []string{"llm-key"}}},
			"search": {Include: config.Selector{KeyIDs: []string{"not-authorized"}}},
		}, Routes: map[string]config.RouteRaw{"chat": {Chain: []string{"llm:model-a"}}}}},
	}
	snap, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := router.New(snap)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := http.NewRequest(http.MethodGet, "http://keypool.invalid/v1/search", nil)
	if _, err := rt.Passthrough(context.Background(), "project-a", "search", "v1/search", in, 2); !errors.Is(err, router.ErrNoTarget) {
		t.Fatalf("passthrough used a key outside project allowlist: %v", err)
	}
}

func TestF7BudgetErrorIsTyped(t *testing.T) {
	up := f7HTTPServer(t)
	defer up.Close()
	_, rt := f7GatewayConfig(t, up.URL, true, 3, 0)
	grant, err := rt.AcquireLease(context.Background(), "project-a", "llm:model-a", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.AcquireLease(context.Background(), "project-a", "llm:model-a", 1, 2)
	var budget *router.ErrDailyBudgetExceeded
	if !errors.As(err, &budget) {
		t.Fatalf("wanted ErrDailyBudgetExceeded, got %v", err)
	}
	grant.Complete("cancel", 0, nil, time.Now())
}

func TestF7UsageStoreRestoresReportedActualTokens(t *testing.T) {
	st := memstore.New(&config.Raw{})
	err := st.AppendUsage(context.Background(), []domain.UsageEvent{
		{At: time.Now().UTC(), Project: "a", Source: "lease", EstimatedTokens: 100, ActualTokens: 37},
		{At: time.Now().UTC(), Project: "a", Source: "warmup", EstimatedTokens: 1},
		{At: time.Now().UTC(), Project: "a", Source: "request", ActualTokens: 15, Usage: &domain.Usage{PromptTokens: 8, CompletionTokens: 9, TotalTokens: 17}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := st
	// ReadProjectDailyTokens queries a UTC calendar day, not a rolling window.
	// Subtracting an hour made this test read yesterday for runs shortly after 00:00 UTC.
	todayUTC := time.Now().UTC().Truncate(24 * time.Hour)
	totals, err := reader.ReadProjectDailyTokens(context.Background(), todayUTC)
	if err != nil {
		t.Fatal(err)
	}
	if totals["a"] != 52 {
		t.Fatalf("restored total=%d want 52", totals["a"])
	}
}
