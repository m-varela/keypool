// Package openaicompat implementa transporte declarativo para APIs de chat OpenAI-compatíveis.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"keypool/internal/classify"
	"keypool/internal/clock"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
)

// Adapter é o adaptador OpenAI-compatível. O relógio pode ser substituído nos testes.
type Adapter struct {
	Clock clock.Clock
}

func (a Adapter) now() time.Time {
	if a.Clock != nil {
		return a.Clock.Now()
	}
	return clock.Real().Now()
}

func (Adapter) Type() string { return "openai_compatible" }

// AdaptPayload adapta nomes de campos e recursos ao modelo solicitado.
func (Adapter) AdaptPayload(s *provider.Spec, model string, payload map[string]json.RawMessage) map[string]json.RawMessage {
	return AdaptPayload(s, model, payload)
}
func init() {
	provider.Register("openai_compatible", func() provider.Adapter { return Adapter{Clock: clock.Real()} })
	provider.Register("groq", func() provider.Adapter { return Adapter{Clock: clock.Real()} })
	provider.Register("openrouter", func() provider.Adapter { return Adapter{Clock: clock.Real()} })
}

// BuildRequest monta uma requisição autenticada; Reveal ocorre exclusivamente neste método.
func (Adapter) BuildRequest(ctx context.Context, s *provider.Spec, key secret.Secret, req *provider.Canonical) (*http.Request, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	payload := AdaptPayload(s, stringValue(req.Payload["model"]), req.Payload)
	for k, v := range s.RequestExtra {
		if _, exists := payload[k]; !exists {
			b, e := json.Marshal(v)
			if e != nil {
				return nil, e
			}
			payload[k] = b
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("provider: codificar payload: %w", err)
	}
	endpoint := strings.TrimRight(s.BaseURL, "/") + s.Paths.Chat
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	switch s.Auth.Scheme {
	case "query":
		q := u.Query()
		q.Set(s.Auth.Query, key.Reveal())
		u.RawQuery = q.Encode()
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range s.Headers {
		r.Header.Set(k, v)
	}
	switch s.Auth.Scheme {
	case "bearer":
		r.Header.Set("Authorization", "Bearer "+key.Reveal())
	case "header":
		r.Header.Set(s.Auth.Header, key.Reveal())
	}
	return r, nil
}
func stringValue(raw json.RawMessage) string { var s string; _ = json.Unmarshal(raw, &s); return s }

// Classify classifica resposta ou falha de transporte e nunca devolve mensagem sem redação.
func (a Adapter) Classify(resp *http.Response, body []byte, err error) classify.Result {
	if err != nil {
		return classify.Classify(0, nil, nil, err, a.now())
	}
	if resp == nil {
		return classify.Classify(0, nil, nil, fmt.Errorf("resposta HTTP ausente"), a.now())
	}
	return classify.Classify(resp.StatusCode, body, resp.Header, nil, a.now())
}

// Hints extrai remaining/reset configurados no preset.
func (a Adapter) Hints(h http.Header, now time.Time) *sched.Hints {
	return parseHints(nil, h, now)
}

// HintsFor aplica os cabeçalhos declarados no preset e entende duration e epoch.
func (a Adapter) HintsFor(s *provider.Spec, h http.Header, now time.Time) *sched.Hints {
	return parseHints(s, h, now)
}

func parseHints(s *provider.Spec, h http.Header, now time.Time) *sched.Hints {
	out := sched.Hints{}
	if h == nil {
		return &out
	}
	parseRemaining := func(v string) (int, bool) {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil && n >= 0
	}
	parseReset := func(v, format string) (time.Time, bool) {
		v = strings.TrimSpace(v)
		if v == "" {
			return time.Time{}, false
		}
		if format == "" || format == "duration" {
			if d, err := time.ParseDuration(v); err == nil {
				return now.Add(d), true
			}
		}
		if format == "seconds" {
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				return now.Add(time.Duration(n * float64(time.Second))), true
			}
		}
		if format == "epoch" || format == "epoch_s" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return time.Unix(n, 0), true
			}
		}
		if format == "epoch_ms" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return time.UnixMilli(n), true
			}
		}
		if format == "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				if n > 1_000_000_000_000 {
					return time.UnixMilli(n), true
				}
				if n > 1_000_000_000 {
					return time.Unix(n, 0), true
				}
			}
		}
		return time.Time{}, false
	}
	if s != nil && len(s.RateHeaders) > 0 {
		for name, spec := range s.RateHeaders {
			if spec.Header == "" {
				continue
			}
			value := h.Get(spec.Header)
			if value == "" {
				continue
			}
			dim := strings.ToLower(strings.TrimSpace(spec.Dimension))
			format := strings.ToLower(spec.Format)
			label := strings.ToLower(name + " " + spec.Header)
			// Presets may declare provider-wide reset/retry headers without a
			// single dimension. Apply those to every local capacity dimension.
			if dim == "" && (strings.Contains(label, "retry") || strings.EqualFold(spec.Header, "Retry-After")) {
				if reset, ok := parseReset(value, format); ok {
					for _, target := range []string{"tpm", "rpm", "rpd", "tpd"} {
						hint := out[target]
						if hint.ResetAt.IsZero() || reset.After(hint.ResetAt) {
							hint.ResetAt = reset
						}
						hint.ObservedAt = now
						out[target] = hint
					}
				}
				continue
			}
			if dim == "" && strings.Contains(label, "reset") {
				if reset, ok := parseReset(value, format); ok {
					for _, target := range []string{"tpm", "rpm", "rpd", "tpd"} {
						hint := out[target]
						if hint.ResetAt.IsZero() || reset.After(hint.ResetAt) {
							hint.ResetAt = reset
						}
						hint.ObservedAt = now
						out[target] = hint
					}
				}
				continue
			}
			if dim == "" {
				continue
			}
			hint := out[dim]
			if strings.Contains(label, "remaining") || format == "integer" || format == "count" {
				if n, ok := parseRemaining(value); ok {
					hint.Remaining, hint.HasRemaining = n, true
					hint.ObservedAt = now
					out[dim] = hint
				}
				continue
			}
			if reset, ok := parseReset(value, format); ok {
				if hint.ResetAt.IsZero() || reset.After(hint.ResetAt) {
					hint.ResetAt = reset
				}
				hint.ObservedAt = now
				out[dim] = hint
			}
		}
	} else {
		for _, item := range []struct {
			name, dim string
			reset     bool
		}{
			{"x-ratelimit-remaining-tokens", "tpm", false}, {"x-ratelimit-remaining-requests", "rpd", false},
			{"x-ratelimit-reset-tokens", "tpm", true}, {"x-ratelimit-reset-requests", "rpd", true},
		} {
			v := strings.TrimSpace(h.Get(item.name))
			if v == "" {
				continue
			}
			hint := out[item.dim]
			if item.reset {
				if reset, ok := parseReset(v, "duration"); ok {
					hint.ResetAt = reset
					hint.ObservedAt = now
					out[item.dim] = hint
				}
			} else if n, ok := parseRemaining(v); ok {
				hint.Remaining = n
				hint.HasRemaining = true
				hint.ObservedAt = now
				out[item.dim] = hint
			}
		}
	}
	if d, ok := classify.RetryAfter(h, now); ok {
		for _, dim := range []string{"tpm", "rpm", "rpd", "tpd"} {
			hint := out[dim]
			if hint.ResetAt.IsZero() || now.Add(d).After(hint.ResetAt) {
				hint.ResetAt = now.Add(d)
			}
			hint.ObservedAt = now
			out[dim] = hint
		}
	}
	return &out
}

