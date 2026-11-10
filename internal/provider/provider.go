// Package provider declara adaptadores e especificações carregáveis por YAML.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"keypool/internal/classify"
	"keypool/internal/sched"
	"keypool/internal/secret"
)

// AuthSpec descreve como injetar a credencial no upstream.
type AuthSpec struct {
	Scheme string `yaml:"scheme" json:"scheme"`
	Header string `yaml:"header,omitempty" json:"header,omitempty"`
	Query  string `yaml:"query,omitempty" json:"query,omitempty"`
}
type CapabilitySpec struct {
	MaxTokensField        string   `yaml:"max_tokens_field" json:"max_tokens_field"`
	ReasoningStyle        string   `yaml:"reasoning_style" json:"reasoning_style"`
	ReasoningEffortModels []string `yaml:"reasoning_effort_models" json:"reasoning_effort_models"`
	StreamIncludeUsage    bool     `yaml:"stream_include_usage" json:"stream_include_usage"`
}
type RateHeaderSpec struct {
	Header    string `yaml:"header" json:"header"`
	Dimension string `yaml:"dimension,omitempty" json:"dimension,omitempty"`
	Format    string `yaml:"format,omitempty" json:"format,omitempty"`
}
type PathsSpec struct {
	Chat   string `yaml:"chat" json:"chat"`
	Models string `yaml:"models" json:"models"`
}
type ProbeSpec struct {
	Kind string `yaml:"kind" json:"kind"`
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
}

// Spec é a configuração declarativa de um provedor.
type Spec struct {
	Name          string                    `yaml:"name" json:"name"`
	Type          string                    `yaml:"type" json:"type"`
	BaseURL       string                    `yaml:"base_url" json:"base_url"`
	Auth          AuthSpec                  `yaml:"auth" json:"auth"`
	Enabled       bool                      `yaml:"enabled" json:"enabled"`
	Headers       map[string]string         `yaml:"headers" json:"headers"`
	RequestExtra  map[string]any            `yaml:"request_extra" json:"request_extra"`
	Timeout       string                    `yaml:"timeout" json:"timeout"`
	Capabilities  CapabilitySpec            `yaml:"capabilities" json:"capabilities"`
	RateHeaders   map[string]RateHeaderSpec `yaml:"rate_headers" json:"rate_headers"`
	Paths         PathsSpec                 `yaml:"paths" json:"paths"`
	Probe         ProbeSpec                 `yaml:"probe" json:"probe"`
	LimitScope    string                    `yaml:"limit_scope" json:"limit_scope"`
	KeyStrategy   string                    `yaml:"key_strategy" json:"key_strategy"`
	QuotaCooldown string                    `yaml:"quota_cooldown" json:"quota_cooldown"`
}

// Validate valida campos essenciais sem fazer qualquer conexão externa.
func (s *Spec) Validate() error {
	if s == nil {
		return fmt.Errorf("provider: especificação ausente")
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("provider: name é obrigatório")
	}
	if s.Type != "openai_compatible" && s.Type != "generic_http" {
		return fmt.Errorf("provider %q: type não suportado: %s", s.Name, s.Type)
	}
	u, e := url.Parse(s.BaseURL)
	if e != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("provider %q: base_url inválida", s.Name)
	}
	if s.Auth.Scheme == "" {
		s.Auth.Scheme = "bearer"
	}
	if s.Auth.Scheme != "bearer" && s.Auth.Scheme != "header" && s.Auth.Scheme != "query" {
		return fmt.Errorf("provider %q: auth.scheme inválido", s.Name)
	}
	if s.Auth.Scheme == "header" && s.Auth.Header == "" {
		return fmt.Errorf("provider %q: auth.header obrigatório", s.Name)
	}
	if s.Auth.Scheme == "query" && s.Auth.Query == "" {
		return fmt.Errorf("provider %q: auth.query obrigatório", s.Name)
	}
	if s.Paths.Chat == "" {
		s.Paths.Chat = "/chat/completions"
	}
	if s.Paths.Models == "" {
		s.Paths.Models = "/models"
	}
	// An omitted probe kind means the standard zero-token L1 /models probe.
	// Only an explicit "none" disables credential validation.
	if s.Probe.Kind == "" {
		if s.Type == "generic_http" {
			s.Probe.Kind = "none"
		} else {
			s.Probe.Kind = "models"
		}
	}
	s.Probe.Kind = strings.ToLower(strings.TrimSpace(s.Probe.Kind))
	switch s.Probe.Kind {
	case "models", "key", "completion", "none":
	default:
		return fmt.Errorf("provider %q: probe.kind inválido: %s", s.Name, s.Probe.Kind)
	}
	if s.Probe.Kind == "key" && strings.TrimSpace(s.Probe.Path) == "" {
		s.Probe.Path = "/key"
	}
	if s.Timeout != "" {
		if _, e := time.ParseDuration(s.Timeout); e != nil {
			return fmt.Errorf("provider %q: timeout inválido: %w", s.Name, e)
		}
	}
	return nil
}

