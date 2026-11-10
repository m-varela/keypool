package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"keypool/internal/clock"
	"keypool/internal/domain"
	"keypool/internal/router"
	"keypool/internal/sched"
)

var gatewayHopHeaders = map[string]bool{"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true}

type activeLease struct {
	id, project, target, requestID string
	grant                          *router.DirectLease
	estimate                       int
	expires                        time.Time
	done                           chan struct{}
}

func (g *Gateway) SetEventSink(fn func(sched.Event)) { g.eventSink = fn }

// CloseLeases is called during graceful shutdown. Outstanding direct leases are
// finalized as leaked before usage/event writers are drained, so reservations
// and audit records are not lost when the daemon stops before their TTL.
func (g *Gateway) CloseLeases() {
	if g == nil {
		return
	}
	g.leaseMu.Lock()
	items := make([]*activeLease, 0, len(g.leases))
	for _, item := range g.leases {
		items = append(items, item)
	}
	g.leaseMu.Unlock()
	for _, item := range items {
		g.finishLease(item.project, item.id, "leaked", item.estimate, nil, true)
	}
}

func (g *Gateway) leaseCreate(w http.ResponseWriter, r *http.Request) {
	g.configMu.RLock()
	defer g.configMu.RUnlock()
	s, ok := g.authorize(w, r)
	if !ok {
		return
	}
	project, ok := g.project(s, r)
	if !ok {
		writeError(w, 400, "invalid_request_error", "projeto ausente ou desconhecido")
		return
	}
	var input struct {
		Target    string `json:"target"`
		EstTokens int    `json:"est_tokens"`
		Priority  *int   `json:"priority"`
		TimeoutMS int    `json:"timeout_ms"`
		TTLS      int    `json:"ttl_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, 400, "invalid_request_error", "JSON de lease inválido")
		return
	}
	if strings.TrimSpace(input.Target) == "" {
		writeError(w, 400, "invalid_request_error", "target é obrigatório no formato provider:model")
		return
	}
	if input.EstTokens <= 0 {
		input.EstTokens = 1
	}
	if input.EstTokens > 1_000_000 {
		writeError(w, 400, "invalid_request_error", "est_tokens excede 1000000")
		return
	}
	priority := 2
	if input.Priority != nil {
		priority = *input.Priority
	}
	if priority < 0 || priority > 9 {
		writeError(w, 400, "invalid_request_error", "priority deve estar entre 0 e 9")
		return
	}
	if input.TimeoutMS == 0 {
		input.TimeoutMS = 30000
	}
	if input.TimeoutMS < 1 || input.TimeoutMS > 120000 {
		writeError(w, 400, "invalid_request_error", "timeout_ms deve estar entre 1 e 120000")
		return
	}
	if input.TTLS == 0 {
		input.TTLS = 120
	}
	if input.TTLS < 1 || input.TTLS > 120 {
		writeError(w, 400, "invalid_request_error", "ttl_s deve estar entre 1 e 120")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(input.TimeoutMS)*time.Millisecond)
	defer cancel()
	grant, err := g.router.AcquireLease(ctx, project, input.Target, input.EstTokens, priority)
	if err != nil {
		g.routeError(w, err)
		return
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		grant.Complete("cancel", 0, nil, g.clock.Now())
		writeError(w, 500, "internal_error", "não foi possível criar lease")
		return
	}
	id := "ls_" + hex.EncodeToString(idBytes)
	expires := g.clock.Now().Add(time.Duration(input.TTLS) * time.Second)
	item := &activeLease{id: id, project: project, target: input.Target, requestID: requestIDFromContext(r.Context()), grant: grant, estimate: input.EstTokens, expires: expires, done: make(chan struct{})}
	g.leaseMu.Lock()
	g.leases[id] = item
	g.leaseMu.Unlock()
	g.watchLease(item, time.Duration(input.TTLS)*time.Second)
	authHeader, authQuery := grant.AuthHeader, grant.AuthQuery
	if grant.AuthScheme == "" || grant.AuthScheme == "bearer" {
		authHeader = "Authorization"
		authQuery = ""
	}
	writeJSON(w, http.StatusCreated, map[string]any{"lease_id": id, "provider": grant.Provider, "model": grant.Model, "base_url": grant.BaseURL, "auth_header": authHeader, "auth_query": authQuery, "auth_scheme": grant.AuthScheme, "secret": grant.Secret.Reveal(), "key_label": grant.KeyLabel, "expires_at": expires.UTC().Format(time.RFC3339Nano)})
	g.emitLeaseEvent("lease.created", item, "reserved")
}

func (g *Gateway) watchLease(item *activeLease, ttl time.Duration) {
	if tc, ok := g.clock.(clock.TimerClock); ok {
		timer := tc.NewTimer(ttl)
		go func() {
			select {
			case <-timer.C():
				g.finishLease(item.project, item.id, "leaked", item.estimate, nil, true)
			case <-item.done:
				timer.Stop()
			}
		}()
		return
	}
	go func() {
		timer := time.NewTimer(ttl)
		defer timer.Stop()
		select {
		case <-timer.C:
			g.finishLease(item.project, item.id, "leaked", item.estimate, nil, true)
		case <-item.done:
		}
	}()
}

func (g *Gateway) leaseReport(w http.ResponseWriter, r *http.Request) {
	g.configMu.RLock()
	defer g.configMu.RUnlock()
	s, ok := g.authorize(w, r)
	if !ok {
		return
	}
	project, ok := g.project(s, r)
	if !ok {
		writeError(w, 400, "invalid_request_error", "projeto ausente ou desconhecido")
		return
	}
	id := r.PathValue("id")
	var input struct {
		Outcome     string            `json:"outcome"`
		Tokens      int               `json:"tokens"`
		RateHeaders map[string]string `json:"rate_headers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, 400, "invalid_request_error", "JSON de relatório inválido")
		return
	}
	input.Outcome = strings.ToLower(strings.TrimSpace(input.Outcome))
	switch input.Outcome {
	case "success", "rate_limited", "rate_limited_short", "rate_limited_long", "quota", "quota_exhausted", "auth", "auth_invalid", "forbidden", "transient", "cancel", "canceled":
	default:
		writeError(w, 400, "invalid_request_error", "outcome inválido")
		return
	}
	if input.Tokens < 0 || input.Tokens > 10_000_000 {
		writeError(w, 400, "invalid_request_error", "tokens inválido")
		return
	}
	headers := make(http.Header)
	if len(input.RateHeaders) > 32 {
		writeError(w, 400, "invalid_request_error", "rate_headers excede 32 campos")
		return
	}
	for name, value := range input.RateHeaders {
		if len(name) > 128 || len(value) > 2048 || strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			writeError(w, 400, "invalid_request_error", "rate_headers contém campo inválido")
			return
		}
		if strings.EqualFold(name, "Authorization") || strings.HasPrefix(strings.ToLower(name), "x-keypool-") {
			continue
		}
		headers.Set(name, value)
	}
	if !g.finishLease(project, id, input.Outcome, input.Tokens, headers, false) {
		writeError(w, 404, "lease_not_found", "lease não existe, expirou ou pertence a outro projeto")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "reported", "lease_id": id})
}

