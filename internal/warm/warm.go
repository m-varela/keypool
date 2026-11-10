// Package warm implements bounded, idle-only connection and credential probes.
package warm

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"keypool/internal/classify"
	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/domain"
	"keypool/internal/provider"
	"keypool/internal/router"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/store"
)

var (
	ErrNotFound = errors.New("warm: provedor ou chave não encontrada")
	ErrBusy     = errors.New("warm: chave ocupada por tráfego real ou outra sonda")
)

type ProbeResult struct {
	Provider       string           `json:"provider"`
	KeyID          string           `json:"key_id"`
	Layer          string           `json:"layer"`
	OK             bool             `json:"ok"`
	Outcome        classify.Outcome `json:"outcome,omitempty"`
	HTTPStatus     int              `json:"http_status,omitempty"`
	LatencyMS      int64            `json:"latency_ms"`
	Message        string           `json:"message,omitempty"`
	Models         []string         `json:"models,omitempty"`
	Model          string           `json:"model,omitempty"`
	AccountInfo    map[string]any   `json:"account_info,omitempty"`
	RetryAfter     time.Duration    `json:"retry_after,omitempty"`
	RetryKnown     bool             `json:"retry_known,omitempty"`
	TokenAttempted bool             `json:"token_attempted,omitempty"`
	Connection     *ConnectionStats `json:"connection,omitempty"`
	At             time.Time        `json:"at"`
}

type ConnectionStats struct {
	DNSMS     int64 `json:"dns_ms"`
	ConnectMS int64 `json:"connect_ms"`
	TLSMS     int64 `json:"tls_ms"`
	TTFBMS    int64 `json:"ttfb_ms"`
	Reused    bool  `json:"reused"`
	Warm      bool  `json:"warm"`
}

type KeyStatus struct {
	Provider          string                   `json:"provider"`
	KeyID             string                   `json:"key_id"`
	CredentialVersion uint64                   `json:"credential_version,omitempty"`
	Label             string                   `json:"label,omitempty"`
	Health            string                   `json:"status"`
	Disabled          bool                     `json:"disabled"`
	Inflight          int                      `json:"inflight"`
	Warming           bool                     `json:"warming"`
	LastActivity      time.Time                `json:"last_activity,omitempty"`
	LastOKAt          time.Time                `json:"last_ok_at,omitempty"`
	LastL0At          time.Time                `json:"last_l0_at,omitempty"`
	LastL1At          time.Time                `json:"last_l1_at,omitempty"`
	LastL2At          time.Time                `json:"last_l2_at,omitempty"`
	Catalog           []string                 `json:"catalog,omitempty"`
	AccountInfo       map[string]any           `json:"account_info,omitempty"`
	CatalogSeenAt     time.Time                `json:"catalog_seen_at,omitempty"`
	LastError         string                   `json:"last_error,omitempty"`
	LastProbe         *ProbeResult             `json:"last_probe,omitempty"`
	Connection        *ConnectionStats         `json:"connection,omitempty"`
	HintObservedAt    time.Time                `json:"hint_observed_at,omitempty"`
	Hints             map[string]sched.Hints   `json:"hints,omitempty"`
	TTFBEWMA          map[string]time.Duration `json:"ttfb_ewma,omitempty"`
	TotalEWMA         map[string]time.Duration `json:"total_ewma,omitempty"`
	LastL0Age         time.Duration            `json:"last_l0_age,omitempty"`
	LastL1Age         time.Duration            `json:"last_l1_age,omitempty"`
	LastL2Age         time.Duration            `json:"last_l2_age,omitempty"`
}

type Status struct {
	Enabled       bool        `json:"enabled"`
	Ready         bool        `json:"ready"`
	ReadyReason   string      `json:"ready_reason,omitempty"`
	Workers       int         `json:"workers"`
	ActiveWorkers int         `json:"active_workers"`
	Queued        int         `json:"queued"`
	Keys          []KeyStatus `json:"keys"`
	LastUpdated   time.Time   `json:"last_updated"`
}

type jobKind string

const (
	layerL0 jobKind = "L0"
	layerL1 jobKind = "L1"
	layerL2 jobKind = "L2"
)

type job struct {
	provider string
	key      sched.KeyID
	kind     jobKind
	at       time.Time
	seq      uint64
	index    int
}
type jobQueue []*job

func (q jobQueue) Len() int { return len(q) }
func (q jobQueue) Less(i, j int) bool {
	if q[i].at.Equal(q[j].at) {
		return q[i].seq < q[j].seq
	}
	return q[i].at.Before(q[j].at)
}
func (q jobQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i]; q[i].index = i; q[j].index = j }
func (q *jobQueue) Push(x any)   { item := x.(*job); item.index = len(*q); *q = append(*q, item) }
func (q *jobQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*q = old[:n-1]
	return item
}

type jobID struct {
	provider string
	key      sched.KeyID
	kind     jobKind
}

func (id jobID) String() string { return id.provider + "/" + string(id.key) + "/" + string(id.kind) }

type Manager struct {
	mu             sync.Mutex
	cfg            config.WarmConfig
	router         *router.Router
	repo           store.Store
	recorder       store.WarmStore
	clk            clock.Clock
	usageSink      func(domain.UsageEvent)
	eventSink      func(sched.Event)
	onReady        func(bool, string)
	ready          bool
	readyReason    string
	readinessEpoch uint64
	started        bool
	closed         bool
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	workers        chan job
	queue          jobQueue
	scheduled      map[jobID]time.Time
	sequence       uint64
	active         map[string]bool
	statuses       map[string]*KeyStatus
	firstPending   map[string]bool
	activeWorkers  int
	queuedJobs     int
	counter        atomic.Uint64
	daily          map[string]dailyBudget
}

type dailyBudget struct {
	day    string
	tokens int
	loaded bool
}

func New(cfg config.WarmConfig, rt *router.Router, repo store.Store) *Manager {
	return NewWithClock(cfg, rt, repo, clock.Real())
}

func NewWithClock(cfg config.WarmConfig, rt *router.Router, repo store.Store, clk clock.Clock) *Manager {
	if clk == nil {
		clk = clock.Real()
	}
	m := &Manager{cfg: cfg, router: rt, repo: repo, clk: clk, workers: make(chan job, 16), scheduled: map[jobID]time.Time{}, active: map[string]bool{}, statuses: map[string]*KeyStatus{}, firstPending: map[string]bool{}, daily: map[string]dailyBudget{}}
	if recorder, ok := repo.(store.WarmStore); ok {
		m.recorder = recorder
	}
	heap.Init(&m.queue)
	return m
}

func (m *Manager) SetUsageSink(fn func(domain.UsageEvent)) {
	m.mu.Lock()
	m.usageSink = fn
	m.mu.Unlock()
}
func (m *Manager) SetEventSink(fn func(sched.Event)) { m.mu.Lock(); m.eventSink = fn; m.mu.Unlock() }
func (m *Manager) SetOnReady(fn func(bool, string)) {
	m.mu.Lock()
	m.onReady = fn
	ready, reason := m.ready, m.readyReason
	m.mu.Unlock()
	if fn != nil && ready {
		fn(true, reason)
	}
}

