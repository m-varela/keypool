// Package events persists operational scheduler events asynchronously.
package events

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"keypool/internal/sched"
)

var ErrClosed = errors.New("events: writer is closed")

// Appender is implemented by the persistent store.
type Appender interface {
	AppendEvent(context.Context, sched.Event) error
}

// Options controls queue capacity and database timeout.
type Options struct {
	QueueSize int
	Timeout   time.Duration
	OnError   func(error)
}

// Writer accepts events without blocking scheduler/request goroutines. Events
// are persisted by one worker, and Close drains all accepted events.
type Writer struct {
	app    Appender
	queue  chan sched.Event
	stop   chan struct{}
	done   chan struct{}
	mu     sync.RWMutex
	closed bool
	drop   atomic.Uint64
	opts   Options
}

func NewWriter(app Appender, opts Options) *Writer {
	if opts.QueueSize <= 0 {
		opts.QueueSize = 512
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	w := &Writer{app: app, queue: make(chan sched.Event, opts.QueueSize), stop: make(chan struct{}), done: make(chan struct{}), opts: opts}
	go w.run()
	return w
}

// Submit is non-blocking and safe for concurrent callers.
func (w *Writer) Submit(e sched.Event) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed || w.app == nil {
		w.drop.Add(1)
		return false
	}
	select {
	case w.queue <- e:
		return true
	default:
		w.drop.Add(1)
		return false
	}
}

// Dropped reports events rejected by a full/closed writer or failed storage.
func (w *Writer) Dropped() uint64 { return w.drop.Load() }

// Close stops accepting events, drains queued items, and waits for the worker.
func (w *Writer) Close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.stop)
	}
	w.mu.Unlock()
	<-w.done
}

func (w *Writer) persist(e sched.Event) {
	if w.app == nil {
		w.drop.Add(1)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.opts.Timeout)
	err := w.app.AppendEvent(ctx, e)
	cancel()
	if err != nil {
		w.drop.Add(1)
		if w.opts.OnError != nil {
			w.opts.OnError(err)
		}
	}
}

func (w *Writer) run() {
	defer close(w.done)
	for {
		select {
		case e := <-w.queue:
			w.persist(e)
		case <-w.stop:
			for {
				select {
				case e := <-w.queue:
					w.persist(e)
				default:
					return
				}
			}
		}
	}
}
