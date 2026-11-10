package pgstore

import (
	"context"
	"fmt"
	"sort"
	"time"

	"keypool/internal/sched"
	"keypool/internal/store"
)

// SaveHealthCheck appends one bounded, secret-free health probe record.
func (s *Store) SaveHealthCheck(ctx context.Context, item store.HealthCheck) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("pgstore: pool ausente")
	}
	var status any
	if item.HTTPStatus != 0 {
		status = item.HTTPStatus
	}
	if item.At.IsZero() {
		item.At = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO health_checks(key_id,probe,ok,http_status,latency_ms,error,at)
VALUES($1::uuid,$2,$3,$4,$5,$6,$7)`, string(item.KeyID), item.Probe, item.OK, status, maxInt(0, item.LatencyMS), item.Error, item.At)
	if err != nil {
		return fmt.Errorf("pgstore: salvar health check: %w", err)
	}
	return nil
}

// SaveModelCatalog replaces the catalog for one key atomically.
func (s *Store) SaveModelCatalog(ctx context.Context, provider string, id sched.KeyID, models []string, at time.Time) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("pgstore: pool ausente")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var storedProvider string
	if err := s.pool.QueryRow(ctx, `SELECT p.name FROM keys k JOIN providers p ON p.id=k.provider_id WHERE k.id=$1::uuid`, string(id)).Scan(&storedProvider); err != nil {
		return fmt.Errorf("pgstore: validar provedor do catálogo: %w", err)
	}
	if storedProvider != provider {
		return fmt.Errorf("pgstore: chave %s pertence ao provedor %s, não %s", id, storedProvider, provider)
	}
	clean := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		if model != "" && !seen[model] {
			seen[model] = true
			clean = append(clean, model)
		}
	}
	// An empty string row is a sentinel that distinguishes a validated empty
	// catalog from a key whose catalog has never been fetched.
	if len(clean) == 0 {
		clean = append(clean, "")
	}
	sort.Strings(clean)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM model_catalog WHERE key_id=$1::uuid`, string(id)); err != nil {
		return fmt.Errorf("pgstore: limpar catálogo: %w", err)
	}
	for _, model := range clean {
		if _, err = tx.Exec(ctx, `INSERT INTO model_catalog(key_id,model_id,seen_at) VALUES($1::uuid,$2,$3) ON CONFLICT(key_id,model_id) DO UPDATE SET seen_at=EXCLUDED.seen_at`, string(id), model, at); err != nil {
			return fmt.Errorf("pgstore: inserir catálogo: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// DeleteModelCatalog invalidates models learned with an older credential.
func (s *Store) DeleteModelCatalog(ctx context.Context, provider string, id sched.KeyID) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("pgstore: pool ausente")
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM model_catalog mc USING keys k, providers p
WHERE mc.key_id=k.id AND k.provider_id=p.id AND k.id=$1::uuid AND p.name=$2`, string(id), provider)
	if err != nil {
		return fmt.Errorf("pgstore: invalidar catálogo: %w", err)
	}
	return nil
}

// LoadModelCatalog restores the most recent visible model list per key.
func (s *Store) LoadModelCatalog(ctx context.Context) ([]store.ModelCatalogEntry, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("pgstore: pool ausente")
	}
	rows, err := s.pool.Query(ctx, `SELECT p.name, mc.key_id::text, mc.model_id, mc.seen_at
FROM model_catalog mc JOIN keys k ON k.id=mc.key_id JOIN providers p ON p.id=k.provider_id
ORDER BY p.name, mc.key_id, mc.model_id`)
	if err != nil {
		return nil, fmt.Errorf("pgstore: carregar catálogo: %w", err)
	}
	defer rows.Close()
	byID := map[string]*store.ModelCatalogEntry{}
	for rows.Next() {
		var providerName, id, model string
		var seen time.Time
		if err := rows.Scan(&providerName, &id, &model, &seen); err != nil {
			return nil, err
		}
		keyID := sched.KeyID(id)
		mapKey := providerName + "/" + id
		entry := byID[mapKey]
		if entry == nil {
			entry = &store.ModelCatalogEntry{Provider: providerName, KeyID: keyID, SeenAt: seen}
			byID[mapKey] = entry
		}
		if seen.After(entry.SeenAt) {
			entry.SeenAt = seen
		}
		if model != "" {
			entry.Models = append(entry.Models, model)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]store.ModelCatalogEntry, 0, len(byID))
	for _, entry := range byID {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider == out[j].Provider {
			return out[i].KeyID < out[j].KeyID
		}
		return out[i].Provider < out[j].Provider
	})
	return out, nil
}

var _ store.WarmStore = (*Store)(nil)

// CountWarmL2Tokens aggregates the per-provider L2 budget without the admin
// usage endpoint's response limit.
func (s *Store) CountWarmL2Tokens(ctx context.Context, provider string, id sched.KeyID, from, to time.Time) (int, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("pgstore: pool ausente")
	}
	var total int
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(est_tokens),0)::integer
FROM usage_events WHERE provider=$1 AND key_id=$2::uuid AND source='warmup'
AND alias='warmup-L2' AND ts >= $3 AND ts < $4`, provider, string(id), from, to).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("pgstore: contar orçamento L2: %w", err)
	}
	return total, nil
}
