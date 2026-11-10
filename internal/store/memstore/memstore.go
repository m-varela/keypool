// Package memstore implementa Store em memória para modo dev e testes.
package memstore

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"keypool/internal/config"
	"keypool/internal/domain"
	"keypool/internal/sched"
	"keypool/internal/store"
)

// Store é uma implementação em memória, protegida por mutex.
type Store struct {
	mu      sync.RWMutex
	raw     *config.Raw
	keys    []store.KeyRecord
	pools   map[string]sched.PoolSnapshot
	usage   []domain.UsageEvent
	events  []sched.Event
	health  []store.HealthCheck
	catalog map[string]store.ModelCatalogEntry
}

// New cria um memstore a partir da configuração já carregada.
func New(raw *config.Raw) *Store {
	cloned, err := cloneRaw(raw)
	if err != nil {
		cloned = raw
	}
	return &Store{raw: cloned, keys: keyRecords(raw), pools: map[string]sched.PoolSnapshot{}, catalog: map[string]store.ModelCatalogEntry{}}
}

func keyRecords(raw *config.Raw) []store.KeyRecord {
	if raw == nil {
		return nil
	}
	var out []store.KeyRecord
	for name, p := range raw.Providers {
		for _, k := range p.Keys {
			out = append(out, store.KeyRecord{Provider: name, ID: k.ID, Label: k.Label, Group: k.Group, Tags: append([]string(nil), k.Tags...), Secret: k.Secret, Enabled: true})
		}
	}
	return out
}

func cloneRaw(raw *config.Raw) (*config.Raw, error) {
	if raw == nil {
		return nil, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var out config.Raw
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	out.APIKey = raw.APIKey
	out.ProjectFile = raw.ProjectFile
	for name, provider := range raw.Providers {
		provider.Keys = append([]sched.KeySpec(nil), provider.Keys...)
		for i := range provider.Keys {
			provider.Keys[i].Tags = append([]string(nil), provider.Keys[i].Tags...)
		}
		out.Providers[name] = provider
	}
	return &out, nil
}

func (s *Store) LoadConfig(ctx context.Context) (*config.Raw, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRaw(s.raw)
}
func (s *Store) SaveConfig(ctx context.Context, r *config.Raw) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	keys := keyRecords(r)
	cloned, err := cloneRaw(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	// SaveConfig reconcilia credenciais habilitadas sem apagar as desabilitadas.
	// O banco PostgreSQL preserva essas linhas com enabled=false; o memstore mantém
	// a mesma semântica para que apply não faça uma exclusão implícita no modo dev.
	for _, old := range s.keys {
		if old.Enabled {
			continue
		}
		if _, providerExists := r.Providers[old.Provider]; providerExists {
			keys = append(keys, old)
		}
	}
	s.raw = cloned
	s.keys = keys
	s.mu.Unlock()
	return nil
}
func (s *Store) LoadKeys(ctx context.Context) ([]store.KeyRecord, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]store.KeyRecord(nil), s.keys...)
	for i := range out {
		out[i].Tags = append([]string(nil), out[i].Tags...)
	}
	return out, nil
}
func (s *Store) LoadPoolState(ctx context.Context, k string) (sched.PoolSnapshot, error) {
	if e := ctx.Err(); e != nil {
		return sched.PoolSnapshot{}, e
	}
	s.mu.RLock()
	state := s.pools[k]
	s.mu.RUnlock()
	return clonePoolSnapshot(state)
}
func (s *Store) SavePoolState(ctx context.Context, k string, p sched.PoolSnapshot) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	state, err := clonePoolSnapshot(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.pools[k] = state
	s.mu.Unlock()
	return nil
}
func clonePoolSnapshot(in sched.PoolSnapshot) (sched.PoolSnapshot, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return sched.PoolSnapshot{}, err
	}
	var out sched.PoolSnapshot
	if err = json.Unmarshal(b, &out); err != nil {
		return sched.PoolSnapshot{}, err
	}
	return out, nil
}
func (s *Store) AppendUsage(ctx context.Context, v []domain.UsageEvent) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	copyEvents := append([]domain.UsageEvent(nil), v...)
	for i := range copyEvents {
		if v[i].Usage != nil {
			u := *v[i].Usage
			copyEvents[i].Usage = &u
		}
	}
	s.mu.Lock()
	s.usage = append(s.usage, copyEvents...)
	s.mu.Unlock()
	return nil
}
func (s *Store) AppendEvent(ctx context.Context, v sched.Event) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	s.mu.Lock()
	s.events = append(s.events, v)
	s.mu.Unlock()
	return nil
}
func (s *Store) Ping(ctx context.Context) error { return ctx.Err() }

var _ store.Store = (*Store)(nil)

