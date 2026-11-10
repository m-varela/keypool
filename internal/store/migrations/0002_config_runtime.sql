-- Additive configuration fields introduced after the original F4 schema.
-- Keep 0001 immutable: databases may already have recorded its checksum.
ALTER TABLE app_settings
    ADD COLUMN runtime_config jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE routes
    ADD COLUMN force jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Retention prunes the whole table by timestamp, not one key at a time.
CREATE INDEX health_checks_at_brin ON health_checks USING brin(at);
