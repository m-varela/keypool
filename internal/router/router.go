// Package router resolve aliases, seleciona chaves e executa fallback por alvo.
package router

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"keypool/internal/clock"

	"keypool/internal/classify"
	"keypool/internal/config"
	"keypool/internal/domain"
	"keypool/internal/limits"
	"keypool/internal/provider"
	_ "keypool/internal/provider/generic"
	_ "keypool/internal/provider/openaicompat"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/sse"
)

// Request é uma chamada canônica de chat.
type Request struct {
	Project, Model string
	Payload        map[string]json.RawMessage
	Priority       *int
	Timeout        time.Duration
	TimeoutSet     bool
	// QueueTimeout limits only the wait for a schedulable key. It must not
	// shorten the upstream generation deadline.
	QueueTimeout    time.Duration
	QueueTimeoutSet bool
	Exclude         []string
	AvoidWeights    []string
}

// ErrQueueTimeout reports that a request could not acquire a key within its
// queue-wait budget while the overall request deadline was still active.
var ErrQueueTimeout = errors.New("router: tempo de espera por capacidade esgotado")

// RouteFailure preserva diagnóstico técnico seguro quando a rota termina sem
// resposta utilizável. Não contém credenciais, payload ou identificadores de chave.
type RouteFailure struct {
	Code              string
	Phase             string
	StatusCode        int
	UpstreamTarget    string
	Attempts          int
	Fallback          bool
	Retryable         bool
	RetryabilityKnown bool
	RetryAfter        time.Duration
	Cause             error
}

func (e *RouteFailure) Error() string {
	if e == nil {
		return "router: falha de rota"
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

func (e *RouteFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func isQueueCapacityError(err error) bool {
	var noCapacity *sched.ErrNoCapacity
	var outranked *sched.ErrOutranked
	return errors.As(err, &noCapacity) || errors.As(err, &outranked)
}

// Response preserva bytes do upstream e metadados de roteamento.
type Response struct {
	KeyID            sched.KeyID
	Status           int
	Header           http.Header
	Body             []byte
	Stream           io.ReadCloser
	StreamUsage      *StreamUsage
	Usage            *domain.Usage
	Outcome          domain.Outcome
	Target, KeyLabel string
	Attempts         int
	QueueMS          int64
	TTFB             time.Duration
	Fallback         bool
}

// StreamUsage sincroniza a leitura do usage final com o término do stream.
type StreamUsage struct {
	mu      sync.Mutex
	done    chan struct{}
	usage   *sse.Usage
	outcome domain.Outcome
}

func (u *StreamUsage) finish(v *sse.Usage, outcome domain.Outcome) {
	u.mu.Lock()
	u.usage = v
	u.outcome = outcome
	u.mu.Unlock()
	close(u.done)
}
func (u *StreamUsage) Outcome() domain.Outcome {
	if u == nil {
		return ""
	}
	<-u.done
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.outcome
}
func (u *StreamUsage) Wait() *sse.Usage {
	if u == nil {
		return nil
	}
	<-u.done
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.usage == nil {
		return nil
	}
	v := *u.usage
	return &v
}

type streamBody struct {
	*io.PipeReader
	closeFn func()
	closed  *atomic.Bool
}

func (b *streamBody) Close() error {
	if b.closed != nil {
		b.closed.Store(true)
	}
	err := b.PipeReader.Close()
	if b.closeFn != nil {
		b.closeFn()
	}
	return err
}

type poolEntry struct {
	pool    *sched.Pool
	spec    provider.Spec
	adapter provider.Adapter
	client  *http.Client
}

// Router mantém pools locais; a configuração recebida é um snapshot imutável.
type Router struct {
	budgetMu      sync.Mutex
	projectBudget map[string]*projectBudgetState
	calibrationMu sync.RWMutex
	tokenFactors  map[string]float64
	warmMu        sync.RWMutex // snapshot apply waits for in-flight warm probes
	mu            sync.RWMutex
	snapshot      *config.Snapshot
	pools         map[string]*poolEntry
	clock         clock.Clock
	deadUntil     map[string]time.Time
	projectSlots  map[string]chan struct{}
	onStateChange func()
	onEvent       func(provider string, event sched.Event)
}

// WarmProvider exposes the current provider runtime to the isolated warm service.
// Secrets remain encapsulated in sched.KeySpec and HTTP clients/transports are shared.
type WarmProvider struct {
	Name    string
	Spec    provider.Spec
	Adapter provider.Adapter
	Client  *http.Client
	Pool    *sched.Pool
	Keys    []sched.KeySpec
}

// WarmProviders returns a coherent snapshot of active provider runtimes.
func (r *Router) WarmProviders() []WarmProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.pools))
	for name := range r.pools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]WarmProvider, 0, len(names))
	for _, name := range names {
		entry := r.pools[name]
		keys := make([]sched.KeySpec, 0)
		for _, key := range entry.pool.Keys() {
			keys = append(keys, key)
		}
		out = append(out, WarmProvider{Name: name, Spec: entry.spec, Adapter: entry.adapter, Client: entry.client, Pool: entry.pool, Keys: keys})
	}
	return out
}

// AcquireWarmProvider pins the current provider runtime until release is called.
// SetSnapshot waits for these probes to finish before replacing pools/transports,
// preventing a probe result from being written into a detached pool.
func (r *Router) AcquireWarmProvider(name string) (WarmProvider, func(), bool) {
	r.warmMu.RLock()
	r.mu.RLock()
	entry := r.pools[name]
	if entry == nil {
		r.mu.RUnlock()
		r.warmMu.RUnlock()
		return WarmProvider{}, func() {}, false
	}
	keys := make([]sched.KeySpec, 0)
	for _, key := range entry.pool.Keys() {
		keys = append(keys, key)
	}
	provider := WarmProvider{Name: name, Spec: entry.spec, Adapter: entry.adapter, Client: entry.client, Pool: entry.pool, Keys: keys}
	r.mu.RUnlock()
	var once sync.Once
	release := func() { once.Do(r.warmMu.RUnlock) }
	return provider, release, true
}

