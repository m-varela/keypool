// Package config carrega JSON e constrói snapshots imutáveis de execução.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"keypool/internal/limits"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Raw é a configuração de desenvolvimento antes da validação.
type Raw struct {
	Listen         string                 `json:"listen,omitempty"`
	DefaultProject string                 `json:"default_project,omitempty"`
	ProjectFile    string                 `json:"-"`
	APIKey         string                 `json:"-"`
	Providers      map[string]ProviderRaw `json:"providers,omitempty"`
	Projects       map[string]ProjectRaw  `json:"projects,omitempty"`
	Warm           WarmRaw                `json:"warm,omitempty"`
}

// WarmRaw configura o aquecimento e a política de prontidão. Ponteiros booleanos
// distinguem um false explícito dos valores padrão seguros.
type WarmRaw struct {
	Enabled             *bool                      `json:"enabled,omitempty"`
	ReadyTimeout        string                     `json:"ready_timeout,omitempty"`
	Workers             int                        `json:"workers,omitempty"`
	IdleThreshold       string                     `json:"idle_threshold,omitempty"`
	CatalogTTL          string                     `json:"catalog_ttl,omitempty"`
	CountsAgainstLimits bool                       `json:"counts_against_limits,omitempty"`
	L0Enabled           *bool                      `json:"l0_enabled,omitempty"`
	L0Interval          string                     `json:"l0_interval,omitempty"`
	L1Enabled           *bool                      `json:"l1_enabled,omitempty"`
	L1Interval          string                     `json:"l1_interval,omitempty"`
	L1Jitter            float64                    `json:"l1_jitter,omitempty"`
	AuthRecheck         string                     `json:"auth_recheck,omitempty"`
	L2                  WarmL2Raw                  `json:"l2,omitempty"`
	Providers           map[string]WarmProviderRaw `json:"providers,omitempty"`
}

type WarmProviderRaw struct {
	Enabled   *bool `json:"enabled,omitempty"`
	L0Enabled *bool `json:"l0_enabled,omitempty"`
	L1Enabled *bool `json:"l1_enabled,omitempty"`
	L2Enabled *bool `json:"l2_enabled,omitempty"`
}

type WarmProviderConfig struct{ Enabled, L0Enabled, L1Enabled, L2Enabled bool }

func (c WarmConfig) ForProvider(name string) WarmProviderConfig {
	out := WarmProviderConfig{Enabled: c.Enabled, L0Enabled: c.L0Enabled, L1Enabled: c.L1Enabled, L2Enabled: c.L2.Enabled}
	if override, ok := c.Providers[name]; ok {
		out.Enabled = out.Enabled && override.Enabled
		out.L0Enabled = out.L0Enabled && override.L0Enabled
		out.L1Enabled = out.L1Enabled && override.L1Enabled
		out.L2Enabled = out.L2Enabled && override.L2Enabled
	}
	return out
}

type WarmL2Raw struct {
	Enabled          bool   `json:"enabled,omitempty"`
	Model            string `json:"model,omitempty"`
	Interval         string `json:"interval,omitempty"`
	IdleInterval     string `json:"idle_interval,omitempty"`
	DailyTokenBudget int    `json:"daily_token_budget,omitempty"`
	MinRPD           int    `json:"min_rpd,omitempty"`
}

// WarmConfig contém valores normalizados usados pelo serviço de aquecimento.
type WarmConfig struct {
	Enabled, L0Enabled, L1Enabled                                                bool
	ReadyTimeout, IdleThreshold, CatalogTTL, L0Interval, L1Interval, AuthRecheck time.Duration
	Workers                                                                      int
	L1Jitter                                                                     float64
	CountsAgainstLimits                                                          bool
	L2                                                                           WarmL2Config
	Providers                                                                    map[string]WarmProviderConfig
}
type WarmL2Config struct {
	Enabled                  bool
	Model                    string
	Interval, IdleInterval   time.Duration
	DailyTokenBudget, MinRPD int
}

