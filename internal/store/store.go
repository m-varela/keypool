// Package store define a persistência consumida pelo serviço e sua implementação em memória.
package store

import (
	"context"
	"encoding/json"
	"keypool/internal/config"
	"keypool/internal/domain"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"time"
)

// KeyRecord associa uma chave decifrada ao provedor; o segredo permanece encapsulado.
type KeyRecord struct {
	Provider     string
	ID           sched.KeyID
	Label, Group string
	Tags         []string
	Secret       secret.Secret
	Enabled      bool
}

// Store é a interface de persistência do KeyPool.
type AdminEvent struct {
	At      time.Time       `json:"at"`
	Level   string          `json:"level"`
	Kind    string          `json:"kind"`
	Subject string          `json:"subject,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// AdminReader expõe consultas de leitura usadas pelo painel administrativo.
type AdminReader interface {
	ReadUsage(context.Context, time.Time, time.Time, string) ([]domain.UsageEvent, error)
	ReadEvents(context.Context, string, time.Time) ([]AdminEvent, error)
}

type Store interface {
	LoadConfig(context.Context) (*config.Raw, error)
	SaveConfig(context.Context, *config.Raw) error
	LoadKeys(context.Context) ([]KeyRecord, error)
	SaveKeys(context.Context, string, []KeyRecord) error
	DeleteKey(context.Context, sched.KeyID) error
	RotateAPIKey(context.Context, string) error
	LoadPoolState(context.Context, string) (sched.PoolSnapshot, error)
	SavePoolState(context.Context, string, sched.PoolSnapshot) error
	AppendUsage(context.Context, []domain.UsageEvent) error
	AppendEvent(context.Context, sched.Event) error
}

// HealthCheck is a persisted result from an L0/L1/L2 warm probe.
type HealthCheck struct {
	Provider   string
	KeyID      sched.KeyID
	Probe      string
	OK         bool
	HTTPStatus int
	LatencyMS  int
	Error      string
	At         time.Time
}

// ModelCatalogEntry is the latest per-key model catalog snapshot.
type ModelCatalogEntry struct {
	Provider string
	KeyID    sched.KeyID
	Models   []string
	SeenAt   time.Time
}

// WarmStore is an optional extension implemented by persistent stores.
type WarmStore interface {
	SaveHealthCheck(context.Context, HealthCheck) error
	SaveModelCatalog(context.Context, string, sched.KeyID, []string, time.Time) error
	DeleteModelCatalog(context.Context, string, sched.KeyID) error
	LoadModelCatalog(context.Context) ([]ModelCatalogEntry, error)
	CountWarmL2Tokens(context.Context, string, sched.KeyID, time.Time, time.Time) (int, error)
}

// ProjectDailyUsageReader carrega o orçamento de tokens já consumido no dia UTC.
// É consultado somente na inicialização, nunca no caminho quente.
type ProjectDailyUsageReader interface {
	ReadProjectDailyTokens(context.Context, time.Time) (map[string]int, error)
}
