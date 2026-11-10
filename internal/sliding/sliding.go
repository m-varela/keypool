// Package sliding implementa janelas deslizantes com soma incremental.
package sliding

import (
	"sort"
	"time"
)

// Ref identifica uma reserva dentro de uma janela.
type Ref uint64

// Entry é uma observação de uso.
type Entry struct {
	At time.Time `json:"at"`
	N  int       `json:"n"`
}
type item struct {
	Entry
	seq uint64
}

// Window mantém entradas em ordem de inserção. O head avança durante a purga;
// sum e count são atualizados incrementalmente para evitar somas no caminho quente.
type Window struct {
	span  time.Duration
	items []item
	head  int
	next  uint64
	sum   int
}

// New cria uma janela com duração positiva (ou 60s se span não for positivo).
func New(span time.Duration) *Window {
	if span <= 0 {
		span = 60 * time.Second
	}
	return &Window{span: span}
}
func (w *Window) purge(now time.Time) {
	cut := now.Add(-w.span)
	for w.head < len(w.items) && !w.items[w.head].At.After(cut) {
		w.sum -= w.items[w.head].N
		w.head++
	}
	if w.head > 256 && w.head*2 >= len(w.items) {
		w.items = append([]item(nil), w.items[w.head:]...)
		w.head = 0
	}
}

// Add registra n e devolve uma referência ajustável.
func (w *Window) Add(now time.Time, n int) Ref {
	w.purge(now)
	w.next++
	w.items = append(w.items, item{Entry: Entry{At: now, N: n}, seq: w.next})
	w.sum += n
	return Ref(w.next)
}

// Adjust substitui o valor da entrada referenciada; se ela expirou, adiciona n em now.
func (w *Window) Adjust(now time.Time, ref Ref, n int) {
	w.purge(now)
	if ref == 0 {
		w.Add(now, n)
		return
	}
	for i := w.head; i < len(w.items); i++ {
		if w.items[i].seq == uint64(ref) {
			w.sum += n - w.items[i].N
			if n == 0 {
				copy(w.items[i:], w.items[i+1:])
				w.items = w.items[:len(w.items)-1]
				return
			}
			w.items[i].N = n
			return
		}
	}
	if n != 0 {
		w.Add(now, n)
	}
}

// Used retorna a soma não negativa das entradas válidas.
func (w *Window) Used(now time.Time) int {
	w.purge(now)
	if w.sum < 0 {
		return 0
	}
	return w.sum
}

// Count retorna o número de entradas válidas.
func (w *Window) Count(now time.Time) int { w.purge(now); return len(w.items) - w.head }

// WaitForRoom calcula a espera até used+need <= limit.
func (w *Window) WaitForRoom(now time.Time, need, limit int) time.Duration {
	if limit <= 0 || need >= limit {
		return 0
	}
	if need < 0 {
		need = 0
	}
	w.purge(now)
	excess := w.sum + need - limit
	if excess <= 0 {
		return 0
	}
	freed := 0
	for i := w.head; i < len(w.items); i++ {
		freed += w.items[i].N
		if freed >= excess {
			d := w.items[i].At.Add(w.span).Sub(now)
			if d < 0 {
				return 0
			}
			return d
		}
	}
	return w.span
}

// WaitForSlots conta entradas, não a soma de seus valores.
func (w *Window) WaitForSlots(now time.Time, limit int) time.Duration {
	if limit <= 0 {
		return 0
	}
	w.purge(now)
	count := len(w.items) - w.head
	if count < limit {
		return 0
	}
	index := count - limit
	at := w.items[w.head+index].At
	d := at.Add(w.span).Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// Entries devolve uma cópia das entradas ainda válidas.
func (w *Window) Entries(now time.Time) []Entry {
	w.purge(now)
	out := make([]Entry, 0, len(w.items)-w.head)
	for _, it := range w.items[w.head:] {
		out = append(out, it.Entry)
	}
	return out
}

// Absorb acrescenta entradas de outra janela preservando os timestamps.
func (w *Window) Absorb(now time.Time, entries []Entry) {
	for _, e := range entries {
		w.next++
		w.items = append(w.items, item{Entry: e, seq: w.next})
		w.sum += e.N
	}
	sort.SliceStable(w.items[w.head:], func(i, j int) bool { return w.items[w.head+i].At.Before(w.items[w.head+j].At) })
	w.purge(now)
}
