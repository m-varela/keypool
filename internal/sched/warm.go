package sched

import (
	"sort"
	"sync"
	"time"

	"keypool/internal/limits"
)

// BeginProbe atomically reserves an idle key for one warm probe. Real traffic
// cannot acquire the key until the returned release function is called.
// force bypasses only the idle threshold, never inflight/circuit/admin state.
func (p *Pool) BeginProbe(id KeyID, idleFor time.Duration, allowInvalid, force bool) (func(), bool) {
	p.mu.Lock()
	k := p.byID[id]
	if k == nil || k.warming || k.inflight != 0 || (k.health == "open" && p.now().Before(k.openUntil)) {
		p.mu.Unlock()
		return nil, false
	}
	if k.disabled && !(allowInvalid && k.health == "invalid") {
		p.mu.Unlock()
		return nil, false
	}
	if k.health == "invalid" && !allowInvalid {
		p.mu.Unlock()
		return nil, false
	}
	if !force && idleFor > 0 && !k.lastActivity.IsZero() && p.now().Sub(k.lastActivity) < idleFor {
		p.mu.Unlock()
		return nil, false
	}
	k.warming = true
	p.signal("")
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if current := p.byID[id]; current != nil {
				current.warming = false
			}
			p.signal("")
			p.mu.Unlock()
		})
	}, true
}

// ProbeEligible reports whether an L0/L1/L2 probe may run without interfering
// with real traffic. Invalid keys remain probeable so L1 can revalidate them.
func (p *Pool) ProbeEligible(id KeyID, idleFor time.Duration) bool {
	return p.CanProbe(id, idleFor, true, false)
}

// CanProbe applies idle, inflight, disabled/invalid and circuit constraints.
// force bypasses only the idle threshold; it never bypasses inflight or circuit state.
func (p *Pool) CanProbe(id KeyID, idleFor time.Duration, allowInvalid, force bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil || k.inflight != 0 || k.health == "open" && p.now().Before(k.openUntil) {
		return false
	}
	if k.disabled && !(allowInvalid && k.health == "invalid") {
		return false
	}
	if k.health == "invalid" && !allowInvalid {
		return false
	}
	if !force && idleFor > 0 && !k.lastActivity.IsZero() && p.now().Sub(k.lastActivity) < idleFor {
		return false
	}
	return true
}

// RecordConnectionProbe tracks L0 transport health without changing credential
// validity or re-enabling a key that was disabled by an administrator.
func (p *Pool) RecordConnectionProbe(id KeyID, ok bool, at time.Time, detail string) {
	p.mu.Lock()
	k := p.byID[id]
	if k == nil {
		p.mu.Unlock()
		return
	}
	if at.IsZero() {
		at = p.now()
	}
	if k.health == "invalid" || k.disabled {
		p.mu.Unlock()
		return
	}
	if ok {
		k.errors = 0
		k.health = "healthy"
		k.probing = false
		k.openUntil = time.Time{}
		k.openCount = 0
	} else {
		k.errors++
		if k.errors < 3 && k.health != "invalid" && k.health != "probing" {
			k.health = "degraded"
		}
		if k.errors >= 3 {
			delay := 30 * time.Second
			if k.openCount > 0 {
				delay *= time.Duration(1 << minInt(k.openCount, 5))
			}
			if delay > 10*time.Minute {
				delay = 10 * time.Minute
			}
			k.openCount++
			k.health = "open"
			k.openUntil = at.Add(delay)
			k.probing = false
			p.emit(Event{Kind: "circuit.open", Subject: k.spec.Label, Detail: delay.String() + ": " + detail, At: at})
		}
	}
	p.signal("")
	p.mu.Unlock()
}

// RecordProbe updates health from an explicit validation result. HTTP 429 must
// be reported as neutral by the caller and never passed as invalid.
func (p *Pool) RecordProbe(id KeyID, ok, invalid bool, at time.Time, detail string) {
	p.mu.Lock()
	k := p.byID[id]
	if k == nil {
		p.mu.Unlock()
		return
	}
	if at.IsZero() {
		at = p.now()
	}
	if ok {
		k.health = "healthy"
		k.disabled = false
		k.probing = false
		k.errors = 0
		k.openCount = 0
		k.openUntil = time.Time{}
		k.lastOK = at
		k.validated = true
	} else if invalid {
		k.health = "invalid"
		k.disabled = true
		k.probing = false
		k.openUntil = time.Time{}
		k.errors = 0
		k.validated = false
	} else {
		k.errors++
		if k.errors < 3 && k.health != "invalid" && k.health != "probing" {
			k.health = "degraded"
		}
		if k.errors >= 3 {
			delay := 30 * time.Second
			if k.openCount > 0 {
				delay *= time.Duration(1 << minInt(k.openCount, 5))
			}
			if delay > 10*time.Minute {
				delay = 10 * time.Minute
			}
			k.openCount++
			k.health = "open"
			k.openUntil = at.Add(delay)
			k.probing = false
			p.emit(Event{Kind: "circuit.open", Subject: k.spec.Label, Detail: delay.String() + ": " + detail, At: at})
		}
	}
	p.signal("")
	p.mu.Unlock()
}

