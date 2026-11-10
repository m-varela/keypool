// Package pgstore contains PostgreSQL-specific persistence primitives.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrAlreadyRunning = errors.New("keypool: another instance already holds the PostgreSQL advisory lock")

// DefaultAdvisoryLockKey is reserved for the single local KeyPool daemon.
const DefaultAdvisoryLockKey int64 = 0x4b4559504f4f4c

// PoolAdvisoryLock pins the owning session for the entire lock lifetime.
type PoolAdvisoryLock struct {
	conn *pgxpool.Conn
	key  int64
	once sync.Once
	err  error
}

// AcquirePoolAdvisoryLock obtains a session-level lock without waiting.
func AcquirePoolAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key int64) (*PoolAdvisoryLock, error) {
	if pool == nil {
		return nil, fmt.Errorf("pgstore: pool is nil")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgstore: reserve lock connection: %w", err)
	}
	var acquired bool
	if err = conn.Conn().QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = conn.Conn().Close(closeCtx)
		cancel()
		conn.Release()
		return nil, fmt.Errorf("pgstore: acquire advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, ErrAlreadyRunning
	}
	return &PoolAdvisoryLock{conn: conn, key: key}, nil
}

// Unlock releases the lock on its owning session. If the unlock query fails,
// the physical connection is closed before release so a pooled session cannot
// accidentally keep the advisory lock alive.
func (l *PoolAdvisoryLock) Unlock(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		var released bool
		if err := l.conn.Conn().QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, l.key).Scan(&released); err != nil {
			l.err = err
			closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = l.conn.Conn().Close(closeCtx)
			cancel()
		} else if !released {
			l.err = errors.New("pgstore: advisory lock was not held by this session")
		}
		l.conn.Release()
	})
	return l.err
}