// SetConfig updates scheduling and readiness policy after an atomic config apply.
func (m *Manager) SetConfig(cfg config.WarmConfig) {
	m.mu.Lock()
	previous := m.cfg
	wasEnabled, wasL1 := previous.Enabled, previous.L1Enabled
	m.cfg = cfg
	started := m.started && !m.closed
	becameReady := false
	readinessEpoch := m.readinessEpoch
	needInitial := cfg.Enabled && cfg.L1Enabled && (!wasEnabled || !wasL1)
	if cfg.Enabled && cfg.L1Enabled && !needInitial {
		names := map[string]bool{}
		for name := range previous.Providers {
			names[name] = true
		}
		for name := range cfg.Providers {
			names[name] = true
		}
		for name := range names {
			if !previous.ForProvider(name).L1Enabled && cfg.ForProvider(name).L1Enabled {
				needInitial = true
				break
			}
		}
	}
	if !cfg.Enabled || !cfg.L1Enabled {
		m.readinessEpoch++
		readinessEpoch = m.readinessEpoch
		m.firstPending = map[string]bool{}
		if !m.ready {
			becameReady = true
		}
		m.ready = true
		if !cfg.Enabled {
			m.readyReason = "warm desativado"
		} else {
			m.readyReason = "L1 desabilitada"
		}
	} else if needInitial && started {
		m.readinessEpoch++
		readinessEpoch = m.readinessEpoch
		m.ready = false
		m.readyReason = "primeira passada L1 após alteração de configuração"
	}
	fn, reason := m.onReady, m.readyReason
	m.mu.Unlock()
	if becameReady && fn != nil {
		fn(true, reason)
	}
	if started && cfg.Enabled {
		if needInitial && fn != nil {
			fn(false, reason)
		}
		m.syncTargets(needInitial)
		if needInitial {
			m.mu.Lock()
			ctx := m.ctx
			if m.closed || ctx == nil {
				ctx = nil
			} else {
				// Add while holding mu so Close cannot start Wait concurrently.
				m.wg.Add(1)
			}
			m.mu.Unlock()
			if ctx != nil {
				go m.readinessTimeout(ctx, cfg.ReadyTimeout, readinessEpoch)
			}
		}
	}
}
func (m *Manager) Start(parent context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	if m.closed {
		m.mu.Unlock()
		return errors.New("warm: manager encerrado")
	}
	m.started = true
	if parent == nil {
		parent = context.Background()
	}
	m.ctx, m.cancel = context.WithCancel(parent)
	ctx := m.ctx
	cfg := m.cfg
	readinessEpoch := m.readinessEpoch
	// Account for every goroutine before releasing mu, so Close cannot race a
	// later WaitGroup.Add and return while Start is still launching workers.
	const maxWorkers = 4
	workerGoroutines := maxWorkers
	if workerGoroutines < 1 {
		workerGoroutines = 1
	}
	readinessGoroutines := 0
	if cfg.Enabled && cfg.L1Enabled {
		readinessGoroutines = 1
	}
	m.wg.Add(workerGoroutines + 1 + readinessGoroutines)
	m.mu.Unlock()
	if cfg.Enabled {
		// Startup restores quota windows, but credential validation/catalogs are
		// intentionally re-established by the first L1 pass for the current keys.
		m.syncTargets(true)
	}
	for i := 0; i < workerGoroutines; i++ {
		go m.workerLoop(ctx)
	}
	go m.schedulerLoop(ctx)
	if cfg.Enabled && cfg.L1Enabled {
		go m.readinessTimeout(ctx, cfg.ReadyTimeout, readinessEpoch)
	} else {
		m.signalReady(true, "warm desativado ou L1 desabilitada")
	}
	m.checkFirstPass()
	return nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

func (m *Manager) syncTargets(initial bool) {
	if m.router == nil {
		return
	}
	providers := m.router.WarmProviders()
	now := m.clk.Now()
	type catalogInvalidation struct {
		provider string
		keyID    sched.KeyID
	}
	var invalidations []catalogInvalidation
	m.mu.Lock()
	present := map[string]bool{}
	for _, p := range providers {
		pcfg := m.cfg.ForProvider(p.Name)
		if !pcfg.Enabled {
			continue
		}
		poolStats := make(map[sched.KeyID]sched.KeyStats)
		for _, stats := range p.Pool.Stats() {
			poolStats[stats.ID] = stats
		}
		for _, k := range p.Keys {
			keyName := p.Name + "/" + string(k.ID)
			present[keyName] = true
			statusKey := keyName
			if !pcfg.L1Enabled {
				delete(m.firstPending, statusKey)
			}
			st := m.statuses[statusKey]
			newKey := st == nil
			if st == nil {
				st = &KeyStatus{Provider: p.Name, KeyID: string(k.ID), Label: k.Label, Health: "unknown"}
				m.statuses[statusKey] = st
			}
			st.Label = k.Label
			keyStats := poolStats[k.ID]
			credentialChanged := !newKey && st.CredentialVersion != 0 && keyStats.CredentialVersion != 0 && st.CredentialVersion != keyStats.CredentialVersion
			st.CredentialVersion = keyStats.CredentialVersion
			invalidateCatalog := credentialChanged || newKey
			if invalidateCatalog {
				// No health/catalog data learned with a previous process or
				// credential generation may be trusted before the first L1 pass.
				if pcfg.L1Enabled && p.Spec.Probe.Kind != "none" {
					// Block fixed-model routing until the current credential has
					// supplied a fresh catalog. A disabled L1 must not strand keys.
					p.Pool.SetCatalogAt(k.ID, nil, now)
					keyStats.CatalogSeenAt = now
					keyStats.CatalogKnown = true
				} else {
					p.Pool.ClearCatalog(k.ID)
					keyStats.CatalogSeenAt = time.Time{}
					keyStats.CatalogKnown = false
				}
				st.Catalog = nil
				st.CatalogSeenAt = keyStats.CatalogSeenAt
				st.AccountInfo = nil
				st.LastOKAt = time.Time{}
				st.LastL0At, st.LastL1At, st.LastL2At = time.Time{}, time.Time{}, time.Time{}
				st.LastProbe = nil
				st.LastError = ""
				invalidations = append(invalidations, catalogInvalidation{provider: p.Name, keyID: k.ID})
			}
			if catalog, known := p.Pool.Catalog(k.ID); known {
				st.Catalog = catalog
				st.CatalogSeenAt = keyStats.CatalogSeenAt
			}
			if pcfg.L1Enabled && p.Spec.Probe.Kind != "none" && (initial || newKey || credentialChanged) {
				id := jobID{provider: p.Name, key: k.ID, kind: layerL1}
				m.firstPending[statusKey] = true
				m.pushLocked(id, now)
			} else if p.Spec.Probe.Kind == "none" {
				delete(m.firstPending, statusKey)
				delete(m.scheduled, jobID{provider: p.Name, key: k.ID, kind: layerL1})
			}
			if pcfg.L0Enabled && !keyStats.Disabled && keyStats.Health != "invalid" && !m.active[keyName] {
				id := jobID{provider: p.Name, key: k.ID, kind: layerL0}
				if _, ok := m.scheduled[id]; !ok {
					m.pushLocked(id, now.Add(2*time.Second))
				}
			}
			if pcfg.L2Enabled && !keyStats.Disabled && keyStats.Health != "invalid" && !m.active[keyName] {
				id := jobID{provider: p.Name, key: k.ID, kind: layerL2}
				if _, ok := m.scheduled[id]; !ok {
					m.pushLocked(id, now.Add(m.cfg.L2.Interval))
				}
			}
		}
	}
	for key := range m.firstPending {
		if !present[key] {
			delete(m.firstPending, key)
		}
	}
	for key := range m.statuses {
		if !present[key] {
			delete(m.statuses, key)
		}
	}
	for id := range m.scheduled {
		keyName := id.provider + "/" + string(id.key)
		pcfg := m.cfg.ForProvider(id.provider)
		allowed := present[keyName] && pcfg.Enabled
		switch id.kind {
		case layerL0:
			allowed = allowed && pcfg.L0Enabled
		case layerL1:
			allowed = allowed && pcfg.L1Enabled && pSpecProbeEnabled(providers, id.provider)
		case layerL2:
			allowed = allowed && pcfg.L2Enabled
		}
		if !allowed {
			delete(m.scheduled, id)
		}
	}
	m.compactQueueLocked()
	m.checkFirstPassLocked()
	recorder := m.recorder
	m.mu.Unlock()
	if len(invalidations) > 0 {
		m.router.MarkStateDirty()
	}
	if recorder != nil {
		for _, item := range invalidations {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := recorder.DeleteModelCatalog(ctx, item.provider, item.keyID); err != nil {
				m.reportEvent(item.provider, "warm.catalog.invalidate_error", string(item.keyID), err.Error())
			}
			cancel()
		}
	}
}

func pSpecProbeEnabled(providers []router.WarmProvider, name string) bool {
	for _, item := range providers {
		if item.Name == name {
			return item.Spec.Probe.Kind != "none"
		}
	}
	return false
}

// compactQueueLocked discards stale heap nodes left by earlier reschedules.
func (m *Manager) compactQueueLocked() {
	queue := make(jobQueue, 0, len(m.scheduled))
	for id, at := range m.scheduled {
		m.sequence++
		queue = append(queue, &job{provider: id.provider, key: id.key, kind: id.kind, at: at, seq: m.sequence})
	}
	m.queue = queue
	heap.Init(&m.queue)
}

func (m *Manager) pushLocked(id jobID, at time.Time) {
	if old, ok := m.scheduled[id]; ok && !at.Before(old) {
		return
	}
	m.sequence++
	m.scheduled[id] = at
	heap.Push(&m.queue, &job{provider: id.provider, key: id.key, kind: id.kind, at: at, seq: m.sequence})
}

func (m *Manager) schedulerLoop(ctx context.Context) {
	defer m.wg.Done()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	refresh := time.NewTicker(5 * time.Second)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			m.syncTargets(false)
		case <-ticker.C:
			m.dispatchDue()
		}
	}
}