// New cria pools e transports para os provedores habilitados no snapshot.
func New(s *config.Snapshot) (*Router, error) { return NewWithClock(s, clock.Real()) }

// NewWithClock permite tornar determinísticas as decisões temporais do roteador.
func NewWithClock(s *config.Snapshot, clk clock.Clock) (*Router, error) {
	if s == nil {
		return nil, fmt.Errorf("router: snapshot ausente")
	}
	if clk == nil {
		clk = clock.Real()
	}
	r := &Router{snapshot: s, pools: map[string]*poolEntry{}, clock: clk, deadUntil: map[string]time.Time{}, projectSlots: map[string]chan struct{}{}, projectBudget: map[string]*projectBudgetState{}, tokenFactors: map[string]float64{}}
	if err := r.buildPools(s); err != nil {
		return nil, err
	}
	r.buildProjectSlots(s)
	return r, nil
}

func (r *Router) buildPools(s *config.Snapshot) error {
	pools, err := r.newPools(s)
	if err != nil {
		return err
	}
	r.pools = pools
	r.installEventSinkLocked()
	return nil
}

func (r *Router) newPools(s *config.Snapshot) (map[string]*poolEntry, error) {
	pools := map[string]*poolEntry{}
	warmConfig := s.WarmConfig()
	closeNew := func() {
		for _, entry := range pools {
			if tr, ok := entry.client.Transport.(*http.Transport); ok {
				tr.CloseIdleConnections()
			}
		}
	}
	for _, name := range s.Providers() {
		ps, _ := s.Provider(name)
		if !ps.Enabled {
			continue
		}
		ad, err := provider.NewAdapter(ps.Spec.Type)
		if err != nil {
			closeNew()
			return nil, err
		}
		pool := sched.NewPool(sched.PoolConfig{Provider: name, Limits: ps.Limits, Scope: ps.Scope, Strategy: ps.Strategy, QuotaCooldown: durationOr(ps.Spec.QuotaCooldown, time.Hour), CatalogTTL: warmConfig.CatalogTTL, Clock: r.clock})
		pool.SetKeys(ps.Keys)
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ForceAttemptHTTP2 = true
		maxIdle := len(ps.Keys)
		if maxIdle < 1 {
			maxIdle = 1
		}
		if maxIdle > 4 {
			maxIdle = 4
		}
		tr.MaxIdleConnsPerHost = maxIdle
		tr.IdleConnTimeout = 90 * time.Second
		pools[name] = &poolEntry{pool: pool, spec: ps.Spec, adapter: ad, client: &http.Client{Transport: tr, Timeout: ps.Spec.TimeoutDuration()}}
	}
	return pools, nil
}

// SetSnapshot troca configuração e pools como uma unidade. O estado de uso e saúde
// é restaurado por ID de chave quando o provedor permanece configurado.
func (r *Router) SetSnapshot(s *config.Snapshot) error {
	if s == nil {
		return fmt.Errorf("router: snapshot ausente")
	}
	r.warmMu.Lock()
	defer r.warmMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.pools
	updated, err := r.newPools(s)
	if err != nil {
		return err
	}
	for name, entry := range updated {
		if prev := old[name]; prev != nil {
			previousKeys := map[sched.KeyID]sched.KeySpec{}
			previousVersions := map[sched.KeyID]uint64{}
			for _, key := range prev.pool.Keys() {
				previousKeys[key.ID] = key
			}
			for _, stats := range prev.pool.Stats() {
				previousVersions[stats.ID] = stats.CredentialVersion
			}
			changedCredentials := make(map[sched.KeyID]uint64)
			for _, key := range entry.pool.Keys() {
				if previous, ok := previousKeys[key.ID]; ok && previous.Secret.Reveal() != key.Secret.Reveal() {
					changedCredentials[key.ID] = previousVersions[key.ID]
				}
			}
			entry.pool.Restore(prev.pool.Snapshot())
			for id, previousVersion := range changedCredentials {
				entry.pool.ResetCredential(id, previousVersion)
			}
		}
	}
	r.pools = updated
	r.installEventSinkLocked()
	r.snapshot = s
	r.deadUntil = map[string]time.Time{}
	r.buildProjectSlots(s)
	for _, entry := range old {
		if tr, ok := entry.client.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	return nil
}

func (r *Router) buildProjectSlots(s *config.Snapshot) {
	r.projectSlots = map[string]chan struct{}{}
	for _, name := range s.Projects() {
		p, ok := s.Project(name)
		if ok && p.Policy.MaxInflight > 0 {
			r.projectSlots[name] = make(chan struct{}, p.Policy.MaxInflight)
		}
	}
}

func durationOr(s string, d time.Duration) time.Duration {
	if s == "" {
		return d
	}
	v, e := time.ParseDuration(s)
	if e != nil {
		return d
	}
	return v
}

// SetOnStateChange instala um sinalizador chamado ao final de cada tentativa de roteamento.
// Deve ser chamado antes de expor o Router a requisições concorrentes.
func (r *Router) SetOnStateChange(fn func()) { r.mu.Lock(); r.onStateChange = fn; r.mu.Unlock() }

// SetOnEvent conecta eventos dos pools ao sink operacional.
func (r *Router) SetOnEvent(fn func(provider string, event sched.Event)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onEvent = fn
	r.installEventSinkLocked()
}
func (r *Router) installEventSinkLocked() {
	for name, entry := range r.pools {
		providerName := name
		entry.pool.SetOnEvent(func(event sched.Event) {
			if r.onEvent != nil {
				r.onEvent(providerName, event)
			}
		})
	}
}

// MarkStateDirty notifies the persistence layer about state changes made by
// background services such as the warm scheduler, outside Router.Do.
func (r *Router) MarkStateDirty() {
	r.mu.RLock()
	fn := r.onStateChange
	r.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// Pools retorna os pools de runtime para snapshot/restore.
func (r *Router) Pools() map[string]*sched.Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*sched.Pool, len(r.pools))
	for name, entry := range r.pools {
		out[name] = entry.pool
	}
	return out
}

