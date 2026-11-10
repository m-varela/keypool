package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"keypool/internal/sched"
)

// SnapshotPool is the minimal runtime surface required by Snapshotter.
type SnapshotPool interface {
	Snapshot() sched.PoolSnapshot
	Restore(sched.PoolSnapshot)
}

type poolStateStore interface {
	LoadPoolState(context.Context, string) (sched.PoolSnapshot, error)
	SavePoolState(context.Context, string, sched.PoolSnapshot) error
}

// Snapshotter periodically persists only changed pool snapshots and performs
// a final save during Close. Pools are keyed by provider name.
type Snapshotter struct {
	store      poolStateStore
	pools      map[string]SnapshotPool
	poolSource func() map[string]SnapshotPool
	interval   time.Duration
	onError    func(error)
	mu         sync.Mutex
	dirty      bool
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once
	startOnce  sync.Once
	saveMu     sync.Mutex
	last       map[string][32]byte
}

func NewSnapshotter(st poolStateStore, pools map[string]SnapshotPool, interval time.Duration, onError func(error)) *Snapshotter {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	copyPools := make(map[string]SnapshotPool, len(pools))
	for name, p := range pools {
		copyPools[name] = p
	}
	return &Snapshotter{store: st, pools: copyPools, interval: interval, onError: onError, dirty: true, stop: make(chan struct{}), done: make(chan struct{}), last: map[string][32]byte{}}
}

// NewDynamicSnapshotter resolves the current provider pools at each restore/save.
// This is required when Router.SetSnapshot replaces runtime pool instances on
// configuration reload; a captured map would otherwise keep persisting detached pools.
func NewDynamicSnapshotter(st poolStateStore, source func() map[string]SnapshotPool, interval time.Duration, onError func(error)) *Snapshotter {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if source == nil {
		source = func() map[string]SnapshotPool { return nil }
	}
	return &Snapshotter{store: st, poolSource: source, interval: interval, onError: onError, dirty: true, stop: make(chan struct{}), done: make(chan struct{}), last: map[string][32]byte{}}
}

func (s *Snapshotter) currentPools() map[string]SnapshotPool {
	if s.poolSource != nil {
		return s.poolSource()
	}
	return s.pools
}

// Restore loads saved state before traffic is accepted. Missing/zero snapshots
// are ignored; other storage errors abort startup.
func (s *Snapshotter) Restore(ctx context.Context) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	missing := false
	for name, p := range s.currentPools() {
		state, err := s.store.LoadPoolState(ctx, name)
		if err != nil {
			return err
		}
		if state.SavedAtMS == 0 {
			missing = true
			delete(s.last, name)
			continue
		}
		if startup, ok := p.(interface{ RestoreStartup(sched.PoolSnapshot) }); ok {
			startup.RestoreStartup(state)
		} else {
			p.Restore(state)
		}
		h, err := hashSnapshot(p.Snapshot())
		if err != nil {
			return err
		}
		s.last[name] = h
	}
	s.mu.Lock()
	s.dirty = missing
	s.mu.Unlock()
	return nil
}

// MarkDirty schedules a save on the next tick. It is safe from request paths.
func (s *Snapshotter) MarkDirty() { s.mu.Lock(); s.dirty = true; s.mu.Unlock() }

// Start launches the periodic worker. Call Restore before Start when restoring
// persisted state is desired.
func (s *Snapshotter) Start() { s.startOnce.Do(func() { go s.run() }) }

func (s *Snapshotter) Save(ctx context.Context) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	for name, p := range s.currentPools() {
		state := p.Snapshot()
		h, err := hashSnapshot(state)
		if err != nil {
			return err
		}
		if old, ok := s.last[name]; ok && old == h {
			continue
		}
		if err := s.store.SavePoolState(ctx, name, state); err != nil {
			return err
		}
		s.last[name] = h
	}
	return nil
}

// hashSnapshot excludes SavedAtMS: it is metadata, not runtime state. Without
// this normalization an otherwise idle pool would be written every interval.
func hashSnapshot(state sched.PoolSnapshot) ([32]byte, error) {
	state.SavedAtMS = 0
	b, err := json.Marshal(state)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func (s *Snapshotter) run() {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.mu.Lock()
			dirty := s.dirty
			s.dirty = false
			s.mu.Unlock()
			if dirty {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := s.Save(ctx)
				cancel()
				if err != nil {
					s.report(err)
					s.MarkDirty()
				}
			}
		case <-s.stop:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := s.Save(ctx)
			cancel()
			if err != nil {
				s.report(err)
			}
			return
		}
	}
}

func (s *Snapshotter) report(err error) {
	if s.onError != nil {
		s.onError(err)
	}
}

// Close is idempotent and waits for the final snapshot attempt.
func (s *Snapshotter) Close() { s.Start(); s.once.Do(func() { close(s.stop) }); <-s.done }