func (m *Manager) dispatchDue() {
	now := m.clk.Now()
	for {
		m.mu.Lock()
		slots := m.cfg.Workers - m.activeWorkers - m.queuedJobs
		if slots <= 0 || !m.cfg.Enabled || len(m.queue) == 0 || m.queue[0].at.After(now) {
			m.mu.Unlock()
			return
		}
		item := heap.Pop(&m.queue).(*job)
		id := jobID{provider: item.provider, key: item.key, kind: item.kind}
		if at, ok := m.scheduled[id]; !ok || !at.Equal(item.at) {
			m.mu.Unlock()
			continue
		}
		delete(m.scheduled, id)
		m.queuedJobs++
		m.mu.Unlock()
		select {
		case m.workers <- *item:
		default:
			m.mu.Lock()
			m.queuedJobs--
			m.pushLocked(id, now.Add(time.Second))
			m.mu.Unlock()
			return
		}
	}
}

func (m *Manager) workerLoop(ctx context.Context) {
	defer m.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-m.workers:
			m.executeScheduled(ctx, item)
		}
	}
}

func (m *Manager) executeScheduled(ctx context.Context, item job) {
	keyName := item.provider + "/" + string(item.key)
	m.mu.Lock()
	if m.queuedJobs > 0 {
		m.queuedJobs--
	}
	if m.active[keyName] {
		m.pushLocked(jobID{item.provider, item.key, item.kind}, m.clk.Now().Add(3*time.Second))
		m.mu.Unlock()
		return
	}
	m.active[keyName] = true
	m.activeWorkers++
	cfg := m.cfg
	m.mu.Unlock()
	started := m.clk.Now()
	result, err := m.runLayer(ctx, item.provider, item.key, item.kind, false)
	finished := m.clk.Now()
	m.mu.Lock()
	delete(m.active, keyName)
	m.activeWorkers--
	if st := m.statuses[keyName]; st != nil {
		if result != nil {
			st.LastProbe = result
			if result.OK {
				st.LastError = ""
			} else if result.Message != "" {
				st.LastError = result.Message
			}
			switch item.kind {
			case layerL0:
				st.LastL0At = result.At
				st.Connection = result.Connection
			case layerL1:
				st.LastL1At = result.At
				if result.OK {
					st.LastOKAt = result.At
				}
				if result.OK {
					// A successful L1 with an empty/unavailable model list is still
					// authoritative: fixed models must not inherit a stale catalog.
					st.Catalog = append([]string(nil), result.Models...)
					st.CatalogSeenAt = result.At
				}
				if result.AccountInfo != nil {
					st.AccountInfo = cloneAnyMap(result.AccountInfo)
				}
			case layerL2:
				st.LastL2At = result.At
			}
		}
		if err != nil {
			st.LastError = err.Error()
		}
	}
	if item.kind == layerL1 && !errors.Is(err, ErrBusy) {
		delete(m.firstPending, keyName)
		m.checkFirstPassLocked()
	}
	if result != nil && (result.Outcome == classify.AuthInvalid || (result.HTTPStatus == http.StatusForbidden && credentialDenied(result.Message))) {
		delete(m.scheduled, jobID{provider: item.provider, key: item.key, kind: layerL0})
		delete(m.scheduled, jobID{provider: item.provider, key: item.key, kind: layerL2})
		m.compactQueueLocked()
	}
	interval := time.Duration(0)
	if errors.Is(err, ErrBusy) {
		if p := m.provider(item.provider); p != nil {
			idleFor := cfg.IdleThreshold
			if item.kind == layerL2 {
				idleFor = cfg.L2.IdleInterval
			}
			interval = p.Pool.ProbeRetryAfterPolicy(item.key, idleFor, item.kind == layerL1)
		}
	} else {
		interval = m.nextInterval(cfg, item.kind, result, err, item.provider, item.key)
	}
	if interval > 0 {
		m.pushLocked(jobID{item.provider, item.key, item.kind}, finished.Add(interval))
	}
	m.mu.Unlock()
	_ = started
}

func (m *Manager) nextInterval(cfg config.WarmConfig, kind jobKind, result *ProbeResult, err error, providerName string, keyID sched.KeyID) time.Duration {
	pcfg := cfg.ForProvider(providerName)
	if !pcfg.Enabled {
		return 0
	}
	var base time.Duration
	switch kind {
	case layerL0:
		if !pcfg.L0Enabled {
			return 0
		}
		base = cfg.L0Interval
	case layerL1:
		if !pcfg.L1Enabled {
			return 0
		}
		base = cfg.L1Interval
	case layerL2:
		if !pcfg.L2Enabled {
			return 0
		}
		base = cfg.L2.Interval
	}
	retryFloor := time.Duration(0)
	if err != nil && result != nil && result.RetryKnown && result.RetryAfter > base {
		base = result.RetryAfter
		retryFloor = result.RetryAfter
	}
	if err != nil && result != nil && (result.Outcome == classify.Transient || result.HTTPStatus >= 500 || result.HTTPStatus == 0) {
		base = m.failureBackoff(providerName, keyID)
	}
	if kind == layerL1 {
		if p := m.provider(providerName); p != nil {
			if _, ok := findKey(*p, keyID); ok {
				_, _, _, health, _ := p.Pool.KeyActivity(keyID)
				if health == "invalid" {
					base = cfg.AuthRecheck
				}
			}
		}
	}
	if base <= 0 {
		return 0
	}
	jitter := cfg.L1Jitter
	if kind != layerL1 {
		jitter = 0.10
	}
	interval := base + jitterDuration(providerName+"/"+string(keyID)+"/"+string(kind)+"/"+m.clk.Now().Format("2006-01-02T15:04"), base, jitter)
	if interval < retryFloor {
		return retryFloor
	}
	return interval
}

