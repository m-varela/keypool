package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"keypool/internal/sched"
)

type snapshotStore struct {
	mu     sync.Mutex
	states map[string]sched.PoolSnapshot
	saves  int
	err    error
}

func (s *snapshotStore) LoadPoolState(_ context.Context, name string) (sched.PoolSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[name], s.err
}
func (s *snapshotStore) SavePoolState(_ context.Context, name string, v sched.PoolSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.states == nil {
		s.states = map[string]sched.PoolSnapshot{}
	}
	s.states[name] = v
	s.saves++
	return nil
}

type snapshotPool struct {
	state    sched.PoolSnapshot
	restored bool
}

func (p *snapshotPool) Snapshot() sched.PoolSnapshot { return p.state }
func (p *snapshotPool) Restore(v sched.PoolSnapshot) { p.state = v; p.restored = true }

func TestSnapshotterRestoreAndSaveOnlyChanges(t *testing.T) {
	ctx := context.Background()
	st := &snapshotStore{states: map[string]sched.PoolSnapshot{"p": {SavedAtMS: 123}}}
	pool := &snapshotPool{}
	s := NewSnapshotter(st, map[string]SnapshotPool{"p": pool}, time.Hour, nil)
	if err := s.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if !pool.restored || pool.state.SavedAtMS != 123 {
		t.Fatal("persisted state was not restored")
	}
	if err := s.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if st.saves != 0 {
		t.Fatalf("unchanged snapshot saved %d times", st.saves)
	}
	pool.state.SavedAtMS = 456 // timestamp-only drift is not a state change
	if err := s.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if st.saves != 0 {
		t.Fatalf("timestamp-only snapshot saved %d times", st.saves)
	}
	pool.state.Merged = true
	if err := s.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if st.saves != 1 {
		t.Fatalf("changed snapshot saves=%d, want 1", st.saves)
	}
}

func TestSnapshotterCloseBeforeStart(t *testing.T) {
	st := &snapshotStore{}
	pool := &snapshotPool{state: sched.PoolSnapshot{SavedAtMS: 7}}
	s := NewSnapshotter(st, map[string]SnapshotPool{"p": pool}, time.Hour, nil)
	s.Close()
	s.Close()
	if st.saves != 1 {
		t.Fatalf("final save count=%d, want 1", st.saves)
	}
}

func TestSnapshotterPersistsFirstSnapshotAfterEmptyRestore(t *testing.T) {
	st := &snapshotStore{states: map[string]sched.PoolSnapshot{}}
	pool := &snapshotPool{state: sched.PoolSnapshot{SavedAtMS: 9}}
	s := NewSnapshotter(st, map[string]SnapshotPool{"p": pool}, time.Hour, nil)
	if err := s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if st.saves != 1 {
		t.Fatalf("initial snapshot saves=%d, want 1", st.saves)
	}
}

func TestSnapshotterRestoreError(t *testing.T) {
	want := errors.New("database unavailable")
	s := NewSnapshotter(&snapshotStore{err: want}, map[string]SnapshotPool{"p": &snapshotPool{}}, time.Hour, nil)
	if err := s.Restore(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Restore error=%v, want %v", err, want)
	}
	s.Close()
}

func TestDynamicSnapshotterUsesCurrentPoolsAfterReload(t *testing.T) {
	st := &snapshotStore{states: map[string]sched.PoolSnapshot{"p": {SavedAtMS: 123, Merged: true}}}
	oldPool := &snapshotPool{state: sched.PoolSnapshot{SavedAtMS: 1}}
	newPool := &snapshotPool{state: sched.PoolSnapshot{SavedAtMS: 2}}
	current := map[string]SnapshotPool{"p": oldPool}
	s := NewDynamicSnapshotter(st, func() map[string]SnapshotPool { return current }, time.Hour, nil)
	current = map[string]SnapshotPool{"p": newPool}
	if err := s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oldPool.restored {
		t.Fatal("detached pre-reload pool was unexpectedly restored")
	}
	if !newPool.restored || !newPool.state.Merged {
		t.Fatal("current pool did not receive persisted state")
	}
	newPool.state.Merged = false
	if err := s.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.states["p"].Merged; got {
		t.Fatal("save did not read the current pool after reload")
	}
}

type startupSnapshotPool struct {
	snapshotPool
	startupRestored bool
}

func (p *startupSnapshotPool) RestoreStartup(v sched.PoolSnapshot) {
	p.snapshotPool.Restore(v)
	p.startupRestored = true
}

func TestSnapshotterPrefersStartupRestoreWhenSupported(t *testing.T) {
	st := &snapshotStore{states: map[string]sched.PoolSnapshot{"p": {SavedAtMS: 123}}}
	pool := &startupSnapshotPool{}
	s := NewSnapshotter(st, map[string]SnapshotPool{"p": pool}, time.Hour, nil)
	if err := s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pool.startupRestored || !pool.restored {
		t.Fatal("startup-specific restore was not used")
	}
	s.Close()
}
