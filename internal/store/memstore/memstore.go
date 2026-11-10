// Package memstore implementa o store único em memória, com checkpoint local opcional.
package memstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/domain"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/store"
)

// Store mantém configuração e credenciais sob mu; histórico e contadores usam usageMu.
// commitMu serializa apenas mutações duráveis e o checkpoint, nunca o caminho quente.
type Store struct {
	mu       sync.RWMutex
	usageMu  sync.Mutex
	commitMu sync.Mutex
	dir      string
	box      interface {
		Encrypt(string, string, []byte) ([]byte, error)
		Decrypt(string, string, []byte) ([]byte, error)
		MasterKeyCheck() ([]byte, error)
		VerifyMasterKey([]byte) error
	}
	clock          clock.Clock
	raw            *config.Raw
	keys           []store.KeyRecord
	pools          map[string]sched.PoolSnapshot
	apiKey         apiKeyCheckpoint
	apiKeyHash     [32]byte
	masterKeyCheck string
	initialized    bool
	closed         bool

	usage        ring[domain.UsageEvent]
	events       ring[sched.Event]
	health       ring[store.HealthCheck]
	catalog      map[string]store.ModelCatalogEntry
	daily        dailyState
	dailyDirty   bool
	dailyVersion uint64
	usageEvicted uint64
}

// New cria um store sem disco, usado apenas por testes.
func New(raw *config.Raw) *Store {
	s := newStore("", nil, clock.Real())
	cloned, err := cloneRaw(raw)
	if err != nil {
		cloned = raw
	}
	s.raw = cloned
	s.keys = keyRecords(raw)
	s.initialized = true
	if raw != nil && raw.APIKey != "" {
		s.apiKeyHash = hashAPIKey(raw.APIKey)
		s.apiKey = apiKeyState(raw.APIKey, s.clock.Now(), apiKeyCheckpoint{})
	}
	if s.apiKey.Hash == "" {
		s.apiKey.Hash = "0000000000000000000000000000000000000000000000000000000000000000"
	}
	return s
}

func newStore(dir string, box interface {
	Encrypt(string, string, []byte) ([]byte, error)
	Decrypt(string, string, []byte) ([]byte, error)
	MasterKeyCheck() ([]byte, error)
	VerifyMasterKey([]byte) error
}, clk clock.Clock) *Store {
	if clk == nil {
		clk = clock.Real()
	}
	now := clk.Now()
	return &Store{
		dir: dir, box: box, clock: clk,
		pools: map[string]sched.PoolSnapshot{}, catalog: map[string]store.ModelCatalogEntry{},
		usage:  newRing(usageCapacity, func(v domain.UsageEvent) time.Time { return v.At }),
		events: newRing(eventCapacity, func(v sched.Event) time.Time { return v.At }),
		health: newRing(healthCapacity, func(v store.HealthCheck) time.Time { return v.At }),
		daily:  newDaily(now),
	}
}

func keyRecords(raw *config.Raw) []store.KeyRecord {
	if raw == nil {
		return nil
	}
	var out []store.KeyRecord
	for name, provider := range raw.Providers {
		for _, key := range provider.Keys {
			out = append(out, store.KeyRecord{
				Provider: name, ID: key.ID, Label: key.Label, Group: key.Group,
				Tags: append([]string(nil), key.Tags...), Secret: key.Secret, Enabled: true,
			})
		}
	}
	return out
}

