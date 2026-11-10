// Package pgstore implements the persistent PostgreSQL store used by keypoold.
package pgstore

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"keypool/internal/config"
	"keypool/internal/cryptobox"
	"keypool/internal/domain"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/store"
)

var (
	ErrNotInitialized = errors.New("pgstore: KeyPool ainda nao foi inicializado; execute keypoold init")
	ErrAlreadyInit    = errors.New("pgstore: banco ja inicializado")
	ErrNoConfig       = errors.New("pgstore: nenhuma configuracao persistida")
)

// Store is safe for concurrent use and owns the pool created by Open.
type Store struct {
	pool *pgxpool.Pool
	box  *cryptobox.Box
}

// Open creates and verifies a PostgreSQL connection pool.
func Open(ctx context.Context, dsn string, box *cryptobox.Box) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("pgstore: KEYPOOL_DATABASE_URL ausente")
	}
	if box == nil {
		return nil, errors.New("pgstore: cryptobox ausente")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: abrir pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: conectar PostgreSQL: %w", err)
	}
	return newStore(pool, box), nil
}

func newStore(pool *pgxpool.Pool, box *cryptobox.Box) *Store {
	return &Store{pool: pool, box: box}
}

// Close releases the PostgreSQL pool owned by this Store.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Ping verifies that the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("pgstore: pool ausente")
	}
	return s.pool.Ping(ctx)
}

// DatabaseClockOffset estimates PostgreSQL clock minus local UTC using the
// midpoint of the request interval, reducing the effect of network latency.
func (s *Store) DatabaseClockOffset(ctx context.Context) (time.Duration, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("pgstore: pool ausente")
	}
	before := time.Now().UTC()
	var databaseNow time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return 0, fmt.Errorf("pgstore: consultar relogio do banco: %w", err)
	}
	after := time.Now().UTC()
	midpoint := before.Add(after.Sub(before) / 2)
	return databaseNow.Sub(midpoint), nil
}

