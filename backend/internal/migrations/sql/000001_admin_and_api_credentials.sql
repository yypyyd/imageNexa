CREATE TABLE IF NOT EXISTS admins (
    id VARCHAR(32) PRIMARY KEY,
    singleton_key SMALLINT NOT NULL DEFAULT 1,
    username VARCHAR(64) NOT NULL,
    email VARCHAR(255) NOT NULL DEFAULT '',
    password_hash VARCHAR(255) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    session_version BIGINT NOT NULL DEFAULT 1,
    last_login_at TIMESTAMPTZ NULL,
    last_login_ip VARCHAR(128) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT admins_singleton_value CHECK (singleton_key = 1),
    CONSTRAINT admins_singleton_unique UNIQUE (singleton_key),
    CONSTRAINT admins_username_unique UNIQUE (username),
    CONSTRAINT admins_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT admins_session_version_positive CHECK (session_version >= 1)
);

CREATE TABLE IF NOT EXISTS api_credentials (
    id VARCHAR(32) PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    key_preview VARCHAR(32) NOT NULL,
    key_hash VARCHAR(255) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    concurrency_limit INTEGER NOT NULL DEFAULT 0,
    last_used_at TIMESTAMPTZ NULL,
    revoked_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT api_credentials_key_hash_unique UNIQUE (key_hash),
    CONSTRAINT api_credentials_name_nonempty CHECK (LENGTH(BTRIM(name)) > 0),
    CONSTRAINT api_credentials_status_valid CHECK (status IN ('active', 'disabled', 'revoked')),
    CONSTRAINT api_credentials_concurrency_nonnegative CHECK (concurrency_limit >= 0),
    CONSTRAINT api_credentials_revocation_consistent CHECK (
        (status IN ('active', 'disabled') AND revoked_at IS NULL)
        OR (status = 'revoked' AND revoked_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_api_credentials_status ON api_credentials (status);

-- Existing installations may still have the old users/api_keys tables. Use
-- dynamic SQL so this same forward migration also works against a fresh DB
-- where neither legacy relation exists.
DO $migration$
BEGIN
    IF to_regclass('public.users') IS NOT NULL THEN
        EXECUTE $sql$
            INSERT INTO admins (
                id, singleton_key, username, email, password_hash, status,
                session_version, last_login_at, last_login_ip, created_at, updated_at
            )
            SELECT
                'admin',
                1,
                CASE
                    WHEN BTRIM(COALESCE(u.name, '')) ~ '^[A-Za-z0-9]{1,24}$'
                    THEN BTRIM(u.name)
                    ELSE 'admin'
                END,
                LOWER(COALESCE(u.email, '')),
                u.password_hash,
                CASE WHEN u.status = 'active' THEN 'active' ELSE 'disabled' END,
                1,
                u.last_login_at,
                COALESCE(u.last_login_ip, ''),
                COALESCE(u.created_at, NOW()),
                COALESCE(u.updated_at, u.created_at, NOW())
            FROM users u
            WHERE u.role = 'admin'
              AND u.status = 'active'
              AND (
                  u.password_hash ~ $regex$^bcrypt\$\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$$regex$
                  OR u.password_hash ~ $regex$^sha256\$[^$]+\$[0-9a-fA-F]{64}$$regex$
              )
            ORDER BY u.created_at NULLS LAST,
                     u.id
            LIMIT 1
            ON CONFLICT (singleton_key) DO NOTHING
        $sql$;
    END IF;
END
$migration$;

DO $migration$
BEGIN
    IF to_regclass('public.users') IS NOT NULL
       AND to_regclass('public.api_keys') IS NOT NULL THEN
        EXECUTE $sql$
            INSERT INTO api_credentials (
                id, name, key_preview, key_hash, status, concurrency_limit,
                last_used_at, revoked_at, created_at, updated_at
            )
            SELECT
                k.id,
                COALESCE(NULLIF(BTRIM(k.name), ''), 'migrated-admin-key'),
                COALESCE(k.key_preview, ''),
                k.key_hash,
                key_state.status,
                0,
                k.last_used_at,
                CASE WHEN key_state.status = 'revoked'
                     THEN COALESCE(
                         NULLIF(to_jsonb(k)->>'revoked_at', '')::timestamptz,
                         k.created_at,
                         NOW()
                     )
                     ELSE NULL
                END,
                COALESCE(k.created_at, NOW()),
                COALESCE(k.created_at, NOW())
            FROM api_keys k
            JOIN (
                SELECT id
                FROM users
                WHERE role = 'admin'
                  AND status = 'active'
                  AND (
                      password_hash ~ $regex$^bcrypt\$\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$$regex$
                      OR password_hash ~ $regex$^sha256\$[^$]+\$[0-9a-fA-F]{64}$$regex$
                  )
                ORDER BY created_at NULLS LAST,
                         id
                LIMIT 1
            ) legacy_admin ON legacy_admin.id = k.user_id
            CROSS JOIN LATERAL (
                SELECT CASE COALESCE(
                    NULLIF(LOWER(BTRIM(to_jsonb(k)->>'status')), ''),
                    'active'
                )
                    WHEN 'active' THEN 'active'
                    WHEN 'disabled' THEN 'disabled'
                    WHEN 'revoked' THEN 'revoked'
                    ELSE 'disabled'
                END AS status
            ) key_state
            WHERE NULLIF(BTRIM(COALESCE(k.key_hash, '')), '') IS NOT NULL
            ON CONFLICT (key_hash) DO NOTHING
        $sql$;
    END IF;
END
$migration$;