func cloneRaw(raw *config.Raw) (*config.Raw, error) {
	if raw == nil {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var out config.Raw
	if err = json.Unmarshal(data, &out); err != nil {
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

func clonePoolSnapshot(in sched.PoolSnapshot) (sched.PoolSnapshot, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return sched.PoolSnapshot{}, err
	}
	var out sched.PoolSnapshot
	if err = json.Unmarshal(data, &out); err != nil {
		return sched.PoolSnapshot{}, err
	}
	return out, nil
}

func cloneUsageEvent(in domain.UsageEvent) domain.UsageEvent {
	out := in
	if in.Usage != nil {
		usage := *in.Usage
		out.Usage = &usage
	}
	return out
}

func (s *Store) commit(ctx context.Context, allowUninitialized bool, mutate func(*checkpoint) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return errClosed
	}
	saved, dailyVersion, err := s.snapshotCheckpoint()
	if errors.Is(err, errNotInitialized) && allowUninitialized {
		saved = &checkpoint{Version: checkpointVersion, Pools: map[string]sched.PoolSnapshot{}}
		s.usageMu.Lock()
		saved.Daily = cloneDaily(s.daily)
		dailyVersion = s.dailyVersion
		s.usageMu.Unlock()
	} else if err != nil {
		return err
	}
	if err = mutate(saved); err != nil {
		return err
	}
	if saved.Version != checkpointVersion {
		return fmt.Errorf("memstore: versão interna de checkpoint inválida: %d", saved.Version)
	}
	if saved.Pools == nil {
		saved.Pools = map[string]sched.PoolSnapshot{}
	}
	if s.dir != "" {
		if s.box == nil {
			return errors.New("memstore: cryptobox ausente para checkpoint persistente")
		}
		if err = s.writeCheckpoint(ctx, saved); err != nil {
			return err
		}
	}
	if err = s.applyCheckpoint(saved); err != nil {
		return err
	}
	s.usageMu.Lock()
	if s.dailyVersion == dailyVersion {
		s.dailyDirty = false
	}
	s.usageMu.Unlock()
	return nil
}

func (s *Store) applyCheckpoint(saved *checkpoint) error {
	raw, err := cloneRaw(saved.Config)
	if err != nil {
		return err
	}
	keys := make([]store.KeyRecord, 0, len(saved.Keys))
	for _, item := range saved.Keys {
		if item.secret == "" {
			return fmt.Errorf("memstore: segredo ausente no commit da chave %s/%s", item.Provider, item.ID)
		}
		keys = append(keys, store.KeyRecord{
			Provider: item.Provider, ID: item.ID, Label: item.Label, Group: item.Group,
			Tags: append([]string(nil), item.Tags...), Secret: secret.New(item.secret), Enabled: item.Enabled,
		})
	}
	pools := make(map[string]sched.PoolSnapshot, len(saved.Pools))
	for name, state := range saved.Pools {
		cloned, cloneErr := clonePoolSnapshot(state)
		if cloneErr != nil {
			return cloneErr
		}
		pools[name] = cloned
	}
	var hash [32]byte
	if saved.APIKey.Hash != "" {
		hash, err = hexDecodeHash(saved.APIKey.Hash)
		if err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.raw = raw
	s.keys = keys
	s.pools = pools
	s.apiKey = saved.APIKey
	s.apiKeyHash = hash
	s.masterKeyCheck = saved.MasterKeyCheck
	s.initialized = true
	s.mu.Unlock()
	return nil
}

func hexDecodeHash(value string) ([32]byte, error) {
	var out [32]byte
	if len(value) != 64 {
		return out, errors.New("memstore: hash da API key inválido")
	}
	for i := 0; i < len(value); i += 2 {
		var decoded byte
		for _, c := range []byte(value[i : i+2]) {
			decoded <<= 4
			switch {
			case c >= '0' && c <= '9':
				decoded |= c - '0'
			case c >= 'a' && c <= 'f':
				decoded |= c - 'a' + 10
			case c >= 'A' && c <= 'F':
				decoded |= c - 'A' + 10
			default:
				return out, errors.New("memstore: hash da API key inválido")
			}
		}
		out[i/2] = decoded
	}
	return out, nil
}

// Initialized informa se o primeiro bootstrap já foi concluído.
func (s *Store) Initialized() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.initialized
}

// Initialize grava a configuração inicial e o hash da API key após o bind do daemon.
func (s *Store) Initialize(ctx context.Context, apiKey string, raw *config.Raw) error {
	if apiKey == "" || raw == nil {
		return errors.New("memstore: API key e configuração são obrigatórias")
	}
	cloned, err := cloneRaw(raw)
	if err != nil {
		return err
	}
	if s.box == nil || s.dir == "" {
		return errors.New("memstore: Initialize exige Open com diretório e cryptobox")
	}
	check, err := s.box.MasterKeyCheck()
	if err != nil {
		return err
	}
	return s.commit(ctx, true, func(saved *checkpoint) error {
		if s.Initialized() {
			return errAlreadyInitialized
		}
		saved.Config = cloned
		saved.Keys = make([]keyCheckpoint, 0)
		for _, key := range keyRecords(raw) {
			saved.Keys = append(saved.Keys, keyCheckpoint{
				Provider: key.Provider, ID: key.ID, Label: key.Label, Group: key.Group,
				Tags: append([]string(nil), key.Tags...), Enabled: true, secret: key.Secret.Reveal(),
			})
		}
		saved.APIKey = apiKeyState(apiKey, s.clock.Now(), apiKeyCheckpoint{})
		saved.MasterKeyCheck = base64String(check)
		return nil
	})
}

// VerifyAPIKey compara o token com o SHA-256 persistido em tempo constante.
func (s *Store) VerifyAPIKey(ctx context.Context, token string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.initialized {
		return false, errNotInitialized
	}
	return verifyAPIKey(s.apiKeyHash, token), nil
}

// APIKeyInfo devolve apenas o prefixo e os instantes de criação/rotação.
func (s *Store) APIKeyInfo(ctx context.Context) (string, time.Time, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.initialized {
		return "", time.Time{}, time.Time{}, errNotInitialized
	}
	return s.apiKey.Prefix, s.apiKey.CreatedAt, s.apiKey.RotatedAt, nil
}

// UsageEvicted informa quantos registros de uso foram descartados pela retenção.
func (s *Store) UsageEvicted() uint64 {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return s.usageEvicted
}

// Close persiste contadores diários sujos e fecha o store; é idempotente.
func (s *Store) Close() error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil
	}
	initialized := s.initialized
	s.mu.RUnlock()
	var err error
	if initialized && s.dir != "" {
		s.usageMu.Lock()
		dirty := s.dailyDirty
		s.usageMu.Unlock()
		if dirty {
			saved, version, snapshotErr := s.snapshotCheckpoint()
			if snapshotErr != nil {
				err = snapshotErr
			} else if writeErr := s.writeCheckpoint(context.Background(), saved); writeErr != nil {
				err = writeErr
			} else {
				s.usageMu.Lock()
				if s.dailyVersion == version {
					s.dailyDirty = false
				}
				s.usageMu.Unlock()
			}
		}
	}
	if err == nil {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	}
	return err
}