// Initialize creates the single settings row and stores only the API key hash.
// It never replaces an existing API key or master-key check.
func (s *Store) Initialize(ctx context.Context, apiKey string) error {
	if s == nil || s.pool == nil || s.box == nil {
		return errors.New("pgstore: store incompleto")
	}
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("pgstore: API key vazia")
	}
	check, err := s.box.MasterKeyCheck()
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(apiKey))
	prefix := apiKey
	if len(prefix) > 7 {
		prefix = prefix[:7]
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO app_settings(id,api_key_hash,api_key_prefix,master_key_check)
VALUES(true,$1,$2,$3) ON CONFLICT(id) DO NOTHING`, hash[:], prefix, check)
	if err != nil {
		return fmt.Errorf("pgstore: inicializar app_settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyInit
	}
	return nil
}

// VerifyMasterKey checks the persisted authenticated marker before any key is read.
func (s *Store) VerifyMasterKey(ctx context.Context) error {
	if s == nil || s.pool == nil || s.box == nil {
		return errors.New("pgstore: store incompleto")
	}
	var check []byte
	if err := s.pool.QueryRow(ctx, `SELECT master_key_check FROM app_settings WHERE id=true`).Scan(&check); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotInitialized
		}
		return fmt.Errorf("pgstore: ler master_key_check: %w", err)
	}
	return s.box.VerifyMasterKey(check)
}

// VerifyAPIKey performs a constant-time comparison of the stored SHA-256 hash.
func (s *Store) VerifyAPIKey(ctx context.Context, token string) (bool, error) {
	if s == nil || s.pool == nil {
		return false, errors.New("pgstore: pool ausente")
	}
	var want []byte
	if err := s.pool.QueryRow(ctx, `SELECT api_key_hash FROM app_settings WHERE id=true`).Scan(&want); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotInitialized
		}
		return false, err
	}
	got := sha256.Sum256([]byte(token))
	return token != "" && len(want) == len(got) && subtle.ConstantTimeCompare(got[:], want) == 1, nil
}

// RotateAPIKey replaces the stored hash; the plaintext is never persisted.
func (s *Store) RotateAPIKey(ctx context.Context, token string) error {
	if s == nil || s.pool == nil {
		return errors.New("pgstore: pool ausente")
	}
	if !strings.HasPrefix(token, "kp_") || len(token) < 20 {
		return errors.New("pgstore: formato de API key inválido")
	}
	hash := sha256.Sum256([]byte(token))
	prefix := token
	if len(prefix) > 7 {
		prefix = prefix[:7]
	}
	tag, err := s.pool.Exec(ctx, `UPDATE app_settings SET api_key_hash=$1,api_key_prefix=$2,api_key_rotated_at=now() WHERE id=true`, hash[:], prefix)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotInitialized
	}
	return nil
}

// APIKeyInfo returns only safe metadata, never the API key.
func (s *Store) APIKeyInfo(ctx context.Context) (prefix string, created, rotated time.Time, err error) {
	if s == nil || s.pool == nil {
		err = errors.New("pgstore: pool ausente")
		return
	}
	var rotatedAt sql.NullTime
	err = s.pool.QueryRow(ctx, `SELECT api_key_prefix,api_key_created_at,api_key_rotated_at
FROM app_settings WHERE id=true`).Scan(&prefix, &created, &rotatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotInitialized
	} else if err == nil && rotatedAt.Valid {
		rotated = rotatedAt.Time
	}
	return
}

// SaveConfig atomically replaces declarative providers, projects, rules and routes.
// Provider keys are deliberately persisted through SaveKeys, never in JSON config.
func (s *Store) SaveConfig(ctx context.Context, raw *config.Raw) error {
	if s == nil || s.pool == nil || s.box == nil {
		return errors.New("pgstore: store incompleto")
	}
	if raw == nil {
		return errors.New("pgstore: configuracao ausente")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	providerIDs := make(map[string]string, len(raw.Providers))
	providerKeyIDs := make(map[string]map[string]string, len(raw.Providers))
	providerNames := sortedProviderNames(raw.Providers)
	for _, name := range providerNames {
		pr := raw.Providers[name]
		stored := pr
		stored.Keys = nil
		stored.Limits = config.RawLimits{}
		spec, err := json.Marshal(stored)
		if err != nil {
			return fmt.Errorf("pgstore: codificar provedor %s: %w", name, err)
		}
		lim, err := json.Marshal(pr.Limits)
		if err != nil {
			return fmt.Errorf("pgstore: codificar limites %s: %w", name, err)
		}
		enabled := true
		if pr.Enabled != nil {
			enabled = *pr.Enabled
		}
		scope := pr.LimitScope
		if scope == "" {
			scope = "key"
		}
		strategy := pr.KeyStrategy
		if strategy == "" {
			strategy = "round_robin"
		}
		cooldown := durationSeconds(pr.QuotaCooldown, 3600)
		providerType := strings.TrimSpace(pr.Type)
		if providerType == "" && strings.TrimSpace(pr.Preset) != "" {
			resolved, e := provider.LoadPreset(pr.Preset + ".yaml")
			if e != nil {
				return fmt.Errorf("pgstore: resolver preset do provedor %s: %w", name, e)
			}
			providerType = resolved.Type
		}
		if providerType == "" {
			return fmt.Errorf("pgstore: provedor %s precisa de type ou preset", name)
		}
		_, err = tx.Exec(ctx, `INSERT INTO providers(name,type,enabled,spec,limit_scope,key_strategy,quota_cooldown_s,limits)
VALUES($1,$2,$3,$4::jsonb,$5,$6,$7,$8::jsonb)
ON CONFLICT(name) DO UPDATE SET type=EXCLUDED.type,enabled=EXCLUDED.enabled,spec=EXCLUDED.spec,
limit_scope=EXCLUDED.limit_scope,key_strategy=EXCLUDED.key_strategy,quota_cooldown_s=EXCLUDED.quota_cooldown_s,
limits=EXCLUDED.limits,updated_at=now()`, name, providerType, enabled, spec, scope, strategy, cooldown, lim)
		if err != nil {
			return fmt.Errorf("pgstore: salvar provedor %s: %w", name, err)
		}
		var id string
		if err = tx.QueryRow(ctx, `SELECT id::text FROM providers WHERE name=$1`, name).Scan(&id); err != nil {
			return err
		}
		providerIDs[name] = id
		providerKeyIDs[name] = make(map[string]string, len(pr.Keys))
		canonicalIDs := make([]string, 0, len(pr.Keys))
		for _, key := range pr.Keys {
			record := store.KeyRecord{Provider: name, ID: key.ID, Label: key.Label, Group: key.Group, Tags: key.Tags, Secret: key.Secret, Enabled: true}
			canonicalID, e := saveKeyTx(ctx, tx, s.box, name, id, record)
			if e != nil {
				return fmt.Errorf("pgstore: importar chave do provedor %s: %w", name, e)
			}
			providerKeyIDs[name][string(key.ID)] = canonicalID
			canonicalIDs = append(canonicalIDs, canonicalID)
		}
		if len(canonicalIDs) == 0 {
			if _, err = tx.Exec(ctx, `UPDATE keys SET enabled=false,updated_at=now() WHERE provider_id=$1::uuid`, id); err != nil {
				return err
			}
		} else {
			if _, err = tx.Exec(ctx, `UPDATE keys SET enabled=false,updated_at=now() WHERE provider_id=$1::uuid AND NOT (id::text = ANY($2::text[]))`, id, canonicalIDs); err != nil {
				return err
			}
		}
	}

	projectNames := sortedProjectNames(raw.Projects)
	for _, name := range projectNames {
		pr := raw.Projects[name]
		policy, err := json.Marshal(pr.Policy)
		if err != nil {
			return fmt.Errorf("pgstore: codificar politica %s: %w", name, err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO projects(name,description,enabled,allow_lease,allow_literal_targets,policy)
VALUES($1,$2,true,$3,$4,$5::jsonb)
ON CONFLICT(name) DO UPDATE SET description=EXCLUDED.description,enabled=true,allow_lease=EXCLUDED.allow_lease,
allow_literal_targets=EXCLUDED.allow_literal_targets,policy=EXCLUDED.policy,updated_at=now()`, name, pr.Description, pr.AllowLease, pr.AllowLiteralTargets, policy)
		if err != nil {
			return fmt.Errorf("pgstore: salvar projeto %s: %w", name, err)
		}
		var id string
		if err = tx.QueryRow(ctx, `SELECT id::text FROM projects WHERE name=$1`, name).Scan(&id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM project_key_rules WHERE project_id=$1::uuid`, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM routes WHERE project_id=$1::uuid`, id); err != nil {
			return err
		}

		providers := make([]string, 0, len(pr.Keys))
		for providerName := range pr.Keys {
			providers = append(providers, providerName)
		}
		sort.Strings(providers)
		for _, providerName := range providers {
			providerID, ok := providerIDs[providerName]
			if !ok {
				return fmt.Errorf("pgstore: projeto %s referencia provedor ausente %s", name, providerName)
			}
			rule := pr.Keys[providerName]
			for position, item := range []struct {
				mode     string
				selector config.Selector
			}{{"include", normalizeSelector(rule.Include, providerKeyIDs[providerName], providerName)}, {"exclude", normalizeSelector(rule.Exclude, providerKeyIDs[providerName], providerName)}} {
				if selectorIsEmpty(item.selector) {
					continue
				}
				b, err := json.Marshal(item.selector)
				if err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `INSERT INTO project_key_rules(project_id,provider_id,mode,selector,position)
VALUES($1::uuid,$2::uuid,$3,$4::jsonb,$5)`, id, providerID, item.mode, b, position); err != nil {
					return err
				}
			}
		}
		aliases := make([]string, 0, len(pr.Routes))
		for alias := range pr.Routes {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			route := pr.Routes[alias]
			chain := make([]map[string]string, 0, len(route.Chain))
			for _, target := range route.Chain {
				parts := strings.SplitN(target, ":", 2)
				if len(parts) != 2 {
					return fmt.Errorf("pgstore: target invalido %q", target)
				}
				chain = append(chain, map[string]string{"provider": parts[0], "model": parts[1]})
			}
			chainJSON, err := json.Marshal(chain)
			if err != nil {
				return fmt.Errorf("pgstore: codificar cadeia %s/%s: %w", name, alias, err)
			}
			defaults, err := json.Marshal(route.Defaults)
			if err != nil {
				return fmt.Errorf("pgstore: codificar defaults %s/%s: %w", name, alias, err)
			}
			force, err := json.Marshal(route.Force)
			if err != nil {
				return fmt.Errorf("pgstore: codificar force %s/%s: %w", name, alias, err)
			}
			requires, err := json.Marshal(route.Requires)
			if err != nil {
				return fmt.Errorf("pgstore: codificar requires %s/%s: %w", name, alias, err)
			}
			if _, err = tx.Exec(ctx, `INSERT INTO routes(project_id,alias,priority,chain,defaults,force,requires)
VALUES($1::uuid,$2,$3,$4::jsonb,$5::jsonb,$6::jsonb,$7::jsonb)`, id, alias, route.Priority, chainJSON, defaults, force, requires); err != nil {
				return fmt.Errorf("pgstore: salvar rota %s/%s: %w", name, alias, err)
			}
		}
	}

	if len(projectNames) == 0 {
		if _, err = tx.Exec(ctx, `DELETE FROM projects`); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec(ctx, `DELETE FROM projects WHERE NOT (name = ANY($1::text[]))`, projectNames); err != nil {
			return err
		}
	}
	if len(providerNames) == 0 {
		if _, err = tx.Exec(ctx, `DELETE FROM providers`); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec(ctx, `DELETE FROM providers WHERE NOT (name = ANY($1::text[]))`, providerNames); err != nil {
			return err
		}
	}
	runtimeConfig, err := json.Marshal(map[string]string{"listen": raw.Listen, "default_project": raw.DefaultProject})
	if err != nil {
		return fmt.Errorf("pgstore: codificar runtime config: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE app_settings SET runtime_config=$1::jsonb,config_version=config_version+1 WHERE id=true`, runtimeConfig)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotInitialized
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgstore: confirmar configuracao: %w", err)
	}
	return nil
}

