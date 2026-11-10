package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestRegistryWritesPrometheusTextAndEscapesLabels(t *testing.T) {
	reg := New()
	reg.Inc("keypool_requests_total", map[string]string{"project": "alpha,beta", "provider": `p\"x`, "model": "model", "outcome": "success"})
	reg.Observe("keypool_queue_seconds", map[string]string{"project": "alpha"}, 0.02)
	reg.Set("keypool_key_state", map[string]string{"provider": "p", "key": "k", "state": "healthy"}, 1)
	var output bytes.Buffer
	if err := reg.WritePrometheus(&output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{
		"# TYPE keypool_requests_total counter",
		`keypool_requests_total{model="model",outcome="success",project="alpha,beta",provider="p\\\"x"} 1`,
		"keypool_queue_seconds_bucket{le=\"+Inf\",project=\"alpha\"} 1",
		"keypool_queue_seconds_sum{project=\"alpha\"} 0.02",
		"keypool_key_state{key=\"k\",provider=\"p\",state=\"healthy\"} 1",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("saida Prometheus nao contem %q:\n%s", expected, text)
		}
	}
}

func TestResetGaugesDoesNotResetCountersOrHistograms(t *testing.T) {
	reg := New()
	reg.Inc("keypool_requests_total", nil)
	reg.Set("keypool_key_state", map[string]string{"state": "healthy"}, 1)
	reg.ResetGauges("keypool_key_state")
	var output bytes.Buffer
	if err := reg.WritePrometheus(&output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "keypool_requests_total 1") {
		t.Fatal("counter foi apagado")
	}
	if strings.Contains(output.String(), "keypool_key_state{") {
		t.Fatal("gauge nao foi removido")
	}
}

func TestCountersAndHistogramsRejectNegativeSamples(t *testing.T) {
	r := New()
	r.Inc("keypool_attempts_total", map[string]string{"provider": "p"})
	r.Add("keypool_attempts_total", map[string]string{"provider": "p"}, -5)
	r.Observe("keypool_queue_seconds", nil, -2)
	var out strings.Builder
	if err := r.WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `keypool_attempts_total{provider="p"} 1`) {
		t.Fatalf("counter deixou de ser monotônico: %s", out.String())
	}
	if strings.Contains(out.String(), "keypool_queue_seconds_count 1") {
		t.Fatalf("histograma aceitou duração negativa: %s", out.String())
	}
}