func (r WarmRaw) Build() (WarmConfig, error) {
	value := func(v *bool, fallback bool) bool {
		if v == nil {
			return fallback
		}
		return *v
	}
	parse := func(name, value string, fallback time.Duration) (time.Duration, error) {
		if strings.TrimSpace(value) == "" {
			return fallback, nil
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("config: warm.%s deve ser duração positiva", name)
		}
		return d, nil
	}
	out := WarmConfig{Enabled: value(r.Enabled, true), L0Enabled: value(r.L0Enabled, true), L1Enabled: value(r.L1Enabled, true), Workers: r.Workers, L1Jitter: r.L1Jitter, CountsAgainstLimits: r.CountsAgainstLimits, Providers: map[string]WarmProviderConfig{}}
	for name, override := range r.Providers {
		if strings.TrimSpace(name) == "" {
			return WarmConfig{}, fmt.Errorf("config: warm.providers contém nome vazio")
		}
		out.Providers[name] = WarmProviderConfig{
			Enabled:   value(override.Enabled, true),
			L0Enabled: value(override.L0Enabled, true),
			L1Enabled: value(override.L1Enabled, true),
			L2Enabled: value(override.L2Enabled, true),
		}
	}
	var err error
	if out.ReadyTimeout, err = parse("ready_timeout", r.ReadyTimeout, 20*time.Second); err != nil {
		return WarmConfig{}, err
	}
	if out.IdleThreshold, err = parse("idle_threshold", r.IdleThreshold, 45*time.Second); err != nil {
		return WarmConfig{}, err
	}
	if out.CatalogTTL, err = parse("catalog_ttl", r.CatalogTTL, time.Hour); err != nil {
		return WarmConfig{}, err
	}
	if out.L0Interval, err = parse("l0_interval", r.L0Interval, 45*time.Second); err != nil {
		return WarmConfig{}, err
	}
	if out.L1Interval, err = parse("l1_interval", r.L1Interval, 10*time.Minute); err != nil {
		return WarmConfig{}, err
	}
	if out.AuthRecheck, err = parse("auth_recheck", r.AuthRecheck, 2*time.Hour); err != nil {
		return WarmConfig{}, err
	}
	if out.Workers == 0 {
		out.Workers = 4
	}
	if out.Workers < 1 || out.Workers > 4 {
		return WarmConfig{}, fmt.Errorf("config: warm.workers deve estar entre 1 e 4")
	}
	if out.L1Jitter == 0 {
		out.L1Jitter = 0.20
	}
	if out.L1Jitter < 0 || out.L1Jitter > 0.20 {
		return WarmConfig{}, fmt.Errorf("config: warm.l1_jitter deve estar entre 0 e 0.20")
	}
	if out.L2.Interval, err = parse("l2.interval", r.L2.Interval, time.Hour); err != nil {
		return WarmConfig{}, err
	}
	if out.L2.IdleInterval, err = parse("l2.idle_interval", r.L2.IdleInterval, 15*time.Minute); err != nil {
		return WarmConfig{}, err
	}
	out.L2.Enabled, out.L2.Model = r.L2.Enabled, strings.TrimSpace(r.L2.Model)
	out.L2.DailyTokenBudget, out.L2.MinRPD = r.L2.DailyTokenBudget, r.L2.MinRPD
	if out.L2.MinRPD == 0 {
		out.L2.MinRPD = 100
	}
	if out.L2.DailyTokenBudget < 0 || out.L2.MinRPD < 0 {
		return WarmConfig{}, fmt.Errorf("config: warm.l2 budgets não podem ser negativos")
	}
	if out.L2.Enabled && (out.L2.Model == "" || out.L2.DailyTokenBudget < 1) {
		return WarmConfig{}, fmt.Errorf("config: warm.l2 exige model e daily_token_budget positivo")
	}
	return out, nil
}

