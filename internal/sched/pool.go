package sched

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"keypool/internal/clock"
	"keypool/internal/limits"
	"keypool/internal/secret"
	"keypool/internal/sliding"
)

type bucketWindows struct {
	tpm, tpd, rpm, rpd *sliding.Window
	inflight           int
}
type groupState struct {
	buckets map[string]*bucketWindows
	hints   map[string]map[string]Hint
}
type keyState struct {
	spec                        KeySpec
	disabled                    bool
	health                      string
	cooldowns                   map[string]time.Time
	longCooldowns               map[string]time.Time
	allCooldown                 time.Time
	quotaUntil                  time.Time
	quotaBuckets                map[string]time.Time
	usedTotal, errors, inflight int
	openUntil                   time.Time
	probing                     bool
	forbidden                   map[string]time.Time
	lastActivity, lastOK        time.Time
	ttfbEWMA, totalEWMA         map[string]time.Duration
	openCount                   int
	credentialVersion           uint64
	catalogKnown, validated     bool
	catalogAt                   time.Time
	catalog                     map[string]struct{}
	warming                     bool
}
type waiter struct {
	ticket   uint64
	priority int
}

// Pool usa um único mutex para chaves, filas, grupos, circuitos e leases. O mutex
// nunca é mantido durante Clock.Wait nem callback OnEvent.
type Pool struct {
	mu         sync.Mutex
	cfg        PoolConfig
	keys       []*keyState
	byID       map[KeyID]*keyState
	groups     map[string]*groupState
	shared     map[string]*groupState
	cursors    map[string]int
	merged     bool
	limited    map[string]map[KeyID]time.Time
	waiters    map[string][]waiter
	wake       map[string]chan struct{}
	nextTicket uint64
	leases     map[uint64]*Lease
	nextLease  uint64
}

// NewPool cria um pool concorrente e injeta relógio e política.
func NewPool(cfg PoolConfig) *Pool {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.QuotaCooldown <= 0 {
		cfg.QuotaCooldown = time.Hour
	}
	if cfg.MergeWindow <= 0 {
		cfg.MergeWindow = 20 * time.Second
	}
	if cfg.MergeMaxRetry <= 0 {
		cfg.MergeMaxRetry = 120 * time.Second
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 15 * time.Minute
	}
	if cfg.CatalogTTL <= 0 {
		cfg.CatalogTTL = time.Hour
	}
	return &Pool{cfg: cfg, byID: map[KeyID]*keyState{}, groups: map[string]*groupState{}, shared: map[string]*groupState{}, cursors: map[string]int{}, limited: map[string]map[KeyID]time.Time{}, waiters: map[string][]waiter{}, wake: map[string]chan struct{}{}, leases: map[uint64]*Lease{}}
}
func (p *Pool) now() time.Time { return p.cfg.Clock.Now() }

// SetOnEvent installs the operational event sink. Configure it before the pool
// is exposed to concurrent requests; the callback itself is always launched
// asynchronously by emit and never runs while the pool mutex is held.
func (p *Pool) SetOnEvent(fn func(Event)) {
	p.mu.Lock()
	p.cfg.OnEvent = fn
	p.mu.Unlock()
}
func (p *Pool) emit(e Event) {
	if p.cfg.OnEvent != nil {
		cb := p.cfg.OnEvent
		go cb(e)
	}
}
func (p *Pool) signal(bucket string) {
	ch := p.wake[bucket]
	if ch != nil {
		close(ch)
	}
	p.wake[bucket] = make(chan struct{})
}
func (p *Pool) wakeChan(bucket string) <-chan struct{} {
	ch := p.wake[bucket]
	if ch == nil {
		ch = make(chan struct{})
		p.wake[bucket] = ch
	}
	return ch
}
func (p *Pool) groupID(k *keyState) string {
	switch p.cfg.Scope {
	case ScopeProvider:
		return "*"
	case ScopeAuto:
		if p.merged {
			return "*"
		}
	case ScopeGroup:
		if k.spec.Group != "" {
			return "group:" + k.spec.Group
		}
	}
	if k.spec.Group != "" && p.cfg.Scope == ScopeAuto {
		return "group:" + k.spec.Group
	}
	return "key:" + string(k.spec.ID)
}
func newGroup() *groupState {
	return &groupState{buckets: map[string]*bucketWindows{}, hints: map[string]map[string]Hint{}}
}
func (p *Pool) newBucketWindows() *bucketWindows {
	rpd := sliding.New(24 * time.Hour)
	if timezone := p.cfg.Limits.RPDResetTimezone; timezone != "" {
		if location, err := time.LoadLocation(timezone); err == nil {
			rpd = sliding.NewCalendarDay(location)
		}
	}
	return &bucketWindows{tpm: sliding.New(time.Minute), tpd: sliding.New(24 * time.Hour), rpm: sliding.New(time.Minute), rpd: rpd}
}

