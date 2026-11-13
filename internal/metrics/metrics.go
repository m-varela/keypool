// Package metrics publica metricas Prometheus sem dependencia externa.
package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type seriesKey struct {
	name   string
	labels string
}

type histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	count  uint64
}

type descriptor struct {
	help     string
	typeName string
}

// Registry e concorrente e nao depende de bibliotecas de instrumentacao.
type Registry struct {
	mu          sync.RWMutex
	counters    map[seriesKey]float64
	gauges      map[seriesKey]float64
	histograms  map[seriesKey]*histogram
	descriptors map[string]descriptor
	bounds      []float64
}

var defaultBounds = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

func New() *Registry {
	r := &Registry{counters: map[seriesKey]float64{}, gauges: map[seriesKey]float64{}, histograms: map[seriesKey]*histogram{}, descriptors: map[string]descriptor{}, bounds: append([]float64(nil), defaultBounds...)}
	r.RegisterCounter("keypool_requests_total", "Total de requisicoes processadas pelo gateway.")
	r.RegisterCounter("keypool_attempts_total", "Total de tentativas enviadas aos provedores.")
	r.RegisterHistogram("keypool_queue_seconds", "Tempo aguardando capacidade no KeyPool.")
	r.RegisterHistogram("keypool_upstream_ttfb_seconds", "Tempo ate o primeiro byte do provedor.")
	r.RegisterGauge("keypool_key_state", "Estado atual da chave (uma serie por estado; um significa ativo).")
	r.RegisterGauge("keypool_window_used_ratio", "Fracao da janela consumida em relacao ao limite configurado ou observado.")
	r.RegisterGauge("keypool_cooldown_seconds", "Segundos restantes de cooldown da chave.")
	r.RegisterGauge("keypool_warm_age_seconds", "Idade em segundos da ultima sonda por chave e camada.")
	r.RegisterCounter("keypool_usage_dropped_total", "Eventos de uso descartados por limite de retenção em memória.")
	r.RegisterCounter("keypool_leases_leaked_total", "Leases expiradas ou abandonadas sem relatorio.")
	r.RegisterCounter("keypool_route_failures_total", "Falhas de rota por fase e codigo estaveis.")
	r.RegisterCounter("keypool_route_attempts_total", "Tentativas upstream preservadas em falhas de rota.")
	r.RegisterCounter("keypool_timeouts_total", "Timeouts por fase queue ou upstream.")
	return r
}

func (r *Registry) register(name, help, kind string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.descriptors[name] = descriptor{help: help, typeName: kind}
}
func (r *Registry) RegisterCounter(name, help string)   { r.register(name, help, "counter") }
func (r *Registry) RegisterGauge(name, help string)     { r.register(name, help, "gauge") }
func (r *Registry) RegisterHistogram(name, help string) { r.register(name, help, "histogram") }

func labelsKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(labels)
	return string(encoded)
}
func (r *Registry) Inc(name string, labels map[string]string) { r.Add(name, labels, 1) }
func (r *Registry) Add(name string, labels map[string]string, value float64) {
	if r == nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return
	}
	k := seriesKey{name: name, labels: labelsKey(labels)}
	r.mu.Lock()
	r.counters[k] += value
	r.mu.Unlock()
}

// SetCounter atualiza um contador refletido de outra fonte monotônica (ex.: fila limitada).
func (r *Registry) SetCounter(name string, labels map[string]string, value float64) {
	if r == nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return
	}
	k := seriesKey{name: name, labels: labelsKey(labels)}
	r.mu.Lock()
	r.counters[k] = value
	r.mu.Unlock()
}

func (r *Registry) Set(name string, labels map[string]string, value float64) {
	if r == nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	k := seriesKey{name: name, labels: labelsKey(labels)}
	r.mu.Lock()
	r.gauges[k] = value
	r.mu.Unlock()
}
func (r *Registry) Observe(name string, labels map[string]string, value float64) {
	if r == nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return
	}
	k := seriesKey{name: name, labels: labelsKey(labels)}
	r.mu.Lock()
	h := r.histograms[k]
	if h == nil {
		h = &histogram{bounds: append([]float64(nil), r.bounds...), counts: make([]uint64, len(r.bounds))}
		r.histograms[k] = h
	}
	h.count++
	h.sum += value
	for i, bound := range h.bounds {
		if value <= bound {
			h.counts[i]++
		}
	}
	r.mu.Unlock()
}

// ResetGauges remove apenas series dinamicas; counters e histogramas sao cumulativos.
func (r *Registry) ResetGauges(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	for k := range r.gauges {
		if name == "" || k.name == name {
			delete(r.gauges, k)
		}
	}
	r.mu.Unlock()
}

func parseLabels(raw string) map[string]string {
	out := map[string]string{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}
func renderLabels(raw, extraName, extraValue string) string {
	labels := parseLabels(raw)
	if extraName != "" {
		labels[extraName] = extraValue
	}
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString("=\"")
		b.WriteString(escape(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// WritePrometheus escreve HELP/TYPE e amostras deterministicas no formato text/plain 0.0.4.
func (r *Registry) WritePrometheus(w io.Writer) error {
	if r == nil {
		return fmt.Errorf("metrics: registry nil")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.descriptors))
	for n := range r.descriptors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		d := r.descriptors[name]
		help := strings.ReplaceAll(d.help, `\`, `\\`)
		help = strings.ReplaceAll(help, "\n", `\n`)
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, d.typeName); err != nil {
			return err
		}
		if d.typeName == "counter" || d.typeName == "gauge" {
			values := r.counters
			if d.typeName == "gauge" {
				values = r.gauges
			}
			keys := make([]seriesKey, 0)
			for k := range values {
				if k.name == name {
					keys = append(keys, k)
				}
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i].labels < keys[j].labels })
			for _, k := range keys {
				if _, err := fmt.Fprintf(w, "%s%s %s\n", name, renderLabels(k.labels, "", ""), strconv.FormatFloat(values[k], 'g', -1, 64)); err != nil {
					return err
				}
			}
			continue
		}
		keys := make([]seriesKey, 0)
		for k := range r.histograms {
			if k.name == name {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].labels < keys[j].labels })
		for _, k := range keys {
			h := r.histograms[k]
			for i, bound := range h.bounds {
				if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, renderLabels(k.labels, "le", strconv.FormatFloat(bound, 'g', -1, 64)), h.counts[i]); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, renderLabels(k.labels, "le", "+Inf"), h.count); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s_sum%s %s\n", name, renderLabels(k.labels, "", ""), strconv.FormatFloat(h.sum, 'g', -1, 64)); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s_count%s %d\n", name, renderLabels(k.labels, "", ""), h.count); err != nil {
				return err
			}
		}
	}
	return nil
}
