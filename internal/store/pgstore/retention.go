package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention applies the documented raw-event retention windows. Old usage
// rows are first rolled into usage_daily in the same transaction.
type Retention struct {
	UsageEvents  time.Duration
	HealthChecks time.Duration
	Events       time.Duration
}

func (r Retention) normalized() Retention {
	if r.UsageEvents <= 0 {
		r.UsageEvents = 30 * 24 * time.Hour
	}
	if r.HealthChecks <= 0 {
		r.HealthChecks = 7 * 24 * time.Hour
	}
	if r.Events <= 0 {
		r.Events = 90 * 24 * time.Hour
	}
	return r
}

// PrunePool is the pgxpool-native retention job. Rollup and raw deletion share
// one transaction so retries cannot lose or double-count a partially rolled-up day.
func PrunePool(ctx context.Context, pool *pgxpool.Pool, now time.Time, retention Retention) (map[string]int64, error) {
	if pool == nil {
		return nil, fmt.Errorf("pgstore: pool is nil")
	}
	r := retention.normalized()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	counts := map[string]int64{}
	cut := now.Add(-r.UsageEvents)
	if _, err = tx.Exec(ctx, `INSERT INTO usage_daily(day,project,provider,model,key_id,requests,errors,rate_limited,prompt_tokens,completion_tokens,total_tokens)
SELECT (ts AT TIME ZONE 'UTC')::date,project,provider,model,
COALESCE(key_id,'00000000-0000-0000-0000-000000000000'::uuid),count(*)::integer,
count(*) FILTER(WHERE outcome<>'success')::integer,
count(*) FILTER(WHERE outcome IN('rate_limited_short','rate_limited_long','quota_exhausted'))::integer,
COALESCE(sum(prompt_tokens),0),COALESCE(sum(completion_tokens),0),
COALESCE(sum(CASE WHEN COALESCE(actual_tokens,0)>0 THEN actual_tokens WHEN COALESCE(prompt_tokens,0)+COALESCE(completion_tokens,0)>0 THEN COALESCE(prompt_tokens,0)+COALESCE(completion_tokens,0) ELSE COALESCE(est_tokens,0) END),0)
FROM usage_events WHERE ts<$1 GROUP BY (ts AT TIME ZONE 'UTC')::date,project,provider,model,COALESCE(key_id,'00000000-0000-0000-0000-000000000000'::uuid)
ON CONFLICT(day,project,provider,model,key_id) DO UPDATE SET requests=usage_daily.requests+EXCLUDED.requests,errors=usage_daily.errors+EXCLUDED.errors,rate_limited=usage_daily.rate_limited+EXCLUDED.rate_limited,prompt_tokens=usage_daily.prompt_tokens+EXCLUDED.prompt_tokens,completion_tokens=usage_daily.completion_tokens+EXCLUDED.completion_tokens,total_tokens=usage_daily.total_tokens+EXCLUDED.total_tokens`, cut); err != nil {
		return nil, fmt.Errorf("pgstore: roll up usage: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM usage_events WHERE ts<$1`, cut)
	if err != nil {
		return nil, err
	}
	counts["usage_events"] = tag.RowsAffected()
	for _, item := range []struct {
		name, query string
		cut         time.Time
	}{{"health_checks", `DELETE FROM health_checks WHERE at<$1`, now.Add(-r.HealthChecks)}, {"events", `DELETE FROM events WHERE ts<$1`, now.Add(-r.Events)}} {
		tag, err = tx.Exec(ctx, item.query, item.cut)
		if err != nil {
			return nil, fmt.Errorf("pgstore: prune %s: %w", item.name, err)
		}
		counts[item.name] = tag.RowsAffected()
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return counts, nil
}
