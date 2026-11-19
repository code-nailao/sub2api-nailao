SET LOCAL lock_timeout = '1s';
SET LOCAL statement_timeout = '5s';

-- Deleted usage contributes to admin history, never to billing. No request bodies or raw-row foreign keys.

CREATE TABLE usage_cleanup_user_hourly (
 bucket_start timestamptz NOT NULL, sub_bucket_start timestamptz NOT NULL, user_id bigint NOT NULL,
 requests bigint NOT NULL DEFAULT 0,
 input_tokens bigint NOT NULL DEFAULT 0,
 output_tokens bigint NOT NULL DEFAULT 0,
 cache_creation_tokens bigint NOT NULL DEFAULT 0,
 cache_read_tokens bigint NOT NULL DEFAULT 0,
 total_cost numeric NOT NULL DEFAULT 0,
 actual_cost numeric NOT NULL DEFAULT 0,
 account_cost numeric NOT NULL DEFAULT 0,
 duration_ms bigint NOT NULL DEFAULT 0,
 PRIMARY KEY (bucket_start,sub_bucket_start,user_id)
);

CREATE TABLE usage_cleanup_model_hourly (
 bucket_start timestamptz NOT NULL, sub_bucket_start timestamptz NOT NULL, model_source text NOT NULL CHECK (model_source IN ('requested','upstream','mapping')), model_name text NOT NULL,
 requests bigint NOT NULL DEFAULT 0,
 input_tokens bigint NOT NULL DEFAULT 0,
 output_tokens bigint NOT NULL DEFAULT 0,
 cache_creation_tokens bigint NOT NULL DEFAULT 0,
 cache_read_tokens bigint NOT NULL DEFAULT 0,
 total_cost numeric NOT NULL DEFAULT 0,
 actual_cost numeric NOT NULL DEFAULT 0,
 account_cost numeric NOT NULL DEFAULT 0,
 duration_ms bigint NOT NULL DEFAULT 0,
 PRIMARY KEY (bucket_start,sub_bucket_start,model_source,model_name)
);

CREATE TABLE usage_cleanup_group_daily (
 bucket_date date NOT NULL, group_id bigint NOT NULL,
 actual_cost numeric NOT NULL DEFAULT 0,
 PRIMARY KEY (bucket_date,group_id)
);

-- Each deleted row moves atomically from the raw branch to the summary branch.
-- Read these views in one SQL snapshot. Do not add them to already-complete rollups.
CREATE VIEW usage_cleanup_admin_source AS
 SELECT created_at,user_id,1::bigint AS requests,
 input_tokens::bigint,output_tokens::bigint,cache_creation_tokens::bigint,cache_read_tokens::bigint,
 total_cost,actual_cost,
 COALESCE(account_stats_cost,total_cost)*COALESCE(account_rate_multiplier,1) AS account_cost,
 COALESCE(duration_ms,0)::bigint AS duration_ms
 FROM usage_logs
 UNION ALL
 SELECT sub_bucket_start,user_id,requests,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,
 total_cost,actual_cost,account_cost,duration_ms FROM usage_cleanup_user_hourly;

CREATE VIEW usage_cleanup_group_source AS
 SELECT created_at,group_id,actual_cost FROM usage_logs WHERE group_id IS NOT NULL
 UNION ALL
 SELECT bucket_date::timestamp AT TIME ZONE 'Asia/Shanghai',group_id,actual_cost
 FROM usage_cleanup_group_daily;

CREATE VIEW usage_cleanup_model_source AS
 SELECT u.created_at,m.model_source,m.model_name,1::bigint AS requests,
 u.input_tokens::bigint,u.output_tokens::bigint,u.cache_creation_tokens::bigint,u.cache_read_tokens::bigint,
 u.total_cost,u.actual_cost,
 COALESCE(u.account_stats_cost,u.total_cost)*COALESCE(u.account_rate_multiplier,1) AS account_cost,
 COALESCE(u.duration_ms,0)::bigint AS duration_ms
 FROM usage_logs u CROSS JOIN LATERAL (VALUES
 ('requested',COALESCE(NULLIF(TRIM(u.requested_model),''),u.model)),
 ('upstream',COALESCE(NULLIF(TRIM(u.upstream_model),''),u.model)),
 ('mapping',COALESCE(NULLIF(TRIM(u.requested_model),''),u.model)||' -> '||COALESCE(NULLIF(TRIM(u.upstream_model),''),u.model))
 ) AS m(model_source,model_name)
 UNION ALL
 SELECT sub_bucket_start,model_source,model_name,requests,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,
 total_cost,actual_cost,account_cost,duration_ms FROM usage_cleanup_model_hourly;

-- Quarter-hour sub-buckets preserve the date boundaries of supported half-hour
-- and quarter-hour IANA offsets. The operator checks every timezone transition
-- in the candidate date range before admission. Public charts remain hourly/day.
CREATE INDEX usage_cleanup_user_sub_bucket_idx ON usage_cleanup_user_hourly(sub_bucket_start);
CREATE INDEX usage_cleanup_model_sub_bucket_idx ON usage_cleanup_model_hourly(sub_bucket_start,model_source);