// LoadConfig retorna uma cópia independente da configuração.
func (s *Store) LoadConfig(ctx context.Context) (*config.Raw, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	raw, err := cloneRaw(s.raw)
	keys := append([]store.KeyRecord(nil), s.keys...)
	s.mu.RUnlock()
	if err != nil || raw == nil {
		return raw, err
	}
	for name, provider := range raw.Providers {
		provider.Keys = nil
		for _, key := range keys {
			if key.Provider == name && key.Enabled {
				provider.Keys = append(provider.Keys, sched.KeySpec{
					ID: key.ID, Label: key.Label, Group: key.Group, Tags: append([]string(nil), key.Tags...), Secret: key.Secret,
				})
			}
		}
		raw.Providers[name] = provider
	}
	return raw, nil
}

// SaveConfig substitui a configuração preservando chaves desabilitadas de provedores existentes.
func (s *Store) SaveConfig(ctx context.Context, raw *config.Raw) error {
	cloned, err := cloneRaw(raw)
	if err != nil {
		return err
	}
	return s.commit(ctx, false, func(saved *checkpoint) error {
		if cloned == nil {
			return errors.New("memstore: configuração ausente")
		}
		keys := keyRecords(cloned)
		existing := map[string]bool{}
		for _, key := range keys {
			existing[key.Provider+"/"+string(key.ID)] = true
		}
		s.mu.RLock()
		oldKeys := append([]store.KeyRecord(nil), s.keys...)
		s.mu.RUnlock()
		for _, old := range oldKeys {
			if old.Enabled || existing[old.Provider+"/"+string(old.ID)] {
				continue
			}
			if _, providerExists := cloned.Providers[old.Provider]; providerExists {
				keys = append(keys, old)
			}
		}
		saved.Config = cloned
		saved.Keys = make([]keyCheckpoint, 0, len(keys))
		for _, key := range keys {
			saved.Keys = append(saved.Keys, keyCheckpoint{
				Provider: key.Provider, ID: key.ID, Label: key.Label, Group: key.Group,
				Tags: append([]string(nil), key.Tags...), Enabled: key.Enabled, secret: key.Secret.Reveal(),
			})
		}
		return nil
	})
}

