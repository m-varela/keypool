package router

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/testutil/fakeupstream"
)

func TestPreferencesKeepOnlyFeasibleTarget(t *testing.T) {
	for _, mode := range []string{"exclude", "avoid", "preferred_fits", "none_fit"} {
		t.Run(mode, func(t *testing.T) {
			up := fakeupstream.New()
			defer up.Close()
			narrowLimit, wideLimit := 100, 8000
			if mode == "preferred_fits" {
				narrowLimit = 8000
			}
			if mode == "none_fit" {
				wideLimit = 100
			}
			raw := &config.Raw{
				APIKey: "kp_test", DefaultProject: "p",
				Providers: map[string]config.ProviderRaw{
					"narrow": {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Limits: config.RawLimits{DefaultTPM: &narrowLimit}, Keys: []sched.KeySpec{{ID: "n", Secret: secret.New("narrow-test")}}},
					"wide":   {Type: "openai_compatible", BaseURL: up.URL() + "/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Limits: config.RawLimits{DefaultTPM: &wideLimit}, Keys: []sched.KeySpec{{ID: "w", Secret: secret.New("wide-test")}}},
				},
				Projects: map[string]config.ProjectRaw{"p": {
					Keys: map[string]config.KeyRule{
						"narrow": {Include: config.Selector{KeyIDs: []string{"n"}}},
						"wide":   {Include: config.Selector{KeyIDs: []string{"w"}}},
					},
					Routes: map[string]config.RouteRaw{"main": {Chain: []string{"wide:large-model", "narrow:small-model"}}},
				}},
			}
			snapshot, err := config.Build(raw)
			if err != nil {
				t.Fatal(err)
			}
			rt, err := NewWithClock(snapshot, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
			if err != nil {
				t.Fatal(err)
			}
			up.Enqueue("Bearer wide-test", fakeupstream.JSON(200, `{"choices":[]}`))
			up.Enqueue("Bearer narrow-test", fakeupstream.JSON(200, `{"choices":[]}`))
			req := &Request{Project: "p", Model: "main", Payload: map[string]json.RawMessage{"model": json.RawMessage(`"main"`), "messages": json.RawMessage(`[]`)}}
			if mode == "avoid" {
				req.AvoidWeights = []string{"large-model"}
			} else {
				req.Exclude = []string{"wide:large-model"}
			}
			resp, err := rt.Do(context.Background(), req)
			if mode == "none_fit" {
				if !errors.Is(err, ErrPromptTooLargeForTPM) {
					t.Fatalf("erro esperado de TPM, recebido %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "wide:large-model"
			if mode == "preferred_fits" {
				want = "narrow:small-model"
			}
			if resp.Target != want {
				t.Fatalf("alvo=%s, esperado=%s", resp.Target, want)
			}
		})
	}
}
