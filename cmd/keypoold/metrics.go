package main

import (
	"time"

	keymetrics "keypool/internal/metrics"
	"keypool/internal/router"
	"keypool/internal/warm"
)

func collectRuntimeMetrics(reg *keymetrics.Registry, rt *router.Router, wm *warm.Manager, usageEvicted func() uint64) {
	if reg == nil {
		return
	}
	for _, name := range []string{"keypool_key_state", "keypool_window_used_ratio", "keypool_cooldown_seconds", "keypool_warm_age_seconds"} {
		reg.ResetGauges(name)
	}
	if rt != nil {
		for _, provider := range rt.WarmProviders() {
			for _, key := range provider.Pool.Stats() {
				base := map[string]string{"provider": provider.Name, "key": string(key.ID)}
				state := key.Health
				if key.Disabled {
					state = "disabled"
				} else if key.CooldownRemaining > 0 && state != "invalid" {
					state = "cooldown"
				}
				for _, candidate := range []string{"healthy", "unknown", "degraded", "open", "probing", "invalid", "cooldown", "disabled"} {
					labels := cloneLabels(base)
					labels["state"] = candidate
					value := 0.0
					if candidate == state {
						value = 1
					}
					reg.Set("keypool_key_state", labels, value)
				}
				reg.Set("keypool_cooldown_seconds", mergeLabels(base, "bucket", "all", "dim", "all"), key.CooldownRemaining.Seconds())
				for bucket, used := range key.WindowUsed {
					resolved := provider.Pool.LimitsForModel(bucket)
					limitsByDim := map[string]*int{"tpm": resolved.TPM, "tpd": resolved.TPD, "rpm": resolved.RPM, "rpd": resolved.RPD}
					hints := provider.Pool.HintSnapshot(key.ID)[bucket]
					for dim, current := range used {
						cap := limitsByDim[dim]
						denominator := 0
						if cap != nil {
							denominator = *cap
						} else if hint, ok := hints[dim]; ok && hint.HasRemaining {
							denominator = current + hint.Remaining
						}
						if denominator > 0 {
							labels := mergeLabels(base, "bucket", bucket, "dim", dim)
							reg.Set("keypool_window_used_ratio", labels, float64(current)/float64(denominator))
						}
					}
				}
			}
		}
	}
	if wm != nil {
		if status, ok := wm.Status().(warm.Status); ok {
			for _, key := range status.Keys {
				name := key.Provider + "/" + key.KeyID
				for probe, age := range map[string]time.Duration{"L0": key.LastL0Age, "L1": key.LastL1Age, "L2": key.LastL2Age} {
					if age > 0 {
						reg.Set("keypool_warm_age_seconds", map[string]string{"key": name, "probe": probe}, age.Seconds())
					}
				}
			}
		}
	}
	if usageEvicted != nil {
		reg.SetCounter("keypool_usage_dropped_total", nil, float64(usageEvicted()))
	}
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func mergeLabels(base map[string]string, pairs ...string) map[string]string {
	out := cloneLabels(base)
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = pairs[i+1]
	}
	return out
}
