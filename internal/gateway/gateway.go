// Package gateway expõe uma API local compatível com OpenAI.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/domain"
	"keypool/internal/metrics"
	"keypool/internal/router"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/sse"
)

const maxBodyBytes = 8 << 20

// Gateway mantém o snapshot em atomic.Pointer; handlers leem somente cópias imutáveis.
type Gateway struct {
	configMu      sync.RWMutex
	snapshot      atomic.Pointer[config.Snapshot]
	router        *router.Router
	ready         atomic.Bool
	readyMu       sync.RWMutex
	readyReason   string
	usageSink     func(domain.UsageEvent)
	eventSink     func(sched.Event)
	adminHandler  http.Handler
	clock         clock.Clock
	metrics       *metrics.Registry
	metricsGather func(*metrics.Registry)
	leaseMu       sync.Mutex
	leases        map[string]*activeLease
}

// New cria o gateway de desenvolvimento.
func New(s *config.Snapshot, r *router.Router) *Gateway {
	return NewWithClock(s, r, clock.Real())
}

// NewWithClock permite testar timestamps e métricas com relógio determinístico.
func NewWithClock(s *config.Snapshot, r *router.Router, clk clock.Clock) *Gateway {
	if clk == nil {
		clk = clock.Real()
	}
	g := &Gateway{router: r, clock: clk, leases: map[string]*activeLease{}}
	g.snapshot.Store(s)
	g.ready.Store(true)
	return g
}

// SetSnapshot publica uma nova configuração já validada.
func (g *Gateway) SetSnapshot(s *config.Snapshot) error {
	if s == nil {
		return errors.New("gateway: snapshot ausente")
	}
	g.configMu.Lock()
	defer g.configMu.Unlock()
	if g.router != nil {
		if err := g.router.SetSnapshot(s); err != nil {
			return err
		}
	}
	g.snapshot.Store(s)
	return nil
}

// SetAdminHandler monta as rotas administrativas sob a autenticação kp_ do gateway.
// Deve ser chamado antes de iniciar o servidor HTTP.
func (g *Gateway) SetAdminHandler(h http.Handler) { g.adminHandler = h }

func (g *Gateway) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := g.authorize(w, r); !ok {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetUsageSink instala uma submissao nao bloqueante de eventos de uso.
// Deve ser chamado antes de iniciar o servidor HTTP.
func (g *Gateway) SetUsageSink(fn func(domain.UsageEvent)) { g.usageSink = fn }

// Handler constrói o ServeMux e os middlewares na ordem definida no plano.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", g.health)
	mux.HandleFunc("GET /readyz", g.readyz)
	mux.HandleFunc("GET /metrics", g.metricsHandler)
	mux.HandleFunc("GET /{$}", g.dashboard)
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	mux.HandleFunc("POST /v1/leases", g.leaseCreate)
	mux.HandleFunc("POST /v1/leases/{id}/report", g.leaseReport)
	mux.HandleFunc("/p/{provider}/{path...}", g.passthrough)
	mux.HandleFunc("GET /v1/models", g.models)
	mux.HandleFunc("GET /v1/capacity", g.capacity)
	if g.adminHandler != nil {
		mux.Handle("/admin/v1/", g.adminAuth(g.adminHandler))
	}
	return g.recover(g.requestID(g.limitBody(mux)))
}
func (g *Gateway) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "erro interno do KeyPool")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (g *Gateway) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := make([]byte, 12)
		if _, e := rand.Read(id); e != nil {
			id = []byte("keypool-local")
		}
		rid := hex.EncodeToString(id)
		w.Header().Set("X-Keypool-Request-Id", rid)
		next.ServeHTTP(w, r.WithContext(contextWithRequestID(r.Context(), rid)))
	})
}
func (g *Gateway) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

type requestIDKey struct{}

func contextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}
func (g *Gateway) authorize(w http.ResponseWriter, r *http.Request) (*config.Snapshot, bool) {
	s := g.snapshot.Load()
	if s == nil {
		writeError(w, http.StatusServiceUnavailable, "keypool_starting", "KeyPool ainda não está pronto")
		return nil, false
	}
	provided := ""
	if v := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(v), "bearer ") {
		provided = strings.TrimSpace(v[7:])
	}
	if provided == "" {
		provided = r.Header.Get("X-API-Key")
	}
	// Basic Auth torna o painel navegavel em um browser comum: usuario "keypool",
	// senha igual a API key. Bearer e X-API-Key continuam sendo os modos preferidos.
	if provided == "" {
		if username, password, ok := r.BasicAuth(); ok && username == "keypool" {
			provided = password
		}
	}
	got := sha256.Sum256([]byte(provided))
	want := s.APIKeyHash()
	ok := subtle.ConstantTimeCompare(got[:], want[:]) == 1 && provided != ""
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="KeyPool local", charset="UTF-8"`)
		writeError(w, http.StatusUnauthorized, "keypool_auth_error", "autenticação inválida")
		return nil, false
	}
	return s, true
}
func (g *Gateway) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
func (g *Gateway) readyz(w http.ResponseWriter, r *http.Request) {
	g.configMu.RLock()
	defer g.configMu.RUnlock()
	if _, ok := g.authorize(w, r); !ok {
		return
	}
	g.readyMu.RLock()
	reason := g.readyReason
	g.readyMu.RUnlock()
	if !g.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting", "reason": reason})
		return
	}
	response := map[string]any{"status": "ready"}
	if reason != "" && reason != "primeira passada L1 concluída" {
		response["warning"] = reason
	}
	writeJSON(w, http.StatusOK, response)
}

// SetReadiness conecta o resultado da primeira passada L1 ao endpoint /readyz.
// Uma prontidão concedida por timeout é exposta com warning, sem ocultar o motivo.
func (g *Gateway) SetReadiness(ready bool, reason string) {
	g.readyMu.Lock()
	g.readyReason = reason
	g.readyMu.Unlock()
	g.ready.Store(ready)
}
func (g *Gateway) project(s *config.Snapshot, r *http.Request) (string, bool) {
	name := r.Header.Get("X-Keypool-Project")
	if name == "" {
		name = s.DefaultProject()
	}
	if name == "" {
		return "", false
	}
	_, ok := s.Project(name)
	return name, ok
}
func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	g.configMu.RLock()
	defer g.configMu.RUnlock()
	s, ok := g.authorize(w, r)
	if !ok {
		return
	}
	project, ok := g.project(s, r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "X-Keypool-Project ausente ou desconhecido")
		return
	}
	body, e := io.ReadAll(r.Body)
	if e != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "corpo JSON inválido ou excede 8 MiB")
		return
	}
	var payload map[string]json.RawMessage
	if e = json.Unmarshal(body, &payload); e != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "corpo JSON inválido")
		return
	}
	var model string
	if json.Unmarshal(payload["model"], &model) != nil || model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "campo model é obrigatório")
		return
	}
	req := &router.Request{Project: project, Model: model, Payload: payload}
	if v := r.Header.Get("X-Keypool-Priority"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 || n > 9 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "X-Keypool-Priority deve estar entre 0 e 9")
			return
		}
		req.Priority = &n
	}
	if v := r.Header.Get("X-Keypool-Timeout-Ms"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "X-Keypool-Timeout-Ms inválido")
			return
		}
		req.Timeout = time.Duration(n) * time.Millisecond
		req.TimeoutSet = true
	}
	if v := r.Header.Get("X-Keypool-Exclude"); v != "" {
		req.Exclude = strings.Split(v, ",")
	}
	if v := r.Header.Get("X-Keypool-Avoid-Weights"); v != "" {
		req.AvoidWeights = strings.Split(v, ",")
	}
	resp, e := g.router.Do(r.Context(), req)
	if e != nil {
		g.recordRequestMetrics(project, "", model, "error", 0, 0, 0)
		g.routeError(w, e)
		return
	}
	if resp == nil {
		g.recordRequestMetrics(project, "", model, "error", 0, 0, 0)
		writeError(w, http.StatusBadGateway, "keypool_upstream_error", "o roteador não retornou resposta")
		return
	}
	providerName, targetModel := splitTarget(resp.Target, model)
	outcome := string(resp.Outcome)
	if outcome == "" {
		outcome = "success"
		if resp.Status >= 400 {
			outcome = "client_error"
		}
	}
	g.recordRequestMetrics(project, providerName, targetModel, outcome, resp.Attempts, time.Duration(resp.QueueMS)*time.Millisecond, resp.TTFB)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Keypool-Target", resp.Target)
	w.Header().Set("X-Keypool-Key", resp.KeyLabel)
	w.Header().Set("X-Keypool-Attempts", strconv.Itoa(resp.Attempts))
	w.Header().Set("X-Keypool-Queue-Ms", strconv.FormatInt(resp.QueueMS, 10))
	w.Header().Set("X-Keypool-Fallback", strconv.FormatBool(resp.Fallback))
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	if resp.Stream != nil {
		w.Header().Del("Content-Length")
		w.WriteHeader(status)
		defer resp.Stream.Close()
		if _, copyErr := io.Copy(gatewayFlushWriter{w}, resp.Stream); copyErr != nil {
			_ = resp.Stream.Close()
		}
		if resp.StreamUsage != nil {
			u := resp.StreamUsage.Wait()
			resp.Outcome = resp.StreamUsage.Outcome()
			if u != nil {
				resp.Usage = &domain.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
			}
		}
		g.recordUsage(r, project, model, resp)
		return
	}
	g.recordUsage(r, project, model, resp)
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

// SetMetrics conecta o registro e uma coleta sob demanda de gauges dos componentes vivos.
func (g *Gateway) SetMetrics(reg *metrics.Registry, gather func(*metrics.Registry)) {
	g.metrics = reg
	g.metricsGather = gather
}

func (g *Gateway) recordRequestMetrics(project, providerName, model, outcome string, attempts int, queue, ttfb time.Duration) {
	if g.metrics == nil {
		return
	}
	labels := map[string]string{"project": project, "provider": providerName, "model": model, "outcome": outcome}
	g.metrics.Inc("keypool_requests_total", labels)
	if attempts > 0 {
		g.metrics.Add("keypool_attempts_total", map[string]string{"project": project, "provider": providerName, "model": model}, float64(attempts))
	}
	g.metrics.Observe("keypool_queue_seconds", map[string]string{"project": project, "provider": providerName, "model": model}, queue.Seconds())
	if ttfb > 0 {
		g.metrics.Observe("keypool_upstream_ttfb_seconds", map[string]string{"provider": providerName, "model": model}, ttfb.Seconds())
	}
}

func splitTarget(target, fallback string) (string, string) {
	parts := strings.SplitN(target, ":", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", fallback
}

func (g *Gateway) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.authorize(w, r); !ok {
		return
	}
	if g.metrics == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics_unavailable", "métricas não configuradas")
		return
	}
	if g.metricsGather != nil {
		g.metricsGather(g.metrics)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := g.metrics.WritePrometheus(w); err != nil {
		return
	}
}

func (g *Gateway) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if _, ok := g.authorize(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	_, _ = w.Write(metrics.DashboardHTML())
}

type gatewayFlushWriter struct{ w http.ResponseWriter }

func (f gatewayFlushWriter) Write(p []byte) (int, error) {
	n, e := f.w.Write(p)
	if n > 0 {
		_ = http.NewResponseController(f.w).Flush()
	}
	return n, e
}
func (g *Gateway) recordUsage(r *http.Request, project, alias string, resp *router.Response) {
	if g.usageSink == nil || resp == nil {
		return
	}
	providerName, model := "", alias
	if parts := strings.SplitN(resp.Target, ":", 2); len(parts) == 2 {
		providerName, model = parts[0], parts[1]
	}
	outcome := resp.Outcome
	if outcome == "" {
		outcome = domain.Success
		if resp.Status >= 400 {
			outcome = domain.ClientError
		}
	}
	event := domain.UsageEvent{At: g.clock.Now().UTC(), RequestID: requestIDFromContext(r.Context()), Project: project, Alias: alias,
		Provider: providerName, Model: model, KeyID: string(resp.KeyID), Outcome: string(outcome), Status: resp.Status, Attempt: resp.Attempts, QueueMS: int(resp.QueueMS)}
	if resp.Usage != nil {
		event.Usage = resp.Usage
	}
	var usage struct {
		Usage *domain.Usage `json:"usage"`
	}
	if event.Usage == nil && json.Unmarshal(resp.Body, &usage) == nil {
		event.Usage = usage.Usage
	}
	if event.Usage == nil && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		if u, err := sse.LastUsage(bytes.NewReader(resp.Body)); err == nil && u != nil {
			event.Usage = &domain.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
		}
	}
	g.usageSink(event)
}
func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey{}).(string)
	return v
}

func (g *Gateway) routeError(w http.ResponseWriter, e error) {
	var capErr *sched.ErrNoCapacity
	var outrank *sched.ErrOutranked
	switch {
	case errors.Is(e, router.ErrProjectForbidden), errors.Is(e, router.ErrLeaseForbidden):
		writeError(w, http.StatusForbidden, "keypool_project_forbidden", e.Error())
	case func() bool { var budget *router.ErrDailyBudgetExceeded; return errors.As(e, &budget) }():
		var budget *router.ErrDailyBudgetExceeded
		if errors.As(e, &budget) {
			seconds := int64(budget.ResetAt.Sub(g.clock.Now()) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
		writeError(w, http.StatusTooManyRequests, "daily_token_budget_exceeded", e.Error())
	case errors.Is(e, router.ErrUnknownRoute):
		writeError(w, http.StatusNotFound, "keypool_unknown_route", e.Error())
	case errors.Is(e, router.ErrNoTarget):
		writeError(w, http.StatusServiceUnavailable, "keypool_no_target", e.Error())
	case errors.Is(e, router.ErrPromptTooLargeForTPM):
		writeError(w, http.StatusBadRequest, "prompt_too_large_for_tpm", e.Error())
	case errors.As(e, &capErr):
		if capErr.RetryIn >= 0 {
			seconds := int64(capErr.RetryIn / time.Second)
			if capErr.RetryIn%time.Second != 0 {
				seconds++
			}
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
		writeError(w, http.StatusTooManyRequests, "keypool_capacity_exhausted", e.Error())
	case isLongLimited(w, e):
		writeError(w, http.StatusTooManyRequests, "keypool_capacity_exhausted", e.Error())
	case errors.As(e, &outrank):
		writeError(w, http.StatusTooManyRequests, "keypool_capacity_exhausted", e.Error())
	case errors.Is(e, context.DeadlineExceeded):
		writeError(w, http.StatusRequestTimeout, "keypool_timeout", "tempo máximo de espera do KeyPool esgotado")
	default:
		writeError(w, http.StatusBadGateway, "keypool_upstream_error", e.Error())
	}
}
func isLongLimited(w http.ResponseWriter, err error) bool {
	var longErr *router.ErrLongLimited
	if !errors.As(err, &longErr) {
		return false
	}
	seconds := int64(longErr.RetryIn / time.Second)
	if longErr.RetryIn%time.Second != 0 {
		seconds++
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	return true
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	g.configMu.RLock()
	defer g.configMu.RUnlock()
	s, ok := g.authorize(w, r)
	if !ok {
		return
	}
	name, ok := g.project(s, r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "projeto ausente ou desconhecido")
		return
	}
	p, _ := s.Project(name)
	aliases := make([]string, 0, len(p.Routes))
	for alias := range p.Routes {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	data := make([]map[string]string, 0, len(aliases))
	for _, alias := range aliases {
		data = append(data, map[string]string{"id": alias, "object": "model"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
func (g *Gateway) capacity(w http.ResponseWriter, r *http.Request) {
	g.configMu.RLock()
	defer g.configMu.RUnlock()
	s, ok := g.authorize(w, r)
	if !ok {
		return
	}
	name, ok := g.project(s, r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "projeto ausente ou desconhecido")
		return
	}
	p, _ := s.Project(name)
	alias := r.URL.Query().Get("alias")
	pools := g.router.Pools()
	out := map[string]any{}
	for a, route := range p.Routes {
		if alias != "" && alias != a {
			continue
		}
		type providerCap struct {
			allowed map[sched.KeyID]bool
			keys    map[sched.KeyID]sched.KeySpec
			live    map[sched.KeyID]bool
			buckets map[string]int
			scope   sched.Scope
			merged  bool
		}
		by := map[string]*providerCap{}
		for _, t := range route.Chain {
			pv, exists := s.Provider(t.Provider)
			if !exists || !pv.Enabled || !router.SupportsRequires(pv.Spec, t.Model, route.Requires) {
				continue
			}
			pc := by[t.Provider]
			if pc == nil {
				pc = &providerCap{allowed: map[sched.KeyID]bool{}, keys: map[sched.KeyID]sched.KeySpec{}, live: map[sched.KeyID]bool{}, buckets: map[string]int{}, scope: pv.Scope}
				by[t.Provider] = pc
			}
			for _, id := range p.Keys[t.Provider] {
				pc.allowed[id] = true
			}
			for _, k := range pv.Keys {
				pc.keys[k.ID] = k
			}
			resolved := pv.Limits.Resolve(t.Model)
			lane := 1
			if resolved.TPM == nil {
				lane = 2
			}
			pc.buckets[resolved.Bucket] = lane
		}
		liveKeys, buckets, lanes := 0, 0, 0
		for providerName, pc := range by {
			if pool := pools[providerName]; pool != nil {
				pc.merged = pool.Merged()
			}
			stats := map[sched.KeyID]sched.KeyStats{}
			if pool := pools[providerName]; pool != nil {
				for _, st := range pool.Stats() {
					stats[st.ID] = st
				}
			}
			groupSet := map[string]bool{}
			keyCount := 0
			for id := range pc.allowed {
				k, exists := pc.keys[id]
				if !exists {
					continue
				}
				st, has := stats[id]
				if has && (st.Disabled || st.Health == "invalid") {
					continue
				}
				pc.live[id] = true
				keyCount++
				group := k.Group
				if group == "" {
					group = string(id)
				}
				groupSet[group] = true
			}
			liveKeys += keyCount
			buckets += len(pc.buckets)
			factor := keyCount
			if pc.scope == sched.ScopeProvider || (pc.scope == sched.ScopeAuto && pc.merged) {
				factor = 1
			} else if pc.scope == sched.ScopeGroup || pc.scope == sched.ScopeAuto {
				factor = len(groupSet)
			}
			for _, perBucket := range pc.buckets {
				lanes += perBucket * factor
			}
		}
		parallelism := lanes
		if parallelism < 1 {
			parallelism = 1
		}
		out[a] = map[string]any{"live_keys": liveKeys, "buckets": buckets, "lanes": lanes, "parallelism": parallelism}
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": name, "capacity": out})
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	msg = secret.Redact(msg)
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": typ, "message": msg, "code": typ}})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
