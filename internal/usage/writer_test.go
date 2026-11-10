package usage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"keypool/internal/domain"
)

type sink struct {
	mu  sync.Mutex
	got []domain.UsageEvent
	err error
}

func (s *sink) AppendUsage(_ context.Context, v []domain.UsageEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.got = append(s.got, v...)
	return nil
}

func TestWriterFlushesOnClose(t *testing.T) {
	s := &sink{}
	w := NewWriter(s, Options{QueueSize: 8, BatchSize: 3, Interval: time.Hour})
	for i := 0; i < 5; i++ {
		if !w.Submit(domain.UsageEvent{RequestID: string(rune('a' + i))}) {
			t.Fatal("submit rejected")
		}
	}
	w.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != 5 {
		t.Fatalf("persisted %d events, want 5", len(s.got))
	}
}

func TestWriterCountsQueueDrops(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	app := appenderFunc(func(context.Context, []domain.UsageEvent) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	})
	w := NewWriter(app, Options{QueueSize: 1, BatchSize: 1, Interval: time.Hour})
	if !w.Submit(domain.UsageEvent{}) {
		t.Fatal("first submit rejected")
	}
	<-entered // worker is blocked in persistence; queue is empty again
	if !w.Submit(domain.UsageEvent{}) {
		t.Fatal("second submit should occupy the queue")
	}
	if w.Submit(domain.UsageEvent{}) {
		t.Fatal("third submit should be dropped")
	}
	close(release)
	w.Close()
	if w.Dropped() != 1 {
		t.Fatalf("dropped=%d, want 1", w.Dropped())
	}
}

type appenderFunc func(context.Context, []domain.UsageEvent) error

func (f appenderFunc) AppendUsage(ctx context.Context, v []domain.UsageEvent) error { return f(ctx, v) }

func TestWriterCountsPersistenceErrors(t *testing.T) {
	s := &sink{err: errors.New("db unavailable")}
	w := NewWriter(s, Options{QueueSize: 4, BatchSize: 1, Interval: time.Hour})
	w.Submit(domain.UsageEvent{})
	w.Close()
	if w.Dropped() != 1 {
		t.Fatalf("dropped=%d, want 1", w.Dropped())
	}
}

func TestCloseConcurrentWithSubmit(t *testing.T) {
	w := NewWriter(&sink{}, Options{QueueSize: 256, BatchSize: 16, Interval: time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				w.Submit(domain.UsageEvent{})
			}
		}()
	}
	go w.Close()
	wg.Wait()
	w.Close()
	select {
	case <-w.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}