// ProviderRaw associa um preset a limites, estratégia e credenciais do ambiente.
type ProviderRaw struct {
	Preset        string                             `json:"preset,omitempty"`
	Type          string                             `json:"type,omitempty"`
	Enabled       *bool                              `json:"enabled,omitempty"`
	EnvPrefix     string                             `json:"env_prefix,omitempty"`
	BaseURL       string                             `json:"base_url,omitempty"`
	Auth          provider.AuthSpec                  `json:"auth,omitempty"`
	Headers       map[string]string                  `json:"headers,omitempty"`
	RequestExtra  map[string]any                     `json:"request_extra,omitempty"`
	Timeout       string                             `json:"timeout,omitempty"`
	Capabilities  provider.CapabilitySpec            `json:"capabilities,omitempty"`
	RateHeaders   map[string]provider.RateHeaderSpec `json:"rate_headers,omitempty"`
	Paths         provider.PathsSpec                 `json:"paths,omitempty"`
	Probe         provider.ProbeSpec                 `json:"probe,omitempty"`
	LimitScope    string                             `json:"limit_scope,omitempty"`
	KeyStrategy   string                             `json:"key_strategy,omitempty"`
	QuotaCooldown string                             `json:"quota_cooldown,omitempty"`
	Limits        RawLimits                          `json:"limits,omitempty"`
	Keys          []sched.KeySpec                    `json:"-"`
}

// RawLimits é a forma declarativa dos limites do provedor.
type RawLimits struct {
	DefaultTPM  *int           `json:"default_tpm,omitempty"`
	DefaultRPM  *int           `json:"default_rpm,omitempty"`
	DefaultRPD  *int           `json:"default_rpd,omitempty"`
	DefaultTPD  *int           `json:"default_tpd,omitempty"`
	DefaultConc *int           `json:"default_conc,omitempty"`
	PerModel    RawModelLimits `json:"per_model,omitempty"`
	Rules       []limits.Rule  `json:"rules,omitempty"`
}

// RawModelLimits aceita o formato legado per_model: modelo: 1200, no qual o inteiro é TPM.
type RawModelLimits map[string]limits.ModelLimits

// UnmarshalJSON aceita inteiros legados (TPM) e objetos com dimensões tipadas.
func (r *RawModelLimits) UnmarshalJSON(data []byte) error {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("config: per_model deve ser um objeto: %w", err)
	}
	out := make(RawModelLimits, len(values))
	for name, value := range values {
		trimmed := strings.TrimSpace(string(value))
		if trimmed == "null" || trimmed == "" {
			return fmt.Errorf("config: per_model.%s deve ser inteiro ou objeto", name)
		}
		if strings.HasPrefix(trimmed, "{") {
			var model limits.ModelLimits
			if err := json.Unmarshal(value, &model); err != nil {
				return fmt.Errorf("config: per_model.%s: %w", name, err)
			}
			out[name] = model
			continue
		}
		var tpm int
		if err := json.Unmarshal(value, &tpm); err != nil {
			return fmt.Errorf("config: per_model.%s deve ser inteiro ou objeto: %w", name, err)
		}
		out[name] = limits.ModelLimits{TPM: &tpm}
	}
	*r = out
	return nil
}

func (r RawLimits) Build() limits.Limits {
	perModel := make(map[string]limits.ModelLimits, len(r.PerModel))
	for model, values := range r.PerModel {
		perModel[model] = values
	}
	return limits.Limits{Default: limits.ModelLimits{TPM: r.DefaultTPM, RPM: r.DefaultRPM, RPD: r.DefaultRPD, TPD: r.DefaultTPD, Conc: r.DefaultConc}, PerModel: perModel, Rules: r.Rules}
}

// Policy descreve limites do projeto.
type Policy struct {
	SpillOnSaturation     bool    `json:"spill_on_saturation,omitempty"`
	TPDReserve            float64 `json:"tpd_reserve,omitempty"`
	ReservePriorityCutoff int     `json:"reserve_priority_cutoff,omitempty"`
	MaxWait               string  `json:"max_wait,omitempty"`
	MaxInflight           int     `json:"max_inflight,omitempty"`
	DailyTokenBudget      int     `json:"daily_token_budget,omitempty"`
	spillConfigured       bool
}