// ResetKey is the explicit administrative re-enable path for an invalid key.
func (p *Pool) ResetKey(id KeyID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil {
		return false
	}
	k.disabled = false
	k.credentialVersion++
	k.health = "unknown"
	k.errors = 0
	k.probing = false
	k.openCount = 0
	k.openUntil = time.Time{}
	k.validated = false
	p.signal("")
	return true
}

// ResetCredential clears state that belongs to a replaced secret and advances
// the credential generation monotonically. The caller supplies the previous
// generation captured from the old runtime so snapshots cannot make a rotated
// credential look unchanged after a router rebuild.
func (p *Pool) ResetCredential(id KeyID, previousVersion uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil {
		return false
	}
	next := previousVersion + 1
	if next <= k.credentialVersion {
		next = k.credentialVersion + 1
	}
	k.credentialVersion = next
	k.disabled = false
	k.health = "unknown"
	k.errors = 0
	k.probing = false
	k.openCount = 0
	k.openUntil = time.Time{}
	k.validated = false
	k.lastOK = time.Time{}
	k.ttfbEWMA = map[string]time.Duration{}
	k.totalEWMA = map[string]time.Duration{}
	k.catalogKnown = false
	k.catalogAt = time.Time{}
	k.catalog = nil
	p.signal("")
	return true
}

// SetCatalog replaces the visible model catalog for a key.
func (p *Pool) SetCatalog(id KeyID, models []string) { p.SetCatalogAt(id, models, p.now()) }

// SetCatalogAt stores model ids and the timestamp used for the one-hour TTL.
func (p *Pool) SetCatalogAt(id KeyID, models []string, seenAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil {
		return
	}
	catalog := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != "" {
			catalog[model] = struct{}{}
		}
	}
	k.catalog = catalog
	k.catalogKnown = true
	if seenAt.IsZero() {
		seenAt = p.now()
	}
	k.catalogAt = seenAt
	p.signal("")
}

// ClearCatalog forgets a possibly stale catalog without asserting that an
// authoritative empty catalog was returned. This is useful when L1 is disabled.
func (p *Pool) ClearCatalog(id KeyID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k := p.byID[id]; k != nil {
		k.catalog = nil
		k.catalogKnown = false
		k.catalogAt = time.Time{}
		p.signal("")
	}
}

// ModelAllowed enforces the per-key catalog after its first successful refresh.
// Dynamic model aliases remain usable after the credential itself was validated.
func (p *Pool) ModelAllowed(id KeyID, model string, dynamic bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil {
		return false
	}
	if !k.catalogKnown {
		return true
	}
	// Expired catalogs cannot authorize fixed models. Dynamic aliases remain
	// available only after an L1 credential validation; the warm scheduler is
	// responsible for refreshing the catalog in the background.
	catalogFresh := p.cfg.CatalogTTL <= 0 || (!k.catalogAt.IsZero() && p.now().Sub(k.catalogAt) <= p.cfg.CatalogTTL)
	if !catalogFresh {
		return dynamic && k.validated
	}
	if _, ok := k.catalog[model]; ok {
		return true
	}
	return dynamic && k.validated
}

// Catalog returns the last known model catalog for a key as a detached slice.
func (p *Pool) Catalog(id KeyID) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil || !k.catalogKnown {
		return nil, false
	}
	out := make([]string, 0, len(k.catalog))
	for model := range k.catalog {
		out = append(out, model)
	}
	sort.Strings(out)
	return out, true
}

