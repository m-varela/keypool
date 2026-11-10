package sched

import (
	"time"

	"keypool/internal/secret"
	"keypool/internal/sliding"
)

type finishKind int

const (
	finishSuccess finishKind = iota
	finishRelease
	finishFailure
	finishNeutral
)

// Lease representa uma reserva e é concluído no máximo uma vez.
type Lease struct {
	pool                           *Pool
	id                             uint64
	key                            *keyState
	bucket, model, group           string
	estimated                      int
	created                        time.Time
	activityBefore                 time.Time
	warm                           bool
	tpmRef, tpdRef, rpmRef, rpdRef sliding.Ref
	done                           bool
}

// KeyID identifica a chave reservada.
func (l *Lease) KeyID() KeyID {
	if l == nil || l.pool == nil {
		return ""
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return l.key.spec.ID
}

// Label retorna o rótulo mascarado configurado para a chave.
func (l *Lease) Label() string {
	if l == nil || l.pool == nil {
		return ""
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return l.key.spec.Label
}

// Secret devolve o segredo encapsulado, sem convertê-lo em texto.
func (l *Lease) Secret() secret.Secret {
	if l == nil || l.pool == nil {
		return secret.Secret{}
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return l.key.spec.Secret
}

func (p *Pool) finishLocked(l *Lease, kind finishKind, actual int, h *Hints) {
	if l.done {
		return
	}
	now := p.now()
	w := p.windows(l.key, l.bucket)
	if kind == finishRelease {
		w.tpm.Adjust(now, l.tpmRef, 0)
		if l.tpdRef != 0 {
			w.tpd.Adjust(now, l.tpdRef, 0)
		}
		w.rpm.Adjust(now, l.rpmRef, 0)
		w.rpd.Adjust(now, l.rpdRef, 0)
	} else if kind == finishSuccess {
		if actual < 0 {
			actual = 0
		}
		w.tpm.Adjust(now, l.tpmRef, actual)
		if l.tpdRef != 0 {
			w.tpd.Adjust(now, l.tpdRef, actual)
		}
		l.key.usedTotal += actual
		elapsed := now.Sub(l.created)
		if elapsed < 0 {
			elapsed = 0
		}
		if !l.warm {
			l.key.health = "healthy"
			l.key.lastOK = now
			l.key.lastActivity = now
			if l.key.totalEWMA == nil {
				l.key.totalEWMA = map[string]time.Duration{}
			}
			prior := l.key.totalEWMA[l.model]
			if prior <= 0 {
				l.key.totalEWMA[l.model] = elapsed
			} else {
				l.key.totalEWMA[l.model] = time.Duration(float64(prior)*0.8 + float64(elapsed)*0.2)
			}
		}
		l.key.errors = 0
	} else if kind == finishFailure {
		l.key.errors++
		if l.key.errors < 3 && l.key.health != "invalid" && l.key.health != "probing" {
			l.key.health = "degraded"
		}
	}
	if w.inflight > 0 {
		w.inflight--
	}
	if l.key.inflight > 0 {
		l.key.inflight--
	}
	l.done = true
	delete(p.leases, l.id)
	if h != nil {
		g := p.group(l.key, l.bucket)
		if g.hints[l.bucket] == nil {
			g.hints[l.bucket] = map[string]Hint{}
		}
		for dim, v := range *h {
			g.hints[l.bucket][dim] = v
		}
	}
	if (kind == finishSuccess || kind == finishNeutral) && l.key.health == "probing" {
		// 429/neutral outcomes prove the endpoint is reachable; release the
		// half-open circuit without treating rate limiting as a failure.
		l.key.health = "healthy"
		l.key.probing = false
		l.key.openUntil = time.Time{}
		l.key.openCount = 0
		l.key.errors = 0
	}
	if kind == finishFailure && l.key.errors >= 3 {
		l.key.health = "open"
		delay := 30 * time.Second
		if l.key.openCount > 0 {
			delay *= time.Duration(1 << minInt(l.key.openCount, 5))
		}
		if delay > 10*time.Minute {
			delay = 10 * time.Minute
		}
		l.key.openCount++
		l.key.openUntil = now.Add(delay)
		l.key.probing = false
		p.emit(Event{Kind: "circuit.open", Subject: l.key.spec.Label, Detail: delay.String(), At: now})
	}
	if kind == finishRelease && l.key.health == "probing" {
		l.key.health = "open"
		l.key.openUntil = now
		l.key.probing = false
	}
	p.signal(l.bucket)
}

// Success ajusta a reserva ao consumo real e alimenta cabeçalhos de limite.
func (l *Lease) Success(actual int, h *Hints) {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	p.finishLocked(l, finishSuccess, actual, h)
	p.mu.Unlock()
}

// RateLimited registra cooldown por bucket e pode aprender que chaves dividem a conta.
func (l *Lease) RateLimited(retryAfter time.Duration, known bool, h *Hints) {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	now := p.now()
	if !known || retryAfter < 0 {
		retryAfter = 60 * time.Second
	}
	until := now.Add(retryAfter)
	isLong := retryAfter >= 120*time.Second
	targets := []*keyState{l.key}
	sharedID := p.groupID(l.key)
	if sharedID == "*" || p.cfg.Scope == ScopeGroup || p.cfg.Scope == ScopeAuto {
		targets = targets[:0]
		for _, candidate := range p.keys {
			if p.groupID(candidate) == sharedID {
				targets = append(targets, candidate)
			}
		}
	}
	for _, k := range targets {
		k.cooldowns[l.bucket] = maxTime(k.cooldowns[l.bucket], until)
		if isLong {
			if k.longCooldowns == nil {
				k.longCooldowns = map[string]time.Time{}
			}
			k.longCooldowns[l.model] = maxTime(k.longCooldowns[l.model], until)
		}
	}
	if p.cfg.Scope == ScopeAuto && !p.merged && retryAfter <= p.cfg.MergeMaxRetry {
		seen := p.limited[l.bucket]
		if seen == nil {
			seen = map[KeyID]time.Time{}
			p.limited[l.bucket] = seen
		}
		seen[l.key.spec.ID] = now
		count := 0
		for id, t := range seen {
			if now.Sub(t) <= p.cfg.MergeWindow && id != l.key.spec.ID {
				count++
			}
		}
		if count > 0 {
			p.mergeLocked(now)
			p.emit(Event{Kind: "scope.merged", Subject: p.cfg.Provider, At: now})
		}
	}
	p.finishLocked(l, finishNeutral, l.estimated, h)
	p.mu.Unlock()
}

// QuotaExhausted bloqueia a chave inteira em modelos gratuitos ou o bucket em modelos pagos.
func (l *Lease) QuotaExhausted() {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	now := p.now()
	until := now.Add(p.cfg.QuotaCooldown)
	resolved := p.cfg.Limits.Resolve(l.model)
	if resolved.Free {
		l.key.quotaUntil = maxTime(l.key.quotaUntil, until)
	} else {
		l.key.quotaBuckets[l.bucket] = maxTime(l.key.quotaBuckets[l.bucket], until)
	}
	p.finishLocked(l, finishNeutral, l.estimated, nil)
	p.mu.Unlock()
}

// AuthFailed invalida definitivamente a chave até uma recarga de configuração.
func (l *Lease) AuthFailed() {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	l.key.disabled = true
	l.key.health = "invalid"
	p.finishLocked(l, finishNeutral, l.estimated, nil)
	p.emit(Event{Kind: "key.invalid", Subject: l.key.spec.Label, At: p.now()})
	p.mu.Unlock()
}

// Forbidden bloqueia o par chave/modelo por uma hora; dois modelos distintos invalidam a chave.
func (l *Lease) Forbidden() {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	now := p.now()
	if l.key.forbidden == nil {
		l.key.forbidden = map[string]time.Time{}
	}
	l.key.forbidden[l.model] = now.Add(time.Hour)
	if len(l.key.forbidden) >= 2 {
		l.key.disabled = true
		l.key.health = "invalid"
		p.emit(Event{Kind: "key.invalid", Subject: l.key.spec.Label, Detail: "forbidden em múltiplos modelos", At: now})
	}
	p.finishLocked(l, finishNeutral, l.estimated, nil)
	p.mu.Unlock()
}

// Failed registra falha transitória e pode abrir o circuito.
func (l *Lease) Failed() {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	delaySeconds := 1 << minInt(l.key.errors, 5)
	if delaySeconds > 30 {
		delaySeconds = 30
	}
	l.key.allCooldown = maxTime(l.key.allCooldown, p.now().Add(time.Duration(delaySeconds)*time.Second))
	p.finishLocked(l, finishFailure, l.estimated, nil)
	p.mu.Unlock()
}

// Release devolve toda a reserva sem contabilizar uso.
func (l *Lease) Release() {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	p.finishLocked(l, finishRelease, 0, nil)
	p.mu.Unlock()
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// MarkWarm tells the scheduler that this reservation is housekeeping, not real
// traffic; its reservation still counts toward configured local rate windows.
func (l *Lease) MarkWarm() {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if l.done {
		return
	}
	l.warm = true
	if l.key.lastActivity.Equal(l.created) {
		l.key.lastActivity = l.activityBefore
	}
}

// RecordTTFB updates the per-key/per-model TTFB EWMA without exposing secrets.
func (l *Lease) RecordTTFB(ttfb time.Duration) {
	if l == nil || l.pool == nil || ttfb < 0 {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if l.done {
		return
	}
	if l.key.ttfbEWMA == nil {
		l.key.ttfbEWMA = map[string]time.Duration{}
	}
	old := l.key.ttfbEWMA[l.model]
	if old <= 0 {
		l.key.ttfbEWMA[l.model] = ttfb
	} else {
		l.key.ttfbEWMA[l.model] = time.Duration(float64(old)*0.8 + float64(ttfb)*0.2)
	}
}
