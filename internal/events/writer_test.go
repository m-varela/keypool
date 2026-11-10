package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"keypool/internal/sched"
)

type fakeAppender struct {
	mu      sync.Mutex
	written []sched.Event
	fail    bool
}

func (f *fakeAppender) AppendEvent(_ context.Context, e sched.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("storage unavailable")
	}
	f.written = append(f.written, e)
	return nil
}

func TestWriterDrainsAcceptedEvents(t *testing.T) {
	app := &fakeAppender{}
	w := NewWriter(app, Options{QueueSize: 16, Timeout: time.Second})
	for i := 0; i < 10; i++ {
		if !w.Submit(sched.Event{Kind: "test", Subject: string(rune('a' + i))}) {
			t.Fatalf("event %d rejected", i)
		}
	}
	w.Close()
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.written) != 10 {
		t.Fatalf("persisted %d events, want 10", len(app.written))
	}
	if w.Dropped() != 0 {
		t.Fatalf("unexpected dropped events: %d", w.Dropped())
	}
	if w.Submit(sched.Event{Kind: "late"}) {
		t.Fatal("submission after Close succeeded")
	}
	if w.Dropped() != 1 {
		t.Fatalf("dropped=%d, want 1", w.Dropped())
	}
}

func TestWriterCountsStorageFailures(t *testing.T) {
	app := &fakeAppender{fail: true}
	w := NewWriter(app, Options{QueueSize: 2, Timeout: time.Second})
	if !w.Submit(sched.Event{Kind: "failure"}) {
		t.Fatal("event rejected")
	}
	w.Close()
	if w.Dropped() != 1 {
		t.Fatalf("dropped=%d, want 1", w.Dropped())
	}
}