// LoadKeys devolve cópias de metadados e tags; Secret permanece encapsulado.
func (s *Store) LoadKeys(ctx context.Context) ([]store.KeyRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]store.KeyRecord(nil), s.keys...)
	for i := range out {
		out[i].Tags = append([]string(nil), out[i].Tags...)
	}
	return out, nil
}

// SaveKeys substitui as chaves de um provedor em uma única mutação durável.
func (s *Store) SaveKeys(ctx context.Context, provider string, keys []store.KeyRecord) error {
	copyKeys := append([]store.KeyRecord(nil), keys...)
	return s.commit(ctx, false, func(saved *checkpoint) error {
		current := make([]keyCheckpoint, 0, len(saved.Keys)+len(copyKeys))
		for _, key := range saved.Keys {
			if key.Provider != provider {
				current = append(current, key)
			}
		}
		for _, key := range copyKeys {
			key.Provider = provider
			current = append(current, keyCheckpoint{
				Provider: provider, ID: key.ID, Label: key.Label, Group: key.Group,
				Tags: append([]string(nil), key.Tags...), Enabled: key.Enabled, secret: key.Secret.Reveal(),
			})
		}
		saved.Keys = current
		return nil
	})
}

// DeleteKey remove uma chave pelo identificador.
func (s *Store) DeleteKey(ctx context.Context, id sched.KeyID) error {
	return s.commit(ctx, false, func(saved *checkpoint) error {
		out := saved.Keys[:0]
		found := false
		for _, key := range saved.Keys {
			if key.ID == id {
				found = true
				continue
			}
			out = append(out, key)
		}
		if !found {
			return errors.New("memstore: chave não encontrada")
		}
		saved.Keys = out
		return nil
	})
}

// RotateAPIKey atualiza somente o hash persistido da API key.
func (s *Store) RotateAPIKey(ctx context.Context, token string) error {
	if token == "" {
		return errors.New("memstore: API key vazia")
	}
	return s.commit(ctx, false, func(saved *checkpoint) error {
		saved.APIKey = apiKeyState(token, s.clock.Now(), saved.APIKey)
		if saved.Config != nil {
			saved.Config.APIKey = token
		}
		return nil
	})
}

// LoadPoolState retorna um snapshot independente; estado ausente tem SavedAtMS zero.
func (s *Store) LoadPoolState(ctx context.Context, provider string) (sched.PoolSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return sched.PoolSnapshot{}, err
	}
	s.mu.RLock()
	state, exists := s.pools[provider]
	s.mu.RUnlock()
	if !exists {
		return sched.PoolSnapshot{SavedAtMS: 0}, nil
	}
	return clonePoolSnapshot(state)
}

// SavePoolState persiste o snapshot e os contadores diários correntes.
func (s *Store) SavePoolState(ctx context.Context, provider string, state sched.PoolSnapshot) error {
	cloned, err := clonePoolSnapshot(state)
	if err != nil {
		return err
	}
	return s.commit(ctx, false, func(saved *checkpoint) error {
		if saved.Pools == nil {
			saved.Pools = map[string]sched.PoolSnapshot{}
		}
		saved.Pools[provider] = cloned
		return nil
	})
}

// AppendUsage anexa eventos ao ring e atualiza os contadores sem I/O.
func (s *Store) AppendUsage(ctx context.Context, values []domain.UsageEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return errClosed
	}
	now := s.clock.Now().UTC()
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.daily.roll(now) {
		s.dailyDirty = true
		s.dailyVersion++
	}
	for _, value := range values {
		event := cloneUsageEvent(value)
		if event.At.IsZero() {
			event.At = now
		}
		s.usageEvicted += uint64(s.usage.push(event, now, usageRetention))
		if event.At.UTC().Format("2006-01-02") != s.daily.Day {
			continue
		}
		if event.Source == "warmup" && event.Alias == "warmup-L2" {
			s.daily.WarmL2Tokens[event.Provider+"/"+event.KeyID] += max(event.EstimatedTokens, 0)
		}
		if isProjectUsage(event.Source) {
			if total := totalEventTokens(event); total > 0 {
				s.daily.ProjectTokens[event.Project] += total
			}
		}
		s.dailyDirty = true
		s.dailyVersion++
	}
	return nil
}