// LoadConfig reconstructs Raw from normalized PostgreSQL tables. APIKey and keys
// are intentionally absent and must be supplied from environment/LoadKeys.
func (s *Store) LoadConfig(ctx context.Context) (*config.Raw, error) {
	raw := &config.Raw{Providers: map[string]config.ProviderRaw{}, Projects: map[string]config.ProjectRaw{}}
	var runtime []byte
	if err := s.pool.QueryRow(ctx, `SELECT runtime_config FROM app_settings WHERE id=true`).Scan(&runtime); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotInitialized
		}
		return nil, err
	}
	var runtimeValues map[string]string
	if len(runtime) > 0 {
		if err := json.Unmarshal(runtime, &runtimeValues); err != nil {
			return nil, fmt.Errorf("pgstore: decodificar runtime_config: %w", err)
		}
	}
	raw.Listen = runtimeValues["listen"]
	raw.DefaultProject = runtimeValues["default_project"]

	rows, err := s.pool.Query(ctx, `SELECT name,enabled,spec,limits FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		var enabled bool
		var specJSON, limitsJSON []byte
		if err = rows.Scan(&name, &enabled, &specJSON, &limitsJSON); err != nil {
			rows.Close()
			return nil, err
		}
		var pr config.ProviderRaw
		if err = json.Unmarshal(specJSON, &pr); err != nil {
			rows.Close()
			return nil, fmt.Errorf("pgstore: decodificar provedor %s: %w", name, err)
		}
		var lim config.RawLimits
		if err = json.Unmarshal(limitsJSON, &lim); err != nil {
			rows.Close()
			return nil, fmt.Errorf("pgstore: decodificar limites %s: %w", name, err)
		}
		pr.Limits = lim
		pr.Enabled = boolPtr(enabled)
		// Preset and explicit overrides remain in spec; the indexed columns are metadata.
		raw.Providers[name] = pr
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(raw.Providers) == 0 {
		return nil, ErrNoConfig
	}

	rows, err = s.pool.Query(ctx, `SELECT name,description,allow_lease,allow_literal_targets,policy FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, description string
		var allowLease, allowLiteral bool
		var policyJSON []byte
		if err = rows.Scan(&name, &description, &allowLease, &allowLiteral, &policyJSON); err != nil {
			rows.Close()
			return nil, err
		}
		var policy config.Policy
		if err = json.Unmarshal(policyJSON, &policy); err != nil {
			rows.Close()
			return nil, err
		}
		raw.Projects[name] = config.ProjectRaw{Project: name, Description: description, AllowLease: allowLease, AllowLiteralTargets: allowLiteral, Policy: policy, Keys: map[string]config.KeyRule{}, Routes: map[string]config.RouteRaw{}}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `SELECT p.name,v.name,r.mode,r.selector FROM project_key_rules r JOIN projects p ON p.id=r.project_id JOIN providers v ON v.id=r.provider_id ORDER BY p.name,v.name,r.position`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var projectName, providerName, mode string
		var selectorJSON []byte
		if err = rows.Scan(&projectName, &providerName, &mode, &selectorJSON); err != nil {
			rows.Close()
			return nil, err
		}
		project, ok := raw.Projects[projectName]
		if !ok {
			continue
		}
		rule := project.Keys[providerName]
		var selector config.Selector
		if err = json.Unmarshal(selectorJSON, &selector); err != nil {
			rows.Close()
			return nil, err
		}
		if mode == "include" {
			rule.Include = selector
		} else if mode == "exclude" {
			rule.Exclude = selector
		}
		project.Keys[providerName] = rule
		raw.Projects[projectName] = project
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `SELECT p.name,r.alias,r.priority,r.chain,r.defaults,r.force,r.requires FROM routes r JOIN projects p ON p.id=r.project_id ORDER BY p.name,r.alias`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var projectName, alias string
		var priority int
		var chainJSON, defaultsJSON, forceJSON, requiresJSON []byte
		if err = rows.Scan(&projectName, &alias, &priority, &chainJSON, &defaultsJSON, &forceJSON, &requiresJSON); err != nil {
			rows.Close()
			return nil, err
		}
		var chain []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if err = json.Unmarshal(chainJSON, &chain); err != nil {
			rows.Close()
			return nil, err
		}
		route := config.RouteRaw{Priority: priority, Defaults: map[string]any{}, Force: map[string]any{}, Requires: map[string]bool{}}
		for _, target := range chain {
			route.Chain = append(route.Chain, target.Provider+":"+target.Model)
		}
		if len(defaultsJSON) > 0 {
			if err = json.Unmarshal(defaultsJSON, &route.Defaults); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if len(forceJSON) > 0 {
			if err = json.Unmarshal(forceJSON, &route.Force); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if len(requiresJSON) > 0 {
			if err = json.Unmarshal(requiresJSON, &route.Requires); err != nil {
				rows.Close()
				return nil, err
			}
		}
		project, ok := raw.Projects[projectName]
		if ok {
			project.Routes[alias] = route
			raw.Projects[projectName] = project
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return raw, nil
}

// SaveKeys encrypts each provider key with ID/provider AAD and upserts by UUID.
func (s *Store) SaveKeys(ctx context.Context, providerName string, keys []store.KeyRecord) error {
	if s == nil || s.pool == nil || s.box == nil {
		return errors.New("pgstore: store incompleto")
	}
	if providerName == "" {
		return errors.New("pgstore: provider vazio")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var providerID string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM providers WHERE name=$1`, providerName).Scan(&providerID); err != nil {
		return fmt.Errorf("pgstore: provedor %s: %w", providerName, err)
	}
	for _, key := range keys {
		if _, err = saveKeyTx(ctx, tx, s.box, providerName, providerID, key); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func saveKeyTx(ctx context.Context, tx pgx.Tx, box *cryptobox.Box, providerName, providerID string, key store.KeyRecord) (string, error) {
	if box == nil {
		return "", errors.New("pgstore: cryptobox ausente")
	}
	plain := key.Secret.Reveal()
	if plain == "" {
		return "", fmt.Errorf("chave %s sem segredo", key.Label)
	}
	if strings.TrimSpace(string(key.ID)) == "" {
		return "", fmt.Errorf("chave %s sem identificador", key.Label)
	}
	hash := sha256.Sum256([]byte(plain))
	label := key.Label
	if label == "" {
		label = secret.Mask(plain)
	}
	id := stableKeyUUID(providerName, string(key.ID))
	// Prefer an already persisted identical credential; next prefer a matching
	// label so rotating the value of a named environment key keeps its DB id.
	var existing string
	err := tx.QueryRow(ctx, `SELECT id::text FROM keys WHERE provider_id=$1::uuid AND (secret_hash=$2 OR label=$3)
ORDER BY (secret_hash=$2) DESC LIMIT 1`, providerID, hash[:], label).Scan(&existing)
	if err == nil {
		id = existing
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	envelope, err := box.Encrypt(id, providerName, []byte(plain))
	if err != nil {
		return "", err
	}
	tags := append([]string{}, key.Tags...)
	_, err = tx.Exec(ctx, `INSERT INTO keys(id,provider_id,label,secret_enc,secret_hash,mask,account_group,tags,enabled)
VALUES($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8::text[],$9)
ON CONFLICT(id) DO UPDATE SET label=EXCLUDED.label,secret_enc=EXCLUDED.secret_enc,secret_hash=EXCLUDED.secret_hash,
mask=EXCLUDED.mask,account_group=EXCLUDED.account_group,tags=EXCLUDED.tags,enabled=EXCLUDED.enabled,updated_at=now()`,
		id, providerID, label, envelope, hash[:], secret.Mask(plain), nullString(key.Group), tags, key.Enabled)
	if err != nil {
		return "", fmt.Errorf("salvar chave %s: %w", label, err)
	}
	return id, nil
}

// DeleteKey remove uma credencial persistida sem retornar seu conteúdo.
func (s *Store) DeleteKey(ctx context.Context, id sched.KeyID) error {
	if s == nil || s.pool == nil {
		return errors.New("pgstore: pool ausente")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM keys WHERE id::text=$1`, string(id))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("pgstore: chave não encontrada")
	}
	return nil
}

// LoadKeys decrypts secrets only after the master-key marker has been checked by startup.
func (s *Store) LoadKeys(ctx context.Context) ([]store.KeyRecord, error) {
	if s == nil || s.pool == nil || s.box == nil {
		return nil, errors.New("pgstore: store incompleto")
	}
	if err := s.VerifyMasterKey(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT p.name,k.id::text,k.label,k.account_group,k.tags,k.secret_enc,k.enabled FROM keys k JOIN providers p ON p.id=k.provider_id ORDER BY p.name,k.label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.KeyRecord, 0)
	for rows.Next() {
		var providerName, id, label string
		var group sql.NullString
		var tags []string
		var envelope []byte
		var enabled bool
		if err := rows.Scan(&providerName, &id, &label, &group, &tags, &envelope, &enabled); err != nil {
			return nil, err
		}
		plain, err := s.box.Decrypt(id, providerName, envelope)
		if err != nil {
			return nil, fmt.Errorf("pgstore: decifrar chave %s/%s: %w", providerName, label, err)
		}
		out = append(out, store.KeyRecord{Provider: providerName, ID: sched.KeyID(id), Label: label, Group: group.String, Tags: tags, Secret: secret.New(string(plain)), Enabled: enabled})
	}
	return out, rows.Err()
}

// LoadPoolState restores the complete snapshot saved for a provider.
func (s *Store) LoadPoolState(ctx context.Context, providerName string) (sched.PoolSnapshot, error) {
	var stateJSON []byte
	err := s.pool.QueryRow(ctx, `SELECT st.state FROM limit_group_state st JOIN providers p ON p.id=st.provider_id WHERE p.name=$1 AND st.group_key='*'`, providerName).Scan(&stateJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return sched.PoolSnapshot{}, nil
	}
	if err != nil {
		return sched.PoolSnapshot{}, err
	}
	var state sched.PoolSnapshot
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		return sched.PoolSnapshot{}, fmt.Errorf("pgstore: decodificar snapshot %s: %w", providerName, err)
	}
	return state, nil
}

// SavePoolState persists snapshots using one row per provider.
func (s *Store) SavePoolState(ctx context.Context, providerName string, state sched.PoolSnapshot) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO limit_group_state(provider_id,group_key,state,saved_at)
SELECT id,'*',$2::jsonb,to_timestamp($3::double precision/1000) FROM providers WHERE name=$1
ON CONFLICT(provider_id,group_key) DO UPDATE SET state=EXCLUDED.state,saved_at=EXCLUDED.saved_at`, providerName, b, state.SavedAtMS)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("pgstore: provedor inexistente ao salvar snapshot: %s", providerName)
	}
	return nil
}

// AppendUsage writes a batch in a single transaction. Call it via usage.Writer,
// never from a request handler.
func (s *Store) AppendUsage(ctx context.Context, events []domain.UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	if s == nil || s.pool == nil {
		return errors.New("pgstore: pool ausente")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Stay comfortably below PostgreSQL's 65,535 bind-parameter limit.
	const fieldsPerRow = 16
	const maxRowsPerStatement = 4000
	for offset := 0; offset < len(events); offset += maxRowsPerStatement {
		end := offset + maxRowsPerStatement
		if end > len(events) {
			end = len(events)
		}
		chunk := events[offset:end]
		args := make([]any, 0, len(chunk)*fieldsPerRow)
		var query strings.Builder
		query.WriteString(`INSERT INTO usage_events(ts,request_id,project,alias,provider,model,key_id,outcome,http_status,attempt,queue_ms,prompt_tokens,completion_tokens,est_tokens,source,actual_tokens) VALUES `)
		for rowIndex, event := range chunk {
			if rowIndex > 0 {
				query.WriteByte(',')
			}
			query.WriteByte('(')
			rowArgs := usageRowArgs(event)
			for column, value := range rowArgs {
				if column > 0 {
					query.WriteByte(',')
				}
				fmt.Fprintf(&query, "$%d", len(args)+1)
				if column == 6 {
					query.WriteString("::uuid")
				}
				args = append(args, value)
			}
			query.WriteByte(')')
		}
		if _, err = tx.Exec(ctx, query.String(), args...); err != nil {
			return fmt.Errorf("pgstore: inserir lote de usage (%d eventos): %w", len(chunk), err)
		}
	}
	return tx.Commit(ctx)
}

func usageRowArgs(e domain.UsageEvent) []any {
	at := e.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var keyID any
	if uuidLike(e.KeyID) {
		keyID = string(e.KeyID)
	}
	var prompt, completion any
	if e.Usage != nil {
		prompt = e.Usage.PromptTokens
		completion = e.Usage.CompletionTokens
	}
	var status any
	if e.Status != 0 {
		status = e.Status
	}
	var estimated any
	if e.EstimatedTokens != 0 {
		estimated = e.EstimatedTokens
	}
	source := e.Source
	if source == "" {
		source = "request"
	}
	if source != "request" && source != "warmup" && source != "lease" && source != "passthrough" {
		source = "request"
	}
	actual := e.ActualTokens
	if actual <= 0 && e.Usage != nil && e.Usage.TotalTokens > 0 {
		actual = e.Usage.TotalTokens
	}
	var actualValue any
	if actual > 0 {
		actualValue = actual
	}
	return []any{at, e.RequestID, e.Project, e.Alias, e.Provider, e.Model, keyID, string(e.Outcome), status, maxInt(1, e.Attempt), maxInt(0, e.QueueMS), prompt, completion, estimated, source, actualValue}
}

// AppendEvent records an operational event without secrets.
func (s *Store) AppendEvent(ctx context.Context, e sched.Event) error {
	at := e.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	level := "info"
	lower := strings.ToLower(e.Kind)
	if strings.Contains(lower, "invalid") || strings.Contains(lower, "failed") {
		level = "warn"
	}
	detail, _ := json.Marshal(map[string]string{"detail": secret.Redact(e.Detail)})
	_, err := s.pool.Exec(ctx, `INSERT INTO events(ts,level,kind,subject,detail) VALUES($1,$2,$3,$4,$5::jsonb)`, at, level, secret.Redact(e.Kind), secret.Redact(e.Subject), detail)
	return err
}

// ConfigVersion returns the monotonically increasing persisted configuration revision.
func (s *Store) ConfigVersion(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT config_version FROM app_settings WHERE id=true`).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotInitialized
	}
	return n, err
}