func (g *Gateway) finishLease(project, id, outcome string, tokens int, headers http.Header, expired bool) bool {
	g.leaseMu.Lock()
	item := g.leases[id]
	if item == nil || item.project != project {
		g.leaseMu.Unlock()
		return false
	}
	delete(g.leases, id)
	close(item.done)
	g.leaseMu.Unlock()
	charged := tokens
	if charged < 0 {
		charged = 0
	}
	switch strings.ToLower(outcome) {
	case "success", "rate_limited", "rate_limited_short", "rate_limited_long", "transient", "leaked", "timeout":
		if charged == 0 {
			charged = item.estimate
		}
	}
	item.grant.Complete(outcome, tokens, headers, g.clock.Now())
	if expired && g.metrics != nil {
		g.metrics.Inc("keypool_leases_leaked_total", nil)
	}
	kind := "lease.reported"
	if expired {
		kind = "lease.leaked"
	}
	g.emitLeaseEvent(kind, item, outcome)
	if g.usageSink != nil {
		usage := &domain.Usage{TotalTokens: tokens}
		g.usageSink(domain.UsageEvent{At: g.clock.Now().UTC(), RequestID: item.requestID, Project: item.project, Alias: item.target, Provider: item.grant.Provider, Model: item.grant.Model, KeyID: string(item.grant.KeyID), Outcome: outcome, Source: "lease", Status: 200, EstimatedTokens: charged, ActualTokens: charged, Usage: usage})
	}
	return true
}

