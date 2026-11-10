package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"keypool/internal/classify"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
)

// DirectLease is an exclusive provider-key reservation used by lease mode and
// generic passthrough. The router snapshot stays pinned until Complete/Close.
type DirectLease struct {
	Provider, Model, BaseURL, AuthHeader, AuthQuery, AuthScheme string
	KeyID                                                       sched.KeyID
	KeyLabel                                                    string
	Secret                                                      secret.Secret
	Client                                                      *http.Client
	Estimated                                                   int
	Spec                                                        provider.Spec
	Adapter                                                     provider.Adapter
	lease                                                       *sched.Lease
	budget                                                      *projectBudgetReservation
	release                                                     func()
	now                                                         func() time.Time
	once                                                        sync.Once
}

func (l *DirectLease) Hints(headers http.Header, now time.Time) *sched.Hints {
	if l == nil || l.Adapter == nil {
		return nil
	}
	if a, ok := l.Adapter.(interface {
		HintsFor(*provider.Spec, http.Header, time.Time) *sched.Hints
	}); ok {
		return a.HintsFor(&l.Spec, headers, now)
	}
	return l.Adapter.Hints(headers, now)
}

// Complete closes the reservation exactly once. Report outcomes deliberately
// keep rate limits neutral and do not turn HTTP 429 into a circuit failure.
func (l *DirectLease) Complete(outcome string, actual int, headers http.Header, now time.Time) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		hints := l.Hints(headers, now)
		switch strings.ToLower(strings.TrimSpace(outcome)) {
		case "success":
			if actual <= 0 {
				actual = l.Estimated
			}
			l.lease.Success(actual, hints)
			if l.budget != nil {
				l.budget.settle(actual)
			}
		case "rate_limited", "rate_limited_short", "rate_limited_long":
			retry, known := classify.RetryAfter(headers, now)
			l.lease.RateLimited(retry, known, hints)
			if l.budget != nil {
				charge := actual
				if charge <= 0 {
					charge = l.Estimated
				}
				l.budget.settle(charge)
			}
		case "quota", "quota_exhausted":
			l.lease.QuotaExhausted()
			if l.budget != nil {
				l.budget.settle(maxPositive(0, actual))
			}
		case "auth", "auth_invalid":
			l.lease.AuthFailed()
			if l.budget != nil {
				l.budget.settle(maxPositive(0, actual))
			}
		case "forbidden":
			l.lease.Forbidden()
			if l.budget != nil {
				l.budget.settle(maxPositive(0, actual))
			}
		case "leaked":
			// The plan treats an abandoned lease as a successful estimated call:
			// the reservation is consumed and the key is not blamed.
			l.lease.Success(l.Estimated, hints)
			if l.budget != nil {
				l.budget.settle(l.Estimated)
			}
		case "transient", "timeout":
			l.lease.Failed()
			if l.budget != nil {
				charge := actual
				if charge <= 0 {
					charge = l.Estimated
				}
				l.budget.settle(charge)
			}
		case "client_error", "not_found":
			// The provider received the request and the key was accepted far
			// enough to produce an endpoint/client response. Count the request
			// without opening the circuit or releasing RPM/RPD reservations.
			l.lease.Success(maxPositive(1, actual), hints)
			if l.budget != nil {
				l.budget.settle(maxPositive(1, actual))
			}
		case "cancel", "canceled", "cancelled":
			l.lease.Release()
			if l.budget != nil {
				l.budget.settle(maxPositive(0, actual))
			}
		default:
			l.lease.Release()
			if l.budget != nil {
				l.budget.settle(0)
			}
		}
		if l.release != nil {
			l.release()
		}
	})
}

