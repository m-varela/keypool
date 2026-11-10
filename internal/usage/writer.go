// Package usage provides a bounded, asynchronous batch writer for request usage.
package usage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"keypool/internal/domain"
)

var ErrClosed = errors.New("usage: writer is closed")

// Appender persists a batch. Store implements this through AppendUsage.
type Appender interface {
	AppendUsage(context.Context, []domain.UsageEvent) error
}

// Options controls queueing and flush behavior. Zero values select safe defaults.
type Options struct {
	QueueSize int
	BatchSize int
	Interval  time.Duration
	Timeout   time.Duration
	OnError   func(error)
}

// Writer never blocks request handlers: Submit either enqueues immediately or
// increments Dropped. Close stops acceptance and flushes all queued events.
type Writer struct {
	app    Appender
	queue  chan domain.UsageEvent
	stop   chan struct{}
	done   chan struct{}
	mu     sync.RWMutex
	closed bool
	drop   atomic.Uint64
	opts   Options
}

func NewWriter(app Appender, opts Options) *Writer {
	if opts.QueueSize <= 0 {
		opts.QueueSize = 4096
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 200
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	w := &Writer{app: app, queue: make(chan domain.UsageEvent, opts.QueueSize), stop: make(chan struct{}), done: make(chan struct{}), opts: opts}
	go w.run()
	return w
}

// Submit is safe for concurrent use and performs no database I/O.
func (w *Writer) Submit(e domain.UsageEvent) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
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

// Dropped returns the cumulative number of events rejected because the queue
// was full or the writer had already been closed.
func (w *Writer) Dropped() uint64 { return w.drop.Load() }

// Close is idempotent. It waits until the worker has attempted to persist all
// events accepted before Close was called.
func (w *Writer) Close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.stop)
	}
	w.mu.Unlock()
	<-w.done
}

func (w *Writer) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()
	batch := make([]domain.UsageEvent, 0, w.opts.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), w.opts.Timeout)
		err := w.app.AppendUsage(ctx, batch)
		cancel()
		if err != nil {
			w.drop.Add(uint64(len(batch)))
			if w.opts.OnError != nil {
				w.opts.OnError(err)
			}
		}
		batch = batch[:0]
	}
	for {
		select {
		case e := <-w.queue:
			batch = append(batch, e)
			if len(batch) >= w.opts.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-w.stop:
			for {
				select {
				case e := <-w.queue:
					batch = append(batch, e)
					if len(batch) >= w.opts.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}