// Probe executa uma validação L1 sem chamar completions.
func (a Adapter) Probe(ctx context.Context, c *http.Client, s *provider.Spec, key secret.Secret) provider.ProbeResult {
	kind := s.Probe.Kind
	if kind == "none" || kind == "" {
		return provider.ProbeResult{OK: true, Message: "sonda desativada", Outcome: classify.Success}
	}
	if kind == "key" {
		path := s.Probe.Path
		if path == "" {
			path = "/key"
		}
		req, err := provider.NewAuthenticatedGET(ctx, s, key, path)
		if err != nil {
			return provider.ProbeResult{Message: secret.Redact(err.Error()), Outcome: classify.Transient}
		}
		resp, err := c.Do(req)
		if err != nil {
			return provider.ProbeResult{Message: secret.Redact(err.Error()), Outcome: classify.Transient}
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		result := classify.Classify(resp.StatusCode, body, resp.Header, nil, a.now())
		out := provider.ProbeResult{OK: result.Outcome == classify.Success, Message: result.Message, HTTPStatus: resp.StatusCode, Outcome: result.Outcome, RetryAfter: result.RetryAfter, RetryKnown: result.RetryKnown, Hints: a.HintsFor(s, resp.Header, a.now()), Headers: resp.Header.Clone()}
		if out.OK {
			out.Message = "OK"
			out.AccountInfo = safeAccountInfo(body)
		}
		return out
	}
	models, status, headers, err := a.listModels(ctx, c, s, key)
	if err != nil {
		result := classify.Classify(status, nil, headers, err, a.now())
		return provider.ProbeResult{Message: secret.Redact(err.Error()), HTTPStatus: status, Outcome: result.Outcome, RetryAfter: result.RetryAfter, RetryKnown: result.RetryKnown, Hints: a.HintsFor(s, headers, a.now()), Headers: headers}
	}
	return provider.ProbeResult{OK: true, Message: "OK", Models: models, HTTPStatus: status, Outcome: classify.Success, Hints: a.HintsFor(s, headers, a.now()), Headers: headers}
}

func safeAccountInfo(body []byte) map[string]any {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	data, ok := root["data"].(map[string]any)
	if !ok {
		data = root
	}
	out := map[string]any{}
	for _, key := range []string{"is_free_tier", "is_free", "limit", "limit_remaining", "usage", "rate_limit"} {
		if value, exists := data[key]; exists {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildGet(ctx context.Context, s *provider.Spec, key secret.Secret, path string) (*http.Request, error) {
	return provider.NewAuthenticatedGET(ctx, s, key, path)
}

// ListModels lê o catálogo visível à credencial.
func (a Adapter) ListModels(ctx context.Context, c *http.Client, s *provider.Spec, key secret.Secret) ([]string, error) {
	models, _, _, err := a.listModels(ctx, c, s, key)
	return models, err
}

func (a Adapter) listModels(ctx context.Context, c *http.Client, s *provider.Spec, key secret.Secret) ([]string, int, http.Header, error) {
	req, err := buildGet(ctx, s, key, s.Paths.Models)
	if err != nil {
		return nil, 0, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("provider: listar modelos: %s", secret.Redact(err.Error()))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Clone(), err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		classified := classify.Classify(resp.StatusCode, body, resp.Header, nil, a.now())
		return nil, resp.StatusCode, resp.Header.Clone(), fmt.Errorf("provider: listar modelos HTTP %d: %s", resp.StatusCode, classified.Message)
	}
	var value struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &value); err != nil {
		return nil, resp.StatusCode, resp.Header.Clone(), fmt.Errorf("provider: JSON de modelos inválido: %w", err)
	}
	models := make([]string, 0, len(value.Data))
	seen := map[string]bool{}
	for _, model := range value.Data {
		if model.ID != "" && !seen[model.ID] {
			seen[model.ID] = true
			models = append(models, model.ID)
		}
	}
	return models, resp.StatusCode, resp.Header.Clone(), nil
}

// AdaptPayload adapta campos incompatíveis e remove valores null.
func AdaptPayload(s *provider.Spec, model string, payload map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(payload)+2)
	for k, v := range payload {
		if strings.TrimSpace(string(v)) != "null" {
			out[k] = append(json.RawMessage(nil), v...)
		}
	}
	field := s.Capabilities.MaxTokensField
	if field == "max_completion_tokens" {
		if _, ok := out[field]; !ok {
			if v, yes := out["max_tokens"]; yes {
				out[field] = v
				delete(out, "max_tokens")
			}
		}
	} else if field == "max_tokens" {
		if _, ok := out[field]; !ok {
			if v, yes := out["max_completion_tokens"]; yes {
				out[field] = v
				delete(out, "max_completion_tokens")
			}
		}
	}
	reasoning, has := out["reasoning_effort"]
	supports := false
	for _, p := range s.Capabilities.ReasoningEffortModels {
		if glob(strings.ToLower(p), strings.ToLower(model)) {
			supports = true
			break
		}
	}
	if has {
		if !supports || s.Capabilities.ReasoningStyle == "none" {
			delete(out, "reasoning_effort")
		} else if s.Capabilities.ReasoningStyle == "nested" {
			var obj map[string]json.RawMessage
			if raw, ok := out["reasoning"]; ok {
				_ = json.Unmarshal(raw, &obj)
			}
			if obj == nil {
				obj = map[string]json.RawMessage{}
			}
			obj["effort"] = reasoning
			if b, e := json.Marshal(obj); e == nil {
				out["reasoning"] = b
			}
			delete(out, "reasoning_effort")
		}
	}
	return out
}
func glob(p, s string) bool {
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == s[si]) {
			pi++
			si++
			continue
		}
		if pi < len(p) && p[pi] == '*' {
			star = pi
			mark = si
			pi++
			continue
		}
		if star >= 0 {
			pi = star + 1
			mark++
			si = mark
			continue
		}
		return false
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// RetryAfter expõe o parser comum para testes e adaptadores específicos.
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	return classify.RetryAfter(h, now)
}

// parseInt evita aceitar valores malformados vindos de headers.
func parseInt(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }
