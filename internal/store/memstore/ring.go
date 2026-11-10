package memstore

import "time"

type ring[T any] struct {
	items []T
	head  int
	count int
	limit int
	at    func(T) time.Time
}

func newRing[T any](limit int, at func(T) time.Time) ring[T] {
	return ring[T]{items: make([]T, limit), limit: limit, at: at}
}

func (r *ring[T]) push(value T, now time.Time, retention time.Duration) (evicted int) {
	if r.limit == 0 {
		return 0
	}
	cutoff := now.Add(-retention)
	for r.count > 0 {
		oldest := r.items[r.head]
		stamp := r.at(oldest)
		if stamp.IsZero() || !stamp.Before(cutoff) {
			break
		}
		r.removeOldest()
		evicted++
	}
	if r.count == r.limit {
		r.removeOldest()
		evicted++
	}
	index := (r.head + r.count) % r.limit
	r.items[index] = value
	r.count++
	return evicted
}

func (r *ring[T]) removeOldest() {
	if r.count == 0 {
		return
	}
	var zero T
	r.items[r.head] = zero
	r.head = (r.head + 1) % r.limit
	r.count--
}

func (r *ring[T]) values() []T {
	out := make([]T, r.count)
	for i := 0; i < r.count; i++ {
		out[i] = r.items[(r.head+i)%r.limit]
	}
	return out
}