func (p *Pool) windows(k *keyState, bucket string) *bucketWindows {
	id := p.groupID(k)
	var g *groupState
	if id == "*" {
		g = p.shared[bucket]
		if g == nil {
			g = newGroup()
			p.shared[bucket] = g
		}
	} else {
		g = p.groups[id]
		if g == nil {
			g = newGroup()
			p.groups[id] = g
		}
	}
	w := g.buckets[bucket]
	if w == nil {
		w = p.newBucketWindows()
		g.buckets[bucket] = w
	}
	return w
}
func (p *Pool) group(k *keyState, bucket string) *groupState {
	id := p.groupID(k)
	var g *groupState
	if id == "*" {
		g = p.shared[bucket]
		if g == nil {
			g = newGroup()
			p.shared[bucket] = g
		}
	} else {
		g = p.groups[id]
		if g == nil {
			g = newGroup()
			p.groups[id] = g
		}
	}
	return g
}

// SetKeys recarrega a configuração preservando estado por KeyID.
func (p *Pool) SetKeys(keys []KeySpec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.byID
	next := make([]*keyState, 0, len(keys))
	by := make(map[KeyID]*keyState, len(keys))
	for _, sp := range keys {
		if sp.ID == "" {
			continue
		}
		if st := old[sp.ID]; st != nil {
			if st.credentialVersion == 0 {
				st.credentialVersion = 1
			}
			if st.ttfbEWMA == nil {
				st.ttfbEWMA = map[string]time.Duration{}
			}
			if st.totalEWMA == nil {
				st.totalEWMA = map[string]time.Duration{}
			}
			if st.spec.Secret.Reveal() != sp.Secret.Reveal() {
				st.credentialVersion++
				st.disabled = false
				st.health = "unknown"
				st.errors = 0
				st.probing = false
				st.openUntil = time.Time{}
				st.validated = false
				st.catalogKnown = false
				st.catalogAt = time.Time{}
				st.catalog = nil
			}
			if st.longCooldowns == nil {
				st.longCooldowns = map[string]time.Time{}
			}
			st.spec = sp
			next = append(next, st)
			by[sp.ID] = st
		} else {
			st := &keyState{spec: sp, health: "unknown", credentialVersion: 1, cooldowns: map[string]time.Time{}, longCooldowns: map[string]time.Time{}, quotaBuckets: map[string]time.Time{}, forbidden: map[string]time.Time{}, ttfbEWMA: map[string]time.Duration{}, totalEWMA: map[string]time.Duration{}}
			next = append(next, st)
			by[sp.ID] = st
		}
	}
	p.keys = next
	p.byID = by
	for bucket := range p.wake {
		p.signal(bucket)
	}
	p.signal("")
}
func (p *Pool) LimitsFor(model string) limits.Resolved { return p.cfg.Limits.Resolve(model) }

