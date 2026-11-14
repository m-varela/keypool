// Package limits resolve os limites efetivos de cada modelo.
package limits

import "strings"

// Resolved contém limites positivos; nil significa sem limite.
type Resolved struct {
	TPM, RPM, RPD, TPD, Conc *int
	Bucket                   string
	Free                     bool
}

// ModelLimits contém dimensões configuráveis.
type ModelLimits struct {
	TPM  *int `json:"tpm,omitempty"`
	RPM  *int `json:"rpm,omitempty"`
	RPD  *int `json:"rpd,omitempty"`
	TPD  *int `json:"tpd,omitempty"`
	Conc *int `json:"conc,omitempty"`
}

// Rule aplica limites a IDs que correspondem a um glob.
type Rule struct {
	Match  string `json:"match"`
	Bucket string `json:"bucket,omitempty"`
	Free   bool   `json:"free,omitempty"`
	ModelLimits
}

// Limits contém padrões, exceções por modelo e regras ordenadas.
type Limits struct {
	Default          ModelLimits            `json:"default"`
	PerModel         map[string]ModelLimits `json:"per_model"`
	Rules            []Rule                 `json:"rules"`
	RPDResetTimezone string                 `json:"rpd_reset_timezone,omitempty"`
}

func positive(v *int) *int {
	if v == nil || *v <= 0 {
		return nil
	}
	n := *v
	return &n
}
func clone(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

// Resolve segue ProviderLimits.resolve do Python: cada dimensão é independente; a primeira regra
// correspondente define bucket/free e preenche apenas dimensões ainda ausentes.
func (l Limits) Resolve(model string) Resolved {
	var out Resolved
	out.Bucket = model
	if v, ok := l.PerModel[model]; ok {
		out.TPM = clone(v.TPM)
		out.RPM = clone(v.RPM)
		out.RPD = clone(v.RPD)
		out.TPD = clone(v.TPD)
		out.Conc = clone(v.Conc)
	}
	for _, r := range l.Rules {
		if GlobMatch(strings.ToLower(r.Match), strings.ToLower(model)) {
			if out.TPM == nil {
				out.TPM = clone(r.TPM)
			}
			if out.RPM == nil {
				out.RPM = clone(r.RPM)
			}
			if out.RPD == nil {
				out.RPD = clone(r.RPD)
			}
			if out.TPD == nil {
				out.TPD = clone(r.TPD)
			}
			if out.Conc == nil {
				out.Conc = clone(r.Conc)
			}
			if r.Bucket != "" {
				out.Bucket = r.Bucket
			}
			out.Free = r.Free
			break
		}
	}
	if out.TPM == nil {
		out.TPM = clone(l.Default.TPM)
	}
	if out.RPM == nil {
		out.RPM = clone(l.Default.RPM)
	}
	if out.RPD == nil {
		out.RPD = clone(l.Default.RPD)
	}
	if out.TPD == nil {
		out.TPD = clone(l.Default.TPD)
	}
	if out.Conc == nil {
		out.Conc = clone(l.Default.Conc)
	}
	out.TPM = positive(out.TPM)
	out.RPM = positive(out.RPM)
	out.RPD = positive(out.RPD)
	out.TPD = positive(out.TPD)
	out.Conc = positive(out.Conc)
	return out
}

// GlobMatch implementa fnmatch: * também atravessa barras e ? casa um caractere.
func GlobMatch(pattern, s string) bool {
	p := []rune(pattern)
	x := []rune(s)
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(x) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == x[si]) {
			pi++
			si++
			continue
		}
		if pi < len(p) && p[pi] == '*' {
			star = pi
			mark = si
			pi++
			continue
		}
		if star >= 0 {
			pi = star + 1
			mark++
			si = mark
			continue
		}
		return false
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
