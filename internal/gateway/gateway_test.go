package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"keypool/internal/config"
	"keypool/internal/domain"
	"keypool/internal/metrics"
	"keypool/internal/provider"
	"keypool/internal/router"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/testutil/fakeupstream"
)

func testGateway(t *testing.T) (*Gateway, *fakeupstream.Server) {
	return testGatewayWith(t, []string{"gsk_test_key_abcdefghijklmnopqrstuvwxyz"}, []string{"groq:test-model"})
}

func testGatewayWith(t *testing.T, keyValues, chain []string) (*Gateway, *fakeupstream.Server) {
	return testGatewayWithConcurrency(t, keyValues, chain, nil)
}

func testGatewayWithConcurrency(t *testing.T, keyValues, chain []string, defaultConc *int) (*Gateway, *fakeupstream.Server) {
	t.Helper()
	up := fakeupstream.New()
	keys := make([]sched.KeySpec, 0, len(keyValues))
	for i, value := range keyValues {
		sec := secret.New(value)
		keys = append(keys, sched.KeySpec{ID: sched.KeyID(fmt.Sprintf("groq-%d", i+1)), Label: secret.Mask(value), Secret: sec, Tags: []string{"example"}})
	}
	raw := &config.Raw{
		APIKey:         "kp_local_test",
		DefaultProject: "example",
		Providers: map[string]config.ProviderRaw{
			"groq": {
				Type:    "openai_compatible",
				BaseURL: up.URL() + "/v1",
				Auth:    provider.AuthSpec{Scheme: "bearer"},
				Paths:   provider.PathsSpec{Chat: "/chat/completions", Models: "/models"},
				Limits:  config.RawLimits{DefaultTPM: intPtr(8000), DefaultTPD: intPtr(100000), DefaultConc: defaultConc},
				Keys:    keys,
			},
		},
		Projects: map[string]config.ProjectRaw{
			"example": {
				Project: "example",
				Keys:    map[string]config.KeyRule{"groq": {Include: config.Selector{TagsAny: []string{"example"}}}},
				Routes:  map[string]config.RouteRaw{"prover": {Priority: 2, Chain: chain}},
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
	return New(snap, rt), up
}

func intPtr(v int) *int { return &v }

func TestGatewayModelsListsProjectAliasesInStableOrder(t *testing.T) {
	up := fakeupstream.New()
	defer up.Close()
	raw := &config.Raw{
		APIKey:         "kp_local_test",
		DefaultProject: "example",
		Providers: map[string]config.ProviderRaw{
			"groq": {
				Type:    "openai_compatible",
				BaseURL: up.URL() + "/v1",
				Auth:    provider.AuthSpec{Scheme: "bearer"},
				Paths:   provider.PathsSpec{Chat: "/chat/completions", Models: "/models"},
				Keys:    []sched.KeySpec{{ID: "k1", Secret: secret.New("gsk_test_key_abcdefghijklmnopqrstuvwxyz"), Tags: []string{"example"}}},
			},
		},
		Projects: map[string]config.ProjectRaw{
			"example": {
				Project: "example",
				Keys:    map[string]config.KeyRule{"groq": {Include: config.Selector{TagsAny: []string{"example"}}}},
				Routes: map[string]config.RouteRaw{
					"zeta":  {Priority: 1, Chain: []string{"groq:test-model"}},
					"alpha": {Priority: 1, Chain: []string{"groq:test-model"}},
				},
			},
		},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := router.New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	g := New(snapshot, rt)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("X-Keypool-Project", "example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 2 || payload.Data[0].ID != "alpha" || payload.Data[1].ID != "zeta" {
		t.Fatalf("model aliases are not stable/sorted: %+v", payload.Data)
	}
}

func TestGatewayHealthAndAuthentication(t *testing.T) {
	g, up := testGateway(t)
	defer up.Close()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status=%d", resp.StatusCode)
	}

	bodies := make([]string, 0, 2)
	for _, auth := range []string{"", "Bearer wrong"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/readyz", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("auth status=%d", resp.StatusCode)
		}
		bodies = append(bodies, string(b))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("401 bodies differ: %q != %q", bodies[0], bodies[1])
	}
}

func postChat(t *testing.T, srv *httptest.Server, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Keypool-Project", "example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func TestGatewayChatCompletionEndToEnd(t *testing.T) {
	g, up := testGateway(t)
	defer up.Close()
	up.Enqueue("Bearer gsk_test_key_abcdefghijklmnopqrstuvwxyz", fakeupstream.JSON(200, `{"id":"chatcmpl-local","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	resp, got := postChat(t, srv, `{"model":"prover","messages":[{"role":"user","content":"hello"}],"max_tokens":64}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	if resp.Header.Get("X-Keypool-Target") != "groq:test-model" {
		t.Fatalf("target=%q", resp.Header.Get("X-Keypool-Target"))
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	if up.Requests() != 1 {
		t.Fatalf("upstream calls=%d", up.Requests())
	}
}

func TestGatewayAuthFailureSwitchesKey(t *testing.T) {
	first, second := "gsk_first_abcdefghijklmnopqrstuvwxyz", "gsk_second_abcdefghijklmnopqrstuvwxyz"
	g, up := testGatewayWith(t, []string{first, second}, []string{"groq:test-model"})
	defer up.Close()
	up.Enqueue("Bearer "+first, fakeupstream.JSON(401, `{"error":{"message":"invalid api key"}}`))
	up.Enqueue("Bearer "+second, fakeupstream.JSON(200, `{"choices":[],"usage":{"total_tokens":4}}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, body := postChat(t, srv, `{"model":"prover","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if up.Requests() != 2 {
		t.Fatalf("upstream calls=%d", up.Requests())
	}
	if strings.Contains(string(body), first) {
		t.Fatal("provider key leaked in response")
	}
}

func TestGatewayShortRateLimitSwitchesKey(t *testing.T) {
	first, second := "gsk_first_abcdefghijklmnopqrstuvwxyz", "gsk_second_abcdefghijklmnopqrstuvwxyz"
	g, up := testGatewayWith(t, []string{first, second}, []string{"groq:test-model"})
	defer up.Close()
	limited := fakeupstream.JSON(429, `{"error":{"message":"rate limited"}}`)
	limited.Header.Set("Retry-After", "5")
	up.Enqueue("Bearer "+first, limited)
	up.Enqueue("Bearer "+second, fakeupstream.JSON(200, `{"choices":[],"usage":{"total_tokens":2}}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, body := postChat(t, srv, `{"model":"prover","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if up.Requests() != 2 {
		t.Fatalf("upstream calls=%d", up.Requests())
	}
}

func TestGatewayFallsBackAfterLongRateLimitAndModelUnavailable(t *testing.T) {
	first, second := "gsk_first_abcdefghijklmnopqrstuvwxyz", "gsk_second_abcdefghijklmnopqrstuvwxyz"
	longRateLimited := fakeupstream.JSON(429, `{"error":{"message":"slow down"}}`)
	longRateLimited.Header.Set("Retry-After", "180")
	cases := []struct {
		name  string
		first fakeupstream.Response
	}{
		{name: "long429", first: longRateLimited},
		{name: "model404", first: fakeupstream.JSON(404, `{"error":{"message":"model not found"}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, up := testGatewayWith(t, []string{first, second}, []string{"groq:first-model", "groq:second-model"})
			defer up.Close()
			up.Enqueue("Bearer "+first, tc.first)
			if tc.name == "long429" {
				limitedSecondKey := fakeupstream.JSON(429, `{"error":{"message":"slow down"}}`)
				limitedSecondKey.Header.Set("Retry-After", "180")
				up.Enqueue("Bearer "+second, limitedSecondKey)
			}
			// O fallback usa o mesmo modelo na proxima posicao da cadeia.
			up.Enqueue("Bearer "+first, fakeupstream.JSON(200, `{"choices":[],"usage":{"total_tokens":2}}`))
			srv := httptest.NewServer(g.Handler())
			defer srv.Close()
			resp, body := postChat(t, srv, `{"model":"prover","messages":[]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			if resp.Header.Get("X-Keypool-Fallback") != "true" {
				t.Fatalf("fallback header=%q", resp.Header.Get("X-Keypool-Fallback"))
			}
		})
	}
}

func TestGatewayRetriesFirstSSEErrorEvent(t *testing.T) {
	first, second := "gsk_first_abcdefghijklmnopqrstuvwxyz", "gsk_second_abcdefghijklmnopqrstuvwxyz"
	g, up := testGatewayWith(t, []string{first, second}, []string{"groq:test-model"})
	defer up.Close()
	up.Enqueue("Bearer "+first, fakeupstream.Response{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   []byte("data: {\"error\":{\"message\":\"temporary upstream failure\"}}\n\ndata: [DONE]\n\n"),
	})
	up.Enqueue("Bearer "+second, fakeupstream.Response{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"),
	})
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, body := postChat(t, srv, `{"model":"prover","messages":[],"stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if up.Requests() != 2 {
		t.Fatalf("stream error was not retried: calls=%d", up.Requests())
	}
}

func TestGatewayStreamsSSEIncrementallyAndRecordsFinalUsage(t *testing.T) {
	key := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	g, up := testGateway(t)
	defer up.Close()
	usageCh := make(chan domain.UsageEvent, 1)
	g.SetUsageSink(func(ev domain.UsageEvent) { usageCh <- ev })
	up.Enqueue("Bearer "+key, fakeupstream.Response{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks: [][]byte{
			[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"primeiro\"}}]}\n\n"),
			[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n"),
			[]byte("data: [DONE]\n\n"),
		},
		ChunkDelays: []time.Duration{0, 900 * time.Millisecond, 0},
	})
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"prover","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("X-Keypool-Project", "example")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) >= 700*time.Millisecond {
		t.Fatalf("gateway buffered the SSE stream; first line arrived after %s", time.Since(started))
	}
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first SSE line=%q", line)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("missing DONE event: %q", rest)
	}
	select {
	case event := <-usageCh:
		if event.Usage == nil || event.Usage.TotalTokens != 7 {
			t.Fatalf("recorded usage=%+v", event.Usage)
		}
	case <-time.After(time.Second):
		t.Fatal("usage event not recorded after stream completion")
	}
}

func TestGatewaySpillsToTargetWithImmediateCapacity(t *testing.T) {
	key := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	g, up := testGatewayWith(t, []string{key}, []string{"groq:first-model", "groq:second-model"})
	defer up.Close()
	pool := g.router.Pools()["groq"]
	held, err := pool.TryAcquire(sched.AcquireRequest{Model: "first-model", Tokens: 7600, Priority: 5})
	if err != nil {
		t.Fatalf("hold preferred bucket: %v", err)
	}
	defer held.Release()
	up.Enqueue("Bearer "+key, fakeupstream.JSON(200, `{"choices":[],"usage":{"total_tokens":2}}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, body := postChat(t, srv, `{"model":"prover","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Keypool-Target"); got != "groq:second-model" {
		t.Fatalf("selected target=%q", got)
	}
	if got := resp.Header.Get("X-Keypool-Fallback"); got != "true" {
		t.Fatalf("fallback=%q", got)
	}
}

func TestGatewayExcludeAndAvoidWeightsFilterTargets(t *testing.T) {
	key := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	cases := []struct {
		name, header, value string
	}{
		{name: "exclude", header: "X-Keypool-Exclude", value: "groq:test-a:free"},
		{name: "avoid weight", header: "X-Keypool-Avoid-Weights", value: "test-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, up := testGatewayWith(t, []string{key}, []string{"groq:test-a:free", "groq:test-b:free"})
			defer up.Close()
			up.Enqueue("Bearer "+key, fakeupstream.JSON(http.StatusOK, `{"choices":[]}`))
			srv := httptest.NewServer(g.Handler())
			defer srv.Close()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"prover","messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer kp_local_test")
			req.Header.Set("X-Keypool-Project", "example")
			req.Header.Set(tc.header, tc.value)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, b)
			}
			if got := resp.Header.Get("X-Keypool-Target"); got != "groq:test-b:free" {
				t.Fatalf("selected target=%q", got)
			}
		})
	}
}

func TestGatewayCapacityCountsDistinctAllowedKeysAndBuckets(t *testing.T) {
	g, up := testGatewayWith(t, []string{"gsk_first_abcdefghijklmnopqrstuvwxyz", "gsk_second_abcdefghijklmnopqrstuvwxyz"}, []string{"groq:test-model"})
	defer up.Close()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/capacity?alias=prover", nil)
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("X-Keypool-Project", "example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Capacity map[string]struct {
			LiveKeys int `json:"live_keys"`
			Buckets  int `json:"buckets"`
			Lanes    int `json:"lanes"`
		} `json:"capacity"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	got := result.Capacity["prover"]
	if got.LiveKeys != 2 || got.Buckets != 1 || got.Lanes != 2 {
		t.Fatalf("capacity=%+v", got)
	}
}

func TestGatewaySnapshotReloadKeepsRouterAndAuthInSync(t *testing.T) {
	key := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	g, up := testGateway(t)
	defer up.Close()
	sec := secret.New(key)
	newSnapshot, err := config.Build(&config.Raw{
		APIKey: "kp_rotated", DefaultProject: "example",
		Providers: map[string]config.ProviderRaw{"groq": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Paths: provider.PathsSpec{Chat: "/chat/completions", Models: "/models"}, Limits: config.RawLimits{DefaultTPM: intPtr(8000)}, Keys: []sched.KeySpec{{ID: "groq-1", Label: secret.Mask(key), Secret: sec, Tags: []string{"example"}}}}},
		Projects:  map[string]config.ProjectRaw{"example": {Project: "example", Keys: map[string]config.KeyRule{"groq": {Include: config.Selector{TagsAny: []string{"example"}}}}, Routes: map[string]config.RouteRaw{"prover": {Priority: 2, Chain: []string{"groq:test-model"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.SetSnapshot(newSnapshot); err != nil {
		t.Fatalf("reload: %v", err)
	}
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	for _, auth := range []string{"Bearer kp_local_test", "Bearer kp_rotated"} {
		up.Enqueue("Bearer "+key, fakeupstream.JSON(200, `{"choices":[]}`))
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"prover","messages":[]}`))
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Keypool-Project", "example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if strings.Contains(auth, "rotated") && resp.StatusCode != http.StatusOK {
			t.Fatalf("new key status=%d", resp.StatusCode)
		}
		if strings.Contains(auth, "local_test") && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("old key status=%d", resp.StatusCode)
		}
	}
}

func TestGatewaySnapshotReloadWaitsForActiveSSE(t *testing.T) {
	key := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	g, up := testGateway(t)
	defer up.Close()
	up.Enqueue("Bearer "+key, fakeupstream.Response{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks: [][]byte{
			[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"),
			[]byte("data: {\"choices\":[],\"usage\":{\"total_tokens\":1}}\n\n"),
			[]byte("data: [DONE]\n\n"),
		},
		ChunkDelays: []time.Duration{0, 350 * time.Millisecond, 0},
	})
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"prover","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("X-Keypool-Project", "example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	if _, err = reader.ReadString('\n'); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}

	sec := secret.New(key)
	newSnapshot, err := config.Build(&config.Raw{
		APIKey: "kp_rotated", DefaultProject: "example",
		Providers: map[string]config.ProviderRaw{"groq": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Paths: provider.PathsSpec{Chat: "/chat/completions", Models: "/models"}, Limits: config.RawLimits{DefaultTPM: intPtr(8000)}, Keys: []sched.KeySpec{{ID: "groq-1", Label: secret.Mask(key), Secret: sec, Tags: []string{"example"}}}}},
		Projects:  map[string]config.ProjectRaw{"example": {Project: "example", Keys: map[string]config.KeyRule{"groq": {Include: config.Selector{TagsAny: []string{"example"}}}}, Routes: map[string]config.RouteRaw{"prover": {Priority: 2, Chain: []string{"groq:test-model"}}}}},
	})
	if err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	reload := make(chan error, 1)
	go func() { reload <- g.SetSnapshot(newSnapshot) }()
	select {
	case err = <-reload:
		resp.Body.Close()
		t.Fatalf("snapshot reload returned before SSE completion: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err = io.ReadAll(reader); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case err = <-reload:
		if err != nil {
			t.Fatalf("snapshot reload: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot reload did not proceed after SSE completion")
	}
}

func TestSupportsRequiresUsesProviderModelCapabilities(t *testing.T) {
	spec := provider.Spec{Type: "openai_compatible", Capabilities: provider.CapabilitySpec{ReasoningEffortModels: []string{"openai/gpt-oss-*"}}}
	requires := map[string]bool{"reasoning_effort": true}
	if !router.SupportsRequires(spec, "openai/gpt-oss-120b", requires) {
		t.Fatal("supported model was filtered")
	}
	if router.SupportsRequires(spec, "meta/llama-3.1", requires) {
		t.Fatal("unsupported model passed requires filter")
	}
}

func TestGatewayQuotaExhaustedSwitchesKey(t *testing.T) {
	first, second := "gsk_first_abcdefghijklmnopqrstuvwxyz", "gsk_second_abcdefghijklmnopqrstuvwxyz"
	g, up := testGatewayWith(t, []string{first, second}, []string{"groq:test-model"})
	defer up.Close()
	up.Enqueue("Bearer "+first, fakeupstream.JSON(http.StatusPaymentRequired, `{"error":{"message":"quota exhausted"}}`))
	up.Enqueue("Bearer "+second, fakeupstream.JSON(http.StatusOK, `{"choices":[],"usage":{"total_tokens":3}}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, body := postChat(t, srv, `{"model":"prover","messages":[]}`)
	if resp.StatusCode != http.StatusOK || up.Requests() != 2 {
		t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, up.Requests(), body)
	}
}

func TestGatewayTransientFailureRetriesAnotherKey(t *testing.T) {
	first, second := "gsk_first_abcdefghijklmnopqrstuvwxyz", "gsk_second_abcdefghijklmnopqrstuvwxyz"
	g, up := testGatewayWith(t, []string{first, second}, []string{"groq:test-model"})
	defer up.Close()
	up.Enqueue("Bearer "+first, fakeupstream.JSON(http.StatusBadGateway, `{"error":{"message":"upstream unavailable"}}`))
	up.Enqueue("Bearer "+second, fakeupstream.JSON(http.StatusOK, `{"choices":[],"usage":{"total_tokens":3}}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, body := postChat(t, srv, `{"model":"prover","messages":[]}`)
	if resp.StatusCode != http.StatusOK || up.Requests() != 2 {
		t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, up.Requests(), body)
	}
}

func TestGatewayForbiddenBlocksModelAndTriesAnotherKey(t *testing.T) {
	first, second := "gsk_first_abcdefghijklmnopqrstuvwxyz", "gsk_second_abcdefghijklmnopqrstuvwxyz"
	g, up := testGatewayWith(t, []string{first, second}, []string{"groq:test-model"})
	defer up.Close()
	up.Enqueue("Bearer "+first, fakeupstream.JSON(http.StatusForbidden, `{"error":{"message":"model access is not enabled"}}`))
	up.Enqueue("Bearer "+second, fakeupstream.JSON(http.StatusOK, `{"choices":[],"usage":{"total_tokens":2}}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, body := postChat(t, srv, `{"model":"prover","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Keypool-Key") != secret.Mask(second) {
		t.Fatalf("selected key=%q, expected second key", resp.Header.Get("X-Keypool-Key"))
	}
	if up.Requests() != 2 {
		t.Fatalf("upstream calls=%d, want 2", up.Requests())
	}
}

func TestGatewayTimeoutWinsOverPreviousTransientResponse(t *testing.T) {
	key := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	g, up := testGateway(t)
	defer up.Close()
	up.Enqueue("Bearer "+key, fakeupstream.JSON(http.StatusBadGateway, `{"error":{"message":"temporary failure"}}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"prover","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("X-Keypool-Project", "example")
	req.Header.Set("X-Keypool-Timeout-Ms", "100")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status=%d body=%s, want 408 after retry timeout", resp.StatusCode, b)
	}
}

func TestQueueTimeoutIsSeparateFromOverallRequestTimeout(t *testing.T) {
	key := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	g, up := testGateway(t)
	defer up.Close()
	body := `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	up.Enqueue("Bearer "+key, fakeupstream.Response{
		Status:      http.StatusOK,
		Header:      http.Header{"Content-Type": []string{"application/json"}},
		Chunks:      [][]byte{[]byte(body[:12]), []byte(body[12:])},
		ChunkDelays: []time.Duration{0, 100 * time.Millisecond},
	})
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"prover","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("X-Keypool-Project", "example")
	req.Header.Set("X-Keypool-Queue-Timeout-Ms", "10")
	req.Header.Set("X-Keypool-Timeout-Ms", "1000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s; queue timeout must not cancel upstream generation", resp.StatusCode, b)
	}
}

func TestGatewayReportsQueueTimeoutSeparately(t *testing.T) {
	g, up := testGatewayWithConcurrency(t, []string{"gsk_test_key_abcdefghijklmnopqrstuvwxyz"}, []string{"groq:test-model"}, intPtr(1))
	defer up.Close()
	pool := g.router.Pools()["groq"]
	held, err := pool.TryAcquire(sched.AcquireRequest{Model: "test-model", Tokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"prover","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("X-Keypool-Project", "example")
	req.Header.Set("X-Keypool-Queue-Timeout-Ms", "30")
	req.Header.Set("X-Keypool-Timeout-Ms", "1000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s; want 429 for queue-only timeout", resp.StatusCode, b)
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "keypool_queue_timeout" {
		t.Fatalf("error code=%q body=%s", payload.Error.Code, b)
	}
}

func TestRouteErrorOmitsNegativeRetryAfter(t *testing.T) {
	g, up := testGateway(t)
	defer up.Close()
	w := httptest.NewRecorder()
	g.routeError(w, &sched.ErrNoCapacity{RetryIn: -1})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("invalid negative Retry-After=%q", got)
	}
}

func TestGatewayLong429TriesEveryKeyBeforeMovingOn(t *testing.T) {
	first, second := "gsk_first_abcdefghijklmnopqrstuvwxyz", "gsk_second_abcdefghijklmnopqrstuvwxyz"
	g, up := testGatewayWith(t, []string{first, second}, []string{"groq:test-model"})
	defer up.Close()
	limited1 := fakeupstream.JSON(http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	limited1.Header.Set("Retry-After", "180")
	up.Enqueue("Bearer "+first, limited1)
	up.Enqueue("Bearer "+second, fakeupstream.JSON(http.StatusOK, `{"choices":[],"usage":{"total_tokens":3}}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	resp, body := postChat(t, srv, `{"model":"prover","messages":[]}`)
	if resp.StatusCode != http.StatusOK || up.Requests() != 2 {
		t.Fatalf("long 429 did not test the second key: status=%d calls=%d body=%s", resp.StatusCode, up.Requests(), body)
	}
}

func TestGateway404TargetIsSkippedOnSubsequentRequests(t *testing.T) {
	key := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	g, up := testGatewayWith(t, []string{key}, []string{"groq:first-model", "groq:second-model"})
	defer up.Close()
	up.Enqueue("Bearer "+key, fakeupstream.JSON(http.StatusNotFound, `{"error":{"message":"model not found"}}`))
	up.Enqueue("Bearer "+key, fakeupstream.JSON(http.StatusOK, `{"choices":[]}`))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	firstResp, firstBody := postChat(t, srv, `{"model":"prover","messages":[]}`)
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", firstResp.StatusCode, firstBody)
	}
	up.Enqueue("Bearer "+key, fakeupstream.JSON(http.StatusOK, `{"choices":[]}`))
	secondResp, secondBody := postChat(t, srv, `{"model":"prover","messages":[]}`)
	if secondResp.StatusCode != http.StatusOK {
		t.Fatalf("second request status=%d body=%s", secondResp.StatusCode, secondBody)
	}
	if got := secondResp.Header.Get("X-Keypool-Target"); got != "groq:second-model" {
		t.Fatalf("dead target was retried: target=%q body=%s", got, secondBody)
	}
	if up.Requests() != 3 {
		t.Fatalf("unexpected upstream calls=%d", up.Requests())
	}
}

func TestGatewayPromptTooLargeReturnsBadRequest(t *testing.T) {
	g, up := testGateway(t)
	defer up.Close()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	content := strings.Repeat("x", 34000)
	payload, _ := json.Marshal(map[string]any{"model": "prover", "messages": []any{map[string]string{"role": "user", "content": content}}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("X-Keypool-Project", "example")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	b, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "prompt_too_large_for_tpm") {
		t.Fatalf("status=%d body=%s", resp2.StatusCode, b)
	}
}

func TestGatewayStreamingClientCancellationReleasesLease(t *testing.T) {
	key := "gsk_test_key_abcdefghijklmnopqrstuvwxyz"
	g, up := testGateway(t)
	defer up.Close()
	usageCh := make(chan domain.UsageEvent, 1)
	g.SetUsageSink(func(ev domain.UsageEvent) { usageCh <- ev })
	up.Enqueue("Bearer "+key, fakeupstream.Response{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks: [][]byte{
			[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"),
			[]byte("data: [DONE]\n\n"),
		},
		ChunkDelays: []time.Duration{0, 5 * time.Second},
	})
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"prover","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer kp_local_test")
	req.Header.Set("X-Keypool-Project", "example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	if _, err = reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case event := <-usageCh:
		if event.Outcome != string(domain.Canceled) {
			t.Fatalf("canceled stream outcome=%q", event.Outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("gateway did not complete cleanup after client cancellation")
	}
	stats := g.router.Pools()["groq"].Stats()
	if len(stats) != 1 || stats[0].Inflight != 0 {
		t.Fatalf("client cancellation leaked lease: %+v", stats)
	}
}

func TestAdminRoutesUseGatewayAuthentication(t *testing.T) {
	g, up := testGateway(t)
	defer up.Close()
	g.SetAdminHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	for _, tc := range []struct {
		name, token string
		want        int
	}{{"missing", "", http.StatusUnauthorized}, {"wrong", "kp_wrong", http.StatusUnauthorized}, {"valid", "kp_local_test", http.StatusNoContent}} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/v1/providers", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestMetricsAndDashboardRequireAPIKey(t *testing.T) {
	gw, upstream := testGateway(t)
	defer upstream.Close()
	gw.SetMetrics(metrics.New(), nil)
	h := gw.Handler()
	for _, path := range []string{"/metrics", "/"} {
		unauthorized := httptest.NewRecorder()
		h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, path, nil))
		if unauthorized.Code != http.StatusUnauthorized {
			t.Fatalf("%s sem auth: status=%d", path, unauthorized.Code)
		}
	}
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer kp_local_test")
	metricsResp := httptest.NewRecorder()
	h.ServeHTTP(metricsResp, metricsReq)
	if metricsResp.Code != http.StatusOK {
		t.Fatalf("/metrics status=%d: %s", metricsResp.Code, metricsResp.Body.String())
	}
	if got := metricsResp.Header().Get("Content-Type"); !strings.Contains(got, "text/plain; version=0.0.4") {
		t.Fatalf("content-type=%q", got)
	}
	if !strings.Contains(metricsResp.Body.String(), "# TYPE keypool_requests_total counter") {
		t.Fatal("exposicao Prometheus incompleta")
	}
	dashboardReq := httptest.NewRequest(http.MethodGet, "/", nil)
	dashboardReq.Header.Set("X-API-Key", "kp_local_test")
	dashboardResp := httptest.NewRecorder()
	h.ServeHTTP(dashboardResp, dashboardReq)
	if dashboardResp.Code != http.StatusOK || !strings.Contains(dashboardResp.Body.String(), "somente leitura") {
		t.Fatalf("dashboard: status=%d body=%q", dashboardResp.Code, dashboardResp.Body.String())
	}
	if got := dashboardResp.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("CSP ausente: %q", got)
	}
	basicReq := httptest.NewRequest(http.MethodGet, "/", nil)
	basicReq.SetBasicAuth("keypool", "kp_local_test")
	basicResp := httptest.NewRecorder()
	h.ServeHTTP(basicResp, basicReq)
	if basicResp.Code != http.StatusOK || !strings.Contains(basicResp.Body.String(), "somente leitura") {
		t.Fatalf("dashboard Basic Auth: status=%d body=%q", basicResp.Code, basicResp.Body.String())
	}
	unauthReq := httptest.NewRequest(http.MethodGet, "/", nil)
	unauthResp := httptest.NewRecorder()
	h.ServeHTTP(unauthResp, unauthReq)
	if !strings.Contains(unauthResp.Header().Get("WWW-Authenticate"), "Basic realm=\"KeyPool local\"") {
		t.Fatalf("challenge Basic Auth ausente: %q", unauthResp.Header().Get("WWW-Authenticate"))
	}
}

func TestRequestMetricsDoNotCountUnsentAttempts(t *testing.T) {
	reg := metrics.New()
	gw := &Gateway{metrics: reg}
	gw.recordRequestMetrics("project", "", "model", "error", 0, 0, 0)
	var out strings.Builder
	if err := reg.WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `keypool_requests_total{model="model",outcome="error",project="project",provider=""} 1`) {
		t.Fatalf("request counter missing: %s", out.String())
	}
	if strings.Contains(out.String(), "keypool_attempts_total{") {
		t.Fatalf("unsent attempts counted: %s", out.String())
	}
}
