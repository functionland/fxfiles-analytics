-- fxfiles-analytics initial schema
--
-- Storage model:
-- - `analytics_cids` holds one row per IPFS CID we've ever recorded a
--   pageview for, with a monotonically-increasing `pageviews` counter.
-- - `analytics_visitors` is a (cid, day, visitor_hash) tuple table that
--   dedupes repeat visits inside one day. The natural compound PK doubles
--   as the dedupe constraint via `ON CONFLICT DO NOTHING`.
-- - `analytics_salt` stores the daily-rotating salt used to compute
--   `visitor_hash = sha256(salt || ip || ua)`. Moving the salt into the DB
--   (rather than a `.salt` file on disk) means backups capture it
--   alongside the data — without the salt, today's visitor uniqueness
--   continuity is lost.
--
-- All `IF NOT EXISTS` so the migration is safely re-runnable.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version INT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS analytics_cids (
    cid TEXT PRIMARY KEY,
    pageviews BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS analytics_visitors (
    cid TEXT NOT NULL,
    day DATE NOT NULL,
    visitor_hash TEXT NOT NULL,
    PRIMARY KEY (cid, day, visitor_hash)
);

-- Secondary index on `day` alone — needed for the daily-pruning DELETE,
-- which can't use the compound PK (it has `cid` as leading column).
CREATE INDEX IF NOT EXISTS idx_analytics_visitors_day
    ON analytics_visitors(day);

CREATE TABLE IF NOT EXISTS analytics_salt (
    day DATE PRIMARY KEY,
    salt BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations(version) VALUES (1) ON CONFLICT DO NOTHING;
