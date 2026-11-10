package sched

import (
	"sort"
	"time"

	"keypool/internal/sliding"
)

// Snapshot captura janelas, cooldowns e saúde usando timestamps de parede.
func (p *Pool) Snapshot() PoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.sweepLeasesLocked(now)
	s := PoolSnapshot{SavedAtMS: now.UnixMilli(), Merged: p.merged, Groups: map[string]map[string]bucketSnapshot{}}
	for _, k := range p.keys {
		ks := keySnapshot{ID: k.spec.ID, Disabled: k.disabled, Health: k.health, UsedTotal: k.usedTotal, Errors: k.errors, AllCooldownMS: zeroOrMS(k.allCooldown), QuotaUntilMS: zeroOrMS(k.quotaUntil), Cooldowns: map[string]int64{}, LongCooldowns: map[string]int64{}, ForbiddenModels: map[string]int64{}, QuotaBuckets: map[string]int64{}, OpenUntilMS: zeroOrMS(k.openUntil), LastActivityMS: zeroOrMS(k.lastActivity), LastOKMS: zeroOrMS(k.lastOK), CatalogAtMS: zeroOrMS(k.catalogAt), OpenCount: k.openCount, CredentialVersion: k.credentialVersion, Validated: k.validated, CatalogKnown: k.catalogKnown, TTFBEWMA: map[string]int64{}, TotalEWMA: map[string]int64{}}
		for model := range k.catalog {
			ks.Catalog = append(ks.Catalog, model)
		}
		sort.Strings(ks.Catalog)
		for model, duration := range k.ttfbEWMA {
			ks.TTFBEWMA[model] = int64(duration)
		}
		for model, duration := range k.totalEWMA {
			ks.TotalEWMA[model] = int64(duration)
		}
		for b, t := range k.cooldowns {
			ks.Cooldowns[b] = zeroOrMS(t)
		}
		for model, t := range k.longCooldowns {
			ks.LongCooldowns[model] = zeroOrMS(t)
		}
		for model, t := range k.forbidden {
			ks.ForbiddenModels[model] = zeroOrMS(t)
		}
		for b, t := range k.quotaBuckets {
			ks.QuotaBuckets[b] = zeroOrMS(t)
		}
		s.Keys = append(s.Keys, ks)
	}
	for gid, g := range p.groups {
		if len(g.buckets) == 0 {
			continue
		}
		s.Groups[gid] = snapshotBuckets(now, g)
	}
	if len(p.shared) > 0 {
		merged := newGroup()
		for _, g := range p.shared {
			for b, w := range g.buckets {
				dst := merged.buckets[b]
				if dst == nil {
					dst = &bucketWindows{tpm: sliding.New(time.Minute), tpd: sliding.New(24 * time.Hour), rpm: sliding.New(time.Minute), rpd: sliding.New(24 * time.Hour)}
					merged.buckets[b] = dst
				}
				dst.tpm.Absorb(now, w.tpm.Entries(now))
				dst.tpd.Absorb(now, w.tpd.Entries(now))
				dst.rpm.Absorb(now, w.rpm.Entries(now))
				dst.rpd.Absorb(now, w.rpd.Entries(now))
			}
		}
		s.Groups["*"] = snapshotBuckets(now, merged)
	}
	return s
}
func zeroOrMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
func snapshotBuckets(now time.Time, g *groupState) map[string]bucketSnapshot {
	out := map[string]bucketSnapshot{}
	for b, w := range g.buckets {
		out[b] = bucketSnapshot{TPM: windowSnapshot(now, w.tpm), TPD: windowSnapshot(now, w.tpd), RPM: windowSnapshot(now, w.rpm), RPD: windowSnapshot(now, w.rpd), Inflight: w.inflight}
	}
	return out
}
func windowSnapshot(now time.Time, w *sliding.Window) dimensionSnapshot {
	out := dimensionSnapshot{}
	for _, e := range w.Entries(now) {
		out.Entries = append(out.Entries, wallEntry{AtMS: e.At.UnixMilli(), N: e.N})
	}
	return out
}

func (p *Pool) mergeLocked(now time.Time) {
	if p.merged {
		return
	}
	p.merged = true
	for _, g := range p.groups {
		for b, w := range g.buckets {
			dst := p.shared[b]
			if dst == nil {
				dst = newGroup()
				p.shared[b] = dst
			}
			dw := dst.buckets[b]
			if dw == nil {
				dw = &bucketWindows{tpm: sliding.New(time.Minute), tpd: sliding.New(24 * time.Hour), rpm: sliding.New(time.Minute), rpd: sliding.New(24 * time.Hour)}
				dst.buckets[b] = dw
			}
			dw.tpm.Absorb(now, w.tpm.Entries(now))
			dw.tpd.Absorb(now, w.tpd.Entries(now))
			dw.rpm.Absorb(now, w.rpm.Entries(now))
			dw.rpd.Absorb(now, w.rpd.Entries(now))
		}
	}
	for _, g := range p.shared {
		_ = g
	}
	all := time.Time{}
	cool := map[string]time.Time{}
	for _, k := range p.keys {
		all = maxTime(all, k.allCooldown)
		for b, t := range k.cooldowns {
			cool[b] = maxTime(cool[b], t)
		}
	}
	for _, k := range p.keys {
		k.allCooldown = all
		for b, t := range cool {
			k.cooldowns[b] = t
		}
	}
	p.groups = map[string]*groupState{}
	for b := range p.shared {
		p.signal(b)
	}
}
