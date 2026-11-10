CREATE TABLE app_settings (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  api_key_hash bytea NOT NULL,
  api_key_prefix text NOT NULL,
  api_key_created_at timestamptz NOT NULL DEFAULT now(),
  api_key_rotated_at timestamptz,
  master_key_check bytea NOT NULL,
  config_version bigint NOT NULL DEFAULT 1
);
CREATE TABLE providers (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9_-]*$'),
  type text NOT NULL, enabled boolean NOT NULL DEFAULT true, spec jsonb NOT NULL,
  limit_scope text NOT NULL DEFAULT 'key' CHECK (limit_scope IN ('key','group','provider','auto')),
  key_strategy text NOT NULL DEFAULT 'round_robin' CHECK (key_strategy IN ('round_robin','least_used','headroom','latency')),
  quota_cooldown_s integer NOT NULL DEFAULT 3600 CHECK (quota_cooldown_s >= 0),
  limits jsonb NOT NULL DEFAULT '{}', warm jsonb NOT NULL DEFAULT '{}', learned jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE keys (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  label text NOT NULL, secret_enc bytea NOT NULL, secret_hash bytea NOT NULL, mask text NOT NULL,
  account_group text, tags text[] NOT NULL DEFAULT '{}', enabled boolean NOT NULL DEFAULT true,
  status text NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown','healthy','degraded','quarantined','invalid')),
  status_reason text, status_at timestamptz, last_ok_at timestamptz, last_error_at timestamptz, last_used_at timestamptz,
  meta jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(provider_id,label), UNIQUE(provider_id,secret_hash)
);
CREATE INDEX keys_provider_idx ON keys(provider_id) WHERE enabled;
CREATE TABLE projects (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9_-]*$'),
 description text NOT NULL DEFAULT '', enabled boolean NOT NULL DEFAULT true, allow_lease boolean NOT NULL DEFAULT false,
 allow_literal_targets boolean NOT NULL DEFAULT false, policy jsonb NOT NULL DEFAULT '{}',
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE project_key_rules (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
 mode text NOT NULL CHECK (mode IN ('include','exclude')), selector jsonb NOT NULL, position integer NOT NULL DEFAULT 0
);
CREATE INDEX project_key_rules_idx ON project_key_rules(project_id,provider_id);
CREATE TABLE routes (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 alias text NOT NULL, priority integer NOT NULL DEFAULT 5 CHECK (priority BETWEEN 0 AND 9),
 chain jsonb NOT NULL, defaults jsonb NOT NULL DEFAULT '{}', requires jsonb NOT NULL DEFAULT '{}', UNIQUE(project_id,alias)
);
CREATE TABLE limit_group_state (
 provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE, group_key text NOT NULL,
 state jsonb NOT NULL, saved_at timestamptz NOT NULL, PRIMARY KEY(provider_id,group_key)
);
CREATE TABLE model_catalog (
 key_id uuid NOT NULL REFERENCES keys(id) ON DELETE CASCADE, model_id text NOT NULL, seen_at timestamptz NOT NULL,
 PRIMARY KEY(key_id,model_id)
);
CREATE TABLE health_checks (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, key_id uuid NOT NULL REFERENCES keys(id) ON DELETE CASCADE,
 probe text NOT NULL, ok boolean NOT NULL, http_status integer, latency_ms integer, error text, at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX health_checks_key_at ON health_checks(key_id,at DESC);
CREATE TABLE usage_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, ts timestamptz NOT NULL, request_id text NOT NULL,
 project text NOT NULL, alias text NOT NULL DEFAULT '', provider text NOT NULL, model text NOT NULL, key_id uuid,
 source text NOT NULL DEFAULT 'request' CHECK (source IN ('request','warmup','lease','passthrough')),
 outcome text NOT NULL, http_status integer, attempt integer NOT NULL DEFAULT 1, queue_ms integer NOT NULL DEFAULT 0,
 ttfb_ms integer, total_ms integer, prompt_tokens integer, completion_tokens integer, est_tokens integer,
 stream boolean NOT NULL DEFAULT false, error_class text
);
CREATE INDEX usage_events_ts_brin ON usage_events USING brin(ts);
CREATE INDEX usage_events_key_ts ON usage_events(key_id,ts DESC);
CREATE TABLE usage_daily (
 day date NOT NULL, project text NOT NULL, provider text NOT NULL, model text NOT NULL, key_id uuid,
 requests integer NOT NULL DEFAULT 0, errors integer NOT NULL DEFAULT 0, rate_limited integer NOT NULL DEFAULT 0,
 prompt_tokens bigint NOT NULL DEFAULT 0, completion_tokens bigint NOT NULL DEFAULT 0,
 PRIMARY KEY(day,project,provider,model,key_id)
);
CREATE TABLE events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, ts timestamptz NOT NULL DEFAULT now(), level text NOT NULL,
 kind text NOT NULL, subject text NOT NULL DEFAULT '', detail jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX events_ts ON events(ts DESC);
