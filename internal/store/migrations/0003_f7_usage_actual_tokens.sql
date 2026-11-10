-- Track reported total usage explicitly; leases do not know the prompt/completion split.
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS actual_tokens integer;
ALTER TABLE usage_daily ADD COLUMN IF NOT EXISTS total_tokens bigint NOT NULL DEFAULT 0;

-- Preserve totals for rows rolled up before this additive column existed.
UPDATE usage_daily SET total_tokens = prompt_tokens + completion_tokens
WHERE total_tokens = 0 AND prompt_tokens + completion_tokens > 0;
