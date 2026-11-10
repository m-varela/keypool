package router

import (
	"fmt"
	"sync"
	"time"
)

type projectBudgetState struct {
	day      string
	used     int
	reserved int
}

type projectBudgetReservation struct {
	router  *Router
	project string
	day     string
	amount  int
	once    sync.Once
}

// ErrDailyBudgetExceeded indica que o orçamento diário do projeto não comporta a reserva.
type ErrDailyBudgetExceeded struct {
	Budget, Used, Reserved, Requested int
	ResetAt                           time.Time
}

func (e *ErrDailyBudgetExceeded) Error() string {
	return fmt.Sprintf("router: orçamento diário do projeto esgotado (usado=%d reservado=%d limite=%d solicitado=%d)", e.Used, e.Reserved, e.Budget, e.Requested)
}

func utcDay(now time.Time) string { return now.UTC().Format("2006-01-02") }
func nextUTCDay(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

func (r *Router) reserveDailyBudget(project string, budget, amount int) (*projectBudgetReservation, error) {
	if budget <= 0 {
		return &projectBudgetReservation{}, nil
	}
	if amount < 0 {
		amount = 0
	}
	now := r.clock.Now().UTC()
	day := utcDay(now)
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	state := r.projectBudget[project]
	if state == nil || state.day != day {
		state = &projectBudgetState{day: day}
		r.projectBudget[project] = state
	}
	if state.used+state.reserved+amount > budget {
		return nil, &ErrDailyBudgetExceeded{
			Budget: budget, Used: state.used, Reserved: state.reserved,
			Requested: amount, ResetAt: nextUTCDay(now),
		}
	}
	state.reserved += amount
	return &projectBudgetReservation{router: r, project: project, day: day, amount: amount}, nil
}

// settle troca a reserva por consumo real. actual=0 libera integralmente a reserva.
func (b *projectBudgetReservation) settle(actual int) {
	if b == nil || b.router == nil {
		return
	}
	b.once.Do(func() {
		r := b.router
		r.budgetMu.Lock()
		defer r.budgetMu.Unlock()
		state := r.projectBudget[b.project]
		// Uma chamada iniciada ontem não pode contaminar o orçamento do novo dia.
		if state == nil || state.day != b.day {
			return
		}
		state.reserved -= b.amount
		if state.reserved < 0 {
			state.reserved = 0
		}
		if actual > 0 {
			state.used += actual
		}
	})
}

// SetProjectDailyUsage restaura os totais do dia UTC carregados do store na inicialização.
func (r *Router) SetProjectDailyUsage(totals map[string]int) {
	if r == nil {
		return
	}
	nowDay := utcDay(r.clock.Now())
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	for project, total := range totals {
		if total < 0 {
			total = 0
		}
		r.projectBudget[project] = &projectBudgetState{day: nowDay, used: total}
	}
}

// ProjectDailyUsage devolve um snapshot seguro dos contadores para diagnóstico e testes.
func (r *Router) ProjectDailyUsage(project string) (used, reserved int) {
	if r == nil {
		return 0, 0
	}
	nowDay := utcDay(r.clock.Now())
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	state := r.projectBudget[project]
	if state == nil || state.day != nowDay {
		return 0, 0
	}
	return state.used, state.reserved
}