func (r *Router) acquireProjectSlot(ctx context.Context, project string) (func(), error) {
	slots := r.projectSlots[project]
	if slots == nil {
		return func() {}, nil
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Router) AcquireLease(ctx context.Context, projectName, target string, estimated, priority int) (*DirectLease, error) {
	return r.acquireDirect(ctx, projectName, target, estimated, priority, true)
}

func (r *Router) acquireDirect(ctx context.Context, projectName, target string, estimated, priority int, requireLeasePermission bool) (*DirectLease, error) {
	if estimated < 1 {
		estimated = 1
	}
	parts := strings.SplitN(strings.TrimSpace(target), ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("router: target deve ser provider:model")
	}
	providerName, model := parts[0], parts[1]
	r.mu.RLock()
	p, ok := r.snapshot.Project(projectName)
	if !ok {
		r.mu.RUnlock()
		return nil, ErrProjectForbidden
	}
	if requireLeasePermission && !p.AllowLease {
		r.mu.RUnlock()
		return nil, ErrLeaseForbidden
	}
	entry := r.pools[providerName]
	if entry == nil {
		r.mu.RUnlock()
		return nil, ErrNoTarget
	}
	allowedIDs := p.Keys[providerName]
	allowed := make(map[sched.KeyID]bool, len(allowedIDs))
	for _, id := range allowedIDs {
		if entry.pool.ModelAllowed(id, model, provider.IsDynamic(model)) {
			allowed[id] = true
		}
	}
	if len(allowed) == 0 {
		r.mu.RUnlock()
		return nil, ErrNoTarget
	}
	// Project max_wait is a hard upper bound, even when the caller supplied
	// a longer timeout/deadline. A shorter caller deadline still wins.
	acqCtx, cancel := context.WithTimeout(ctx, p.ParseMaxWait())
	releaseSlot, err := r.acquireProjectSlot(acqCtx, projectName)
	if err != nil {
		cancel()
		r.mu.RUnlock()
		return nil, err
	}
	lease, err := entry.pool.Acquire(acqCtx, sched.AcquireRequest{Model: model, Tokens: estimated, Priority: priority, Reserve: reserveFor(p, priority), Allow: func(id sched.KeyID) bool { return allowed[id] }})
	cancel()
	if err != nil {
		releaseSlot()
		r.mu.RUnlock()
		return nil, err
	}
	budget, err := r.reserveDailyBudget(projectName, p.Policy.DailyTokenBudget, estimated)
	if err != nil {
		lease.Release()
		releaseSlot()
		r.mu.RUnlock()
		return nil, err
	}
	key := lease.Secret()
	return &DirectLease{Provider: providerName, Model: model, BaseURL: entry.spec.BaseURL, AuthHeader: entry.spec.Auth.Header,
		AuthQuery: entry.spec.Auth.Query, AuthScheme: entry.spec.Auth.Scheme, KeyID: lease.KeyID(), KeyLabel: lease.Label(),
		Secret: key, Client: entry.client, Estimated: estimated, Spec: entry.spec, Adapter: entry.adapter, lease: lease, budget: budget, now: r.clock.Now,
		release: func() {
			releaseSlot()
			dirty := r.onStateChange
			r.mu.RUnlock()
			if dirty != nil {
				dirty()
			}
		}}, nil
}

// PassthroughResponse owns the scheduler lease and project slot until Close.
type PassthroughResponse struct {
	Response       *http.Response
	Provider, Path string
	KeyID          sched.KeyID
	KeyLabel       string
	QueueMS        int64
	TTFB           time.Duration
	lease          *DirectLease
	once           sync.Once
}

func (p *PassthroughResponse) Finish(outcome string, tokens int, now time.Time) {
	if p == nil {
		return
	}
	p.once.Do(func() {
		if p.Response != nil && p.Response.Body != nil {
			_ = p.Response.Body.Close()
		}
		if p.lease != nil {
			p.lease.Complete(outcome, tokens, responseHeaders(p.Response), now)
		}
	})
}
func (p *PassthroughResponse) Close() error {
	if p == nil {
		return nil
	}
	var err error
	p.once.Do(func() {
		if p.Response != nil && p.Response.Body != nil {
			err = p.Response.Body.Close()
		}
		if p.lease != nil {
			now := time.Now().UTC()
			if p.lease.now != nil {
				now = p.lease.now().UTC()
			}
			p.lease.Complete(passthroughOutcome(p.Response), passthroughTokens(p.Response), responseHeaders(p.Response), now)
		}
	})
	return err
}
func responseHeaders(resp *http.Response) http.Header {
	if resp == nil {
		return nil
	}
	return resp.Header
}
func passthroughTokens(resp *http.Response) int {
	if resp != nil {
		return 1
	}
	return 0
}
func passthroughOutcome(resp *http.Response) string {
	if resp == nil {
		return "transient"
	}
	switch resp.StatusCode {
	case 401:
		return "auth"
	case 402:
		return "quota"
	case 403:
		return "forbidden"
	case 429:
		return "rate_limited"
	default:
		if resp.StatusCode >= 500 || resp.StatusCode < 200 {
			return "transient"
		}
		if resp.StatusCode == 404 {
			return "not_found"
		}
		if resp.StatusCode >= 400 {
			return "client_error"
		}
		return "success"
	}
}

var hopHeaders = map[string]bool{"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true}

func isHopByHopHeader(name string, headers http.Header) bool {
	canonical := http.CanonicalHeaderKey(name)
	if hopHeaders[canonical] {
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

// Passthrough sends an arbitrary HTTP request through a generic_http provider.
// It forwards only end-to-end headers and replaces all caller credentials.
func (r *Router) Passthrough(ctx context.Context, projectName, providerName, path string, inbound *http.Request, priority int) (*PassthroughResponse, error) {
	if inbound == nil {
		return nil, errors.New("router: requisição passthrough ausente")
	}
	cleanPath := strings.TrimSpace(path)
	if cleanPath == "" || strings.Contains(cleanPath, "..") || strings.ContainsRune(cleanPath, '\\') {
		return nil, errors.New("router: caminho passthrough inválido")
	}
	model := "/" + strings.Trim(cleanPath, "/")
	direct, err := r.acquireDirect(ctx, projectName, providerName+":"+model, 1, priority, false)
	if err != nil {
		return nil, err
	}
	if direct.Spec.Type != "generic_http" {
		direct.Complete("cancel", 0, nil, r.clock.Now())
		return nil, fmt.Errorf("router: provedor %s não é generic_http", providerName)
	}
	base, err := url.Parse(direct.BaseURL)
	if err != nil {
		direct.Complete("cancel", 0, nil, r.clock.Now())
		return nil, err
	}
	joined, err := url.JoinPath(base.String(), cleanPath)
	if err != nil {
		direct.Complete("cancel", 0, nil, r.clock.Now())
		return nil, err
	}
	u, err := url.Parse(joined)
	if err != nil {
		direct.Complete("cancel", 0, nil, r.clock.Now())
		return nil, err
	}
	u.RawQuery = inbound.URL.RawQuery
	upReq, err := http.NewRequestWithContext(ctx, inbound.Method, u.String(), inbound.Body)
	if err != nil {
		direct.Complete("cancel", 0, nil, r.clock.Now())
		return nil, err
	}
	for name, values := range inbound.Header {
		if isHopByHopHeader(name, inbound.Header) || strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "X-API-Key") || strings.HasPrefix(strings.ToLower(name), "x-keypool-") {
			continue
		}
		for _, value := range values {
			upReq.Header.Add(name, value)
		}
	}
	for name, value := range direct.Spec.Headers {
		upReq.Header.Set(name, value)
	}
	secretValue := direct.Secret.Reveal()
	switch direct.AuthScheme {
	case "", "bearer":
		upReq.Header.Set("Authorization", "Bearer "+secretValue)
	case "header":
		upReq.Header.Set(direct.AuthHeader, secretValue)
	case "query":
		q := upReq.URL.Query()
		q.Set(direct.AuthQuery, secretValue)
		upReq.URL.RawQuery = q.Encode()
	}
	// Reuse the current provider transport, but disable the chat client's
	// total timeout because arbitrary uploads/downloads may stream for longer.
	entryClient := &http.Client{Transport: direct.Client.Transport, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	started := r.clock.Now()
	resp, err := entryClient.Do(upReq)
	ttfb := time.Duration(0)
	if resp != nil {
		ttfb = r.clock.Now().Sub(started)
		if direct.lease != nil {
			direct.lease.RecordTTFB(ttfb)
		}
	}
	if err != nil {
		direct.Complete("transient", 1, nil, r.clock.Now().UTC())
		return nil, err
	}
	return &PassthroughResponse{Response: resp, Provider: providerName, Path: model, KeyID: direct.KeyID, KeyLabel: direct.KeyLabel, TTFB: ttfb, lease: direct}, nil
}

// classifyDirectOutcome adapts provider classifications for explicit lease reports.
func classifyDirectOutcome(out classify.Outcome) string {
	switch out {
	case classify.Success:
		return "success"
	case classify.RateLimitedShort, classify.RateLimitedLong:
		return "rate_limited"
	case classify.AuthInvalid:
		return "auth"
	case classify.QuotaExhausted:
		return "quota"
	case classify.Forbidden:
		return "forbidden"
	case classify.Canceled:
		return "cancel"
	default:
		return "transient"
	}
}

func maxPositive(a, b int) int {
	if a > b {
		return a
	}
	return b
}
