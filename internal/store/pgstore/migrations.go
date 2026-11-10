package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"keypool/internal/store/migrations"
)

const migrationAdvisoryLockKey int64 = 0x4b4559504d494752

// Migrate opens PostgreSQL, applies every pending embedded migration and then
// validates that the database contains the exact checksums shipped in binary.
func Migrate(ctx context.Context, dsn string) error {
	if dsn == "" {
		return errors.New("pgstore: database URL ausente")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("pgstore: abrir PostgreSQL para migrar: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pgstore: conectar PostgreSQL para migrar: %w", err)
	}
	if err := migratePool(ctx, pool); err != nil {
		return err
	}
	return checkPoolMigrations(ctx, pool)
}

// Migrate applies pending migrations using the Store's connection pool.
func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("pgstore: pool ausente")
	}
	if err := migratePool(ctx, s.pool); err != nil {
		return err
	}
	return checkPoolMigrations(ctx, s.pool)
}

// CheckMigrations verifies migration state without changing the database.
func (s *Store) CheckMigrations(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("pgstore: pool ausente")
	}
	return checkPoolMigrations(ctx, s.pool)
}

func migratePool(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("migrations: pool PostgreSQL ausente")
	}
	// Serialize control-table creation as well as individual migrations.
	controlTx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrations: iniciar controle: %w", err)
	}
	if _, err = controlTx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationAdvisoryLockKey); err == nil {
		_, err = controlTx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`)
	}
	if err != nil {
		_ = controlTx.Rollback(ctx)
		return fmt.Errorf("migrations: criar controle: %w", err)
	}
	if err = controlTx.Commit(ctx); err != nil {
		return fmt.Errorf("migrations: confirmar controle: %w", err)
	}
	list, err := migrations.List()
	if err != nil {
		return err
	}
	for _, migration := range list {
		// The transaction-level advisory lock serializes the check/apply pair.
		// It also works with a pool configured for a single connection.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("migrations: iniciar %s: %w", migration.Name, err)
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationAdvisoryLockKey); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migrations: lock %s: %w", migration.Name, err)
		}
		var existing string
		err = tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, migration.Name).Scan(&existing)
		if err == nil {
			if existing != migration.Checksum {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("migrations: checksum divergente para %s", migration.Name)
			}
			if err = tx.Commit(ctx); err != nil {
				return fmt.Errorf("migrations: confirmar verificacao %s: %w", migration.Name, err)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migrations: consultar %s: %w", migration.Name, err)
		}
		// Simple protocol is required because one immutable .sql file may
		// contain multiple DDL statements. The SQL itself is embedded/trusted.
		if _, err = tx.Exec(ctx, migration.SQL, pgx.QueryExecModeSimpleProtocol); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, migration.Name, migration.Checksum)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migrations: aplicar %s: %w", migration.Name, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("migrations: confirmar %s: %w", migration.Name, err)
		}
	}
	return nil
}

func checkPoolMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("migrations: pool PostgreSQL ausente")
	}
	list, err := migrations.List()
	if err != nil {
		return err
	}
	expected := make(map[string]string, len(list))
	for _, migration := range list {
		expected[migration.Name] = migration.Checksum
	}
	rows, err := pool.Query(ctx, `SELECT version,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("migrations: ler controle: %w", err)
	}
	applied := make(map[string]string, len(list))
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return fmt.Errorf("migrations: ler registro: %w", err)
		}
		if _, known := expected[version]; !known {
			rows.Close()
			return fmt.Errorf("migrations: banco possui versao desconhecida %s; atualize o binario KeyPool", version)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("migrations: percorrer controle: %w", err)
	}
	rows.Close()
	for _, migration := range list {
		checksum, ok := applied[migration.Name]
		if !ok {
			return fmt.Errorf("migrations: pendente: %s", migration.Name)
		}
		if checksum != migration.Checksum {
			return fmt.Errorf("migrations: checksum divergente para %s", migration.Name)
		}
	}
	return nil
}

// AcquireAdvisoryLock reserves the single daemon lock through Store.
func (s *Store) AcquireAdvisoryLock(ctx context.Context, key int64) (*PoolAdvisoryLock, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("pgstore: pool ausente")
	}
	return AcquirePoolAdvisoryLock(ctx, s.pool, key)
}

// Prune runs the retention transaction against this Store's pool.
func (s *Store) Prune(ctx context.Context, now time.Time, retention Retention) (map[string]int64, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("pgstore: pool ausente")
	}
	return PrunePool(ctx, s.pool, now, retention)
}
