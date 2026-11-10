package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"keypool/internal/config"
	"keypool/internal/provider"
	"keypool/internal/router"
	"keypool/internal/sched"
	"keypool/internal/secret"
)

// BenchmarkGatewayPassthrough exercises the complete authenticated gateway,
// project admission, scheduler acquisition, credential injection and response
// forwarding path against an in-process upstream.
func BenchmarkGatewayPassthrough(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/search" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer generic-benchmark-secret-abcdefghijklmnopqrstuvwxyz" {
			http.Error(w, "missing provider credential", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	raw := &config.Raw{
		APIKey: "kp_benchmark",
		Projects: map[string]config.ProjectRaw{
			"bench": {Project: "bench", Keys: map[string]config.KeyRule{
				"search": {Include: config.Selector{KeyIDs: []string{"benchmark-key"}}},
				"llm":    {Include: config.Selector{KeyIDs: []string{"llm-key"}}},
			}, Routes: map[string]config.RouteRaw{"chat": {Chain: []string{"llm:model"}}}},
		},
		Providers: map[string]config.ProviderRaw{
			"search": {Type: "generic_http", BaseURL: upstream.URL, Auth: provider.AuthSpec{Scheme: "bearer"}, Probe: provider.ProbeSpec{Kind: "none"}, Keys: []sched.KeySpec{{ID: "benchmark-key", Secret: secret.New("generic-benchmark-secret-abcdefghijklmnopqrstuvwxyz")}}},
			"llm":    {Type: "openai_compatible", BaseURL: upstream.URL, Auth: provider.AuthSpec{Scheme: "bearer"}, Paths: provider.PathsSpec{Chat: "/chat", Models: "/models"}, Probe: provider.ProbeSpec{Kind: "none"}, Keys: []sched.KeySpec{{ID: "llm-key", Secret: secret.New("llm-benchmark-secret-abcdefghijklmnopqrstuvwxyz")}}},
		},
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		b.Fatal(err)
	}
	rt, err := router.New(snapshot)
	if err != nil {
		b.Fatal(err)
	}
	server := httptest.NewServer(New(snapshot, rt).Handler())
	defer server.Close()
	client := server.Client()
	endpoint := server.URL + "/p/search/v1/search"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"q":"bench"}`))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer kp_benchmark")
		req.Header.Set("X-Keypool-Project", "bench")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("unexpected status: %d", resp.StatusCode)
		}
	}
}
