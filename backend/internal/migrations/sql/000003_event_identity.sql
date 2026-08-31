CREATE TABLE IF NOT EXISTS event_logs (
    id VARCHAR(32) PRIMARY KEY,
    api_credential_id VARCHAR(32) NULL,
    request_id VARCHAR(191) NOT NULL DEFAULT '',
    request_fingerprint VARCHAR(64) NOT NULL DEFAULT '',
    response_format VARCHAR(16) NOT NULL DEFAULT '',
    mime_type VARCHAR(100) NOT NULL DEFAULT '',
    ts TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    kind VARCHAR(32) NOT NULL,
    status VARCHAR(32) NOT NULL,
    model VARCHAR(255) NOT NULL DEFAULT '',
    provider VARCHAR(100) NOT NULL DEFAULT '',
    prompt TEXT NOT NULL DEFAULT '',
    ratio VARCHAR(32) NOT NULL DEFAULT '',
    resolution VARCHAR(32) NOT NULL DEFAULT '',
    duration VARCHAR(32) NOT NULL DEFAULT '',
    refs INTEGER NOT NULL DEFAULT 0,
    de_ai BOOLEAN NOT NULL DEFAULT FALSE,
    ref_files JSONB NULL,
    source VARCHAR(32) NOT NULL DEFAULT '',
    account_id VARCHAR(64) NOT NULL DEFAULT '',
    account_email VARCHAR(255) NOT NULL DEFAULT '',
    user_id VARCHAR(32) NOT NULL DEFAULT '',
    cost DOUBLE PRECISION NOT NULL DEFAULT 0,
    refunded BOOLEAN NOT NULL DEFAULT FALSE,
    elapsed_ms INTEGER NOT NULL DEFAULT 0,
    file VARCHAR(500) NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE event_logs
    ADD COLUMN IF NOT EXISTS api_credential_id VARCHAR(32),
    ADD COLUMN IF NOT EXISTS request_fingerprint VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS response_format VARCHAR(16) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS mime_type VARCHAR(100) NOT NULL DEFAULT '';

DO $migration$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'event_logs_response_format_valid'
          AND conrelid = 'event_logs'::regclass
    ) THEN
        ALTER TABLE event_logs
            ADD CONSTRAINT event_logs_response_format_valid
            CHECK (response_format IN ('', 'url', 'b64_json'));
    END IF;
END
$migration$;

CREATE INDEX IF NOT EXISTS idx_event_logs_api_credential_id
    ON event_logs (api_credential_id);

-- Carry historical admin-key attribution forward when the legacy user id is
-- still available. New writes never use user_id.
DO $migration$
BEGIN
    IF to_regclass('public.api_keys') IS NOT NULL THEN
        EXECUTE $sql$
            WITH sole_legacy_key AS (
                SELECT k.user_id, MIN(k.id) AS credential_id
                FROM api_keys k
                JOIN api_credentials c ON c.id = k.id
                GROUP BY k.user_id
                HAVING COUNT(*) = 1
            )
            UPDATE event_logs e
            SET api_credential_id = k.credential_id
            FROM sole_legacy_key k
            WHERE e.api_credential_id IS NULL
              AND e.user_id = k.user_id
        $sql$;
    END IF;
END
$migration$;

DROP INDEX IF EXISTS uniq_event_v1_image_request;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_event_api_request
    ON event_logs (api_credential_id, kind, request_id)
    WHERE api_credential_id IS NOT NULL AND request_id <> '';
