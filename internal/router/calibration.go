package router

import (
	"math"
)

func calibrationKey(provider, model string) string { return provider + "\x00" + model }

func clampFactor(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 1
	}
	if v < 0.5 {
		return 0.5
	}
	if v > 2.0 {
		return 2.0
	}
	return v
}

func (r *Router) tokenFactor(provider, model string) float64 {
	r.calibrationMu.RLock()
	defer r.calibrationMu.RUnlock()
	if factor := r.tokenFactors[calibrationKey(provider, model)]; factor > 0 {
		return factor
	}
	return 1
}

// recordTokenUsage aprende um fator EWMA por (provedor, modelo), sempre limitado a [0,5; 2,0].
func (r *Router) recordTokenUsage(provider, model string, messagesBytes, actualPrompt int) {
	if actualPrompt <= 0 || messagesBytes <= 0 {
		return
	}
	rawEstimate := (messagesBytes + 3) / 4
	if rawEstimate <= 0 {
		return
	}
	sample := clampFactor(float64(actualPrompt) / float64(rawEstimate))
	key := calibrationKey(provider, model)
	r.calibrationMu.Lock()
	old := r.tokenFactors[key]
	if old <= 0 {
		old = 1
	}
	r.tokenFactors[key] = clampFactor(old*0.8 + sample*0.2)
	r.calibrationMu.Unlock()
}

// TokenCalibration devolve o fator aprendido; sem amostras, o fator neutro é 1.
func (r *Router) TokenCalibration(provider, model string) float64 {
	return r.tokenFactor(provider, model)
}

// ResetTokenCalibration é útil em testes e para invalidar manualmente um modelo alterado.
func (r *Router) ResetTokenCalibration(provider, model string) {
	r.calibrationMu.Lock()
	delete(r.tokenFactors, calibrationKey(provider, model))
	r.calibrationMu.Unlock()
}
