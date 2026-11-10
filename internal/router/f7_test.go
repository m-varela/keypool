package router

import (
	"testing"

	"keypool/internal/config"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
)

func TestF7CalibrationEWMAIsPerProviderModelAndClamped(t *testing.T) {
	raw := &config.Raw{
		APIKey: "kp",
		Providers: map[string]config.ProviderRaw{
			"p": {Type: "openai_compatible", BaseURL: "http://127.0.0.1:8000", Auth: provider.AuthSpec{Scheme: "bearer"}, Probe: provider.ProbeSpec{Kind: "none"}, Keys: []sched.KeySpec{{ID: "k", Secret: secret.New("sk-test-key-abcdefghijklmnopqrstuvwxyz")}}},
		},
		Projects: map[string]config.ProjectRaw{
			"a": {Project: "a", Keys: map[string]config.KeyRule{"p": {Include: config.Selector{KeyIDs: []string{"k"}}}}, Routes: map[string]config.RouteRaw{"chat": {Chain: []string{"p:model"}}}},
		},
	}
	snap, err := config.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(snap)
	if err != nil {
		t.Fatal(err)
	}
	r.recordTokenUsage("p", "model", 100, 1000) // sample 40 is clamped to 2; EWMA: 1*0.8+2*0.2
	if got := r.TokenCalibration("p", "model"); got < 1.199999 || got > 1.200001 {
		t.Fatalf("upper EWMA sample clamp factor=%v", got)
	}
	if got := r.TokenCalibration("p", "other"); got != 1 {
		t.Fatalf("calibration leaked across models: %v", got)
	}
	r.ResetTokenCalibration("p", "model")
	r.recordTokenUsage("p", "model", 100, 1) // sample below .5 is clamped to .5; EWMA: 1*.8+.5*.2
	if got := r.TokenCalibration("p", "model"); got < 0.899999 || got > 0.900001 {
		t.Fatalf("lower EWMA sample clamp factor=%v", got)
	}
}
