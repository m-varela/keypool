// Package clock fornece relógios injetáveis para lógica temporal determinística.
package clock

import (
	"context"
	"sync"
	"time"
)

// Clock abstrai a leitura e a espera de tempo.
type Clock interface {
	Now() time.Time
	Wait(context.Context, time.Duration, <-chan struct{}) error
}

// Timer representa um disparador cancelável do relógio injetado.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// TimerClock é implementado pelos relógios que permitem watchdogs determinísticos.
type TimerClock interface {
	Clock
	NewTimer(time.Duration) Timer
}

type realClock struct{}

// Real retorna o relógio de parede.
func Real() Clock                                { return realClock{} }
func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }
func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) Wait(ctx context.Context, d time.Duration, wake <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d < 0 {
		d = 0
	}
	if wake != nil {
		select {
		case <-wake:
			return nil
		default:
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	case <-t.C:
		return nil
	}
}

// Fake é um relógio determinístico. Wait avança o tempo sem bloquear.
type Fake struct {
	mu         sync.Mutex
	now, start time.Time
	timers     map[*fakeTimer]struct{}
}

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop() bool          { return t.timer.Stop() }

type fakeTimer struct {
	clock    *Fake
	deadline time.Time
	ch       chan time.Time
	stopped  bool
	fired    bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	delete(t.clock.timers, t)
	return true
}

// NewFake cria um relógio falso no instante inicial informado.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start, start: start, timers: map[*fakeTimer]struct{}{}}
}
func (f *Fake) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }

// Advance avança o relógio falso.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	for timer := range f.timers {
		if !timer.stopped && !timer.fired && !timer.deadline.After(f.now) {
			timer.fired = true
			delete(f.timers, timer)
			timer.ch <- f.now
		}
	}
	f.mu.Unlock()
}

// NewTimer cria um timer que só dispara quando Advance alcança seu prazo.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clock: f, deadline: f.now.Add(maxDuration(d, 0)), ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.fired = true
		t.ch <- f.now
	} else {
		f.timers[t] = struct{}{}
	}
	return t
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// Elapsed devolve o tempo avançado desde a criação.
func (f *Fake) Elapsed() time.Duration { f.mu.Lock(); defer f.mu.Unlock(); return f.now.Sub(f.start) }

// Wait aplica a semântica não bloqueante do relógio falso.
func (f *Fake) Wait(ctx context.Context, d time.Duration, wake <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if wake != nil {
		select {
		case <-wake:
			return nil
		default:
		}
	}
	if d > 0 {
		f.Advance(d)
	}
	return nil
}