func (m *Manager) failureBackoff(providerName string, keyID sched.KeyID) time.Duration {
	failures := 1
	if p := m.provider(providerName); p != nil {
		for _, stats := range p.Pool.Stats() {
			if stats.ID == keyID && stats.Errors > 0 {
				failures = stats.Errors
				if stats.Health == "open" {
					if remaining := p.Pool.ProbeRetryAfterPolicy(keyID, 0, true); remaining > 0 {
						return remaining
					}
				}
				break
			}
		}
	}
	delay := 30 * time.Second
	for n := 1; n < failures && delay < 10*time.Minute; n++ {
		delay *= 2
	}
	if delay > 10*time.Minute {
		return 10 * time.Minute
	}
	return delay
}

func jitterDuration(seed string, base time.Duration, factor float64) time.Duration {
	if factor <= 0 || base <= 0 {
		return 0
	}
	h := sha256.Sum256([]byte(seed))
	fraction := float64(uint16(h[0])<<8|uint16(h[1])) / 65535.0
	return time.Duration((fraction*2 - 1) * factor * float64(base))
}

func (m *Manager) readinessTimeout(ctx context.Context, timeout time.Duration, epoch uint64) {
	defer m.wg.Done()
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if err := m.clk.Wait(ctx, timeout, nil); err != nil {
		return
	}
	m.mu.Lock()
	done := len(m.firstPending) == 0
	current := epoch == m.readinessEpoch
	fn := m.onReady
	if !done && current {
		m.ready = true
		m.readyReason = "ready_timeout expirou antes da primeira passada L1"
	}
	reason := m.readyReason
	m.mu.Unlock()
	if !done && current && fn != nil {
		fn(true, reason)
	}
}

func (m *Manager) checkFirstPass() { m.mu.Lock(); m.checkFirstPassLocked(); m.mu.Unlock() }
func (m *Manager) checkFirstPassLocked() {
	if !m.cfg.Enabled || !m.cfg.L1Enabled || len(m.firstPending) != 0 {
		return
	}
	const completeReason = "primeira passada L1 concluída"
	changed := !m.ready || m.readyReason != completeReason
	m.ready = true
	m.readyReason = completeReason
	if changed && m.onReady != nil {
		fn := m.onReady
		go fn(true, completeReason)
	}
}
func (m *Manager) signalReady(ready bool, reason string) {
	m.mu.Lock()
	if !m.ready || reason != "" {
		m.ready = ready
		m.readyReason = reason
	}
	fn := m.onReady
	m.mu.Unlock()
	if fn != nil {
		fn(ready, reason)
	}
}

func (m *Manager) provider(name string) *router.WarmProvider {
	for _, p := range m.router.WarmProviders() {
		if p.Name == name {
			return &p
		}
	}
	return nil
}
func findKey(p router.WarmProvider, id sched.KeyID) (sched.KeySpec, bool) {
	for _, k := range p.Keys {
		if k.ID == id {
			return k, true
		}
	}
	return sched.KeySpec{}, false
}

func (m *Manager) runLayer(ctx context.Context, providerName string, keyID sched.KeyID, kind jobKind, force bool) (*ProbeResult, error) {
	p, releaseRuntime, ok := m.router.AcquireWarmProvider(providerName)
	if !ok {
		return nil, ErrNotFound
	}
	defer releaseRuntime()
	m.mu.Lock()
	pcfg := m.cfg.ForProvider(providerName)
	globalEnabled := m.cfg.Enabled
	l2Enabled := m.cfg.L2.Enabled
	m.mu.Unlock()
	if !globalEnabled || !pcfg.Enabled || (kind == layerL0 && !pcfg.L0Enabled) || (kind == layerL1 && !pcfg.L1Enabled) || (kind == layerL2 && !pcfg.L2Enabled) {
		message := "camada desabilitada"
		if kind == layerL2 && !l2Enabled {
			message = "L2 desativada"
		}
		return &ProbeResult{Provider: providerName, KeyID: string(keyID), Layer: string(kind), Message: message, At: m.clk.Now()}, errors.New(message)
	}
	key, ok := findKey(p, keyID)
	if !ok {
		return nil, ErrNotFound
	}
	m.mu.Lock()
	idleFor := m.cfg.IdleThreshold
	if kind == layerL2 {
		idleFor = m.cfg.L2.IdleInterval
	}
	m.mu.Unlock()
	releaseProbe, reserved := p.Pool.BeginProbe(keyID, idleFor, kind == layerL1, force)
	if !reserved {
		return nil, ErrBusy
	}
	defer releaseProbe()
	if kind == layerL0 && !force {
		now := m.clk.Now()
		for _, keyStats := range p.Pool.Stats() {
			if keyStats.Inflight > 0 || (!keyStats.LastActivity.IsZero() && now.Sub(keyStats.LastActivity) < idleFor) {
				return nil, ErrBusy
			}
		}
	}
	started := m.clk.Now()
	var result *ProbeResult
	var err error
	switch kind {
	case layerL0:
		result, err = m.runL0(ctx, p, key)
	case layerL1:
		result, err = m.runL1(ctx, p, key)
	case layerL2:
		result, err = m.runL2(ctx, p, key)
	default:
		return nil, fmt.Errorf("warm: camada inválida")
	}
	if result == nil {
		result = &ProbeResult{Provider: providerName, KeyID: string(keyID), Layer: string(kind), At: m.clk.Now()}
	}
	if result.LatencyMS == 0 {
		result.LatencyMS = m.clk.Now().Sub(started).Milliseconds()
	}
	if err != nil && result.Message == "" {
		result.Message = secret.Redact(err.Error())
	}
	result.Message = redactForKey(result.Message, key.Secret)
	m.persistHealth(ctx, p, key, *result)
	m.recordUsage(ctx, p, key, *result)
	m.router.MarkStateDirty()
	return result, err
}