// DailyQuotaUsage reports the normalized consumption of the daily quota bucket
// resolved for model. It aggregates all independent key/group buckets in this
// provider pool, but counts a shared provider bucket only once. The result is
// intentionally an estimate based on configured limits and locally observed
// usage; upstream providers remain authoritative and may enforce different
// limits or reset windows.
type DailyQuotaUsage struct {
	RequestsUsed  int
	RequestsLimit int
	TokensUsed    int
	TokensLimit   int
	Ratio         float64
	HasLimits     bool
}

func (p *Pool) DailyQuotaUsage(model string) DailyQuotaUsage {
	if p == nil {
		return DailyQuotaUsage{}
	}
	resolved := p.cfg.Limits.Resolve(model)
	out := DailyQuotaUsage{}
	if resolved.RPD == nil && resolved.TPD == nil {
		return out
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	seen := make(map[string]struct{}, len(p.keys))
	for _, key := range p.keys {
		groupID := p.groupID(key)
		if _, ok := seen[groupID]; ok {
			continue
		}
		seen[groupID] = struct{}{}
		var group *groupState
		if groupID == "*" {
			group = p.shared[resolved.Bucket]
		} else {
			group = p.groups[groupID]
		}
		if group == nil {
			// The bucket has not been used in this scope yet.
			if resolved.RPD != nil {
				out.RequestsLimit += *resolved.RPD
			}
			if resolved.TPD != nil {
				out.TokensLimit += *resolved.TPD
			}
			continue
		}
		window := group.buckets[resolved.Bucket]
		if window != nil {
			out.RequestsUsed += window.rpd.Count(now)
			out.TokensUsed += window.tpd.Used(now)
		}
		if resolved.RPD != nil {
			out.RequestsLimit += *resolved.RPD
		}
		if resolved.TPD != nil {
			out.TokensLimit += *resolved.TPD
		}
	}
	if out.RequestsLimit > 0 {
		out.HasLimits = true
		out.Ratio = float64(out.RequestsUsed) / float64(out.RequestsLimit)
	}
	if out.TokensLimit > 0 {
		tokenRatio := float64(out.TokensUsed) / float64(out.TokensLimit)
		if !out.HasLimits || tokenRatio > out.Ratio {
			out.Ratio = tokenRatio
		}
		out.HasLimits = true
	}
	if out.Ratio < 0 {
		out.Ratio = 0
	}
	return out
}

// Merged informa se ScopeAuto já detectou limites compartilhados entre chaves.
func (p *Pool) Merged() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.merged }
func (p *Pool) allDisabled(r AcquireRequest) bool {
	for _, k := range p.keys {
		if !k.disabled && k.health != "invalid" && (r.Allow == nil || r.Allow(k.spec.ID)) {
			return false
		}
	}
	return true
}
func (p *Pool) bucketWait(k *keyState, r AcquireRequest, res limits.Resolved, now time.Time) (time.Duration, bool) {
	if k.warming && !r.Warm {
		return -1, true
	}
	if k.disabled || k.health == "invalid" {
		return 0, false
	}
	if k.health == "open" {
		if now.Before(k.openUntil) {
			return k.openUntil.Sub(now), true
		}
		if k.probing {
			return -1, false
		}
	} else if k.health == "probing" && k.probing {
		return -1, false
	}
	wait := time.Duration(0)
	max := func(d time.Duration) {
		if d > wait {
			wait = d
		}
	}
	if now.Before(k.allCooldown) {
		max(k.allCooldown.Sub(now))
	}
	if until := k.longCooldowns[r.Model]; now.Before(until) {
		max(until.Sub(now))
	}
	if until := k.forbidden[r.Model]; now.Before(until) {
		max(until.Sub(now))
	}
	if t := k.cooldowns[res.Bucket]; now.Before(t) {
		max(t.Sub(now))
	}
	if now.Before(k.quotaUntil) {
		max(k.quotaUntil.Sub(now))
	}
	if !res.Free {
		if t := k.quotaBuckets[res.Bucket]; now.Before(t) {
			max(t.Sub(now))
		}
	}
	w := p.windows(k, res.Bucket)
	if res.TPM != nil {
		max(w.tpm.WaitForRoom(now, r.Tokens, *res.TPM))
	}
	if res.TPD != nil {
		reserve := math.Max(0, math.Min(.95, r.Reserve))
		lim := int(math.Floor(float64(*res.TPD) * (1 - reserve)))
		if lim < 1 {
			lim = 1
		}
		max(w.tpd.WaitForRoom(now, r.Tokens, lim))
	}
	if res.RPM != nil {
		max(w.rpm.WaitForSlots(now, *res.RPM))
	}
	if res.RPD != nil {
		max(w.rpd.WaitForSlots(now, *res.RPD))
	}
	if res.Conc != nil && w.inflight >= *res.Conc && wait == 0 {
		wait = -1
	}
	g := p.group(k, res.Bucket)
	for dim, h := range g.hints[res.Bucket] {
		if !h.HasRemaining {
			continue
		}
		limitNeed := 1
		if dim == "tpm" || dim == "tpd" {
			limitNeed = r.Tokens
		}
		margin := 0
		if dim == "tpm" {
			margin = 100
		}
		if h.ResetAt.After(now) && h.Remaining < limitNeed+margin {
			max(h.ResetAt.Sub(now))
		}
	}
	return wait, true
}
func (p *Pool) outranked(bucket string, pri int, ticket uint64) bool {
	for _, w := range p.waiters[bucket] {
		if w.ticket == ticket && ticket != 0 {
			continue
		}
		if w.priority < pri || (w.priority == pri && (ticket == 0 || w.ticket < ticket)) {
			return true
		}
	}
	return false
}
func (p *Pool) strategy(candidates []*keyState, bucket, model string, now time.Time, res limits.Resolved) *keyState {
	if len(candidates) == 0 {
		return nil
	}
	// Todos os critérios de desempate preservam o round-robin por bucket.
	start := 0
	if len(p.keys) > 0 {
		start = p.cursors[bucket] % len(p.keys)
	}
	ordered := make([]*keyState, 0, len(candidates))
	for offset := 0; offset < len(p.keys); offset++ {
		k := p.keys[(start+offset)%len(p.keys)]
		for _, c := range candidates {
			if c == k {
				ordered = append(ordered, k)
				break
			}
		}
	}
	candidates = ordered
	switch p.cfg.Strategy {
	case LeastUsed:
		sort.SliceStable(candidates, func(i, j int) bool {
			return p.windows(candidates[i], bucket).tpm.Used(now) < p.windows(candidates[j], bucket).tpm.Used(now)
		})
		return candidates[0]
	case Headroom:
		score := func(k *keyState) float64 {
			w := p.windows(k, bucket)
			best := 1.0
			apply := func(limit *int, used int) {
				if limit != nil && *limit > 0 {
					ratio := float64(*limit-used) / float64(*limit)
					if ratio < best {
						best = ratio
					}
				}
			}
			apply(res.TPM, w.tpm.Used(now))
			apply(res.TPD, w.tpd.Used(now))
			apply(res.RPM, w.rpm.Count(now))
			apply(res.RPD, w.rpd.Count(now))
			apply(res.Conc, w.inflight)
			return best
		}
		sort.SliceStable(candidates, func(i, j int) bool { return score(candidates[i]) > score(candidates[j]) })
		return candidates[0]
	case Latency: // P2C: compara duas opções pelo EWMA de TTFB do modelo.
		if len(candidates) > 1 {
			a, b := candidates[0], candidates[1]
			la, lb := a.ttfbEWMA[model], b.ttfbEWMA[model]
			if lb > 0 && (la == 0 || lb < la) {
				return b
			}
		}
		return candidates[0]
	}
	return candidates[0]
}
func (p *Pool) reserve(k *keyState, r AcquireRequest, res limits.Resolved, now time.Time) *Lease {
	// A meia-abertura so e consumida quando a chave foi escolhida para uma reserva real.
	if k.health == "open" && !now.Before(k.openUntil) {
		k.health = "probing"
		k.probing = true
	}
	w := p.windows(k, res.Bucket)
	lr := &Lease{pool: p, key: k, bucket: res.Bucket, model: r.Model, estimated: maxInt(0, r.Tokens), created: now, activityBefore: k.lastActivity, group: p.groupID(k)}
	lr.tpmRef = w.tpm.Add(now, lr.estimated)
	if res.TPD != nil {
		lr.tpdRef = w.tpd.Add(now, lr.estimated)
	}
	lr.rpmRef = w.rpm.Add(now, 1)
	lr.rpdRef = w.rpd.Add(now, 1)
	w.inflight++
	k.inflight++
	k.lastActivity = now
	p.nextLease++
	lr.id = p.nextLease
	p.leases[lr.id] = lr
	p.cursors[res.Bucket] = (indexOf(p.keys, k) + 1) % len(p.keys)
	return lr
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func indexOf(a []*keyState, k *keyState) int {
	for i, x := range a {
		if x == k {
			return i
		}
	}
	return 0
}
func (p *Pool) sweepLeasesLocked(now time.Time) {
	for _, l := range p.leases {
		if !l.done && now.Sub(l.created) >= p.cfg.LeaseTTL {
			p.finishLocked(l, finishSuccess, l.estimated, nil)
			p.emit(Event{Kind: "lease.leaked", Subject: l.key.spec.Label, At: now})
		}
	}
}

// Acquire reserva uma chave, aguardando capacidade até o contexto expirar.
func (p *Pool) Acquire(ctx context.Context, r AcquireRequest) (*Lease, error) {
	need := maxInt(0, r.Tokens)
	r.Tokens = need
	res := p.cfg.Limits.Resolve(r.Model)
	if res.TPM != nil && need >= *res.TPM {
		return nil, ErrExceedsLimit
	}
	var ticket uint64
	defer func() {
		if ticket != 0 {
			p.mu.Lock()
			q := p.waiters[res.Bucket]
			for i := range q {
				if q[i].ticket == ticket {
					p.waiters[res.Bucket] = append(q[:i], q[i+1:]...)
					break
				}
			}
			p.mu.Unlock()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		now := p.now()
		p.sweepLeasesLocked(now)
		if len(p.keys) == 0 {
			p.mu.Unlock()
			return nil, ErrNoKey
		}
		if p.allDisabled(r) {
			p.mu.Unlock()
			return nil, ErrAllDisabled
		}
		var eligible []*keyState
		best := time.Duration(math.MaxInt64)
		indef := false
		for _, k := range p.keys {
			if r.Allow != nil && !r.Allow(k.spec.ID) {
				continue
			}
			w, ok := p.bucketWait(k, r, res, now)
			if !ok {
				continue
			}
			if w == 0 {
				eligible = append(eligible, k)
			} else if w < 0 {
				indef = true
			} else if w < best {
				best = w
			}
		}
		if len(eligible) > 0 && !p.outranked(res.Bucket, r.Priority, ticket) {
			chosen := p.strategy(eligible, res.Bucket, r.Model, now, res)
			if chosen != nil {
				l := p.reserve(chosen, r, res, now)

				p.mu.Unlock()
				return l, nil
			}
		}
		if ticket == 0 {
			p.nextTicket++
			ticket = p.nextTicket
			p.waiters[res.Bucket] = append(p.waiters[res.Bucket], waiter{ticket: ticket, priority: r.Priority})
		}
		wake := p.wakeChan(res.Bucket)
		remain := time.Duration(math.MaxInt64)
		if dl, ok := ctx.Deadline(); ok {
			remain = dl.Sub(now)
			if remain <= 0 {
				retry := best
				if retry == time.Duration(math.MaxInt64) && indef {
					retry = -1
				}
				out := error(&ErrNoCapacity{RetryIn: retry})
				if p.outranked(res.Bucket, r.Priority, ticket) {
					out = &ErrOutranked{RetryIn: retry}
				}
				p.mu.Unlock()
				return nil, out
			}
		}
		wait := best
		if wait == time.Duration(math.MaxInt64) {
			wait = 5 * time.Second
		}
		if wait <= 0 || indef {
			wait = 5 * time.Second
		}
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		if remain < wait {
			wait = remain
		}
		p.mu.Unlock()
		if err := p.cfg.Clock.Wait(ctx, wait, wake); err != nil {
			p.mu.Lock()
			outranked := p.outranked(res.Bucket, r.Priority, ticket)
			p.mu.Unlock()
			retry := best
			if retry == time.Duration(math.MaxInt64) && indef {
				retry = -1
			}
			if outranked {
				return nil, &ErrOutranked{RetryIn: retry}
			}
			return nil, &ErrNoCapacity{RetryIn: retry}
		}
	}
}

// TryAcquire tenta reservar sem aguardar.
func (p *Pool) TryAcquire(r AcquireRequest) (*Lease, error) {
	return p.tryImmediate(r)
}
func (p *Pool) tryImmediate(r AcquireRequest) (*Lease, error) {
	r.Tokens = maxInt(0, r.Tokens)
	res := p.cfg.Limits.Resolve(r.Model)
	if res.TPM != nil && r.Tokens >= *res.TPM {
		return nil, ErrExceedsLimit
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.sweepLeasesLocked(now)
	if len(p.keys) == 0 {
		return nil, ErrNoKey
	}
	if p.allDisabled(r) {
		return nil, ErrAllDisabled
	}
	var candidates []*keyState
	var best time.Duration = time.Duration(math.MaxInt64)
	indeterminate := false
	for _, k := range p.keys {
		if r.Allow != nil && !r.Allow(k.spec.ID) {
			continue
		}
		w, ok := p.bucketWait(k, r, res, now)
		if ok && w == 0 {
			candidates = append(candidates, k)
		} else if ok && w < 0 {
			indeterminate = true
		} else if ok && w > 0 && w < best {
			best = w
		}
	}
	if len(candidates) == 0 {
		if best == time.Duration(math.MaxInt64) && indeterminate {
			best = -1
		}
		return nil, &ErrNoCapacity{RetryIn: best}
	}
	if p.outranked(res.Bucket, r.Priority, 0) {
		return nil, &ErrOutranked{RetryIn: best}
	}
	return p.reserve(p.strategy(candidates, res.Bucket, r.Model, now, res), r, res, now), nil
}

// EstimateWait estima a espera sem reservar uso.
func (p *Pool) EstimateWait(r AcquireRequest) (time.Duration, bool) {
	r.Tokens = maxInt(0, r.Tokens)
	res := p.cfg.Limits.Resolve(r.Model)
	if res.TPM != nil && r.Tokens >= *res.TPM {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.sweepLeasesLocked(now)
	if len(p.keys) == 0 || p.allDisabled(r) {
		return 0, false
	}
	best := time.Duration(math.MaxInt64)
	indeterminate := false
	for _, k := range p.keys {
		if r.Allow != nil && !r.Allow(k.spec.ID) {
			continue
		}
		w, ok := p.bucketWait(k, r, res, now)
		if ok && w == 0 {
			return 0, true
		}
		if ok && w < 0 {
			indeterminate = true
		}
		if ok && w > 0 && w < best {
			best = w
		}
	}
	if best == time.Duration(math.MaxInt64) {
		if indeterminate {
			return -1, true
		}
		return 0, false
	}
	return best, true
}

// LongLimited informa se todas as chaves permitidas estão esgotadas por um 429 longo.
// O retorno contém o instante em que o bucket pode voltar a ser tentado.
func (p *Pool) LongLimited(r AcquireRequest) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	found := false
	until := time.Time{}
	for _, k := range p.keys {
		if r.Allow != nil && !r.Allow(k.spec.ID) {
			continue
		}
		if k.disabled || k.health == "invalid" {
			continue
		}
		found = true
		t := k.longCooldowns[r.Model]
		if !t.After(now) {
			return time.Time{}, false
		}
		if until.IsZero() || t.Before(until) {
			until = t
		}
	}
	return until, found
}

// Stats devolve cópias serializáveis, sem segredos.
func (p *Pool) Stats() []KeyStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.sweepLeasesLocked(now)
	out := make([]KeyStats, 0, len(p.keys))
	for _, k := range p.keys {
		used := map[string]int{}
		windowUsed := map[string]map[string]int{}
		addWindows := func(bucket string, bw *bucketWindows) {
			if bw == nil {
				return
			}
			if windowUsed[bucket] == nil {
				windowUsed[bucket] = map[string]int{}
			}
			windowUsed[bucket]["tpm"] = bw.tpm.Used(now)
			windowUsed[bucket]["tpd"] = bw.tpd.Used(now)
			windowUsed[bucket]["rpm"] = bw.rpm.Used(now)
			windowUsed[bucket]["rpd"] = bw.rpd.Used(now)
			used[bucket] = windowUsed[bucket]["tpm"]
		}
		if group := p.groups[p.groupID(k)]; group != nil {
			for bucket, bw := range group.buckets {
				addWindows(bucket, bw)
			}
		}
		for bucket, g := range p.shared {
			if bw := g.buckets[bucket]; bw != nil {
				addWindows(bucket, bw)
			}
		}
		cool := k.allCooldown
		for _, t := range k.cooldowns {
			if t.After(cool) {
				cool = t
			}
		}
		quotaBlocked := k.quotaUntil.After(now)
		for _, until := range k.quotaBuckets {
			quotaBlocked = quotaBlocked || until.After(now)
		}
		out = append(out, KeyStats{ID: k.spec.ID, Label: k.spec.Label, Disabled: k.disabled, Health: k.health, CooldownRemaining: maxDuration(0, cool.Sub(now)), QuotaBlocked: quotaBlocked, UsedTotal: k.usedTotal, Errors: k.errors, Inflight: k.inflight, UsedPerBucket: used, WindowUsed: windowUsed, LastActivity: k.lastActivity, LastOK: k.lastOK, CatalogKnown: k.catalogKnown, CatalogSeenAt: k.catalogAt, CatalogModels: len(k.catalog), CredentialVersion: k.credentialVersion, Validated: k.validated, TTFBEWMA: cloneDurationMap(k.ttfbEWMA), TotalEWMA: cloneDurationMap(k.totalEWMA)})
	}
	return out
}
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// Restore restaura janelas e estados limitando timestamps futuros a agora.
func (p *Pool) Restore(s PoolSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	wall := time.UnixMilli(s.SavedAtMS)
	if s.SavedAtMS == 0 {
		wall = now
	}
	p.merged = s.Merged
	for _, ks := range s.Keys {
		k := p.byID[ks.ID]
		if k == nil {
			continue
		}
		k.disabled = ks.Disabled
		k.health = ks.Health
		k.usedTotal = ks.UsedTotal
		k.errors = ks.Errors
		k.openUntil = restoreTime(now, wall, ks.OpenUntilMS)
		k.credentialVersion = ks.CredentialVersion
		k.lastActivity = restoreTime(now, wall, ks.LastActivityMS)
		k.lastOK = restoreTime(now, wall, ks.LastOKMS)
		k.catalogAt = restoreTime(now, wall, ks.CatalogAtMS)
		k.openCount = ks.OpenCount
		k.validated = ks.Validated
		k.catalogKnown = ks.CatalogKnown
		if ks.CatalogKnown {
			k.catalog = make(map[string]struct{}, len(ks.Catalog))
			for _, model := range ks.Catalog {
				if model != "" {
					k.catalog[model] = struct{}{}
				}
			}
		}
		if len(ks.TTFBEWMA) > 0 {
			k.ttfbEWMA = map[string]time.Duration{}
			for model, value := range ks.TTFBEWMA {
				k.ttfbEWMA[model] = time.Duration(value)
			}
		}
		if len(ks.TotalEWMA) > 0 {
			k.totalEWMA = map[string]time.Duration{}
			for model, value := range ks.TotalEWMA {
				k.totalEWMA[model] = time.Duration(value)
			}
		}
		k.allCooldown = restoreTime(now, wall, ks.AllCooldownMS)
		for b, ms := range ks.Cooldowns {
			k.cooldowns[b] = restoreTime(now, wall, ms)
		}
		for model, ms := range ks.LongCooldowns {
			if k.longCooldowns == nil {
				k.longCooldowns = map[string]time.Time{}
			}
			k.longCooldowns[model] = restoreTime(now, wall, ms)
		}
		for model, ms := range ks.ForbiddenModels {
			if k.forbidden == nil {
				k.forbidden = map[string]time.Time{}
			}
			k.forbidden[model] = restoreTime(now, wall, ms)
		}
		for b, ms := range ks.QuotaBuckets {
			k.quotaBuckets[b] = restoreTime(now, wall, ms)
		}
		k.quotaUntil = restoreTime(now, wall, ks.QuotaUntilMS)
	}
	for gid, buckets := range s.Groups {
		for b, bs := range buckets {
			g := p.groups[gid]
			if gid == "*" {
				g = p.shared[b]
				if g == nil {
					g = newGroup()
					p.shared[b] = g
				}
			} else if g == nil {
				g = newGroup()
				p.groups[gid] = g
			}
			w := p.newBucketWindows()
			restoreWindow(w.tpm, now, wall, bs.TPM.Entries)
			restoreWindow(w.tpd, now, wall, bs.TPD.Entries)
			restoreWindow(w.rpm, now, wall, bs.RPM.Entries)
			restoreWindow(w.rpd, now, wall, bs.RPD.Entries)
			g.buckets[b] = w
		}
	}
}

// RestoreStartup restores quota windows and usage history, but deliberately
// invalidates credential-derived health, validation and model-catalog data.
// A process restart may follow credential rotation while the process was down;
// without a persisted credential fingerprint, reusing old validation is unsafe.
// Router.SetSnapshot continues to use Restore (not RestoreStartup) so live reloads
// preserve the current credential generation and health state.
func (p *Pool) RestoreStartup(s PoolSnapshot) {
	p.Restore(s)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		k.disabled = false
		k.health = "unknown"
		k.errors = 0
		k.openUntil = time.Time{}
		k.openCount = 0
		k.probing = false
		k.validated = false
		k.lastOK = time.Time{}
		k.catalogKnown = false
		k.catalogAt = time.Time{}
		k.catalog = nil
		k.ttfbEWMA = map[string]time.Duration{}
		k.totalEWMA = map[string]time.Duration{}
	}
}

func restoreTime(now, wall time.Time, ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	t := time.UnixMilli(ms)
	if t.After(wall) {
		t = wall
	}
	age := wall.Sub(t)
	return now.Add(-age)
}
func restoreWindow(w *sliding.Window, now, wall time.Time, entries []wallEntry) {
	es := make([]sliding.Entry, 0, len(entries))
	for _, e := range entries {
		t := time.UnixMilli(e.AtMS)
		if t.After(wall) {
			t = wall
		}
		age := wall.Sub(t)
		es = append(es, sliding.Entry{At: now.Add(-age), N: e.N})
	}
	w.Absorb(now, es)
}

var _ = errors.Is
var _ = secret.New

func cloneDurationMap(in map[string]time.Duration) map[string]time.Duration {
	out := map[string]time.Duration{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