// UnmarshalJSON preserva a distinção ao carregar a configuração persistida.
func (p *Policy) UnmarshalJSON(data []byte) error {
	type plain Policy
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = Policy(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, p.spillConfigured = fields["spill_on_saturation"]
	return nil
}

// MarshalJSON retém um false explícito em armazenamento mesmo com omitempty.
func (p Policy) MarshalJSON() ([]byte, error) {
	type plain Policy
	b, err := json.Marshal(plain(p))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	if p.spillConfigured || p.SpillOnSaturation {
		v, _ := json.Marshal(p.SpillOnSaturation)
		fields["spill_on_saturation"] = v
	}
	return json.Marshal(fields)
}

// SpillEnabled aplica true como padrão e respeita false quando declarado explicitamente.
func (p Policy) SpillEnabled() bool { return p.SpillOnSaturation || !p.spillConfigured }

// Selector define um conjunto de chaves por identificador, rótulo, tag ou conta.
type Selector struct {
	KeyIDs        []string `json:"key_ids,omitempty"`
	Labels        []string `json:"labels,omitempty"`
	TagsAny       []string `json:"tags_any,omitempty"`
	TagsAll       []string `json:"tags_all,omitempty"`
	AccountGroups []string `json:"account_groups,omitempty"`
}
type KeyRule struct {
	Include Selector `json:"include,omitempty"`
	Exclude Selector `json:"exclude,omitempty"`
}

// RouteRaw define um alias e sua cadeia de modelos.
type RouteRaw struct {
	Priority int             `json:"priority"`
	Defaults map[string]any  `json:"defaults,omitempty"`
	Force    map[string]any  `json:"force,omitempty"`
	Requires map[string]bool `json:"requires,omitempty"`
	Chain    []string        `json:"chain"`
}

// ProjectRaw é o perfil declarativo de um projeto.
type ProjectRaw struct {
	Project             string              `json:"project,omitempty"`
	Description         string              `json:"description,omitempty"`
	AllowLease          bool                `json:"allow_lease,omitempty"`
	AllowLiteralTargets bool                `json:"allow_literal_targets,omitempty"`
	Policy              Policy              `json:"policy,omitempty"`
	Keys                map[string]KeyRule  `json:"keys,omitempty"`
	Routes              map[string]RouteRaw `json:"routes,omitempty"`
}
type ProviderSnapshot struct {
	Spec     provider.Spec
	Limits   limits.Limits
	Keys     []sched.KeySpec
	Scope    sched.Scope
	Strategy sched.Strategy
	Enabled  bool
}
type Route struct {
	Priority int
	Defaults map[string][]byte
	Force    map[string][]byte
	Requires map[string]bool
	Chain    []Target
}
type Target struct{ Provider, Model string }
type ProjectSnapshot struct {
	Name, Description               string
	AllowLease, AllowLiteralTargets bool
	Policy                          Policy
	Keys                            map[string][]sched.KeyID
	Routes                          map[string]Route
}

// Snapshot é construído uma vez e tratado como imutável por todos os consumidores.
type Snapshot struct {
	listen, defaultProject string
	apiKeyHash             [32]byte
	providers              map[string]ProviderSnapshot
	projects               map[string]ProjectSnapshot
	warm                   WarmConfig
}

// Listen devolve o endereço configurado.
func (s *Snapshot) Listen() string {
	if s.listen == "" {
		return "127.0.0.1:8787"
	}
	return s.listen
}

// DefaultProject devolve o projeto padrão, se definido.
func (s *Snapshot) DefaultProject() string { return s.defaultProject }

// APIKeyHash devolve uma cópia do hash de autenticação.
func (s *Snapshot) APIKeyHash() [32]byte { return s.apiKeyHash }

// WarmConfig devolve a política normalizada de aquecimento.
func (s *Snapshot) WarmConfig() WarmConfig { return s.warm }

// Provider devolve a configuração de um provedor.
func (s *Snapshot) Provider(name string) (ProviderSnapshot, bool) {
	v, ok := s.providers[name]
	if !ok {
		return ProviderSnapshot{}, false
	}
	v.Keys = cloneKeys(v.Keys)
	v.Spec = cloneSpec(v.Spec)
	v.Limits = cloneLimits(v.Limits)
	return v, true
}

// Providers lista nomes dos provedores ativos.
func (s *Snapshot) Providers() []string {
	out := make([]string, 0, len(s.providers))
	for k, v := range s.providers {
		if v.Enabled {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Project devolve o perfil do projeto.
func (s *Snapshot) Project(name string) (ProjectSnapshot, bool) {
	v, ok := s.projects[name]
	if !ok {
		return ProjectSnapshot{}, false
	}
	v.Keys = cloneProjectKeys(v.Keys)
	v.Routes = cloneRoutes(v.Routes)
	return v, true
}

// Projects lista os nomes de projeto.
func (s *Snapshot) Projects() []string {
	out := make([]string, 0, len(s.projects))
	for k := range s.projects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Build valida Raw e gera um snapshot sem referências mutáveis externas.
func Build(raw *Raw) (*Snapshot, error) {
	if raw == nil {
		return nil, fmt.Errorf("config: configuração ausente")
	}
	if raw.APIKey == "" {
		return nil, fmt.Errorf("config: KEYPOOL_API_KEY ausente")
	}
	warmConfig, warmErr := raw.Warm.Build()
	if warmErr != nil {
		return nil, warmErr
	}
	s := &Snapshot{listen: raw.Listen, defaultProject: raw.DefaultProject, apiKeyHash: sha256.Sum256([]byte(raw.APIKey)), providers: map[string]ProviderSnapshot{}, projects: map[string]ProjectSnapshot{}, warm: warmConfig}
	for name, pr := range raw.Providers {
		sp := provider.Spec{Name: name, Type: pr.Type, BaseURL: pr.BaseURL, Auth: pr.Auth, Headers: pr.Headers, RequestExtra: pr.RequestExtra, Timeout: pr.Timeout, Capabilities: pr.Capabilities, RateHeaders: pr.RateHeaders, Paths: pr.Paths, Probe: pr.Probe, LimitScope: pr.LimitScope, KeyStrategy: pr.KeyStrategy, QuotaCooldown: pr.QuotaCooldown}
		if pr.Preset != "" {
			loaded, e := provider.LoadPreset(pr.Preset + ".json")
			if e != nil {
				return nil, fmt.Errorf("config: preset do provedor %s: %w", name, e)
			}
			base := *loaded
			if sp.Type != "" {
				base.Type = sp.Type
			}
			if sp.BaseURL != "" {
				base.BaseURL = sp.BaseURL
			}
			if sp.Auth.Scheme != "" {
				base.Auth = sp.Auth
			}
			if len(sp.Headers) > 0 {
				base.Headers = sp.Headers
			}
			if len(sp.RequestExtra) > 0 {
				base.RequestExtra = sp.RequestExtra
			}
			if sp.Timeout != "" {
				base.Timeout = sp.Timeout
			}
			if sp.Capabilities.MaxTokensField != "" {
				base.Capabilities = sp.Capabilities
			}
			if len(sp.RateHeaders) > 0 {
				base.RateHeaders = sp.RateHeaders
			}
			if sp.Paths.Chat != "" || sp.Paths.Models != "" {
				base.Paths = sp.Paths
			}
			if sp.Probe.Kind != "" {
				base.Probe = sp.Probe
			}
			if sp.LimitScope != "" {
				base.LimitScope = sp.LimitScope
			}
			if sp.KeyStrategy != "" {
				base.KeyStrategy = sp.KeyStrategy
			}
			if sp.QuotaCooldown != "" {
				base.QuotaCooldown = sp.QuotaCooldown
			}
			sp = base
		}
		if sp.Name == "" {
			sp.Name = name
		}
		if e := sp.Validate(); e != nil {
			return nil, e
		}
		enabled := true
		if pr.Enabled != nil {
			enabled = *pr.Enabled
		}
		scope := parseScope(sp.LimitScope)
		strategy := parseStrategy(sp.KeyStrategy)
		keys := cloneKeys(pr.Keys)
		s.providers[name] = ProviderSnapshot{Spec: cloneSpec(sp), Limits: cloneLimits(pr.Limits.Build()), Keys: keys, Scope: scope, Strategy: strategy, Enabled: enabled}
	}
	for name := range warmConfig.Providers {
		if _, ok := raw.Providers[name]; !ok {
			return nil, fmt.Errorf("config: warm.providers referencia provedor inexistente %q", name)
		}
	}
	for name, pr := range raw.Projects {
		if pr.Project != "" && pr.Project != name {
			return nil, fmt.Errorf("config: project %q diverge da chave %q", pr.Project, name)
		}
		if len(pr.Routes) == 0 {
			return nil, fmt.Errorf("config: projeto %s não possui rotas", name)
		}
		if pr.Policy.TPDReserve < 0 || pr.Policy.TPDReserve > 0.95 {
			return nil, fmt.Errorf("config: tpd_reserve do projeto %s deve estar entre 0 e 0.95", name)
		}
		if pr.Policy.ReservePriorityCutoff < 0 || pr.Policy.ReservePriorityCutoff > 9 {
			return nil, fmt.Errorf("config: reserve_priority_cutoff do projeto %s deve estar entre 0 e 9", name)
		}
		if pr.Policy.MaxInflight < 0 {
			return nil, fmt.Errorf("config: max_inflight do projeto %s nao pode ser negativo", name)
		}
		if pr.Policy.DailyTokenBudget < 0 {
			return nil, fmt.Errorf("config: daily_token_budget do projeto %s nao pode ser negativo", name)
		}
		if pr.Policy.MaxWait != "" {
			wait, parseErr := time.ParseDuration(pr.Policy.MaxWait)
			if parseErr != nil || wait <= 0 {
				return nil, fmt.Errorf("config: max_wait invalido no projeto %s", name)
			}
		}
		ps := ProjectSnapshot{Name: name, Description: pr.Description, AllowLease: pr.AllowLease, AllowLiteralTargets: pr.AllowLiteralTargets, Policy: pr.Policy, Keys: map[string][]sched.KeyID{}, Routes: map[string]Route{}}
		for providerName, rule := range pr.Keys {
			pv, ok := s.providers[providerName]
			if !ok {
				return nil, fmt.Errorf("config: provedor %s do projeto %s não existe", providerName, name)
			}
			allowed := []sched.KeyID{}
			if selectorEmpty(rule.Include) {
				continue
			}
			for _, k := range pv.Keys {
				if matchesSelector(k, rule.Include) && !matchesSelector(k, rule.Exclude) {
					allowed = append(allowed, k.ID)
				}
			}
			ps.Keys[providerName] = allowed
		}
		for alias, rr := range pr.Routes {
			if alias == "" || len(rr.Chain) == 0 {
				return nil, fmt.Errorf("config: rota %s inválida no projeto %s", alias, name)
			}
			if rr.Priority < 0 || rr.Priority > 9 {
				return nil, fmt.Errorf("config: prioridade da rota %s fora de 0..9", alias)
			}
			rt := Route{Priority: rr.Priority, Defaults: map[string][]byte{}, Force: map[string][]byte{}, Requires: map[string]bool{}, Chain: []Target{}}
			for k, v := range rr.Defaults {
				b, e := marshalValue(v)
				if e != nil {
					return nil, e
				}
				rt.Defaults[k] = b
			}
			for k, v := range rr.Force {
				b, e := marshalValue(v)
				if e != nil {
					return nil, e
				}
				rt.Force[k] = b
			}
			for k, v := range rr.Requires {
				rt.Requires[k] = v
			}
			for _, target := range rr.Chain {
				parts := strings.SplitN(target, ":", 2)
				if len(parts) != 2 || parts[1] == "" {
					return nil, fmt.Errorf("config: target inválido %q", target)
				}
				pv, exists := s.providers[parts[0]]
				if !exists {
					return nil, fmt.Errorf("config: provedor %s do target %s não existe", parts[0], target)
				}
				if pv.Spec.Type == "generic_http" {
					return nil, fmt.Errorf("config: provedor generic_http %s deve ser usado por /p/{provider}/*, não em rotas de chat", parts[0])
				}
				if len(ps.Keys[parts[0]]) == 0 {
					return nil, fmt.Errorf("config: rota %s usa provedor %s sem chaves incluídas", alias, parts[0])
				}
				rt.Chain = append(rt.Chain, Target{Provider: parts[0], Model: parts[1]})
			}
			ps.Routes[alias] = rt
		}
		s.projects[name] = ps
	}
	if len(s.projects) == 0 {
		return nil, fmt.Errorf("config: nenhum projeto configurado")
	}
	if s.defaultProject != "" {
		if _, ok := s.projects[s.defaultProject]; !ok {
			return nil, fmt.Errorf("config: default_project desconhecido")
		}
	}
	return s, nil
}
func cloneKeys(in []sched.KeySpec) []sched.KeySpec {
	out := append([]sched.KeySpec(nil), in...)
	for i := range out {
		out[i].Tags = append([]string(nil), out[i].Tags...)
	}
	return out
}
func cloneSpec(in provider.Spec) provider.Spec {
	out := in
	out.Headers = cloneStringMap(in.Headers)
	out.RequestExtra = make(map[string]any, len(in.RequestExtra))
	for k, v := range in.RequestExtra {
		out.RequestExtra[k] = cloneAny(v)
	}
	out.Capabilities.ReasoningEffortModels = append([]string(nil), in.Capabilities.ReasoningEffortModels...)
	out.RateHeaders = make(map[string]provider.RateHeaderSpec, len(in.RateHeaders))
	for k, v := range in.RateHeaders {
		out.RateHeaders[k] = v
	}
	return out
}
func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func cloneLimits(in limits.Limits) limits.Limits {
	out := in
	out.Default = cloneModelLimits(in.Default)
	out.PerModel = make(map[string]limits.ModelLimits, len(in.PerModel))
	for k, v := range in.PerModel {
		out.PerModel[k] = cloneModelLimits(v)
	}
	out.Rules = append([]limits.Rule(nil), in.Rules...)
	for i := range out.Rules {
		out.Rules[i].ModelLimits = cloneModelLimits(in.Rules[i].ModelLimits)
	}
	return out
}
func cloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = cloneAny(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneAny(item)
		}
		return out
	case []byte:
		return append([]byte(nil), x...)
	default:
		return v
	}
}
func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}
func cloneModelLimits(v limits.ModelLimits) limits.ModelLimits {
	return limits.ModelLimits{TPM: cloneInt(v.TPM), RPM: cloneInt(v.RPM), RPD: cloneInt(v.RPD), TPD: cloneInt(v.TPD), Conc: cloneInt(v.Conc)}
}

func cloneProjectKeys(in map[string][]sched.KeyID) map[string][]sched.KeyID {
	out := make(map[string][]sched.KeyID, len(in))
	for k, v := range in {
		out[k] = append([]sched.KeyID(nil), v...)
	}
	return out
}
func cloneRoutes(in map[string]Route) map[string]Route {
	out := make(map[string]Route, len(in))
	for k, v := range in {
		c := v
		c.Chain = append([]Target(nil), v.Chain...)
		c.Defaults = cloneBytesMap(v.Defaults)
		c.Force = cloneBytesMap(v.Force)
		c.Requires = make(map[string]bool, len(v.Requires))
		for rk, rv := range v.Requires {
			c.Requires[rk] = rv
		}
		out[k] = c
	}
	return out
}
func cloneBytesMap(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

func marshalValue(v any) ([]byte, error) { return json.Marshal(v) }
func parseScope(s string) sched.Scope {
	switch s {
	case "group":
		return sched.ScopeGroup
	case "provider":
		return sched.ScopeProvider
	case "auto":
		return sched.ScopeAuto
	default:
		return sched.ScopeKey
	}
}
func parseStrategy(s string) sched.Strategy {
	switch s {
	case "least_used":
		return sched.LeastUsed
	case "headroom":
		return sched.Headroom
	case "latency":
		return sched.Latency
	default:
		return sched.RoundRobin
	}
}
func selectorEmpty(s Selector) bool {
	return len(s.KeyIDs)+len(s.Labels)+len(s.TagsAny)+len(s.TagsAll)+len(s.AccountGroups) == 0
}
func matchesSelector(k sched.KeySpec, s Selector) bool {
	if selectorEmpty(s) {
		return false
	}
	if len(s.KeyIDs) > 0 && !contains(s.KeyIDs, string(k.ID)) {
		return false
	}
	if len(s.Labels) > 0 && !contains(s.Labels, k.Label) {
		return false
	}
	if len(s.AccountGroups) > 0 && !contains(s.AccountGroups, k.Group) {
		return false
	}
	for _, tag := range s.TagsAny {
		if contains(k.Tags, tag) {
			goto anyDone
		}
	}
	if len(s.TagsAny) > 0 {
		return false
	}
anyDone:
	for _, tag := range s.TagsAll {
		if !contains(k.Tags, tag) {
			return false
		}
	}
	return true
}
func contains(a []string, v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}

func decodeJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("conteúdo adicional após documento JSON")
	}
	return nil
}

// LoadFile lê a configuração JSON, um perfil de projeto e as chaves do ambiente.
func LoadFile(path string, environ map[string]string) (*Raw, error) {
	if environ == nil {
		environ = map[string]string{}
		for _, item := range os.Environ() {
			pair := strings.SplitN(item, "=", 2)
			if len(pair) == 2 {
				environ[pair[0]] = pair[1]
			}
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Raw
		ProjectFile string `json:"project_file,omitempty"`
	}
	if err = decodeJSON(data, &file); err != nil {
		return nil, fmt.Errorf("config: ler %s: %w", path, err)
	}
	raw := file.Raw
	raw.ProjectFile = file.ProjectFile
	if raw.APIKey == "" {
		raw.APIKey = environ["KEYPOOL_API_KEY"]
	}
	for name, provider := range raw.Providers {
		prefix := provider.EnvPrefix
		if prefix == "" {
			prefix = name
		}
		provider.Keys = keysFromEnv(prefix, environ)
		raw.Providers[name] = provider
	}
	if raw.ProjectFile != "" {
		projectPath := raw.ProjectFile
		if !filepath.IsAbs(projectPath) {
			projectPath = filepath.Join(filepath.Dir(path), projectPath)
		}
		projectData, readErr := os.ReadFile(projectPath)
		if readErr != nil {
			return nil, readErr
		}
		var project ProjectRaw
		if err = decodeJSON(projectData, &project); err != nil {
			return nil, fmt.Errorf("config: ler projeto: %w", err)
		}
		if project.Project == "" {
			return nil, fmt.Errorf("config: project obrigatório no arquivo de projeto")
		}
		if raw.Projects == nil {
			raw.Projects = map[string]ProjectRaw{}
		}
		raw.Projects[project.Project] = project
	}
	return &raw, nil
}

func keysFromEnv(prefix string, env map[string]string) []sched.KeySpec {
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	var values []string
	split := func(v string) {
		for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n' }) {
			values = append(values, part)
		}
	}
	split(env[prefix+"_API_KEYS"])
	if v := strings.TrimSpace(env[prefix+"_API_KEY"]); v != "" {
		values = append(values, v)
	}
	type pair struct {
		n int
		v string
	}
	var numbered []pair
	for name, v := range env {
		suffix := strings.TrimPrefix(name, prefix+"_API_KEY_")
		if suffix == name || strings.TrimSpace(v) == "" {
			continue
		}
		n, e := strconv.Atoi(suffix)
		if e == nil && n >= 0 {
			numbered = append(numbered, pair{n, strings.TrimSpace(v)})
		}
	}
	sort.SliceStable(numbered, func(i, j int) bool { return numbered[i].n < numbered[j].n })
	for _, p := range numbered {
		values = append(values, p.v)
	}
	seen := map[string]bool{}
	out := []sched.KeySpec{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		id := fmt.Sprintf("%s-%d", strings.ToLower(prefix), len(out)+1)
		out = append(out, sched.KeySpec{ID: sched.KeyID(id), Label: secret.Mask(v), Secret: secret.New(v), Tags: []string{"mathlab"}})
	}
	return out
}

// ParseMaxWait converte o max_wait configurado.
func (p ProjectSnapshot) ParseMaxWait() time.Duration {
	if p.Policy.MaxWait == "" {
		return 10 * time.Minute
	}
	d, e := time.ParseDuration(p.Policy.MaxWait)
	if e != nil {
		return 10 * time.Minute
	}
	return d
}

// KeysFromEnv extrai credenciais seguindo exatamente a regra usada por LoadFile.
// O mapa de ambiente deve ser construído pelo chamador; valores nunca são logados.
func KeysFromEnv(prefix string, environ map[string]string) []sched.KeySpec {
	return keysFromEnv(prefix, environ)
}