// HintBuckets resolves the unique local buckets represented by the given models.
func (p *Pool) HintBuckets(models []string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	// The empty bucket is the provider/account-wide floor. Keep it even after
	// the model catalog is known; otherwise global RPD hints would be copied
	// only to currently listed buckets and could be missed by a new alias.
	set := map[string]struct{}{"": {}}
	if len(models) == 0 {
		set[p.cfg.Limits.Resolve("").Bucket] = struct{}{}
	} else {
		for _, model := range models {
			set[p.cfg.Limits.Resolve(model).Bucket] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for bucket := range set {
		out = append(out, bucket)
	}
	sort.Strings(out)
	return out
}

// ApplyHints merges provider-observed limits into a local bucket.
func (p *Pool) ApplyHints(id KeyID, bucket string, hints Hints) {
	if len(hints) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil {
		return
	}
	g := p.group(k, bucket)
	if g.hints[bucket] == nil {
		g.hints[bucket] = map[string]Hint{}
	}
	for dim, hint := range hints {
		g.hints[bucket][dim] = hint
	}
	p.signal(bucket)
}

// RecordNeutralProbe records a reachable endpoint response (for example 429 or
// 401 during L0) without changing credential validity or counting a breaker error.
func (p *Pool) RecordNeutralProbe(id KeyID, at time.Time) {
	p.mu.Lock()
	k := p.byID[id]
	if k == nil {
		p.mu.Unlock()
		return
	}
	if at.IsZero() {
		at = p.now()
	}
	if k.health == "probing" || k.health == "open" && !at.Before(k.openUntil) {
		k.health = "healthy"
		k.probing = false
		k.openUntil = time.Time{}
		k.openCount = 0
		k.errors = 0
	}
	p.signal("")
	p.mu.Unlock()
}

// HintSnapshot returns a detached copy of all learned provider limits for a key.
func (p *Pool) HintSnapshot(id KeyID) map[string]Hints {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil {
		return nil
	}
	groupID := p.groupID(k)
	out := map[string]Hints{}
	copyGroup := func(bucket string, group *groupState) {
		if group == nil || len(group.hints[bucket]) == 0 {
			return
		}
		values := Hints{}
		for dimension, hint := range group.hints[bucket] {
			values[dimension] = hint
		}
		out[bucket] = values
	}
	if groupID == "*" {
		for bucket, group := range p.shared {
			copyGroup(bucket, group)
		}
	} else if group := p.groups[groupID]; group != nil {
		for bucket := range group.hints {
			copyGroup(bucket, group)
		}
	}
	return out
}

// KeyActivity returns a wall-clock view of activity and status for diagnostics.
func (p *Pool) KeyActivity(id KeyID) (lastActivity, lastOK time.Time, inflight int, health string, found bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil {
		return time.Time{}, time.Time{}, 0, "", false
	}
	return k.lastActivity, k.lastOK, k.inflight, k.health, true
}

// Keys returns a secret-safe copy of scheduler key specifications. The Secret
// value remains encapsulated; callers can only pass it to trusted adapters.
func (p *Pool) Keys() []KeySpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]KeySpec, 0, len(p.keys))
	for _, k := range p.keys {
		sp := k.spec
		sp.Tags = append([]string(nil), sp.Tags...)
		out = append(out, sp)
	}
	return out
}

// LimitsForModel resolves the configured limits for one model.
func (p *Pool) LimitsForModel(model string) limits.Resolved { return p.cfg.Limits.Resolve(model) }

// ProbeRetryAfter estimates when a currently busy key can be probed again.
// A short positive fallback prevents a missed initial pass from being treated
// as complete while a circuit is still waiting for half-open eligibility.
func (p *Pool) ProbeRetryAfter(id KeyID, idleFor time.Duration) time.Duration {
	return p.ProbeRetryAfterPolicy(id, idleFor, false)
}

// ProbeRetryAfterPolicy returns a retry delay for a busy probe reservation.
// Invalid/disabled keys are terminal for L0/L2; L1 may still revalidate them.
func (p *Pool) ProbeRetryAfterPolicy(id KeyID, idleFor time.Duration, allowInvalid bool) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byID[id]
	if k == nil || ((k.disabled || k.health == "invalid") && !allowInvalid) {
		return 0
	}
	now := p.now()
	if k.health == "open" && k.openUntil.After(now) {
		return k.openUntil.Sub(now)
	}
	if k.inflight > 0 || k.warming {
		return 3 * time.Second
	}
	if idleFor > 0 && !k.lastActivity.IsZero() {
		remaining := idleFor - now.Sub(k.lastActivity)
		if remaining > 0 {
			return remaining
		}
	}
	return 3 * time.Second
}