// TimeoutDuration converte timeout em duração; 60s é o padrão.
func (s *Spec) TimeoutDuration() time.Duration {
	if s.Timeout == "" {
		return 60 * time.Second
	}
	d, e := time.ParseDuration(s.Timeout)
	if e != nil {
		return 60 * time.Second
	}
	return d
}

// Canonical mantém o corpo OpenAI sem reserializar mensagens desconhecidas.
type Canonical struct {
	Payload map[string]json.RawMessage
	Stream  bool
}
type ProbeResult struct {
	OK          bool             `json:"ok"`
	Message     string           `json:"message"`
	Models      []string         `json:"models,omitempty"`
	HTTPStatus  int              `json:"http_status,omitempty"`
	Outcome     classify.Outcome `json:"outcome,omitempty"`
	RetryAfter  time.Duration    `json:"retry_after,omitempty"`
	RetryKnown  bool             `json:"retry_known,omitempty"`
	Hints       *sched.Hints     `json:"hints,omitempty"`
	AccountInfo map[string]any   `json:"account_info,omitempty"`
	Headers     http.Header      `json:"-"`
}

// NewAuthenticatedGET monta uma chamada GET autenticada para uma sonda L0.
// A função centraliza a mesma política de autenticação usada pelos adaptadores.
func NewAuthenticatedGET(ctx context.Context, s *Spec, key secret.Secret, path string) (*http.Request, error) {
	if s == nil {
		return nil, fmt.Errorf("provider: especificação ausente")
	}
	u, err := url.Parse(strings.TrimRight(s.BaseURL, "/") + "/" + strings.TrimLeft(path, "/"))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	for name, value := range s.Headers {
		req.Header.Set(name, value)
	}
	switch s.Auth.Scheme {
	case "", "bearer":
		req.Header.Set("Authorization", "Bearer "+key.Reveal())
	case "header":
		req.Header.Set(s.Auth.Header, key.Reveal())
	case "query":
		q := req.URL.Query()
		q.Set(s.Auth.Query, key.Reveal())
		req.URL.RawQuery = q.Encode()
	}
	return req, nil
}

// Adapter implementa transporte, classificação e capacidades de um provedor.
type Adapter interface {
	Type() string
	BuildRequest(context.Context, *Spec, secret.Secret, *Canonical) (*http.Request, error)
	Classify(*http.Response, []byte, error) classify.Result
	Hints(http.Header, time.Time) *sched.Hints
	Probe(context.Context, *http.Client, *Spec, secret.Secret) ProbeResult
	ListModels(context.Context, *http.Client, *Spec, secret.Secret) ([]string, error)
	AdaptPayload(*Spec, string, map[string]json.RawMessage) map[string]json.RawMessage
}
type Factory func() Adapter

var registry = struct {
	sync.RWMutex
	m map[string]Factory
}{m: map[string]Factory{}}

// Register registra um adaptador. Use em init dos pacotes de adaptador.
func Register(kind string, f Factory) { registry.Lock(); defer registry.Unlock(); registry.m[kind] = f }

// NewAdapter cria o adaptador registrado para type.
func NewAdapter(kind string) (Adapter, error) {
	registry.RLock()
	f := registry.m[kind]
	registry.RUnlock()
	if f == nil {
		return nil, fmt.Errorf("provider: adaptador %q não registrado", kind)
	}
	return f(), nil
}

// IsDynamic reports aliases whose concrete model may be resolved by the provider
// instead of being present in the per-key static catalog.
func IsDynamic(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	return lower == "openrouter/free" || strings.HasSuffix(lower, ":free") || strings.HasSuffix(lower, "/*")
}