func (m *Manager) runL0(ctx context.Context, p router.WarmProvider, key sched.KeySpec) (*ProbeResult, error) {
	started := m.clk.Now()
	out := &ProbeResult{Provider: p.Name, KeyID: string(key.ID), Layer: string(layerL0), At: started}
	probeCtx, cancel := context.WithTimeout(ctx, minDuration(p.Spec.TimeoutDuration(), 15*time.Second))
	defer cancel()
	var accountingLease *sched.Lease
	var err error
	if m.countsAgainstLimits() {
		accountingLease, err = p.Pool.TryAcquire(sched.AcquireRequest{Tokens: 0, Warm: true, Allow: func(id sched.KeyID) bool { return id == key.ID }})
		if err != nil {
			out.Message = err.Error()
			return out, err
		}
		accountingLease.MarkWarm()
	}
	req, err := provider.NewAuthenticatedGET(probeCtx, &p.Spec, key.Secret, p.Spec.Paths.Models)
	if err != nil {
		if accountingLease != nil {
			accountingLease.Release()
		}
		out.Message = redactForKey(err.Error(), key.Secret)
		p.Pool.RecordConnectionProbe(key.ID, false, m.clk.Now(), out.Message)
		return out, err
	}
	var traceMu sync.Mutex
	var dnsStart, connectStart, tlsStart time.Time
	var conn ConnectionStats
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			traceMu.Lock()
			dnsStart = m.clk.Now()
			traceMu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			now := m.clk.Now()
			traceMu.Lock()
			if !dnsStart.IsZero() {
				conn.DNSMS = now.Sub(dnsStart).Milliseconds()
			}
			traceMu.Unlock()
		},
		ConnectStart: func(_, _ string) {
			traceMu.Lock()
			connectStart = m.clk.Now()
			traceMu.Unlock()
		},
		ConnectDone: func(_, _ string, _ error) {
			now := m.clk.Now()
			traceMu.Lock()
			if !connectStart.IsZero() {
				conn.ConnectMS = now.Sub(connectStart).Milliseconds()
			}
			traceMu.Unlock()
		},
		TLSHandshakeStart: func() {
			traceMu.Lock()
			tlsStart = m.clk.Now()
			traceMu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			now := m.clk.Now()
			traceMu.Lock()
			if !tlsStart.IsZero() {
				conn.TLSMS = now.Sub(tlsStart).Milliseconds()
			}
			traceMu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			traceMu.Lock()
			conn.Reused = info.Reused
			traceMu.Unlock()
		},
		GotFirstResponseByte: func() {
			now := m.clk.Now()
			traceMu.Lock()
			conn.TTFBMS = now.Sub(started).Milliseconds()
			traceMu.Unlock()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := p.Client.Do(req)
	if err != nil {
		if accountingLease != nil {
			accountingLease.Release()
		}
		out.Message = redactForKey(err.Error(), key.Secret)
		p.Pool.RecordConnectionProbe(key.ID, false, m.clk.Now(), out.Message)
		return out, err
	}
	defer resp.Body.Close()
	out.HTTPStatus = resp.StatusCode
	out.RetryAfter, out.RetryKnown = classify.RetryAfter(resp.Header, m.clk.Now())
	if resp.StatusCode == http.StatusTooManyRequests {
		out.Outcome = classify.RateLimitedShort
	} else if resp.StatusCode == http.StatusUnauthorized {
		out.Outcome = classify.AuthInvalid
	} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		out.Outcome = classify.Success
	} else {
		out.Outcome = classify.Transient
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
	out.LatencyMS = m.clk.Now().Sub(started).Milliseconds()
	traceMu.Lock()
	conn.Warm = true
	connectionStats := conn
	traceMu.Unlock()
	out.Connection = &connectionStats
	hints := adapterHints(p.Adapter, &p.Spec, resp.Header, m.clk.Now())
	m.applyHints(p, key.ID, hints, nil)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out.Message = fmt.Sprintf("sonda de conexão HTTP %d", resp.StatusCode)
		if accountingLease != nil {
			if resp.StatusCode == http.StatusTooManyRequests {
				accountingLease.RateLimited(0, false, hints)
			} else if resp.StatusCode == http.StatusUnauthorized {
				accountingLease.AuthFailed()
			} else {
				accountingLease.Release()
			}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			p.Pool.RecordProbe(key.ID, false, true, m.clk.Now(), out.Message)
		} else if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
			p.Pool.RecordNeutralProbe(key.ID, m.clk.Now())
		} else {
			p.Pool.RecordConnectionProbe(key.ID, false, m.clk.Now(), out.Message)
		}
		return out, fmt.Errorf("%s", out.Message)
	}
	if accountingLease != nil {
		accountingLease.Success(0, hints)
	}
	p.Pool.RecordConnectionProbe(key.ID, true, m.clk.Now(), "")
	out.OK = true
	out.Message = "conexão aquecida"
	return out, nil
}

func (m *Manager) runL1(ctx context.Context, p router.WarmProvider, key sched.KeySpec) (*ProbeResult, error) {
	started := m.clk.Now()
	out := &ProbeResult{Provider: p.Name, KeyID: string(key.ID), Layer: string(layerL1), At: started}
	if p.Spec.Probe.Kind == "none" {
		out.Message = "L1 desabilitada pelo provedor (probe.kind=none)"
		return out, errors.New(out.Message)
	}
	probeCtx, cancel := context.WithTimeout(ctx, p.Spec.TimeoutDuration())
	defer cancel()
	var accountingLease *sched.Lease
	if m.countsAgainstLimits() {
		var acquireErr error
		accountingLease, acquireErr = p.Pool.TryAcquire(sched.AcquireRequest{Tokens: 0, Warm: true, Allow: func(id sched.KeyID) bool { return id == key.ID }})
		if acquireErr != nil {
			out.Message = acquireErr.Error()
			return out, acquireErr
		}
		accountingLease.MarkWarm()
	}
	probe := p.Adapter.Probe(probeCtx, p.Client, &p.Spec, key.Secret)
	out.HTTPStatus = probe.HTTPStatus
	out.Outcome = probe.Outcome
	out.Message = redactForKey(probe.Message, key.Secret)
	out.Models = append([]string(nil), probe.Models...)
	out.AccountInfo = cloneAnyMap(probe.AccountInfo)
	out.RetryAfter = probe.RetryAfter
	out.RetryKnown = probe.RetryKnown
	if probe.Headers != nil {
		m.applyHints(p, key.ID, probe.Hints, probe.Headers)
	} else {
		m.applyHints(p, key.ID, probe.Hints, nil)
	}
	if probe.OK && p.Spec.Probe.Kind == "key" && len(out.Models) == 0 {
		models, err := p.Adapter.ListModels(probeCtx, p.Client, &p.Spec, key.Secret)
		if err == nil {
			out.Models = models
		} else {
			out.Message = "credencial válida; catálogo indisponível: " + err.Error()
		}
	}
	out.LatencyMS = m.clk.Now().Sub(started).Milliseconds()
	if probe.OK {
		out.OK = true
		if accountingLease != nil {
			accountingLease.Success(0, probe.Hints)
		}
		p.Pool.RecordProbe(key.ID, true, false, m.clk.Now(), "")
		// Mark the catalog as known even when the provider returns no models or
		// its catalog endpoint is unavailable. Dynamic aliases remain possible
		// after validation; fixed models require an explicit catalog entry.
		p.Pool.SetCatalogAt(key.ID, out.Models, m.clk.Now())
		// L1 may reveal the catalog and limits in the same response. Apply the
		// hints again after catalog refresh so per-model buckets get populated.
		m.applyHints(p, key.ID, probe.Hints, probe.Headers)
		if m.recorder != nil {
			if err := m.recorder.SaveModelCatalog(ctx, p.Name, key.ID, out.Models, m.clk.Now()); err != nil {
				m.reportEvent(p.Name, "warm.catalog.persist_error", key.Label, err.Error())
			}
		}
		return out, nil
	}
	out.OK = false
	outcome := probe.Outcome
	invalid := outcome == classify.AuthInvalid || probe.HTTPStatus == http.StatusUnauthorized || (probe.HTTPStatus == http.StatusForbidden && credentialDenied(probe.Message))
	if invalid {
		if accountingLease != nil {
			accountingLease.AuthFailed()
		} else {
			p.Pool.RecordProbe(key.ID, false, true, m.clk.Now(), probe.Message)
		}
	} else if outcome == classify.RateLimitedShort || outcome == classify.RateLimitedLong {
		if accountingLease != nil {
			accountingLease.RateLimited(probe.RetryAfter, probe.RetryKnown, probe.Hints)
		} else {
			p.Pool.RecordNeutralProbe(key.ID, m.clk.Now())
		}
	} else {
		if accountingLease != nil {
			accountingLease.Release()
		}
		if outcome == classify.Transient || outcome == "" {
			p.Pool.RecordProbe(key.ID, false, false, m.clk.Now(), probe.Message)
		}
	}
	if outcome == classify.RateLimitedShort || outcome == classify.RateLimitedLong {
		out.RetryAfter = probe.RetryAfter
		out.RetryKnown = probe.RetryKnown
		if out.Message == "" {
			out.Message = "sonda limitada (HTTP 429)"
		}
		return out, fmt.Errorf("%s", out.Message)
	}
	if out.Message == "" {
		out.Message = "sonda L1 falhou"
	}
	return out, fmt.Errorf("%s", out.Message)
}