func boolPtr(v bool) *bool { return &v }
func nullString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
func durationSeconds(v string, fallback int) int {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	d, e := time.ParseDuration(v)
	if e != nil || d < 0 {
		return fallback
	}
	return int(d / time.Second)
}
func sortedProviderNames(m map[string]config.ProviderRaw) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func sortedProjectNames(m map[string]config.ProjectRaw) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func selectorIsEmpty(s config.Selector) bool {
	return len(s.KeyIDs)+len(s.Labels)+len(s.TagsAny)+len(s.TagsAll)+len(s.AccountGroups) == 0
}
func normalizeSelector(s config.Selector, canonicalIDs map[string]string, providerName string) config.Selector {
	s.KeyIDs = append([]string(nil), s.KeyIDs...)
	for i, id := range s.KeyIDs {
		if canonical, ok := canonicalIDs[id]; ok {
			s.KeyIDs[i] = canonical
		} else {
			s.KeyIDs[i] = stableKeyUUID(providerName, id)
		}
	}
	return s
}
func stableKeyUUID(providerName, id string) string {
	if uuidLike(id) {
		return strings.ToLower(id)
	}
	sum := sha256.Sum256([]byte("keypool:key:" + providerName + ":" + id))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x80
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func uuidLike(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i, c := range v {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ store.Store = (*Store)(nil)

// ReadUsage consulta metadados de uso sem ler prompts ou respostas.
func (s *Store) ReadUsage(ctx context.Context, from, to time.Time, project string) ([]domain.UsageEvent, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("pgstore: pool ausente")
	}
	rows, err := s.pool.Query(ctx, `
SELECT ts,request_id,project,alias,provider,model,COALESCE(key_id::text,''),
       outcome,COALESCE(http_status,0),attempt,queue_ms,COALESCE(prompt_tokens,0),
       COALESCE(completion_tokens,0),COALESCE(est_tokens,0),source,COALESCE(actual_tokens,0)
FROM usage_events
WHERE ($1::timestamptz IS NULL OR ts >= $1)
  AND ($2::timestamptz IS NULL OR ts <= $2)
  AND ($3='' OR project=$3)
ORDER BY ts DESC LIMIT 5000`, nullableTime(from), nullableTime(to), project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.UsageEvent, 0)
	for rows.Next() {
		var e domain.UsageEvent
		var prompt, completion int
		if err = rows.Scan(
			&e.At, &e.RequestID, &e.Project, &e.Alias, &e.Provider, &e.Model,
			&e.KeyID, &e.Outcome, &e.Status, &e.Attempt, &e.QueueMS,
			&prompt, &completion, &e.EstimatedTokens, &e.Source, &e.ActualTokens,
		); err != nil {
			return nil, err
		}
		if prompt > 0 || completion > 0 || e.ActualTokens > 0 {
			e.Usage = &domain.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: maxInt(e.ActualTokens, prompt+completion)}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReadEvents consulta a trilha administrativa, sempre sem segredos.
func (s *Store) ReadEvents(ctx context.Context, kind string, since time.Time) ([]store.AdminEvent, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("pgstore: pool ausente")
	}
	rows, err := s.pool.Query(ctx, `
SELECT ts,level,kind,subject,detail FROM events
WHERE ($1='' OR kind=$1) AND ($2::timestamptz IS NULL OR ts >= $2)
ORDER BY ts DESC LIMIT 2000`, kind, nullableTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.AdminEvent, 0)
	for rows.Next() {
		var e store.AdminEvent
		if err = rows.Scan(&e.At, &e.Level, &e.Kind, &e.Subject, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func nullableTime(v time.Time) any {
	if v.IsZero() {
		return nil
	}
	return v
}

// ReadProjectDailyTokens carrega os totais do dia UTC sem limitar a quantidade de eventos.
// usage_events retém pelo menos 30 dias, portanto o dia atual ainda não foi agregado/removido.
func (s *Store) ReadProjectDailyTokens(ctx context.Context, from time.Time) (map[string]int, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("pgstore: pool ausente")
	}
	rows, err := s.pool.Query(ctx, `
SELECT project, COALESCE(sum(CASE
  WHEN COALESCE(actual_tokens,0)>0 THEN actual_tokens
  WHEN COALESCE(prompt_tokens,0)+COALESCE(completion_tokens,0)>0 THEN COALESCE(prompt_tokens,0)+COALESCE(completion_tokens,0)
  ELSE COALESCE(est_tokens,0) END),0)::bigint
FROM usage_events
WHERE ts >= $1 AND source IN ('request','lease','passthrough')
GROUP BY project`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var project string
		var total int64
		if err = rows.Scan(&project, &total); err != nil {
			return nil, err
		}
		if total > int64(^uint(0)>>1) {
			total = int64(^uint(0) >> 1)
		}
		out[project] = int(total)
	}
	return out, rows.Err()
}

var _ store.ProjectDailyUsageReader = (*Store)(nil)
