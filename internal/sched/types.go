// Package sched agenda chaves e reservas sem I/O no caminho quente.
package sched

import (
	"errors"
	"fmt"
	"time"

	"keypool/internal/clock"
	"keypool/internal/limits"
	"keypool/internal/secret"
)

type KeyID string

// KeySpec descreve uma credencial e seus metadados não secretos.
type KeySpec struct {
	ID     KeyID         `json:"id"`
	Label  string        `json:"label"`
	Secret secret.Secret `json:"secret"`
	Group  string        `json:"group,omitempty"`
	Tags   []string      `json:"tags,omitempty"`
}
type Scope int

const (
	ScopeKey Scope = iota
	ScopeGroup
	ScopeProvider
	ScopeAuto
)

type Strategy int

const (
	RoundRobin Strategy = iota
	LeastUsed
	Headroom
	Latency
)

// Event é um evento operacional sem conteúdo de credenciais.
type Event struct {
	Kind    string    `json:"kind"`
	Subject string    `json:"subject,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	At      time.Time `json:"at"`
}

// PoolConfig define políticas e dependências do pool.
type PoolConfig struct {
	Provider                                            string
	Limits                                              limits.Limits
	Scope                                               Scope
	Strategy                                            Strategy
	QuotaCooldown, MergeWindow, MergeMaxRetry, LeaseTTL time.Duration
	CatalogTTL                                          time.Duration
	Clock                                               clock.Clock
	OnEvent                                             func(Event)
}
type AcquireRequest struct {
	Model    string
	Tokens   int
	Priority int
	Reserve  float64
	Allow    func(KeyID) bool
	// Warm is reserved for the isolated housekeeping service. It may acquire
	// a key while holding that key's probe reservation; real requests may not.
	Warm bool
}

var (
	ErrNoKey        = errors.New("sched: no keys configured")
	ErrAllDisabled  = errors.New("sched: all keys disabled")
	ErrExceedsLimit = errors.New("sched: request exceeds the model's per-minute token limit")
)

// ErrNoCapacity informa quando a próxima capacidade estará disponível.
type ErrNoCapacity struct{ RetryIn time.Duration }

func (e *ErrNoCapacity) Error() string {
	if e.RetryIn < 0 {
		return "sched: capacidade indisponível por tempo indeterminado"
	}
	return fmt.Sprintf("sched: capacidade indisponível por %s", e.RetryIn)
}

// ErrOutranked informa que uma chamada mais urgente tem precedência.
type ErrOutranked struct{ RetryIn time.Duration }

func (e *ErrOutranked) Error() string {
	return "sched: chamada cedeu a vez para uma prioridade mais alta"
}

// Hint representa uma observação real de limite do provedor.
type Hint struct {
	Remaining    int       `json:"remaining"`
	HasRemaining bool      `json:"has_remaining"`
	ResetAt      time.Time `json:"reset_at"`
	ObservedAt   time.Time `json:"observed_at"`
}

// Hints agrupa observações por dimensão (tpm, rpm, rpd, tpd).
type Hints map[string]Hint

// KeyStats é o estado serializável de uma chave, sem segredo.
type KeyStats struct {
	ID                KeyID                     `json:"id"`
	Label             string                    `json:"label"`
	Disabled          bool                      `json:"disabled"`
	Health            string                    `json:"health"`
	CooldownRemaining time.Duration             `json:"cooldown_remaining"`
	QuotaBlocked      bool                      `json:"quota_blocked"`
	UsedTotal         int                       `json:"used_total"`
	Errors            int                       `json:"errors"`
	Inflight          int                       `json:"inflight"`
	UsedPerBucket     map[string]int            `json:"used_per_bucket"`
	WindowUsed        map[string]map[string]int `json:"window_used,omitempty"`
	LastActivity      time.Time                 `json:"last_activity,omitempty"`
	LastOK            time.Time                 `json:"last_ok_at,omitempty"`
	CatalogKnown      bool                      `json:"catalog_known"`
	CatalogSeenAt     time.Time                 `json:"catalog_seen_at,omitempty"`
	CatalogModels     int                       `json:"catalog_models"`
	CredentialVersion uint64                    `json:"credential_version"`
	Validated         bool                      `json:"validated"`
	TTFBEWMA          map[string]time.Duration  `json:"ttfb_ewma,omitempty"`
	TotalEWMA         map[string]time.Duration  `json:"total_ewma,omitempty"`
}

type dimensionSnapshot struct {
	Entries []wallEntry `json:"entries"`
}
type wallEntry struct {
	AtMS int64 `json:"at_ms"`
	N    int   `json:"n"`
}
type bucketSnapshot struct {
	TPM, TPD, RPM, RPD dimensionSnapshot
	Inflight           int
}
type keySnapshot struct {
	ID                KeyID
	Disabled          bool
	Health            string
	Cooldowns         map[string]int64
	LongCooldowns     map[string]int64
	ForbiddenModels   map[string]int64
	AllCooldownMS     int64
	QuotaUntilMS      int64
	QuotaBuckets      map[string]int64
	UsedTotal, Errors int
	OpenUntilMS       int64
	LastActivityMS    int64
	LastOKMS          int64
	CatalogAtMS       int64
	OpenCount         int
	CredentialVersion uint64
	Validated         bool
	CatalogKnown      bool
	Catalog           []string
	TTFBEWMA          map[string]int64
	TotalEWMA         map[string]int64
}

// PoolSnapshot guarda o estado volátil necessário à restauração.
type PoolSnapshot struct {
	SavedAtMS int64                                `json:"saved_at_ms"`
	Merged    bool                                 `json:"merged"`
	Keys      []keySnapshot                        `json:"keys"`
	Groups    map[string]map[string]bucketSnapshot `json:"groups"`
}

var _ = limits.Resolved{}