// AppendEvent anexa um evento ao ring limitado em memória.
func (s *Store) AppendEvent(ctx context.Context, event sched.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return errClosed
	}
	now := s.clock.Now().UTC()
	if event.At.IsZero() {
		event.At = now
	}
	s.usageMu.Lock()
	s.events.push(event, now, eventRetention)
	s.usageMu.Unlock()
	return nil
}

// SaveHealthCheck mantém somente os últimos resultados em memória.
func (s *Store) SaveHealthCheck(ctx context.Context, item store.HealthCheck) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	if item.At.IsZero() {
		item.At = now
	}
	s.usageMu.Lock()
	s.health.push(item, now, healthRetention)
	s.usageMu.Unlock()
	return nil
}

func (s *Store) SaveModelCatalog(ctx context.Context, provider string, id sched.KeyID, models []string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.catalog == nil {
		s.catalog = map[string]store.ModelCatalogEntry{}
	}
	s.catalog[provider+"/"+string(id)] = store.ModelCatalogEntry{Provider: provider, KeyID: id, Models: append([]string(nil), models...), SeenAt: at}
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

// CountWarmL2Tokens usa o contador diário atual ou consulta o histórico volátil.
func (s *Store) CountWarmL2Tokens(ctx context.Context, provider string, id sched.KeyID, from, to time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	key := provider + "/" + string(id)
	s.usageMu.Lock()
	if from.UTC().Format("2006-01-02") == to.Add(-time.Nanosecond).UTC().Format("2006-01-02") && s.daily.Day == from.UTC().Format("2006-01-02") {
		value := s.daily.WarmL2Tokens[key]
		s.usageMu.Unlock()
		return value, nil
	}
	values := s.usage.values()
	s.usageMu.Unlock()
	total := 0
	for _, event := range values {
		if event.Source == "warmup" && event.Alias == "warmup-L2" && event.Provider == provider && event.KeyID == string(id) && !event.At.Before(from) && event.At.Before(to) {
			total += event.EstimatedTokens
		}
	}
	return total, nil
}

// ReadProjectDailyTokens retorna o orçamento persistente do dia UTC correspondente.
func (s *Store) ReadProjectDailyTokens(ctx context.Context, from time.Time) (map[string]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	out := map[string]int{}
	if from.UTC().Format("2006-01-02") != s.daily.Day {
		return out, nil
	}
	for project, tokens := range s.daily.ProjectTokens {
		out[project] = tokens
	}
	return out, nil
}

// ReadUsage retorna uma cópia filtrada do histórico volátil.
func (s *Store) ReadUsage(ctx context.Context, from, to time.Time, project string) ([]domain.UsageEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.usageMu.Lock()
	values := s.usage.values()
	s.usageMu.Unlock()
	out := make([]domain.UsageEvent, 0, len(values))
	for _, event := range values {
		if !from.IsZero() && event.At.Before(from) || !to.IsZero() && event.At.After(to) || project != "" && event.Project != project {
			continue
		}
		out = append(out, cloneUsageEvent(event))
	}
	return out, nil
}

// ReadEvents retorna uma cópia filtrada dos eventos voláteis.
func (s *Store) ReadEvents(ctx context.Context, kind string, since time.Time) ([]store.AdminEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.usageMu.Lock()
	values := s.events.values()
	s.usageMu.Unlock()
	out := make([]store.AdminEvent, 0, len(values))
	for _, event := range values {
		if kind != "" && event.Kind != kind || !since.IsZero() && event.At.Before(since) {
			continue
		}
		detail, _ := json.Marshal(map[string]string{"detail": event.Detail})
		out = append(out, store.AdminEvent{At: event.At, Level: "info", Kind: event.Kind, Subject: event.Subject, Detail: detail})
	}
	return out, nil
}

var _ store.Store = (*Store)(nil)
var _ store.WarmStore = (*Store)(nil)
var _ store.ProjectDailyUsageReader = (*Store)(nil)
var _ store.AdminReader = (*Store)(nil)