// Do executa a cadeia configurada. O read-lock mantém snapshot e pools coerentes
// durante toda a requisição; uma recarga aguarda as leases em andamento.
func (r *Router) Do(ctx context.Context, req *Request) (*Response, error) {
	r.mu.RLock()
	lockHeld := true
	onStateChange := r.onStateChange
	defer func() {
		if lockHeld {
			r.mu.RUnlock()
			if onStateChange != nil {
				onStateChange()
			}
		}
	}()
	if req == nil {
		return nil, fmt.Errorf("router: requisição ausente")
	}
	project, ok := r.snapshot.Project(req.Project)
	if !ok {
		return nil, ErrProjectForbidden
	}
	route, routed := project.Routes[req.Model]
	var chain []config.Target
	priority := 5
	defaults, force := map[string][]byte{}, map[string][]byte{}
	requires := map[string]bool{}
	if routed {
		chain = append(chain, route.Chain...)
		priority = route.Priority
		defaults = route.Defaults
		force = route.Force
		requires = route.Requires
	} else {
		if !project.AllowLiteralTargets {
			return nil, ErrUnknownRoute
		}
		parts := strings.SplitN(req.Model, ":", 2)
		if len(parts) != 2 {
			return nil, ErrUnknownRoute
		}
		chain = []config.Target{{Provider: parts[0], Model: parts[1]}}
	}
	if req.Priority != nil {
		priority = *req.Priority
	}
	payloadBase := mergePayload(defaults, req.Payload, force)
	if !req.TimeoutSet && req.Timeout <= 0 {
		req.Timeout = project.ParseMaxWait()
	}
	requestCtx := ctx
	cancelRequest := func() {}
	if req.TimeoutSet || req.Timeout > 0 {
		requestCtx, cancelRequest = context.WithTimeout(ctx, req.Timeout)
	}
	var releaseProjectSlot func()
	cleanupOnReturn := true
	defer func() {
		if cleanupOnReturn {
			cancelRequest()
			if releaseProjectSlot != nil {
				releaseProjectSlot()
			}
		}
	}()
	if slots := r.projectSlots[project.Name]; slots != nil {
		select {
		case slots <- struct{}{}:
			releaseProjectSlot = func() { <-slots }
		case <-requestCtx.Done():
			return nil, routeFailure(requestCtx.Err(), 0, "", false, "queue")
		}
	}
	if len(chain) == 0 {
		return nil, ErrNoTarget
	}

	// Filtra capacidades e modelos temporariamente mortos antes de estimar a cadeia.
	filtered := make([]config.Target, 0, len(chain))
	for _, t := range chain {
		entry := r.pools[t.Provider]
		if entry == nil {
			continue
		}
		if !supportsRequires(entry.spec, t.Model, requires) {
			continue
		}
		if until := r.deadUntil[t.Provider+":"+t.Model]; until.After(r.clock.Now()) {
			continue
		}
		filtered = append(filtered, t)
	}
	if len(filtered) == 0 {
		return nil, ErrNoTarget
	}
	// Exclude e avoid-weights são preferências; se eliminarem tudo, preserva a cadeia.
	if len(req.Exclude) > 0 {
		kept := filterTargets(filtered, func(t config.Target) bool { return !excluded(req.Exclude, t) })
		if len(kept) > 0 {
			filtered = kept
		}
	}
	if len(req.AvoidWeights) > 0 {
		kept := filterTargets(filtered, func(t config.Target) bool { return !avoidedWeight(req.AvoidWeights, t.Model) })
		if len(kept) > 0 {
			filtered = kept
		}
	}

	type candidate struct {
		target        config.Target
		entry         *poolEntry
		allowed       map[sched.KeyID]bool
		payload       map[string]json.RawMessage
		estimate      int
		wait          time.Duration
		viable        bool
		original      int
		balanceGroup  int
		quotaRatio    float64
		hasQuotaRatio bool
	}
	candidates := make([]candidate, 0, len(filtered))
	promptTooLarge := false
	longRetry := time.Duration(-1)
	for i, t := range filtered {
		entry := r.pools[t.Provider]
		ids := project.Keys[t.Provider]
		allowed := make(map[sched.KeyID]bool, len(ids))
		for _, id := range ids {
			if entry.pool.ModelAllowed(id, t.Model, provider.IsDynamic(t.Model)) {
				allowed[id] = true
			}
		}
		if len(allowed) == 0 {
			continue
		}
		p := clonePayload(payloadBase)
		modelBytes, _ := json.Marshal(t.Model)
		p["model"] = modelBytes
		if isStream(p) && entry.spec.Capabilities.StreamIncludeUsage {
			var options map[string]any
			_ = json.Unmarshal(p["stream_options"], &options)
			if options == nil {
				options = map[string]any{}
			}
			options["include_usage"] = true
			p["stream_options"], _ = json.Marshal(options)
		}
		p = entry.adapter.AdaptPayload(&entry.spec, t.Model, p)
		estimated, promptErr := fitBudgetFactor(entry.pool, t.Model, p, &entry.spec, r.tokenFactor(t.Provider, t.Model))
		if promptErr != nil {
			if errors.Is(promptErr, ErrPromptTooLargeForTPM) {
				promptTooLarge = true
			}
			continue
		}
		acq := sched.AcquireRequest{Model: t.Model, Tokens: estimated, Priority: priority, Reserve: reserveFor(project, priority), Allow: func(id sched.KeyID) bool {
			return allowed[id]
		}}
		if until, exhausted := entry.pool.LongLimited(acq); exhausted {
			d := until.Sub(r.clock.Now())
			if longRetry < 0 || d < longRetry {
				longRetry = d
			}
			continue
		}
		wait, viable := entry.pool.EstimateWait(acq)
		if !viable {
			continue
		}
		balanceGroup := len(route.BalanceGroups) + i
		for groupIndex, group := range route.BalanceGroups {
			for _, providerName := range group {
				if providerName == t.Provider {
					balanceGroup = groupIndex
					break
				}
			}
			if balanceGroup == groupIndex {
				break
			}
		}
		quota := entry.pool.DailyQuotaUsage(t.Model)
		candidates = append(candidates, candidate{target: t, entry: entry, allowed: allowed, payload: p, estimate: estimated, wait: wait, viable: viable, original: i, balanceGroup: balanceGroup, quotaRatio: quota.Ratio, hasQuotaRatio: quota.HasLimits})
	}
	if len(candidates) == 0 {
		if longRetry >= 0 {
			return nil, &ErrLongLimited{RetryIn: longRetry}
		}
		if promptTooLarge {
			return nil, ErrPromptTooLargeForTPM
		}
		return nil, ErrNoTarget
	}
	spill := project.Policy.SpillEnabled()
	// Com spill, alvos disponíveis agora vêm primeiro; entre provedores da mesma
	// faixa de preferência, menor fração de cota diária consumida vence. Assim,
	// rotas balanceadas distribuem carga proporcionalmente à capacidade declarada
	// sem permitir que um fallback ultrapasse a faixa preferida enquanto ela está livre.
	if spill {
		sort.SliceStable(candidates, func(i, j int) bool {
			wi, wj := candidates[i].wait, candidates[j].wait
			if wi < 0 {
				wi = time.Duration(1<<63 - 1)
			}
			if wj < 0 {
				wj = time.Duration(1<<63 - 1)
			}
			if wi != wj {
				return wi < wj
			}
			if len(route.BalanceGroups) > 0 && candidates[i].balanceGroup != candidates[j].balanceGroup {
				return candidates[i].balanceGroup < candidates[j].balanceGroup
			}
			if len(route.BalanceGroups) > 0 && candidates[i].balanceGroup == candidates[j].balanceGroup && candidates[i].hasQuotaRatio && candidates[j].hasQuotaRatio {
				if candidates[i].quotaRatio != candidates[j].quotaRatio {
					return candidates[i].quotaRatio < candidates[j].quotaRatio
				}
			}
			return candidates[i].original < candidates[j].original
		})
		// Immediate candidates sort first, but keep queued candidates behind
		// them. If every immediately available upstream fails transiently, the
		// remaining providers still get a bounded queue/fallback attempt.
	} else {
		candidates = candidates[:1]
	}
	var last *Response
	var lastErr error
	totalAttempts := 0
	lastTarget := ""
	firstAttemptedTarget := ""
	fallbackAttempted := false
	for round := 0; round < 5; round++ {
		shortLimited := false
		for ci, c := range candidates {
			for attempt := 0; attempt < 8; attempt++ {
				start := r.clock.Now()
				acq := sched.AcquireRequest{Model: c.target.Model, Tokens: c.estimate, Priority: priority, Reserve: reserveFor(project, priority), Allow: func(id sched.KeyID) bool {
					return c.allowed[id]
				}}
				var lease *sched.Lease
				var err error
				canSpill := spill && ci < len(candidates)-1
				waitForCandidate := attempt == 0 && c.wait != 0 && (req.TimeoutSet || req.Timeout > 0 || req.QueueTimeoutSet)
				if waitForCandidate {
					queueTimeout := req.QueueTimeout
					queueTimeoutSet := req.QueueTimeoutSet
					if !queueTimeoutSet {
						// Preserve the scheduler's estimate when known (it may become
						// available during this slice). For indeterminate waits use a
						// short slice so the next configured provider still gets a turn.
						queueTimeout = c.wait
						if queueTimeout <= 0 {
							queueTimeout = 2 * time.Second
						}
						if req.Timeout > 0 && req.Timeout < queueTimeout {
							queueTimeout = req.Timeout
						}
						queueTimeoutSet = true
					}
					if queueTimeoutSet && queueTimeout <= 0 {
						lease, err = c.entry.pool.TryAcquire(acq)
						if err != nil && requestCtx.Err() == nil && isQueueCapacityError(err) {
							err = ErrQueueTimeout
						}
					} else if queueTimeoutSet {
						queueCtx, cancelQueue := context.WithTimeout(requestCtx, queueTimeout)
						lease, err = c.entry.pool.Acquire(queueCtx, acq)
						queueDeadlineReached := errors.Is(queueCtx.Err(), context.DeadlineExceeded)
						cancelQueue()
						if err != nil && requestCtx.Err() == nil && (queueDeadlineReached || isQueueCapacityError(err)) {
							err = ErrQueueTimeout
						}
					} else {
						lease, err = c.entry.pool.Acquire(requestCtx, acq)
					}
				} else if attempt == 0 && !canSpill && req.QueueTimeoutSet && req.QueueTimeout <= 0 {
					lease, err = c.entry.pool.TryAcquire(acq)
					if err != nil && requestCtx.Err() == nil && isQueueCapacityError(err) {
						err = ErrQueueTimeout
					}
				} else {
					lease, err = c.entry.pool.TryAcquire(acq)
				}
				if err != nil {
					if requestCtx.Err() != nil {
						return nil, routeFailure(requestCtx.Err(), totalAttempts, lastTarget, fallbackAttempted, failurePhase(totalAttempts, false))
					}
					lastErr = err
					if errors.Is(err, ErrQueueTimeout) && ci < len(candidates)-1 {
						fallbackAttempted = true
					}
					if last != nil && last.Outcome == domain.RateLimitedShort {
						shortLimited = true
					}
					break
				}
				queueMS := r.clock.Now().Sub(start).Milliseconds()
				budgetReservation, budgetErr := r.reserveDailyBudget(project.Name, project.Policy.DailyTokenBudget, c.estimate)
				if budgetErr != nil {
					lease.Release()
					return nil, budgetErr
				}
				canonical := &provider.Canonical{Payload: c.payload, Stream: isStream(c.payload)}
				upReq, err := c.entry.adapter.BuildRequest(requestCtx, &c.entry.spec, lease.Secret(), canonical)
				if err != nil {
					lease.Release()
					budgetReservation.settle(0)
					lastErr = err
					break
				}
				client := *c.entry.client
				if canonical.Stream {
					client.Timeout = 0
				} else if attemptTimeout := project.ParseAttemptTimeout(); attemptTimeout > 0 && (client.Timeout <= 0 || attemptTimeout < client.Timeout) {
					// Bound this upstream only. The outer request context remains alive
					// so a transient timeout can spill to the next configured provider.
					client.Timeout = attemptTimeout
				}
				if c.original > 0 {
					fallbackAttempted = true
				}
				doStarted := r.clock.Now()
				totalAttempts++
				lastTarget = c.target.Provider + ":" + c.target.Model
				if firstAttemptedTarget == "" {
					firstAttemptedTarget = lastTarget
				} else if lastTarget != firstAttemptedTarget {
					fallbackAttempted = true
				}
				upResp, doErr := client.Do(upReq)
				ttfb := time.Duration(0)
				if upResp != nil {
					ttfb = r.clock.Now().Sub(doStarted)
					lease.RecordTTFB(ttfb)
				}
				var body []byte
				var result classify.Result
				var stream *streamBody
				var streamUsage *StreamUsage
				var hints *sched.Hints
				if doErr != nil {
					result = c.entry.adapter.Classify(nil, nil, doErr)
				} else if canonical.Stream && upResp != nil && upResp.StatusCode >= 200 && upResp.StatusCode < 300 && strings.Contains(strings.ToLower(upResp.Header.Get("Content-Type")), "text/event-stream") {
					first, peekErr := sse.PeekFirstDataEvent(bufio.NewReader(upResp.Body))
					if peekErr != nil {
						result = classify.Classify(http.StatusOK, nil, upResp.Header, peekErr, r.clock.Now())
						_ = upResp.Body.Close()
					} else {
						if sse.IsDone(first.Event) {
							result = classify.Result{Outcome: classify.Success, Status: http.StatusOK}
						} else {
							result = classify.Classify(http.StatusOK, []byte(first.Event.Data), nil, nil, r.clock.Now())
						}
						if result.Outcome == classify.Success {
							hints = adapterHints(c.entry.adapter, &c.entry.spec, headerOrEmpty(upResp), r.clock.Now())
							streamCleanup := func() {
								cancelRequest()
								if releaseProjectSlot != nil {
									releaseProjectSlot()
								}
								r.mu.RUnlock()
								if onStateChange != nil {
									onStateChange()
								}
							}
							budgetFinish := func(u *sse.Usage, succeeded bool) {
								if !succeeded {
									budgetReservation.settle(c.estimate)
									return
								}
								actual := c.estimate
								if u != nil {
									if u.TotalTokens > 0 {
										actual = u.TotalTokens
									} else if u.PromptTokens+u.CompletionTokens > 0 {
										actual = u.PromptTokens + u.CompletionTokens
									}
									if u.PromptTokens > 0 {
										r.recordTokenUsage(c.target.Provider, c.target.Model, len(c.payload["messages"]), u.PromptTokens)
									}
								}
								budgetReservation.settle(actual)
							}
							lockHeld = false
							cleanupOnReturn = false
							stream, streamUsage = startSSEStream(first.Reader, upResp.Body, lease, c.estimate, hints, requestCtx, streamCleanup, budgetFinish)
							headers := copyHeaders(upResp.Header)
							return &Response{KeyID: lease.KeyID(), Status: upResp.StatusCode, Header: headers, Stream: stream, StreamUsage: streamUsage, Outcome: domain.Success, Target: c.target.Provider + ":" + c.target.Model, KeyLabel: lease.Label(), Attempts: totalAttempts, QueueMS: queueMS, TTFB: ttfb, Fallback: fallbackAttempted}, nil
						}
						_ = upResp.Body.Close()
					}
				} else {
					body, doErr = io.ReadAll(io.LimitReader(upResp.Body, 32<<20))
					_ = upResp.Body.Close()
					result = c.entry.adapter.Classify(upResp, body, doErr)
				}
				if hints == nil {
					hints = adapterHints(c.entry.adapter, &c.entry.spec, headerOrEmpty(upResp), r.clock.Now())
				}
				if result.Outcome == classify.Success {
					actual := usageTotal(body)
					if actual <= 0 {
						actual = c.estimate
					}
					if promptTokens := usagePrompt(body); promptTokens > 0 {
						r.recordTokenUsage(c.target.Provider, c.target.Model, len(c.payload["messages"]), promptTokens)
					}
					budgetReservation.settle(actual)
					lease.Success(actual, hints)
					headers := http.Header{}
					if upResp != nil {
						headers = copyHeaders(upResp.Header)
					}
					return &Response{KeyID: lease.KeyID(), Status: statusOr(upResp, 200), Header: headers, Body: body, Outcome: domain.Success, Target: c.target.Provider + ":" + c.target.Model, KeyLabel: lease.Label(), Attempts: totalAttempts, QueueMS: queueMS, TTFB: ttfb, Fallback: fallbackAttempted}, nil
				}
				budgetCharge := 0
				if result.Outcome == classify.ClientError {
					budgetCharge = c.estimate
				} else if result.Outcome == classify.Transient {
					// A falha de rede/5xx pode ocorrer depois de o provedor aceitar a chamada.
					budgetCharge = usageTotal(body)
					if budgetCharge <= 0 {
						budgetCharge = c.estimate
					}
				}
				budgetReservation.settle(budgetCharge)
				switch result.Outcome {
				case classify.AuthInvalid:
					lease.AuthFailed()
				case classify.QuotaExhausted:
					lease.QuotaExhausted(hints)
				case classify.RateLimitedShort, classify.RateLimitedLong:
					lease.RateLimited(result.RetryAfter, result.RetryKnown, hints)
				case classify.Transient:
					if requestCtx.Err() != nil {
						lease.Release()
						return nil, routeFailure(requestCtx.Err(), totalAttempts, lastTarget, fallbackAttempted, "upstream")
					}
					lease.Failed()
				case classify.ClientError:
					lease.Success(c.estimate, hints)
				case classify.Forbidden:
					lease.Forbidden()
				case classify.Canceled:
					lease.Release()
					return nil, routeFailure(requestCtx.Err(), totalAttempts, lastTarget, fallbackAttempted, "upstream")
				default:
					lease.Release()
				}
				lastErr = fmt.Errorf("upstream %s: %s", c.target.Provider+":"+c.target.Model, secret.Redact(result.Message))
				body = []byte(secret.Redact(string(body)))
				finalStatus := statusOr(upResp, result.Status)
				if finalStatus >= 200 && finalStatus < 300 && result.Outcome != classify.ClientError {
					finalStatus = http.StatusBadGateway
				}
				last = &Response{KeyID: lease.KeyID(), Status: finalStatus, Header: copyHeaders(headerOrEmpty(upResp)), Body: body, Outcome: result.Outcome, Target: c.target.Provider + ":" + c.target.Model, KeyLabel: lease.Label(), Attempts: totalAttempts, QueueMS: queueMS, TTFB: ttfb, Fallback: fallbackAttempted}
				if result.Outcome == classify.RateLimitedShort {
					shortLimited = true
				}
				if result.Outcome == classify.RateLimitedLong {
					if until, exhausted := c.entry.pool.LongLimited(acq); exhausted {
						d := until.Sub(r.clock.Now())
						if longRetry < 0 || d < longRetry {
							longRetry = d
						}
						break
					}
					// A resposta longa bloqueia esta chave; teste outra chave do mesmo alvo.
					continue
				}
				if result.Outcome == classify.ClientError || result.Outcome == classify.ModelUnavailable {
					if result.Outcome == classify.ModelUnavailable {
						r.deadUntil[c.target.Provider+":"+c.target.Model] = r.clock.Now().Add(30 * time.Minute)
					}
					break
				}
				if result.Outcome == classify.Transient {
					if attempt < 7 {
						// Outra chave não corrige a latência/5xx deste modelo; preserve
						// o prazo restante para o próximo alvo antes de repetir o mesmo.
						if canSpill {
							break
						}
						wait, available := c.entry.pool.EstimateWait(acq)
						if available && wait == 0 {
							continue // outra chave livre: não atrasar o fallback por backoff
						}
						backoff := time.Second * time.Duration(1<<minRouterInt(attempt, 5))
						if backoff > 30*time.Second {
							backoff = 30 * time.Second
						}
						if available && wait > backoff {
							backoff = wait
						}
						if err := r.clock.Wait(requestCtx, backoff, nil); err != nil {
							lastErr = err
							break
						}
						continue
					}
					break
				}
				// Auth, quota e 429 curto tentam outra chave do mesmo provedor.
				if result.Outcome == classify.AuthInvalid || result.Outcome == classify.QuotaExhausted || result.Outcome == classify.RateLimitedShort || result.Outcome == classify.Forbidden {
					continue
				}
				break
			}
		}
		if (shortLimited || (last != nil && last.Outcome == domain.RateLimitedShort)) && round < 4 {
			wait := time.Duration(-1)
			for _, c := range candidates {
				acq := sched.AcquireRequest{Model: c.target.Model, Tokens: c.estimate, Priority: priority, Reserve: reserveFor(project, priority), Allow: func(id sched.KeyID) bool {
					return c.allowed[id]
				}}
				d, ok := c.entry.pool.EstimateWait(acq)
				if ok && d >= 0 && (wait < 0 || d < wait) {
					wait = d
				}
			}
			if wait < 500*time.Millisecond {
				wait = 500 * time.Millisecond
			}
			wait += time.Duration(maxPriority(priority)) * 400 * time.Millisecond
			if wait > 65*time.Second {
				wait = 65 * time.Second
			}
			if err := r.clock.Wait(requestCtx, wait, nil); err != nil {
				lastErr = err
				break
			}
			continue
		}
		break
	}
	if errors.Is(lastErr, context.DeadlineExceeded) || errors.Is(lastErr, context.Canceled) {
		return nil, routeFailure(lastErr, totalAttempts, lastTarget, fallbackAttempted, failurePhase(totalAttempts, false))
	}
	if errors.Is(lastErr, ErrQueueTimeout) {
		failure := routeFailure(lastErr, totalAttempts, lastTarget, fallbackAttempted, "queue")
		// Recalcule após a espera: a estimativa inicial já pode estar vencida.
		// Ocupação sem prazo conhecido não autoriza inventar um Retry-After.
		retryIn := time.Duration(-1)
		unknownWait := false
		for _, c := range candidates {
			wait, viable := c.entry.pool.EstimateWait(sched.AcquireRequest{
				Model: c.target.Model, Tokens: c.estimate, Priority: priority,
				Reserve: reserveFor(project, priority),
				Allow:   func(id sched.KeyID) bool { return c.allowed[id] },
			})
			if !viable {
				continue
			}
			if wait <= 0 {
				unknownWait = true
				break
			}
			if retryIn < 0 || wait < retryIn {
				retryIn = wait
			}
		}
		if !unknownWait && retryIn > 0 {
			failure.RetryAfter = retryIn
		}
		return nil, failure
	}
	if longRetry >= 0 {
		return nil, &ErrLongLimited{RetryIn: longRetry}
	}
	if last != nil && lastErr != nil {
		return last, nil
	}
	if lastErr == nil {
		lastErr = ErrNoTarget
	}
	return nil, lastErr
}