func (m *Manager) runL2(ctx context.Context, p router.WarmProvider, key sched.KeySpec) (*ProbeResult, error) {
	started := m.clk.Now()
	out := &ProbeResult{Provider: p.Name, KeyID: string(key.ID), Layer: string(layerL2), Model: "", At: started}
	m.mu.Lock()
	cfg := m.cfg.L2
	budgetID := p.Name + "/" + string(key.ID)
	budget := m.daily[budgetID]
	m.mu.Unlock()
	day := started.UTC().Format("2006-01-02")
	if budget.day != day {
		budget = dailyBudget{day: day}
	}
	if !budget.loaded {
		tokens, loadErr := m.loadPersistedL2Budget(ctx, p.Name, key.ID, started)
		if loadErr != nil {
			out.Message = "orçamento diário L2 indisponível: " + secret.Redact(loadErr.Error())
			return out, fmt.Errorf("%s", out.Message)
		}
		budget.tokens = tokens
		budget.loaded = true
	}
	out.Model = cfg.Model
	if !cfg.Enabled {
		out.Message = "L2 desativada"
		return out, errors.New(out.Message)
	}
	if budget.tokens >= cfg.DailyTokenBudget {
		out.Message = "orçamento diário L2 esgotado"
		return out, errors.New(out.Message)
	}
	if st := m.status(p.Name, key.ID); st != nil && accountIsFree(st.AccountInfo) {
		out.Message = "L2 bloqueada: provedor informou conta free"
		return out, errors.New(out.Message)
	}
	validated := false
	for _, stats := range p.Pool.Stats() {
		if stats.ID == key.ID {
			validated = stats.Validated && !stats.Disabled && stats.Health != "invalid"
			break
		}
	}
	if !validated {
		out.Message = "L2 bloqueada: credencial ainda não validada por L1"
		return out, errors.New(out.Message)
	}
	resolved := p.Pool.LimitsForModel(cfg.Model)
	if resolved.Free || resolved.RPD == nil || *resolved.RPD < cfg.MinRPD || learnedRPDRemainingBelow(p.Pool, key.ID, cfg.Model, cfg.MinRPD) {
		out.Message = "L2 bloqueada: bucket free, RPD baixo ou limite RPD desconhecido"
		return out, errors.New(out.Message)
	}
	if !p.Pool.ModelAllowed(key.ID, cfg.Model, provider.IsDynamic(cfg.Model)) {
		out.Message = "L2 bloqueada: modelo ausente do catálogo da chave"
		return out, errors.New(out.Message)
	}
	lease, err := p.Pool.TryAcquire(sched.AcquireRequest{Model: cfg.Model, Tokens: 1, Warm: true, Allow: func(id sched.KeyID) bool { return id == key.ID }})
	if err != nil {
		out.Message = err.Error()
		return out, err
	}
	lease.MarkWarm()
	payload := map[string]json.RawMessage{}
	payload["model"], _ = json.Marshal(cfg.Model)
	payload["messages"], _ = json.Marshal([]map[string]string{{"role": "user", "content": "ping"}})
	payload["max_tokens"], _ = json.Marshal(1)
	payload = p.Adapter.AdaptPayload(&p.Spec, cfg.Model, payload)
	req, err := p.Adapter.BuildRequest(ctx, &p.Spec, key.Secret, &provider.Canonical{Payload: payload})
	if err != nil {
		lease.Release()
		out.Message = redactForKey(err.Error(), key.Secret)
		return out, err
	}
	probeCtx, cancel := context.WithTimeout(req.Context(), p.Spec.TimeoutDuration())
	defer cancel()
	req = req.WithContext(probeCtx)
	// Count the daily token budget only once the transport confirms that the
	// request was written. BuildRequest/acquire/cancel-before-write do not spend it.
	var requestWritten atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			requestWritten.Store(true)
		}
	}}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := p.Client.Do(req)
	if requestWritten.Load() || resp != nil {
		m.mu.Lock()
		budget.tokens++
		m.daily[budgetID] = budget
		m.mu.Unlock()
		out.TokenAttempted = true
	}
	if err != nil {
		lease.Failed()
		out.Message = redactForKey(err.Error(), key.Secret)
		return out, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	classified := p.Adapter.Classify(resp, body, readErr)
	out.HTTPStatus = resp.StatusCode
	out.Outcome = classified.Outcome
	out.LatencyMS = m.clk.Now().Sub(started).Milliseconds()
	if classified.Outcome == classify.Success {
		var value struct {
			Usage struct {
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(body, &value)
		actual := value.Usage.CompletionTokens
		if actual < 1 {
			actual = 1
		}
		if actual > 1 {
			actual = 1
		}
		lease.Success(actual, adapterHints(p.Adapter, &p.Spec, resp.Header, m.clk.Now()))
		out.OK = true
		out.Message = "completion de 1 token concluída"
		return out, nil
	}
	switch classified.Outcome {
	case classify.AuthInvalid:
		lease.AuthFailed()
	case classify.RateLimitedShort, classify.RateLimitedLong:
		lease.RateLimited(classified.RetryAfter, classified.RetryKnown, nil)
		out.RetryAfter = classified.RetryAfter
		out.RetryKnown = classified.RetryKnown
	case classify.QuotaExhausted:
		lease.QuotaExhausted()
	default:
		lease.Failed()
	}
	out.Message = classified.Message
	return out, fmt.Errorf("%s", out.Message)
}

func (m *Manager) loadPersistedL2Budget(ctx context.Context, providerName string, keyID sched.KeyID, now time.Time) (int, error) {
	start := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	var warmErr error
	if m.recorder != nil {
		if total, err := m.recorder.CountWarmL2Tokens(ctx, providerName, keyID, start, end); err == nil {
			return total, nil
		} else {
			warmErr = err
		}
	}
	reader, ok := m.repo.(store.AdminReader)
	if !ok {
		if warmErr != nil {
			return 0, warmErr
		}
		return 0, nil
	}
	rows, err := reader.ReadUsage(ctx, start, end, "")
	if err != nil {
		if warmErr != nil {
			return 0, fmt.Errorf("warm store: %v; usage reader: %w", warmErr, err)
		}
		return 0, err
	}
	total := 0
	for _, row := range rows {
		if row.Source == "warmup" && row.Provider == providerName && row.KeyID == string(keyID) && row.Alias == "warmup-L2" {
			total += row.EstimatedTokens
		}
	}
	return total, nil
}

func learnedRPDRemainingBelow(pool *sched.Pool, keyID sched.KeyID, model string, minimum int) bool {
	if pool == nil || minimum <= 0 {
		return false
	}
	snapshot := pool.HintSnapshot(keyID)
	buckets := pool.HintBuckets([]string{model})
	seen := map[string]bool{}
	for _, bucket := range buckets {
		seen[bucket] = true
	}
	// Empty bucket represents provider/account-wide hints learned before a
	// model catalog exists. It must remain a floor for all model buckets.
	seen[""] = true
	for bucket := range seen {
		if hint, ok := snapshot[bucket]["rpd"]; ok && hint.HasRemaining && hint.Remaining < minimum {
			return true
		}
	}
	return false
}

func (m *Manager) applyHints(p router.WarmProvider, id sched.KeyID, hints *sched.Hints, headers http.Header) {
	if hints == nil && headers != nil {
		hints = adapterHints(p.Adapter, &p.Spec, headers, m.clk.Now())
	}
	if hints == nil || len(*hints) == 0 {
		return
	}
	models, known := p.Pool.Catalog(id)
	if !known {
		if st := m.status(p.Name, id); st != nil {
			models = st.Catalog
		}
	}
	for _, bucket := range p.Pool.HintBuckets(models) {
		p.Pool.ApplyHints(id, bucket, *hints)
	}
	observed := time.Time{}
	for _, hint := range *hints {
		if hint.ObservedAt.After(observed) {
			observed = hint.ObservedAt
		}
	}
	if observed.IsZero() {
		observed = m.clk.Now()
	}
	m.mu.Lock()
	if st := m.statuses[p.Name+"/"+string(id)]; st != nil {
		st.HintObservedAt = observed
	}
	m.mu.Unlock()
}

func adapterHints(adapter provider.Adapter, spec *provider.Spec, headers http.Header, now time.Time) *sched.Hints {
	if adapter == nil {
		return nil
	}
	if configurable, ok := adapter.(interface {
		HintsFor(*provider.Spec, http.Header, time.Time) *sched.Hints
	}); ok {
		return configurable.HintsFor(spec, headers, now)
	}
	return adapter.Hints(headers, now)
}

func (m *Manager) status(providerName string, id sched.KeyID) *KeyStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.statuses[providerName+"/"+string(id)]; st != nil {
		copy := *st
		copy.Catalog = append([]string(nil), st.Catalog...)
		return &copy
	}
	return nil
}

func (m *Manager) persistHealth(ctx context.Context, p router.WarmProvider, key sched.KeySpec, result ProbeResult) {
	if m.recorder == nil {
		return
	}
	item := store.HealthCheck{Provider: p.Name, KeyID: key.ID, Probe: result.Layer, OK: result.OK, HTTPStatus: result.HTTPStatus, LatencyMS: int(result.LatencyMS), Error: result.Message, At: result.At}
	if len(item.Error) > 500 {
		item.Error = item.Error[:500]
	}
	if err := m.recorder.SaveHealthCheck(ctx, item); err != nil {
		m.reportEvent(p.Name, "warm.health.persist_error", key.Label, err.Error())
	}
}
func (m *Manager) recordUsage(ctx context.Context, p router.WarmProvider, key sched.KeySpec, result ProbeResult) {
	if result.Layer == string(layerL2) && !result.TokenAttempted {
		return
	}
	event := domain.UsageEvent{At: result.At, RequestID: fmt.Sprintf("warm-%d", m.counter.Add(1)), Project: "keypool", Alias: "warmup-" + result.Layer, Provider: p.Name, Model: result.Model, KeyID: string(key.ID), Outcome: string(domain.Success), Status: result.HTTPStatus, Attempt: 1, Source: "warmup"}
	if result.Layer == string(layerL2) {
		event.EstimatedTokens = 1
	}
	if !result.OK {
		switch result.Outcome {
		case classify.AuthInvalid:
			event.Outcome = string(domain.AuthInvalid)
		case classify.RateLimitedShort, classify.RateLimitedLong:
			event.Outcome = string(domain.RateLimitedShort)
		case classify.QuotaExhausted:
			event.Outcome = string(domain.QuotaExhausted)
		default:
			event.Outcome = string(domain.Transient)
		}
	}
	m.mu.Lock()
	sink := m.usageSink
	m.mu.Unlock()
	if sink != nil {
		sink(event)
		return
	}
	if m.repo != nil {
		if err := m.repo.AppendUsage(ctx, []domain.UsageEvent{event}); err != nil {
			m.reportEvent(p.Name, "warm.usage.persist_error", key.Label, err.Error())
		}
	}
}
func (m *Manager) reportEvent(providerName, kind, subject, detail string) {
	e := sched.Event{Kind: kind, Subject: providerName + "/" + subject, Detail: detail, At: m.clk.Now()}
	m.mu.Lock()
	sink := m.eventSink
	m.mu.Unlock()
	if sink != nil {
		sink(e)
	}
}

func (m *Manager) countsAgainstLimits() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.CountsAgainstLimits
}

func cloneAnyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func accountIsFree(info map[string]any) bool {
	for _, key := range []string{"is_free_tier", "is_free"} {
		if value, ok := info[key].(bool); ok && value {
			return true
		}
	}
	return false
}

func credentialDenied(message string) bool {
	v := strings.ToLower(message)
	for _, marker := range []string{"invalid api key", "invalid key", "authentication", "credential", "unauthorized", "api key"} {
		if strings.Contains(v, marker) {
			return true
		}
	}
	return false
}

func redactForKey(message string, key secret.Secret) string {
	if value := key.Reveal(); value != "" {
		message = strings.ReplaceAll(message, value, "[redacted-key]")
	}
	return secret.Redact(message)
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 || a > b {
		return b
	}
	return a
}

// Run forces L0 and L1 for an optional provider/key filter. It never runs L2.
func (m *Manager) Run(ctx context.Context, providerName, key string) (any, error) {
	if !m.acquireManualWorker() {
		return nil, ErrBusy
	}
	defer m.releaseManualWorker()
	providers := m.router.WarmProviders()
	results := make([]ProbeResult, 0)
	for _, p := range providers {
		if providerName != "" && p.Name != providerName {
			continue
		}
		for _, k := range p.Keys {
			if key != "" && string(k.ID) != key {
				continue
			}
			for _, kind := range []jobKind{layerL0, layerL1} {
				keyName := p.Name + "/" + string(k.ID)
				m.mu.Lock()
				busy := m.active[keyName]
				if !busy {
					m.active[keyName] = true
				}
				m.mu.Unlock()
				if busy {
					return results, ErrBusy
				}
				result, err := m.runLayer(ctx, p.Name, k.ID, kind, true)
				m.mu.Lock()
				delete(m.active, keyName)
				m.mu.Unlock()
				m.recordManualResult(p.Name, k, kind, result, err)
				if result != nil {
					results = append(results, *result)
				}
				if errors.Is(err, ErrBusy) {
					return results, err
				}
				if err != nil {
					continue
				}
			}
		}
	}
	if (key != "" || providerName != "") && len(results) == 0 {
		return results, ErrNotFound
	}
	return results, nil
}

func (m *Manager) recordManualResult(providerName string, key sched.KeySpec, kind jobKind, result *ProbeResult, err error) {
	if result == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keyName := providerName + "/" + string(key.ID)
	st := m.statuses[keyName]
	if st == nil {
		st = &KeyStatus{Provider: providerName, KeyID: string(key.ID), Label: key.Label, Health: "unknown"}
		m.statuses[keyName] = st
	}
	st.LastProbe = result
	if result.OK {
		st.LastError = ""
	} else if result.Message != "" {
		st.LastError = result.Message
	} else if err != nil {
		st.LastError = redactForKey(err.Error(), key.Secret)
	}
	switch kind {
	case layerL0:
		st.LastL0At = result.At
		st.Connection = result.Connection
	case layerL1:
		st.LastL1At = result.At
		if result.OK {
			st.LastOKAt = result.At
			st.Catalog = append([]string(nil), result.Models...)
			st.CatalogSeenAt = result.At
		}
		if result.AccountInfo != nil {
			st.AccountInfo = cloneAnyMap(result.AccountInfo)
		}
	case layerL2:
		st.LastL2At = result.At
	}
}