func (g *Gateway) emitLeaseEvent(kind string, item *activeLease, detail string) {
	if g.eventSink != nil {
		g.eventSink(sched.Event{Kind: kind, Subject: item.target, Detail: fmt.Sprintf("project=%s lease=%s key_id=%s key_label=%s outcome=%s", item.project, item.id, item.grant.KeyID, item.grant.KeyLabel, detail), At: g.clock.Now().UTC()})
	}
}

func (g *Gateway) passthrough(w http.ResponseWriter, r *http.Request) {
	g.configMu.RLock()
	defer g.configMu.RUnlock()
	s, ok := g.authorize(w, r)
	if !ok {
		return
	}
	project, ok := g.project(s, r)
	if !ok {
		writeError(w, 400, "invalid_request_error", "projeto ausente ou desconhecido")
		return
	}
	priority := 2
	if v := r.Header.Get("X-Keypool-Priority"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 9 {
			writeError(w, 400, "invalid_request_error", "X-Keypool-Priority deve estar entre 0 e 9")
			return
		}
		priority = n
	}
	providerName := r.PathValue("provider")
	path := r.PathValue("path")
	started := g.clock.Now()
	resp, err := g.router.Passthrough(r.Context(), project, providerName, path, r, priority)
	if err != nil {
		g.recordRequestMetrics(project, providerName, path, "error", 0, 0, 0)
		g.routeError(w, err)
		return
	}
	defer resp.Close()
	for name, values := range resp.Response.Header {
		if isGatewayHopHeader(name, resp.Response.Header) {
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.Header().Set("X-Keypool-Provider", resp.Provider)
	w.Header().Set("X-Keypool-Key", resp.KeyLabel)
	w.Header().Set("X-Keypool-Path", resp.Path)
	w.WriteHeader(resp.Response.StatusCode)
	_, copyErr := io.Copy(gatewayFlushWriter{w}, resp.Response.Body)
	outcome := passthroughStatusOutcome(resp.Response.StatusCode)
	if copyErr != nil {
		outcome = "transient"
	}
	resp.Finish(outcome, passthroughStatusTokens(resp.Response.StatusCode), g.clock.Now())
	ttfb := resp.TTFB
	if ttfb <= 0 {
		ttfb = g.clock.Now().Sub(started)
	}
	g.recordRequestMetrics(project, providerName, path, outcome, 1, 0, ttfb)
	if g.usageSink != nil {
		status := resp.Response.StatusCode
		charged := 0
		if status > 0 {
			charged = 1
		}
		g.usageSink(domain.UsageEvent{At: g.clock.Now().UTC(), RequestID: requestIDFromContext(r.Context()), Project: project, Alias: "/p/" + providerName + "/" + path, Provider: providerName, Model: resp.Path, KeyID: string(resp.KeyID), Outcome: outcome, Source: "passthrough", Status: status, EstimatedTokens: charged, ActualTokens: charged, Usage: &domain.Usage{TotalTokens: charged}})
	}
}

func isGatewayHopHeader(name string, headers http.Header) bool {
	if gatewayHopHeaders[http.CanonicalHeaderKey(name)] {
		return true
	}
	for _, connection := range headers.Values("Connection") {
		for _, token := range strings.Split(connection, ",") {
			if strings.EqualFold(strings.TrimSpace(token), name) {
				return true
			}
		}
	}
	return false
}

func passthroughStatusOutcome(status int) string {
	switch status {
	case 401:
		return "auth"
	case 402:
		return "quota"
	case 403:
		return "forbidden"
	case 429:
		return "rate_limited"
	default:
		if status >= 500 {
			return "transient"
		}
		if status == 404 {
			return "not_found"
		}
		if status >= 400 {
			return "client_error"
		}
		return "success"
	}
}
func passthroughStatusTokens(status int) int {
	if status > 0 {
		return 1
	}
	return 0
}