func failurePhase(attempts int, routing bool) string {
	if attempts > 0 {
		return "upstream"
	}
	if routing {
		return "routing"
	}
	return "queue"
}

func routeFailure(cause error, attempts int, target string, fallback bool, phase string) *RouteFailure {
	code, status, retryable := "keypool_internal_error", http.StatusBadGateway, false
	if errors.Is(cause, ErrQueueTimeout) {
		code, status, retryable, phase = "keypool_queue_timeout", http.StatusTooManyRequests, true, "queue"
	} else if errors.Is(cause, context.DeadlineExceeded) {
		code, status = "keypool_timeout", http.StatusRequestTimeout
	} else if errors.Is(cause, context.Canceled) {
		code, status = "keypool_request_cancelled", 499
	}
	if phase == "" {
		phase = failurePhase(attempts, false)
	}
	return &RouteFailure{Code: code, Phase: phase, StatusCode: status, UpstreamTarget: target,
		Attempts: attempts, Fallback: fallback, Retryable: retryable,
		RetryabilityKnown: errors.Is(cause, ErrQueueTimeout), Cause: cause}
}

func usagePrompt(b []byte) int {
	var v struct {
		Usage struct {
			Prompt int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &v) != nil {
		return 0
	}
	return v.Usage.Prompt
}

func minRouterInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func clonePayload(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}
func filterTargets(in []config.Target, keep func(config.Target) bool) []config.Target {
	out := make([]config.Target, 0, len(in))
	for _, t := range in {
		if keep(t) {
			out = append(out, t)
		}
	}
	return out
}
func supportsRequires(spec provider.Spec, model string, req map[string]bool) bool {
	for capability, required := range req {
		if !required {
			continue
		}
		switch capability {
		case "reasoning_effort":
			ok := false
			for _, pattern := range spec.Capabilities.ReasoningEffortModels {
				if limitsGlob(pattern, model) {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		case "stream_include_usage":
			if !spec.Capabilities.StreamIncludeUsage {
				return false
			}
		case "stream":
			if spec.Type == "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}
func SupportsRequires(spec provider.Spec, model string, req map[string]bool) bool {
	return supportsRequires(spec, model, req)
}

func limitsGlob(pattern, model string) bool {
	return limits.GlobMatch(strings.ToLower(pattern), strings.ToLower(model))
}
func avoidedWeight(weights []string, model string) bool {
	id := model
	if i := strings.IndexByte(id, ':'); i >= 0 {
		id = id[:i]
	}
	for _, v := range weights {
		if strings.EqualFold(strings.TrimSpace(v), id) {
			return true
		}
	}
	return false
}

func adapterHints(adapter provider.Adapter, spec *provider.Spec, headers http.Header, now time.Time) *sched.Hints {
	if configurable, ok := adapter.(interface {
		HintsFor(*provider.Spec, http.Header, time.Time) *sched.Hints
	}); ok {
		return configurable.HintsFor(spec, headers, now)
	}
	return adapter.Hints(headers, now)
}

func startSSEStream(source io.Reader, upstream io.ReadCloser, lease *sched.Lease, estimated int, hints *sched.Hints, ctx context.Context, cleanup func(), budgetFinish func(*sse.Usage, bool)) (*streamBody, *StreamUsage) {
	pr, pw := io.Pipe()
	usage := &StreamUsage{done: make(chan struct{})}
	closed := &atomic.Bool{}
	var cleanupOnce sync.Once
	finish := func() { cleanupOnce.Do(cleanup) }
	body := &streamBody{PipeReader: pr, closeFn: func() { _ = upstream.Close() }, closed: closed}
	go func() {
		defer finish()
		sr := sse.NewReader(source)
		var last *sse.Usage
		sawDone := false
		succeeded := false
		for {
			ev, err := sr.NextRaw()
			if err == io.EOF {
				succeeded = sawDone
				if !succeeded {
					err = io.ErrUnexpectedEOF
				}
				if succeeded {
					break
				}
			}
			if err != nil {
				if closed.Load() || ctx.Err() != nil {
					lease.Release()
				} else {
					lease.Failed()
				}
				if budgetFinish != nil {
					budgetFinish(last, false)
				}
				if closed.Load() || ctx.Err() != nil {
					usage.finish(last, domain.Canceled)
				} else {
					usage.finish(last, domain.Transient)
				}
				_ = pw.CloseWithError(err)
				_ = upstream.Close()
				return
			}
			isDone := sse.IsDoneRaw(ev)
			if isDone {
				sawDone = true
			}
			if len(ev.Data) > 0 && !isDone {
				var v struct {
					Usage *sse.Usage `json:"usage"`
				}
				if json.Unmarshal(ev.Data, &v) == nil && v.Usage != nil {
					u := *v.Usage
					last = &u
				}
			}
			if _, err = pw.Write(ev.Raw); err != nil {
				lease.Release()
				if budgetFinish != nil {
					budgetFinish(last, false)
				}
				usage.finish(last, domain.Canceled)
				_ = upstream.Close()
				return
			}
		}
		_ = upstream.Close()
		actual := estimated
		if last != nil {
			actual = last.TotalTokens
			if actual <= 0 {
				actual = last.PromptTokens + last.CompletionTokens
			}
			if actual <= 0 {
				actual = estimated
			}
		}
		if succeeded {
			lease.Success(actual, hints)
		} else {
			lease.Failed()
		}
		if budgetFinish != nil {
			budgetFinish(last, succeeded)
		}
		if succeeded {
			usage.finish(last, domain.Success)
		} else {
			usage.finish(last, domain.Transient)
		}
		_ = pw.Close()
	}()
	return body, usage
}

var (
	ErrProjectForbidden     = errors.New("projeto inexistente ou desabilitado")
	ErrLeaseForbidden       = errors.New("modo lease não permitido para este projeto")
	ErrPromptTooLargeForTPM = errors.New("prompt grande demais para o limite de TPM de todos os alvos")
	ErrUnknownRoute         = errors.New("alias de rota desconhecido")
	ErrNoTarget             = errors.New("nenhum alvo disponível")
)

// ErrLongLimited indica que todos os alvos restantes foram esgotados por 429 longo.
type ErrLongLimited struct{ RetryIn time.Duration }

func (e *ErrLongLimited) Error() string {
	return fmt.Sprintf("router: todos os alvos estão em cooldown longo por %s", e.RetryIn)
}
func maxPriority(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func mergePayload(defaults map[string][]byte, body map[string]json.RawMessage, force map[string][]byte) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range defaults {
		out[k] = append(json.RawMessage(nil), v...)
	}
	for k, v := range body {
		out[k] = append(json.RawMessage(nil), v...)
	}
	for k, v := range force {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}
func excluded(ex []string, t config.Target) bool {
	for _, v := range ex {
		v = strings.TrimSpace(v)
		if v == t.Provider+":"+t.Model || v == t.Model {
			return true
		}
	}
	return false
}
func isStream(p map[string]json.RawMessage) bool {
	var b bool
	_ = json.Unmarshal(p["stream"], &b)
	return b
}
func reserveFor(p config.ProjectSnapshot, priority int) float64 {
	if priority <= p.Policy.ReservePriorityCutoff {
		return 0
	}
	return p.Policy.TPDReserve
}
func fitBudget(pool *sched.Pool, model string, p map[string]json.RawMessage, spec *provider.Spec) (int, error) {
	return fitBudgetFactor(pool, model, p, spec, 1)
}

func fitBudgetFactor(pool *sched.Pool, model string, p map[string]json.RawMessage, spec *provider.Spec, factor float64) (int, error) {
	if factor <= 0 {
		factor = 1
	}
	factor = clampFactor(factor)
	rawPrompt := (len(p["messages"]) + 3) / 4
	prompt := int(float64(rawPrompt)*factor + 0.999999)
	want := 1024
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v := p[k]; len(v) > 0 {
			var n int
			if json.Unmarshal(v, &n) == nil && n > 0 {
				want = n
				break
			}
		}
	}
	resolved := pool.LimitsFor(model)
	if resolved.TPM != nil {
		room := *resolved.TPM - prompt - 100
		if want > room {
			if room < 1024 {
				return 0, ErrPromptTooLargeForTPM
			}
			want = room
		}
	}
	field := "max_tokens"
	if spec != nil && spec.Capabilities.MaxTokensField != "" {
		field = spec.Capabilities.MaxTokensField
	}
	if field == "max_completion_tokens" {
		delete(p, "max_tokens")
	} else {
		delete(p, "max_completion_tokens")
	}
	p[field] = json.RawMessage(strconv.Itoa(want))
	return prompt + want, nil
}
func usageTotal(b []byte) int {
	var v struct {
		Usage struct {
			Total      int `json:"total_tokens"`
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &v) != nil {
		return 0
	}
	if v.Usage.Total > 0 {
		return v.Usage.Total
	}
	return v.Usage.Prompt + v.Usage.Completion
}
func statusOr(r *http.Response, def int) int {
	if r == nil {
		return def
	}
	return r.StatusCode
}
func headerOrEmpty(r *http.Response) http.Header {
	if r == nil {
		return http.Header{}
	}
	return r.Header
}
func copyHeaders(h http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"Content-Type", "Cache-Control", "Retry-After"} {
		if v := h.Values(k); len(v) > 0 {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}