// ProbeKey runs an explicit L1 for the administrative key/test endpoint.
func (m *Manager) ProbeKey(ctx context.Context, key string) (any, error) {
	if !m.acquireManualWorker() {
		return nil, ErrBusy
	}
	defer m.releaseManualWorker()
	providers := m.router.WarmProviders()
	for _, p := range providers {
		for _, k := range p.Keys {
			if string(k.ID) == key {
				keyName := p.Name + "/" + key
				m.mu.Lock()
				if m.active[keyName] {
					m.mu.Unlock()
					return nil, ErrBusy
				}
				m.active[keyName] = true
				m.mu.Unlock()
				result, err := m.runLayer(ctx, p.Name, k.ID, layerL1, true)
				m.mu.Lock()
				delete(m.active, keyName)
				m.mu.Unlock()
				m.recordManualResult(p.Name, k, layerL1, result, err)
				return result, err
			}
		}
	}
	return nil, ErrNotFound
}

// Manual admin probes share the same global worker budget as scheduled jobs.
// queuedJobs also reserves capacity so a manual call cannot steal a slot already
// promised to a job that is waiting in the worker channel.
func (m *Manager) acquireManualWorker() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := m.cfg.Workers
	if limit < 1 {
		limit = 1
	}
	if m.closed || m.activeWorkers+m.queuedJobs >= limit {
		return false
	}
	m.activeWorkers++
	return true
}

func (m *Manager) releaseManualWorker() {
	m.mu.Lock()
	if m.activeWorkers > 0 {
		m.activeWorkers--
	}
	m.mu.Unlock()
}

// EnableKey clears a previous invalid state after an explicit administrative enable.
func (m *Manager) EnableKey(id string) {
	for _, p := range m.router.WarmProviders() {
		if p.Pool.ResetKey(sched.KeyID(id)) {
			m.router.MarkStateDirty()
			m.mu.Lock()
			pcfg := m.cfg.ForProvider(p.Name)
			if m.cfg.Enabled && pcfg.Enabled && pcfg.L1Enabled {
				m.readinessEpoch++
				epoch := m.readinessEpoch
				m.pushLocked(jobID{p.Name, sched.KeyID(id), layerL1}, m.clk.Now())
				m.firstPending[p.Name+"/"+id] = true
				m.ready = false
				m.readyReason = "revalidação após key enable"
				fn := m.onReady
				ctx := m.ctx
				startTimeout := m.started && !m.closed && ctx != nil
				if startTimeout {
					m.wg.Add(1)
				}
				timeout := m.cfg.ReadyTimeout
				m.mu.Unlock()
				if startTimeout {
					go m.readinessTimeout(ctx, timeout, epoch)
				}
				if fn != nil {
					fn(false, "revalidação após key enable")
				}
			} else {
				m.mu.Unlock()
			}
			return
		}
	}
}

// Status returns a copy of warm status and per-key diagnostics.
func (m *Manager) Status() any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := Status{Enabled: m.cfg.Enabled, Ready: m.ready, ReadyReason: m.readyReason, Workers: m.cfg.Workers, ActiveWorkers: m.activeWorkers, Queued: len(m.queue) + m.queuedJobs, LastUpdated: m.clk.Now()}
	if m.router != nil {
		for _, p := range m.router.WarmProviders() {
			stats := p.Pool.Stats()
			for _, ks := range stats {
				key := p.Name + "/" + string(ks.ID)
				st := m.statuses[key]
				if st == nil {
					st = &KeyStatus{Provider: p.Name, KeyID: string(ks.ID), Label: ks.Label}
					m.statuses[key] = st
				}
				st.Health = ks.Health
				st.CredentialVersion = ks.CredentialVersion
				st.Disabled = ks.Disabled
				st.Inflight = ks.Inflight
				st.Warming = m.active[key]
				st.LastActivity = ks.LastActivity
				st.TTFBEWMA = cloneDurationMapWarm(ks.TTFBEWMA)
				st.TotalEWMA = cloneDurationMapWarm(ks.TotalEWMA)
				st.Hints = p.Pool.HintSnapshot(ks.ID)
				for _, dimensions := range st.Hints {
					for _, hint := range dimensions {
						if hint.ObservedAt.After(st.HintObservedAt) {
							st.HintObservedAt = hint.ObservedAt
						}
					}
				}
				if ks.LastOK.After(st.LastOKAt) {
					st.LastOKAt = ks.LastOK
				}
				if catalog, known := p.Pool.Catalog(ks.ID); known {
					st.Catalog = catalog
				} else if st.Catalog == nil && ks.CatalogKnown {
					st.Catalog = []string{}
				}
				if ks.CatalogSeenAt.After(st.CatalogSeenAt) {
					st.CatalogSeenAt = ks.CatalogSeenAt
				}
			}
		}
	}
	out.Keys = out.Keys[:0]
	for _, st := range m.statuses {
		cp := *st
		cp.Catalog = append([]string(nil), st.Catalog...)
		cp.AccountInfo = cloneAnyMap(st.AccountInfo)
		cp.TTFBEWMA = cloneDurationMapWarm(st.TTFBEWMA)
		cp.TotalEWMA = cloneDurationMapWarm(st.TotalEWMA)
		cp.Hints = cloneHints(st.Hints)
		if !st.LastL0At.IsZero() {
			cp.LastL0Age = maxDurationWarm(0, out.LastUpdated.Sub(st.LastL0At))
		}
		if !st.LastL1At.IsZero() {
			cp.LastL1Age = maxDurationWarm(0, out.LastUpdated.Sub(st.LastL1At))
		}
		if !st.LastL2At.IsZero() {
			cp.LastL2Age = maxDurationWarm(0, out.LastUpdated.Sub(st.LastL2At))
		}
		if st.LastProbe != nil {
			pr := cloneProbeResult(*st.LastProbe)
			cp.LastProbe = &pr
		}
		if st.Connection != nil {
			co := *st.Connection
			cp.Connection = &co
		}
		out.Keys = append(out.Keys, cp)
	}
	sort.Slice(out.Keys, func(i, j int) bool {
		if out.Keys[i].Provider == out.Keys[j].Provider {
			return out.Keys[i].KeyID < out.Keys[j].KeyID
		}
		return out.Keys[i].Provider < out.Keys[j].Provider
	})
	return out
}

func cloneProbeResult(in ProbeResult) ProbeResult {
	out := in
	out.Models = append([]string(nil), in.Models...)
	out.AccountInfo = cloneAnyMap(in.AccountInfo)
	if in.Connection != nil {
		connection := *in.Connection
		out.Connection = &connection
	}
	return out
}

func cloneDurationMapWarm(in map[string]time.Duration) map[string]time.Duration {
	if in == nil {
		return nil
	}
	out := make(map[string]time.Duration, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneHints(in map[string]sched.Hints) map[string]sched.Hints {
	if in == nil {
		return nil
	}
	out := make(map[string]sched.Hints, len(in))
	for bucket, dimensions := range in {
		copyDimensions := sched.Hints{}
		for name, hint := range dimensions {
			copyDimensions[name] = hint
		}
		out[bucket] = copyDimensions
	}
	return out
}

func maxDurationWarm(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// ModelLimits returns limits for a target model; exported to keep warm logic away
// from internal scheduler configuration.
func (m *Manager) String() string { status, _ := json.Marshal(m.Status()); return string(status) }