// SaveKeys substitui os metadados das chaves de um provedor sem expor segredos.
func (s *Store) SaveKeys(ctx context.Context, provider string, keys []store.KeyRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.keys[:0]
	for _, key := range s.keys {
		if key.Provider != provider {
			out = append(out, key)
		}
	}
	for _, key := range keys {
		key.Provider = provider
		key.Tags = append([]string(nil), key.Tags...)
		out = append(out, key)
	}
	s.keys = out
	return nil
}

// DeleteKey remove uma chave pelo identificador.
func (s *Store) DeleteKey(ctx context.Context, id sched.KeyID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.keys[:0]
	found := false
	for _, key := range s.keys {
		if key.ID == id {
			found = true
			continue
		}
		out = append(out, key)
	}
	if !found {
		return errors.New("memstore: chave não encontrada")
	}
	s.keys = out
	return nil
}

// RotateAPIKey atualiza a chave de autenticação no modo dev.
func (s *Store) RotateAPIKey(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.raw == nil {
		return errors.New("memstore: configuração ausente")
	}
	s.raw.APIKey = token
	return nil
}

// ReadUsage retorna registros em memória para o modo dev.
func (s *Store) ReadUsage(ctx context.Context, from, to time.Time, project string) ([]domain.UsageEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.UsageEvent, 0)
	for _, e := range s.usage {
		if !from.IsZero() && e.At.Before(from) {
			continue
		}
		if !to.IsZero() && e.At.After(to) {
			continue
		}
		if project != "" && e.Project != project {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// ReadEvents retorna eventos em memória para o modo dev.
func (s *Store) ReadEvents(ctx context.Context, kind string, since time.Time) ([]store.AdminEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.AdminEvent, 0)
	for _, e := range s.events {
		if kind != "" && e.Kind != kind {
			continue
		}
		if !since.IsZero() && e.At.Before(since) {
			continue
		}
		detail, _ := json.Marshal(map[string]string{"detail": e.Detail})
		out = append(out, store.AdminEvent{At: e.At, Level: "info", Kind: e.Kind, Subject: e.Subject, Detail: detail})
	}
	return out, nil
}

func (s *Store) SaveHealthCheck(ctx context.Context, item store.HealthCheck) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.health = append(s.health, item)
	s.mu.Unlock()
	return nil
}
func (s *Store) SaveModelCatalog(ctx context.Context, provider string, id sched.KeyID, models []string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	copyModels := append([]string(nil), models...)
	s.mu.Lock()
	if s.catalog == nil {
		s.catalog = map[string]store.ModelCatalogEntry{}
	}
	s.catalog[provider+"/"+string(id)] = store.ModelCatalogEntry{Provider: provider, KeyID: id, Models: copyModels, SeenAt: at}
	s.mu.Unlock()
	return nil
}
func (s *Store) DeleteModelCatalog(ctx context.Context, provider string, id sched.KeyID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.catalog, provider+"/"+string(id))
	s.mu.Unlock()
	return nil
}

func (s *Store) LoadModelCatalog(ctx context.Context) ([]store.ModelCatalogEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.ModelCatalogEntry, 0, len(s.catalog))
	for _, entry := range s.catalog {
		entry.Models = append([]string(nil), entry.Models...)
		out = append(out, entry)
	}
	return out, nil
}

var _ store.WarmStore = (*Store)(nil)

// CountWarmL2Tokens returns the exact per-provider daily warmup token budget.
func (s *Store) CountWarmL2Tokens(ctx context.Context, provider string, id sched.KeyID, from, to time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, event := range s.usage {
		if event.Source == "warmup" && event.Alias == "warmup-L2" && event.Provider == provider && event.KeyID == string(id) && !event.At.Before(from) && event.At.Before(to) {
			total += event.EstimatedTokens
		}
	}
	return total, nil
}

// ReadProjectDailyTokens soma tokens reais (ou a estimativa quando ausente) por projeto.
func (s *Store) ReadProjectDailyTokens(ctx context.Context, from time.Time) (map[string]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, event := range s.usage {
		if event.At.Before(from) {
			continue
		}
		source := event.Source
		if source == "" || (source != "warmup" && source != "lease" && source != "passthrough" && source != "request") {
			source = "request" // matches pgstore's default/normalization
		}
		if source == "warmup" {
			continue
		}
		n := event.EstimatedTokens
		if event.Usage != nil {
			if event.Usage.TotalTokens > 0 {
				n = event.Usage.TotalTokens
			} else if event.Usage.PromptTokens+event.Usage.CompletionTokens > 0 {
				n = event.Usage.PromptTokens + event.Usage.CompletionTokens
			}
		}
		if event.ActualTokens > 0 {
			n = event.ActualTokens // explicit total is authoritative
		}
		if n > 0 {
			out[event.Project] += n
		}
	}
	return out, nil
}

var _ store.ProjectDailyUsageReader = (*Store)(nil)
