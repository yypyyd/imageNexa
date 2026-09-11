package migrations

// migrationSources contains immutable, forward-only database migrations.
// Append new versions; editing an applied migration prevents existing databases from starting.
// String escapes preserve the original SQL bytes and checksums on every platform.
var migrationSources = map[string]string{
	"000001_admin_and_api_credentials.sql": "CREATE TABLE IF NOT EXISTS admins (\n" +
		"    id VARCHAR(32) PRIMARY KEY,\n" +
		"    singleton_key SMALLINT NOT NULL DEFAULT 1,\n" +
		"    username VARCHAR(64) NOT NULL,\n" +
		"    email VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    password_hash VARCHAR(255) NOT NULL,\n" +
		"    status VARCHAR(32) NOT NULL DEFAULT 'active',\n" +
		"    session_version BIGINT NOT NULL DEFAULT 1,\n" +
		"    last_login_at TIMESTAMPTZ NULL,\n" +
		"    last_login_ip VARCHAR(128) NOT NULL DEFAULT '',\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    CONSTRAINT admins_singleton_value CHECK (singleton_key = 1),\n" +
		"    CONSTRAINT admins_singleton_unique UNIQUE (singleton_key),\n" +
		"    CONSTRAINT admins_username_unique UNIQUE (username),\n" +
		"    CONSTRAINT admins_status_valid CHECK (status IN ('active', 'disabled')),\n" +
		"    CONSTRAINT admins_session_version_positive CHECK (session_version >= 1)\n" +
		");\n" +
		"\n" +
		"CREATE TABLE IF NOT EXISTS api_credentials (\n" +
		"    id VARCHAR(32) PRIMARY KEY,\n" +
		"    name VARCHAR(100) NOT NULL,\n" +
		"    key_preview VARCHAR(32) NOT NULL,\n" +
		"    key_hash VARCHAR(255) NOT NULL,\n" +
		"    status VARCHAR(32) NOT NULL DEFAULT 'active',\n" +
		"    concurrency_limit INTEGER NOT NULL DEFAULT 0,\n" +
		"    last_used_at TIMESTAMPTZ NULL,\n" +
		"    revoked_at TIMESTAMPTZ NULL,\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    CONSTRAINT api_credentials_key_hash_unique UNIQUE (key_hash),\n" +
		"    CONSTRAINT api_credentials_name_nonempty CHECK (LENGTH(BTRIM(name)) > 0),\n" +
		"    CONSTRAINT api_credentials_status_valid CHECK (status IN ('active', 'disabled', 'revoked')),\n" +
		"    CONSTRAINT api_credentials_concurrency_nonnegative CHECK (concurrency_limit >= 0),\n" +
		"    CONSTRAINT api_credentials_revocation_consistent CHECK (\n" +
		"        (status IN ('active', 'disabled') AND revoked_at IS NULL)\n" +
		"        OR (status = 'revoked' AND revoked_at IS NOT NULL)\n" +
		"    )\n" +
		");\n" +
		"\n" +
		"CREATE INDEX IF NOT EXISTS idx_api_credentials_status ON api_credentials (status);\n" +
		"\n" +
		"-- Existing installations may still have the old users/api_keys tables. Use\n" +
		"-- dynamic SQL so this same forward migration also works against a fresh DB\n" +
		"-- where neither legacy relation exists.\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF to_regclass('public.users') IS NOT NULL THEN\n" +
		"        EXECUTE $sql$\n" +
		"            INSERT INTO admins (\n" +
		"                id, singleton_key, username, email, password_hash, status,\n" +
		"                session_version, last_login_at, last_login_ip, created_at, updated_at\n" +
		"            )\n" +
		"            SELECT\n" +
		"                'admin',\n" +
		"                1,\n" +
		"                CASE\n" +
		"                    WHEN BTRIM(COALESCE(u.name, '')) ~ '^[A-Za-z0-9]{1,24}$'\n" +
		"                    THEN BTRIM(u.name)\n" +
		"                    ELSE 'admin'\n" +
		"                END,\n" +
		"                LOWER(COALESCE(u.email, '')),\n" +
		"                u.password_hash,\n" +
		"                CASE WHEN u.status = 'active' THEN 'active' ELSE 'disabled' END,\n" +
		"                1,\n" +
		"                u.last_login_at,\n" +
		"                COALESCE(u.last_login_ip, ''),\n" +
		"                COALESCE(u.created_at, NOW()),\n" +
		"                COALESCE(u.updated_at, u.created_at, NOW())\n" +
		"            FROM users u\n" +
		"            WHERE u.role = 'admin'\n" +
		"              AND u.status = 'active'\n" +
		"              AND (\n" +
		"                  u.password_hash ~ $regex$^bcrypt\\$\\$2[aby]\\$[0-9]{2}\\$[./A-Za-z0-9]{53}$$regex$\n" +
		"                  OR u.password_hash ~ $regex$^sha256\\$[^$]+\\$[0-9a-fA-F]{64}$$regex$\n" +
		"              )\n" +
		"            ORDER BY u.created_at NULLS LAST,\n" +
		"                     u.id\n" +
		"            LIMIT 1\n" +
		"            ON CONFLICT (singleton_key) DO NOTHING\n" +
		"        $sql$;\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n" +
		"\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF to_regclass('public.users') IS NOT NULL\n" +
		"       AND to_regclass('public.api_keys') IS NOT NULL THEN\n" +
		"        EXECUTE $sql$\n" +
		"            INSERT INTO api_credentials (\n" +
		"                id, name, key_preview, key_hash, status, concurrency_limit,\n" +
		"                last_used_at, revoked_at, created_at, updated_at\n" +
		"            )\n" +
		"            SELECT\n" +
		"                k.id,\n" +
		"                COALESCE(NULLIF(BTRIM(k.name), ''), 'migrated-admin-key'),\n" +
		"                COALESCE(k.key_preview, ''),\n" +
		"                k.key_hash,\n" +
		"                key_state.status,\n" +
		"                0,\n" +
		"                k.last_used_at,\n" +
		"                CASE WHEN key_state.status = 'revoked'\n" +
		"                     THEN COALESCE(\n" +
		"                         NULLIF(to_jsonb(k)->>'revoked_at', '')::timestamptz,\n" +
		"                         k.created_at,\n" +
		"                         NOW()\n" +
		"                     )\n" +
		"                     ELSE NULL\n" +
		"                END,\n" +
		"                COALESCE(k.created_at, NOW()),\n" +
		"                COALESCE(k.created_at, NOW())\n" +
		"            FROM api_keys k\n" +
		"            JOIN (\n" +
		"                SELECT id\n" +
		"                FROM users\n" +
		"                WHERE role = 'admin'\n" +
		"                  AND status = 'active'\n" +
		"                  AND (\n" +
		"                      password_hash ~ $regex$^bcrypt\\$\\$2[aby]\\$[0-9]{2}\\$[./A-Za-z0-9]{53}$$regex$\n" +
		"                      OR password_hash ~ $regex$^sha256\\$[^$]+\\$[0-9a-fA-F]{64}$$regex$\n" +
		"                  )\n" +
		"                ORDER BY created_at NULLS LAST,\n" +
		"                         id\n" +
		"                LIMIT 1\n" +
		"            ) legacy_admin ON legacy_admin.id = k.user_id\n" +
		"            CROSS JOIN LATERAL (\n" +
		"                SELECT CASE COALESCE(\n" +
		"                    NULLIF(LOWER(BTRIM(to_jsonb(k)->>'status')), ''),\n" +
		"                    'active'\n" +
		"                )\n" +
		"                    WHEN 'active' THEN 'active'\n" +
		"                    WHEN 'disabled' THEN 'disabled'\n" +
		"                    WHEN 'revoked' THEN 'revoked'\n" +
		"                    ELSE 'disabled'\n" +
		"                END AS status\n" +
		"            ) key_state\n" +
		"            WHERE NULLIF(BTRIM(COALESCE(k.key_hash, '')), '') IS NOT NULL\n" +
		"            ON CONFLICT (key_hash) DO NOTHING\n" +
		"        $sql$;\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n",
	"000002_routing.sql": "CREATE TABLE IF NOT EXISTS provider_accounts (\n" +
		"    id VARCHAR(64) PRIMARY KEY,\n" +
		"    pool VARCHAR(64) NOT NULL,\n" +
		"    value TEXT NOT NULL DEFAULT '',\n" +
		"    status VARCHAR(32) NOT NULL DEFAULT 'active',\n" +
		"    fails INTEGER NOT NULL DEFAULT 0,\n" +
		"    fail_total INTEGER NOT NULL DEFAULT 0,\n" +
		"    upstream_fails INTEGER NOT NULL DEFAULT 0,\n" +
		"    success_total INTEGER NOT NULL DEFAULT 0,\n" +
		"    dead BOOLEAN NOT NULL DEFAULT FALSE,\n" +
		"    meta JSONB NOT NULL DEFAULT '{}'::jsonb,\n" +
		"    added_at TIMESTAMPTZ NULL,\n" +
		"    last_used_at TIMESTAMPTZ NULL,\n" +
		"    cached_quota_reset_after VARCHAR(128) NOT NULL DEFAULT '',\n" +
		"    quota_recover_at TIMESTAMPTZ NULL,\n" +
		"    image_limited BOOLEAN NOT NULL DEFAULT FALSE,\n" +
		"    video_limited BOOLEAN NOT NULL DEFAULT FALSE,\n" +
		"    account_email VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    account_display_name VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    weight INTEGER NOT NULL DEFAULT 0,\n" +
		"    concurrency INTEGER NOT NULL DEFAULT 0,\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    CONSTRAINT provider_accounts_status_valid CHECK (status IN ('active','disabled','quota','pending')),\n" +
		"    CONSTRAINT provider_accounts_concurrency_nonnegative CHECK (concurrency >= 0)\n" +
		");\n" +
		"\n" +
		"CREATE INDEX IF NOT EXISTS idx_provider_accounts_pool_status ON provider_accounts (pool, status);\n" +
		"CREATE INDEX IF NOT EXISTS idx_provider_accounts_last_used ON provider_accounts (last_used_at);\n" +
		"\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF to_regclass('public.token_accounts') IS NOT NULL THEN\n" +
		"        EXECUTE $sql$\n" +
		"            INSERT INTO provider_accounts (\n" +
		"                id,pool,value,status,fails,fail_total,upstream_fails,success_total,dead,meta,\n" +
		"                added_at,last_used_at,cached_quota_reset_after,quota_recover_at,image_limited,\n" +
		"                video_limited,account_email,account_display_name,weight,concurrency,created_at,updated_at\n" +
		"            )\n" +
		"            SELECT id,pool,COALESCE(value,''),status,fails,fail_total,upstream_fails,success_total,dead,\n" +
		"                   COALESCE(meta,'{}'::jsonb),added_at,last_used_at,\n" +
		"                   COALESCE(cached_quota_reset_after,''),quota_recover_at,image_limited,\n" +
		"                   video_limited,COALESCE(account_email,''),COALESCE(account_display_name,''),\n" +
		"                   weight,concurrency,COALESCE(created_at,NOW()),\n" +
		"                   COALESCE(updated_at,created_at,NOW())\n" +
		"            FROM token_accounts\n" +
		"            WHERE pool IN ('chatgpt','byteplus','adobe','runway','grok','oreate','custom')\n" +
		"            ON CONFLICT (id) DO NOTHING\n" +
		"        $sql$;\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n" +
		"\n" +
		"CREATE TABLE IF NOT EXISTS logical_models (\n" +
		"    id VARCHAR(191) PRIMARY KEY,\n" +
		"    kind VARCHAR(32) NOT NULL,\n" +
		"    name VARCHAR(255) NOT NULL,\n" +
		"    enabled BOOLEAN NOT NULL DEFAULT TRUE,\n" +
		"    weight INTEGER NOT NULL DEFAULT 0,\n" +
		"    generation_count BIGINT NOT NULL DEFAULT 0,\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    CONSTRAINT logical_models_kind_valid CHECK (kind IN ('text','image','video'))\n" +
		");\n" +
		"\n" +
		"CREATE INDEX IF NOT EXISTS idx_logical_models_enabled_kind ON logical_models (enabled, kind);\n" +
		"\n" +
		"CREATE TABLE IF NOT EXISTS model_routes (\n" +
		"    id VARCHAR(191) PRIMARY KEY,\n" +
		"    logical_model_id VARCHAR(191) NOT NULL REFERENCES logical_models(id) ON DELETE CASCADE,\n" +
		"    provider VARCHAR(64) NOT NULL,\n" +
		"    runtime_model VARCHAR(255) NOT NULL,\n" +
		"    upstream_model VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    enabled BOOLEAN NOT NULL DEFAULT TRUE,\n" +
		"    priority INTEGER NOT NULL DEFAULT 0,\n" +
		"    weight INTEGER NOT NULL DEFAULT 1,\n" +
		"    quota_bucket_key VARCHAR(128) NOT NULL DEFAULT '',\n" +
		"    quota_costs JSONB NOT NULL DEFAULT '{}'::jsonb,\n" +
		"    capabilities JSONB NOT NULL,\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    CONSTRAINT ux_route_identity UNIQUE (logical_model_id, provider, runtime_model),\n" +
		"    CONSTRAINT model_routes_weight_positive CHECK (weight > 0),\n" +
		"    CONSTRAINT model_routes_capabilities_array CHECK (jsonb_typeof(capabilities) = 'array')\n" +
		");\n" +
		"\n" +
		"CREATE INDEX IF NOT EXISTS idx_model_routes_logical_enabled ON model_routes (logical_model_id, enabled, priority DESC);\n" +
		"CREATE INDEX IF NOT EXISTS idx_model_routes_provider ON model_routes (provider, enabled);\n" +
		"\n" +
		"CREATE TABLE IF NOT EXISTS account_model_routes (\n" +
		"    id VARCHAR(191) PRIMARY KEY,\n" +
		"    account_id VARCHAR(64) NOT NULL REFERENCES provider_accounts(id) ON DELETE CASCADE,\n" +
		"    model_route_id VARCHAR(191) NOT NULL REFERENCES model_routes(id) ON DELETE CASCADE,\n" +
		"    enabled BOOLEAN NOT NULL DEFAULT TRUE,\n" +
		"    entitled BOOLEAN NOT NULL DEFAULT TRUE,\n" +
		"    quota_bucket_key VARCHAR(128) NOT NULL DEFAULT '',\n" +
		"    consecutive_fails INTEGER NOT NULL DEFAULT 0,\n" +
		"    success_total BIGINT NOT NULL DEFAULT 0,\n" +
		"    cooldown_until TIMESTAMPTZ NULL,\n" +
		"    last_failure_class VARCHAR(32) NOT NULL DEFAULT '',\n" +
		"    last_failure_at TIMESTAMPTZ NULL,\n" +
		"    last_success_at TIMESTAMPTZ NULL,\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    CONSTRAINT ux_account_route UNIQUE (account_id, model_route_id),\n" +
		"    CONSTRAINT account_model_routes_failures_nonnegative CHECK (consecutive_fails >= 0)\n" +
		");\n" +
		"\n" +
		"CREATE INDEX IF NOT EXISTS idx_account_model_routes_route_enabled ON account_model_routes (model_route_id, enabled, entitled);\n" +
		"CREATE INDEX IF NOT EXISTS idx_account_model_routes_cooldown ON account_model_routes (cooldown_until);\n" +
		"\n" +
		"CREATE TABLE IF NOT EXISTS account_quota_buckets (\n" +
		"    id VARCHAR(191) PRIMARY KEY,\n" +
		"    account_id VARCHAR(64) NOT NULL REFERENCES provider_accounts(id) ON DELETE CASCADE,\n" +
		"    bucket_key VARCHAR(128) NOT NULL,\n" +
		"    unit VARCHAR(32) NOT NULL DEFAULT 'credits',\n" +
		"    total DOUBLE PRECISION NULL,\n" +
		"    remaining DOUBLE PRECISION NULL,\n" +
		"    reserved DOUBLE PRECISION NOT NULL DEFAULT 0,\n" +
		"    reset_at TIMESTAMPTZ NULL,\n" +
		"    revision BIGINT NOT NULL DEFAULT 0,\n" +
		"    refreshed_at TIMESTAMPTZ NULL,\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    CONSTRAINT ux_account_bucket UNIQUE (account_id, bucket_key),\n" +
		"    CONSTRAINT account_quota_buckets_nonnegative CHECK (\n" +
		"        (total IS NULL OR total >= 0) AND (remaining IS NULL OR remaining >= 0) AND reserved >= 0\n" +
		"    )\n" +
		");\n" +
		"\n" +
		"CREATE INDEX IF NOT EXISTS idx_account_quota_buckets_schedulable ON account_quota_buckets (bucket_key, remaining, reset_at);\n" +
		"\n" +
		"CREATE TABLE IF NOT EXISTS dispatch_attempts (\n" +
		"    id VARCHAR(64) PRIMARY KEY,\n" +
		"    event_id VARCHAR(64) NOT NULL,\n" +
		"    model_route_id VARCHAR(191) NOT NULL REFERENCES model_routes(id),\n" +
		"    account_id VARCHAR(64) NULL REFERENCES provider_accounts(id) ON DELETE SET NULL,\n" +
		"    state VARCHAR(32) NOT NULL,\n" +
		"    failure_class VARCHAR(32) NOT NULL DEFAULT '',\n" +
		"    upstream_task_id VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    error TEXT NOT NULL DEFAULT '',\n" +
		"    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    finished_at TIMESTAMPTZ NULL,\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    CONSTRAINT dispatch_attempts_state_valid CHECK (state IN ('created','submitting','accepted','unknown','succeeded','failed')),\n" +
		"    CONSTRAINT dispatch_attempts_failure_valid CHECK (failure_class IN ('','auth','quota','entitlement','temporary','request','content'))\n" +
		");\n" +
		"\n" +
		"CREATE INDEX IF NOT EXISTS idx_dispatch_attempts_event ON dispatch_attempts (event_id, started_at);\n" +
		"CREATE INDEX IF NOT EXISTS idx_dispatch_attempts_account_state ON dispatch_attempts (account_id, state);\n" +
		"\n" +
		"CREATE TABLE IF NOT EXISTS quota_reservations (\n" +
		"    id VARCHAR(64) PRIMARY KEY,\n" +
		"    event_id VARCHAR(64) NOT NULL,\n" +
		"    dispatch_attempt_id VARCHAR(64) NULL REFERENCES dispatch_attempts(id) ON DELETE SET NULL,\n" +
		"    quota_bucket_id VARCHAR(191) NOT NULL REFERENCES account_quota_buckets(id),\n" +
		"    amount DOUBLE PRECISION NOT NULL,\n" +
		"    status VARCHAR(32) NOT NULL,\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    settled_at TIMESTAMPTZ NULL,\n" +
		"    CONSTRAINT quota_reservations_amount_nonnegative CHECK (amount >= 0),\n" +
		"    CONSTRAINT quota_reservations_status_valid CHECK (status IN ('held','settled','released','uncertain'))\n" +
		");\n" +
		"\n" +
		"CREATE INDEX IF NOT EXISTS idx_quota_reservations_event ON quota_reservations (event_id);\n" +
		"CREATE INDEX IF NOT EXISTS idx_quota_reservations_open ON quota_reservations (quota_bucket_id, status)\n" +
		"    WHERE status IN ('held','uncertain');\n" +
		"\n" +
		"INSERT INTO logical_models (id,kind,name,enabled,weight) VALUES\n" +
		" ('gpt-5-5-mini','text','GPT-5.5 Mini',TRUE,0),\n" +
		" ('gpt-5-5-thinking','text','GPT-5.5 Thinking',TRUE,0),\n" +
		" ('grok-4.5','text','Grok 4.5',TRUE,0),\n" +
		" ('grok-chat-fast','text','Grok Chat Fast',TRUE,0),\n" +
		" ('gpt-image-2','image','GPT Image 2',TRUE,0),\n" +
		" ('seedream-5.0-pro','image','Seedream 5.0 Pro',TRUE,0),\n" +
		" ('seedream-5.0-lite','image','Seedream 5.0 Lite',TRUE,0),\n" +
		" ('nano-banana-2','image','Nano Banana 2',TRUE,0),\n" +
		" ('nano-banana-pro','image','Nano Banana Pro',TRUE,0),\n" +
		" ('grok-imagine-image','image','Grok Imagine Image',TRUE,0),\n" +
		" ('veo-3.1','video','Veo 3.1',TRUE,0),\n" +
		" ('veo-3.1-lite','video','Veo 3.1 Lite',TRUE,0),\n" +
		" ('kling-3','video','Kling 3',TRUE,0),\n" +
		" ('kling-o3','video','Kling O3',TRUE,0),\n" +
		" ('runway-gen-4.5','video','Runway Gen-4.5',TRUE,0),\n" +
		" ('runway-gen-4-turbo','video','Runway Gen-4 Turbo',TRUE,0),\n" +
		" ('seedance-2.0','video','Seedance 2.0',TRUE,0),\n" +
		" ('seedance-2.0-fast','video','Seedance 2.0 Fast',TRUE,0),\n" +
		" ('seedance-2.0-mini','video','Seedance 2.0 Mini',TRUE,0),\n" +
		" ('seedance-1.5-pro','video','Seedance 1.5 Pro',TRUE,0),\n" +
		" ('seedance-2.5','video','Seedance 2.5',TRUE,0),\n" +
		" ('grok-imagine-video','video','Grok Imagine Video',TRUE,0),\n" +
		" ('luma-ray','video','Luma Ray',TRUE,0),\n" +
		" ('firefly-video','video','Firefly Video',TRUE,0)\n" +
		"ON CONFLICT (id) DO NOTHING;\n" +
		"\n" +
		"-- Each capabilities value is an array of complete profiles. Request matching\n" +
		"-- must satisfy one profile; fields are never matched against independent unions.\n" +
		"INSERT INTO model_routes (id,logical_model_id,provider,runtime_model,upstream_model,priority,quota_bucket_key,capabilities) VALUES\n" +
		" ('text.gpt-5-5-mini.chatgpt','gpt-5-5-mini','chatgpt','gpt-5-5-mini','gpt-5-5-mini',100,'chatgpt.text','[{\"operations\":[\"completion\"]}]'),\n" +
		" ('text.gpt-5-5-thinking.chatgpt','gpt-5-5-thinking','chatgpt','gpt-5-5-thinking','gpt-5-5-thinking',100,'chatgpt.text','[{\"operations\":[\"completion\"]}]'),\n" +
		" ('text.grok-4.5.grok','grok-4.5','grok','grok-4.5','grok-4.5',100,'grok.text','[{\"operations\":[\"completion\"]}]'),\n" +
		" ('text.grok-chat-fast.grok','grok-chat-fast','grok','grok-chat-fast','grok-chat-fast',100,'grok.text','[{\"operations\":[\"completion\"]}]'),\n" +
		"\n" +
		" ('image.gpt-image-2.chatgpt','gpt-image-2','chatgpt','gpt-image-2','gpt-image-2',100,'chatgpt.image','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"16:9\",\"9:16\",\"4:3\",\"3:4\"],\"resolutions\":[\"1K\"],\"max_reference_images\":6,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.gpt-image-2.byteplus','gpt-image-2','byteplus','lumina-gpt-image-2','6824519374061285743',90,'byteplus.computing_points','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"16:9\",\"9:16\",\"4:3\",\"3:4\"],\"resolutions\":[\"1K\",\"2K\",\"4K\"],\"max_reference_images\":14,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.gpt-image-2.adobe','gpt-image-2','adobe','firefly-gpt-image-2','',80,'adobe.image','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"5:4\",\"9:16\",\"21:9\",\"16:9\",\"4:3\",\"3:2\",\"4:5\",\"3:4\",\"2:3\"],\"resolutions\":[\"1K\",\"2K\",\"4K\"],\"max_reference_images\":6,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.seedream-5.0-pro.byteplus','seedream-5.0-pro','byteplus','lumina-seedream-5.0-pro','7657401949175693322',100,'byteplus.computing_points','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"16:9\",\"9:16\",\"4:3\",\"3:4\"],\"resolutions\":[\"1K\",\"2K\"],\"max_reference_images\":10,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.seedream-5.0-lite.byteplus','seedream-5.0-lite','byteplus','lumina-seedream-5.0-lite','7604761017696141358',100,'byteplus.computing_points','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"16:9\",\"9:16\",\"4:3\",\"3:4\"],\"resolutions\":[\"2K\"],\"max_reference_images\":10,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.nano-banana-2.byteplus','nano-banana-2','byteplus','lumina-nano-banana-2','8162745039814627354',100,'byteplus.computing_points','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"16:9\",\"9:16\",\"4:3\",\"3:4\"],\"resolutions\":[\"1K\",\"2K\",\"4K\"],\"max_reference_images\":14,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.nano-banana-2.runway','nano-banana-2','runway','nano-banana-2','nano-banana-2',90,'runway.credits','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"1:4\",\"1:8\",\"2:3\",\"3:2\",\"3:4\",\"4:1\",\"4:3\",\"4:5\",\"5:4\",\"8:1\",\"9:16\",\"16:9\",\"21:9\"],\"resolutions\":[\"1K\",\"2K\",\"4K\"],\"max_reference_images\":6,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.nano-banana-2.adobe','nano-banana-2','adobe','firefly-nano-banana-2','',80,'adobe.image','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"5:4\",\"9:16\",\"21:9\",\"16:9\",\"4:3\",\"3:2\",\"4:5\",\"3:4\",\"2:3\"],\"resolutions\":[\"1K\",\"2K\",\"4K\"],\"max_reference_images\":6,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.nano-banana-pro.byteplus','nano-banana-pro','byteplus','lumina-nano-banana-pro','8162745039814627353',100,'byteplus.computing_points','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"16:9\",\"9:16\",\"4:3\",\"3:4\"],\"resolutions\":[\"1K\",\"2K\",\"4K\"],\"max_reference_images\":14,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.nano-banana-pro.runway','nano-banana-pro','runway','nano-banana-pro','nano-banana-pro',90,'runway.credits','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"1:4\",\"1:8\",\"2:3\",\"3:2\",\"3:4\",\"4:1\",\"4:3\",\"4:5\",\"5:4\",\"8:1\",\"9:16\",\"16:9\",\"21:9\"],\"resolutions\":[\"1K\",\"2K\",\"4K\"],\"max_reference_images\":6,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.nano-banana-pro.adobe','nano-banana-pro','adobe','firefly-nano-banana-pro','',80,'adobe.image','[{\"operations\":[\"generation\",\"edit\"],\"ratios\":[\"1:1\",\"5:4\",\"9:16\",\"21:9\",\"16:9\",\"4:3\",\"3:2\",\"4:5\",\"3:4\",\"2:3\"],\"resolutions\":[\"1K\",\"2K\",\"4K\"],\"max_reference_images\":6,\"reference_mode\":\"asset\"}]'),\n" +
		" ('image.grok-imagine-image.grok','grok-imagine-image','grok','grok-imagine-image','grok-imagine-image',100,'grok.media','[{\"operations\":[\"generation\"],\"ratios\":[\"2:3\",\"3:2\",\"1:1\",\"9:16\",\"16:9\"],\"resolutions\":[\"1K\"],\"reference_mode\":\"asset\"}]'),\n" +
		"\n" +
		" ('video.veo-3.1.adobe','veo-3.1','adobe','gemini-veo31','',100,'adobe.video','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\"],\"resolutions\":[\"720p\",\"1080p\"],\"durations\":[\"4s\",\"6s\",\"8s\"],\"max_reference_images\":2,\"supports_audio_output\":true,\"reference_mode\":\"frame\"}]'),\n" +
		" ('video.veo-3.1-lite.adobe','veo-3.1-lite','adobe','gemini-veo31-lite','',100,'adobe.video','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\"],\"resolutions\":[\"720p\",\"1080p\"],\"durations\":[\"4s\",\"6s\",\"8s\"],\"max_reference_images\":2,\"reference_mode\":\"frame\"}]'),\n" +
		" ('video.kling-3.adobe','kling-3','adobe','firefly-kling-3','',100,'adobe.video','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\"],\"resolutions\":[\"720p\",\"1080p\"],\"durations\":[\"3s\",\"4s\",\"5s\",\"6s\",\"7s\",\"8s\",\"9s\",\"10s\",\"11s\",\"12s\",\"13s\",\"14s\",\"15s\"],\"max_reference_images\":1,\"max_reference_videos\":1,\"supports_audio_output\":true,\"reference_mode\":\"frame\"}]'),\n" +
		" ('video.kling-o3.adobe','kling-o3','adobe','firefly-kling-o3','',100,'adobe.video','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\"],\"resolutions\":[\"720p\",\"1080p\"],\"durations\":[\"3s\",\"4s\",\"5s\",\"6s\",\"7s\",\"8s\",\"9s\",\"10s\",\"11s\",\"12s\",\"13s\",\"14s\",\"15s\"],\"max_reference_images\":1,\"max_reference_videos\":1,\"supports_audio_output\":true,\"reference_mode\":\"frame\"}]'),\n" +
		" ('video.runway-gen-4.5.adobe','runway-gen-4.5','adobe','firefly-runway-4.5','',100,'adobe.video','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\"],\"resolutions\":[\"720p\"],\"durations\":[\"5s\",\"8s\",\"10s\"],\"max_reference_images\":1,\"reference_mode\":\"frame\"}]'),\n" +
		" ('video.runway-gen-4-turbo.runway','runway-gen-4-turbo','runway','runway-gen4-turbo','gen4_turbo',100,'runway.credits','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\",\"1:1\",\"4:3\",\"3:4\",\"21:9\"],\"resolutions\":[\"720p\"],\"durations\":[\"5s\",\"10s\"],\"max_reference_images\":1,\"reference_mode\":\"frame\",\"requires_reference\":true}]'),\n" +
		" ('video.seedance-2.0.adobe','seedance-2.0','adobe','firefly-seedance-2','',100,'adobe.video','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\"],\"resolutions\":[\"480p\",\"720p\",\"1080p\"],\"durations\":[\"4s\",\"5s\",\"6s\",\"7s\",\"8s\",\"9s\",\"10s\",\"11s\",\"12s\",\"13s\",\"14s\",\"15s\"],\"max_reference_images\":9,\"max_reference_videos\":3,\"max_reference_audios\":3,\"max_reference_media\":9,\"supports_audio_output\":true,\"reference_mode\":\"asset\"}]'),\n" +
		" ('video.seedance-2.0.oreate','seedance-2.0','oreate','oreate-seedance-2.0','seedance-2.0',90,'oreate.points','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"1:1\",\"3:4\",\"4:3\",\"9:16\",\"21:9\"],\"resolutions\":[\"480p\",\"720p\",\"1080p\"],\"durations\":[\"5s\",\"10s\"],\"max_reference_images\":9,\"max_reference_videos\":3,\"max_reference_media\":12,\"supports_audio_output\":true,\"reference_mode\":\"asset\"}]'),\n" +
		" ('video.seedance-2.0-fast.adobe','seedance-2.0-fast','adobe','firefly-seedance-2-fast','',100,'adobe.video','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\"],\"resolutions\":[\"480p\",\"720p\",\"1080p\"],\"durations\":[\"4s\",\"5s\",\"6s\",\"7s\",\"8s\",\"9s\",\"10s\",\"11s\",\"12s\",\"13s\",\"14s\",\"15s\"],\"max_reference_images\":9,\"max_reference_videos\":3,\"max_reference_audios\":3,\"max_reference_media\":9,\"supports_audio_output\":true,\"reference_mode\":\"asset\"}]'),\n" +
		" ('video.seedance-2.0-fast.oreate','seedance-2.0-fast','oreate','oreate-seedance-2.0-fast','seedance-2.0-fast',90,'oreate.points','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"1:1\",\"3:4\",\"4:3\",\"9:16\",\"21:9\"],\"resolutions\":[\"480p\",\"720p\"],\"durations\":[\"5s\",\"10s\"],\"max_reference_images\":9,\"max_reference_videos\":3,\"max_reference_media\":12,\"supports_audio_output\":true,\"reference_mode\":\"asset\"}]'),\n" +
		" ('video.seedance-2.0-mini.oreate','seedance-2.0-mini','oreate','oreate-seedance-2.0-mini','seedance-2.0-mini',100,'oreate.points','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"1:1\",\"3:4\",\"4:3\",\"9:16\",\"21:9\"],\"resolutions\":[\"480p\",\"720p\"],\"durations\":[\"5s\",\"10s\"],\"max_reference_images\":9,\"max_reference_videos\":3,\"max_reference_media\":12,\"supports_audio_output\":true,\"reference_mode\":\"asset\"}]'),\n" +
		" ('video.seedance-1.5-pro.oreate','seedance-1.5-pro','oreate','oreate-seedance-1.5-pro','seedance-1.5-pro',100,'oreate.points','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"1:1\",\"3:4\",\"4:3\",\"9:16\",\"21:9\"],\"resolutions\":[\"480p\",\"720p\",\"1080p\"],\"durations\":[\"5s\",\"10s\"],\"max_reference_images\":2,\"max_reference_media\":2,\"supports_audio_output\":true,\"reference_mode\":\"frame\"}]'),\n" +
		" ('video.seedance-2.5.oreate','seedance-2.5','oreate','oreate-seedance-2.5','seedance-2.5',100,'oreate.points','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"1:1\",\"3:4\",\"4:3\",\"9:16\",\"21:9\"],\"resolutions\":[\"480p\",\"720p\"],\"durations\":[\"5s\",\"10s\",\"20s\",\"30s\"],\"max_reference_images\":9,\"max_reference_videos\":3,\"max_reference_media\":12,\"supports_audio_output\":true,\"reference_mode\":\"asset\"}]'),\n" +
		" ('video.grok-imagine-video.grok','grok-imagine-video','grok','grok-video','grok-imagine-video',100,'grok.media','[{\"operations\":[\"generation\"],\"ratios\":[\"2:3\",\"3:2\",\"1:1\",\"9:16\",\"16:9\"],\"resolutions\":[\"720p\"],\"durations\":[\"6s\",\"10s\"],\"max_reference_images\":6,\"reference_mode\":\"asset\"}]'),\n" +
		" ('video.luma-ray.adobe','luma-ray','adobe','firefly-ray','',100,'adobe.video','[{\"operations\":[\"generation\"],\"ratios\":[\"21:9\",\"16:9\",\"4:3\",\"1:1\",\"3:4\",\"9:16\",\"9:21\"],\"resolutions\":[\"720p\",\"1080p\",\"4K\"],\"durations\":[\"5s\"],\"max_reference_images\":2,\"max_reference_videos\":1,\"reference_mode\":\"frame\"}]'),\n" +
		" ('video.firefly-video.adobe','firefly-video','adobe','firefly-video','',100,'adobe.video','[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"1:1\",\"9:16\"],\"resolutions\":[\"540p\",\"720p\",\"1080p\"],\"durations\":[\"5s\"],\"max_reference_images\":2,\"max_reference_videos\":1,\"reference_mode\":\"frame\"}]')\n" +
		"ON CONFLICT (id) DO NOTHING;\n" +
		"\n" +
		"-- Existing accounts initially inherit every route in their provider pool. A\n" +
		"-- later live entitlement refresh can disable only the rejected account-route.\n" +
		"INSERT INTO account_model_routes (id,account_id,model_route_id,enabled,entitled,quota_bucket_key)\n" +
		"SELECT a.id || ':' || r.id, a.id, r.id, TRUE, TRUE, r.quota_bucket_key\n" +
		"FROM provider_accounts a\n" +
		"JOIN model_routes r ON r.provider = a.pool\n" +
		"ON CONFLICT (account_id,model_route_id) DO NOTHING;\n" +
		"\n" +
		"-- Seed one shared quota row per real provider allowance domain, never one row\n" +
		"-- per logical model. The JSON cache is only a migration snapshot; live refresh\n" +
		"-- replaces it after every generation.\n" +
		"INSERT INTO account_quota_buckets (id,account_id,bucket_key,unit,total,remaining,reserved,reset_at,revision,refreshed_at)\n" +
		"SELECT DISTINCT ON (a.id, r.quota_bucket_key)\n" +
		"       a.id || ':' || r.quota_bucket_key,\n" +
		"       a.id,\n" +
		"       r.quota_bucket_key,\n" +
		"       'credits',\n" +
		"       CASE WHEN (a.meta->>'cached_quota_total') ~ '^[0-9]+(\\.[0-9]+)?$' THEN (a.meta->>'cached_quota_total')::double precision END,\n" +
		"       CASE WHEN (a.meta->>'cached_quota_remaining') ~ '^[0-9]+(\\.[0-9]+)?$' THEN (a.meta->>'cached_quota_remaining')::double precision END,\n" +
		"       COALESCE(CASE WHEN (a.meta->>'cached_quota_reserved') ~ '^[0-9]+(\\.[0-9]+)?$' THEN (a.meta->>'cached_quota_reserved')::double precision END,0),\n" +
		"       a.quota_recover_at,\n" +
		"       1,\n" +
		"       a.updated_at\n" +
		"FROM provider_accounts a\n" +
		"JOIN model_routes r ON r.provider = a.pool\n" +
		"WHERE r.quota_bucket_key <> ''\n" +
		"ON CONFLICT (account_id,bucket_key) DO NOTHING;\n" +
		"\n" +
		"-- Merge legacy successful counters onto canonical ids without retaining any\n" +
		"-- removed public alias at runtime.\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF to_regclass('public.event_logs') IS NOT NULL THEN\n" +
		"        EXECUTE $sql$\n" +
		"            UPDATE logical_models lm SET generation_count = history.count\n" +
		"            FROM (\n" +
		"                SELECT canonical_id, COUNT(*)::bigint AS count\n" +
		"                FROM (\n" +
		"                    SELECT CASE model\n" +
		"                        WHEN 'lumina-gpt-image-2' THEN 'gpt-image-2'\n" +
		"                        WHEN 'firefly-gpt-image-2' THEN 'gpt-image-2'\n" +
		"                        WHEN 'lumina-seedream-5.0-pro' THEN 'seedream-5.0-pro'\n" +
		"                        WHEN 'lumina-seedream-5.0-lite' THEN 'seedream-5.0-lite'\n" +
		"                        WHEN 'lumina-nano-banana-2' THEN 'nano-banana-2'\n" +
		"                        WHEN 'firefly-nano-banana-2' THEN 'nano-banana-2'\n" +
		"                        WHEN 'lumina-nano-banana-pro' THEN 'nano-banana-pro'\n" +
		"                        WHEN 'firefly-nano-banana-pro' THEN 'nano-banana-pro'\n" +
		"                        WHEN 'gemini-veo31' THEN 'veo-3.1'\n" +
		"                        WHEN 'gemini-veo31-lite' THEN 'veo-3.1-lite'\n" +
		"                        WHEN 'firefly-kling-3' THEN 'kling-3'\n" +
		"                        WHEN 'firefly-kling-o3' THEN 'kling-o3'\n" +
		"                        WHEN 'firefly-runway-4.5' THEN 'runway-gen-4.5'\n" +
		"                        WHEN 'runway-gen4-turbo' THEN 'runway-gen-4-turbo'\n" +
		"                        WHEN 'firefly-seedance-2' THEN 'seedance-2.0'\n" +
		"                        WHEN 'oreate-seedance-2.0' THEN 'seedance-2.0'\n" +
		"                        WHEN 'firefly-seedance-2-fast' THEN 'seedance-2.0-fast'\n" +
		"                        WHEN 'oreate-seedance-2.0-fast' THEN 'seedance-2.0-fast'\n" +
		"                        WHEN 'oreate-seedance-2.0-mini' THEN 'seedance-2.0-mini'\n" +
		"                        WHEN 'oreate-seedance-1.5-pro' THEN 'seedance-1.5-pro'\n" +
		"                        WHEN 'oreate-seedance-2.5' THEN 'seedance-2.5'\n" +
		"                        WHEN 'grok-video' THEN 'grok-imagine-video'\n" +
		"                        WHEN 'firefly-ray' THEN 'luma-ray'\n" +
		"                        ELSE model\n" +
		"                    END AS canonical_id\n" +
		"                    FROM event_logs WHERE status = 'success'\n" +
		"                ) mapped\n" +
		"                WHERE canonical_id IN (SELECT id FROM logical_models)\n" +
		"                GROUP BY canonical_id\n" +
		"            ) history\n" +
		"            WHERE lm.id = history.canonical_id AND lm.generation_count = 0\n" +
		"        $sql$;\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n" +
		"\n" +
		"-- Provider accounts are a closed control-plane set. Run this after the routing\n" +
		"-- and quota foreign keys exist so cascades clean any manually pre-created\n" +
		"-- bindings/buckets while dispatch history safely keeps a NULL account id.\n" +
		"DELETE FROM provider_accounts\n" +
		"WHERE pool NOT IN ('chatgpt','byteplus','adobe','runway','grok','oreate','custom');\n",
	"000003_event_identity.sql": "CREATE TABLE IF NOT EXISTS event_logs (\n" +
		"    id VARCHAR(32) PRIMARY KEY,\n" +
		"    api_credential_id VARCHAR(32) NULL,\n" +
		"    request_id VARCHAR(191) NOT NULL DEFAULT '',\n" +
		"    request_fingerprint VARCHAR(64) NOT NULL DEFAULT '',\n" +
		"    response_format VARCHAR(16) NOT NULL DEFAULT '',\n" +
		"    mime_type VARCHAR(100) NOT NULL DEFAULT '',\n" +
		"    ts TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    kind VARCHAR(32) NOT NULL,\n" +
		"    status VARCHAR(32) NOT NULL,\n" +
		"    model VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    provider VARCHAR(100) NOT NULL DEFAULT '',\n" +
		"    prompt TEXT NOT NULL DEFAULT '',\n" +
		"    ratio VARCHAR(32) NOT NULL DEFAULT '',\n" +
		"    resolution VARCHAR(32) NOT NULL DEFAULT '',\n" +
		"    duration VARCHAR(32) NOT NULL DEFAULT '',\n" +
		"    refs INTEGER NOT NULL DEFAULT 0,\n" +
		"    de_ai BOOLEAN NOT NULL DEFAULT FALSE,\n" +
		"    ref_files JSONB NULL,\n" +
		"    source VARCHAR(32) NOT NULL DEFAULT '',\n" +
		"    account_id VARCHAR(64) NOT NULL DEFAULT '',\n" +
		"    account_email VARCHAR(255) NOT NULL DEFAULT '',\n" +
		"    user_id VARCHAR(32) NOT NULL DEFAULT '',\n" +
		"    cost DOUBLE PRECISION NOT NULL DEFAULT 0,\n" +
		"    refunded BOOLEAN NOT NULL DEFAULT FALSE,\n" +
		"    elapsed_ms INTEGER NOT NULL DEFAULT 0,\n" +
		"    file VARCHAR(500) NOT NULL DEFAULT '',\n" +
		"    error TEXT NOT NULL DEFAULT '',\n" +
		"    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),\n" +
		"    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()\n" +
		");\n" +
		"\n" +
		"ALTER TABLE event_logs\n" +
		"    ADD COLUMN IF NOT EXISTS api_credential_id VARCHAR(32),\n" +
		"    ADD COLUMN IF NOT EXISTS request_fingerprint VARCHAR(64) NOT NULL DEFAULT '',\n" +
		"    ADD COLUMN IF NOT EXISTS response_format VARCHAR(16) NOT NULL DEFAULT '',\n" +
		"    ADD COLUMN IF NOT EXISTS mime_type VARCHAR(100) NOT NULL DEFAULT '';\n" +
		"\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF NOT EXISTS (\n" +
		"        SELECT 1\n" +
		"        FROM pg_constraint\n" +
		"        WHERE conname = 'event_logs_response_format_valid'\n" +
		"          AND conrelid = 'event_logs'::regclass\n" +
		"    ) THEN\n" +
		"        ALTER TABLE event_logs\n" +
		"            ADD CONSTRAINT event_logs_response_format_valid\n" +
		"            CHECK (response_format IN ('', 'url', 'b64_json'));\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n" +
		"\n" +
		"CREATE INDEX IF NOT EXISTS idx_event_logs_api_credential_id\n" +
		"    ON event_logs (api_credential_id);\n" +
		"\n" +
		"-- Carry historical admin-key attribution forward when the legacy user id is\n" +
		"-- still available. New writes never use user_id.\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF to_regclass('public.api_keys') IS NOT NULL THEN\n" +
		"        EXECUTE $sql$\n" +
		"            WITH sole_legacy_key AS (\n" +
		"                SELECT k.user_id, MIN(k.id) AS credential_id\n" +
		"                FROM api_keys k\n" +
		"                JOIN api_credentials c ON c.id = k.id\n" +
		"                GROUP BY k.user_id\n" +
		"                HAVING COUNT(*) = 1\n" +
		"            )\n" +
		"            UPDATE event_logs e\n" +
		"            SET api_credential_id = k.credential_id\n" +
		"            FROM sole_legacy_key k\n" +
		"            WHERE e.api_credential_id IS NULL\n" +
		"              AND e.user_id = k.user_id\n" +
		"        $sql$;\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n" +
		"\n" +
		"DROP INDEX IF EXISTS uniq_event_v1_image_request;\n" +
		"\n" +
		"CREATE UNIQUE INDEX IF NOT EXISTS uniq_event_api_request\n" +
		"    ON event_logs (api_credential_id, kind, request_id)\n" +
		"    WHERE api_credential_id IS NOT NULL AND request_id <> '';\n",
	"000004_closed_catalog_and_quota_costs.sql": "-- Every route has an explicit allowance-cost policy. Unknown means that the\n" +
		"-- provider exposes a balance but not a trustworthy per-request price; it is\n" +
		"-- refreshed after each attempt without inventing a one-credit reservation.\n" +
		"UPDATE model_routes\n" +
		"SET quota_costs = CASE\n" +
		"    WHEN provider = 'custom'\n" +
		"        THEN '{\"mode\":\"unmetered\"}'::jsonb\n" +
		"    WHEN provider = 'byteplus' AND id LIKE 'image.%'\n" +
		"        THEN '{\"mode\":\"metered\",\"unit\":\"points\",\"calculator\":\"byteplus_image\"}'::jsonb\n" +
		"    WHEN provider = 'oreate' AND id LIKE 'video.%'\n" +
		"        THEN '{\"mode\":\"metered\",\"unit\":\"points\",\"calculator\":\"oreate_seedance\"}'::jsonb\n" +
		"    WHEN provider = 'runway' AND id LIKE 'video.%'\n" +
		"        THEN '{\"mode\":\"metered\",\"unit\":\"credits\",\"calculator\":\"per_second\",\"per_second\":5}'::jsonb\n" +
		"    WHEN provider = 'chatgpt' AND id LIKE 'image.%'\n" +
		"        THEN '{\"mode\":\"metered\",\"unit\":\"generations\",\"calculator\":\"fixed\",\"value\":1}'::jsonb\n" +
		"    ELSE '{\"mode\":\"unknown\",\"unit\":\"credits\"}'::jsonb\n" +
		"END,\n" +
		"updated_at = NOW();\n" +
		"\n" +
		"-- Adobe's credits endpoint returns one account-wide Firefly allowance. Merge\n" +
		"-- the old image/video scheduler buckets without making that balance spendable\n" +
		"-- twice. The newest snapshot supplies the upstream value; every in-flight hold\n" +
		"-- across all three historical buckets remains held in the shared bucket.\n" +
		"WITH candidates AS (\n" +
		"    SELECT *\n" +
		"    FROM account_quota_buckets\n" +
		"    WHERE bucket_key IN ('adobe.image', 'adobe.video', 'adobe.credits')\n" +
		"), latest AS (\n" +
		"    SELECT DISTINCT ON (account_id)\n" +
		"           account_id, unit, total, remaining, reserved, reset_at, revision,\n" +
		"           refreshed_at, created_at, updated_at\n" +
		"    FROM candidates\n" +
		"    ORDER BY account_id, refreshed_at DESC NULLS LAST, updated_at DESC\n" +
		"), held AS (\n" +
		"    SELECT account_id, SUM(reserved) AS reserved\n" +
		"    FROM candidates\n" +
		"    GROUP BY account_id\n" +
		")\n" +
		"INSERT INTO account_quota_buckets (\n" +
		"    id, account_id, bucket_key, unit, total, remaining, reserved, reset_at,\n" +
		"    revision, refreshed_at, created_at, updated_at\n" +
		")\n" +
		"SELECT latest.account_id || ':adobe.credits', latest.account_id,\n" +
		"       'adobe.credits', latest.unit, latest.total,\n" +
		"       CASE WHEN latest.remaining IS NULL THEN NULL\n" +
		"            ELSE GREATEST(0, latest.remaining + latest.reserved - held.reserved)\n" +
		"       END,\n" +
		"       held.reserved, latest.reset_at, latest.revision + 1,\n" +
		"       latest.refreshed_at, latest.created_at, NOW()\n" +
		"FROM latest\n" +
		"JOIN held USING (account_id)\n" +
		"ON CONFLICT (account_id, bucket_key) DO UPDATE SET\n" +
		"    unit = EXCLUDED.unit,\n" +
		"    total = EXCLUDED.total,\n" +
		"    remaining = EXCLUDED.remaining,\n" +
		"    reserved = EXCLUDED.reserved,\n" +
		"    reset_at = EXCLUDED.reset_at,\n" +
		"    revision = EXCLUDED.revision,\n" +
		"    refreshed_at = EXCLUDED.refreshed_at,\n" +
		"    updated_at = NOW();\n" +
		"\n" +
		"-- Do not assume the shared row uses the conventional account:key primary key.\n" +
		"-- Older/manual rows can have another id even though (account_id,bucket_key) is\n" +
		"-- unique; always redirect reservations to the actual conflict winner.\n" +
		"UPDATE quota_reservations reservation\n" +
		"SET quota_bucket_id = target.id,\n" +
		"    updated_at = NOW()\n" +
		"FROM account_quota_buckets source\n" +
		"JOIN account_quota_buckets target\n" +
		"  ON target.account_id = source.account_id\n" +
		" AND target.bucket_key = 'adobe.credits'\n" +
		"WHERE reservation.quota_bucket_id = source.id\n" +
		"  AND source.bucket_key IN ('adobe.image', 'adobe.video');\n" +
		"\n" +
		"DELETE FROM account_quota_buckets\n" +
		"WHERE bucket_key IN ('adobe.image', 'adobe.video');\n" +
		"\n" +
		"UPDATE model_routes\n" +
		"SET quota_bucket_key = 'adobe.credits', updated_at = NOW()\n" +
		"WHERE provider = 'adobe';\n" +
		"\n" +
		"UPDATE account_model_routes binding\n" +
		"SET quota_bucket_key = 'adobe.credits', updated_at = NOW()\n" +
		"FROM model_routes route\n" +
		"WHERE binding.model_route_id = route.id\n" +
		"  AND route.provider = 'adobe';\n" +
		"\n" +
		"-- This exact route table is the executable built-in catalog. Matching only the\n" +
		"-- logical id is insufficient: a retired provider route can otherwise hide\n" +
		"-- under a canonical model and outrank its supported routes.\n" +
		"CREATE TEMP TABLE migration_000004_canonical_routes (\n" +
		"    id VARCHAR(191) PRIMARY KEY,\n" +
		"    logical_model_id VARCHAR(191) NOT NULL,\n" +
		"    provider VARCHAR(64) NOT NULL,\n" +
		"    runtime_model VARCHAR(255) NOT NULL,\n" +
		"    upstream_model VARCHAR(255) NOT NULL\n" +
		");\n" +
		"\n" +
		"INSERT INTO migration_000004_canonical_routes\n" +
		"    (id, logical_model_id, provider, runtime_model, upstream_model)\n" +
		"VALUES\n" +
		" ('text.gpt-5-5-mini.chatgpt','gpt-5-5-mini','chatgpt','gpt-5-5-mini','gpt-5-5-mini'),\n" +
		" ('text.gpt-5-5-thinking.chatgpt','gpt-5-5-thinking','chatgpt','gpt-5-5-thinking','gpt-5-5-thinking'),\n" +
		" ('text.grok-4.5.grok','grok-4.5','grok','grok-4.5','grok-4.5'),\n" +
		" ('text.grok-chat-fast.grok','grok-chat-fast','grok','grok-chat-fast','grok-chat-fast'),\n" +
		" ('image.gpt-image-2.chatgpt','gpt-image-2','chatgpt','gpt-image-2','gpt-image-2'),\n" +
		" ('image.gpt-image-2.byteplus','gpt-image-2','byteplus','lumina-gpt-image-2','6824519374061285743'),\n" +
		" ('image.gpt-image-2.adobe','gpt-image-2','adobe','firefly-gpt-image-2',''),\n" +
		" ('image.seedream-5.0-pro.byteplus','seedream-5.0-pro','byteplus','lumina-seedream-5.0-pro','7657401949175693322'),\n" +
		" ('image.seedream-5.0-lite.byteplus','seedream-5.0-lite','byteplus','lumina-seedream-5.0-lite','7604761017696141358'),\n" +
		" ('image.nano-banana-2.byteplus','nano-banana-2','byteplus','lumina-nano-banana-2','8162745039814627354'),\n" +
		" ('image.nano-banana-2.runway','nano-banana-2','runway','nano-banana-2','nano-banana-2'),\n" +
		" ('image.nano-banana-2.adobe','nano-banana-2','adobe','firefly-nano-banana-2',''),\n" +
		" ('image.nano-banana-pro.byteplus','nano-banana-pro','byteplus','lumina-nano-banana-pro','8162745039814627353'),\n" +
		" ('image.nano-banana-pro.runway','nano-banana-pro','runway','nano-banana-pro','nano-banana-pro'),\n" +
		" ('image.nano-banana-pro.adobe','nano-banana-pro','adobe','firefly-nano-banana-pro',''),\n" +
		" ('image.grok-imagine-image.grok','grok-imagine-image','grok','grok-imagine-image','grok-imagine-image'),\n" +
		" ('video.veo-3.1.adobe','veo-3.1','adobe','gemini-veo31',''),\n" +
		" ('video.veo-3.1-lite.adobe','veo-3.1-lite','adobe','gemini-veo31-lite',''),\n" +
		" ('video.kling-3.adobe','kling-3','adobe','firefly-kling-3',''),\n" +
		" ('video.kling-o3.adobe','kling-o3','adobe','firefly-kling-o3',''),\n" +
		" ('video.runway-gen-4.5.adobe','runway-gen-4.5','adobe','firefly-runway-4.5',''),\n" +
		" ('video.runway-gen-4-turbo.runway','runway-gen-4-turbo','runway','runway-gen4-turbo','gen4_turbo'),\n" +
		" ('video.seedance-2.0.adobe','seedance-2.0','adobe','firefly-seedance-2',''),\n" +
		" ('video.seedance-2.0.oreate','seedance-2.0','oreate','oreate-seedance-2.0','seedance-2.0'),\n" +
		" ('video.seedance-2.0-fast.adobe','seedance-2.0-fast','adobe','firefly-seedance-2-fast',''),\n" +
		" ('video.seedance-2.0-fast.oreate','seedance-2.0-fast','oreate','oreate-seedance-2.0-fast','seedance-2.0-fast'),\n" +
		" ('video.seedance-2.0-mini.oreate','seedance-2.0-mini','oreate','oreate-seedance-2.0-mini','seedance-2.0-mini'),\n" +
		" ('video.seedance-1.5-pro.oreate','seedance-1.5-pro','oreate','oreate-seedance-1.5-pro','seedance-1.5-pro'),\n" +
		" ('video.seedance-2.5.oreate','seedance-2.5','oreate','oreate-seedance-2.5','seedance-2.5'),\n" +
		" ('video.grok-imagine-video.grok','grok-imagine-video','grok','grok-video','grok-imagine-video'),\n" +
		" ('video.luma-ray.adobe','luma-ray','adobe','firefly-ray',''),\n" +
		" ('video.firefly-video.adobe','firefly-video','adobe','firefly-video','');\n" +
		"\n" +
		"-- Legacy custom accounts stored a comma-separated meta.models list. Missing or\n" +
		"-- blank meant \"all models\"; unknown/retired names are deliberately ignored.\n" +
		"-- Materialize the entitlement set before scrubbing the legacy credential table.\n" +
		"CREATE TEMP TABLE migration_000004_custom_bindings AS\n" +
		"SELECT account.id AS account_id, canonical.logical_model_id\n" +
		"FROM provider_accounts account\n" +
		"CROSS JOIN (\n" +
		"    SELECT DISTINCT logical_model_id\n" +
		"    FROM migration_000004_canonical_routes\n" +
		") canonical\n" +
		"WHERE account.pool = 'custom'\n" +
		"  AND (\n" +
		"      NULLIF(BTRIM(COALESCE(account.meta->>'models', '')), '') IS NULL\n" +
		"      OR EXISTS (\n" +
		"          SELECT 1\n" +
		"          FROM regexp_split_to_table(account.meta->>'models', ',') requested(model_id)\n" +
		"          WHERE LOWER(BTRIM(requested.model_id)) = canonical.logical_model_id\n" +
		"      )\n" +
		"  );\n" +
		"\n" +
		"-- A custom route exposes the union of the canonical model's complete native\n" +
		"-- capability profiles. Existing administrator-disabled custom routes stay\n" +
		"-- disabled; the upsert repairs only their immutable identity/policy fields.\n" +
		"WITH distinct_profiles AS (\n" +
		"    SELECT DISTINCT route.logical_model_id, profile.value AS profile\n" +
		"    FROM model_routes route\n" +
		"    JOIN migration_000004_canonical_routes canonical\n" +
		"      ON canonical.id = route.id\n" +
		"     AND canonical.logical_model_id = route.logical_model_id\n" +
		"     AND canonical.provider = route.provider\n" +
		"     AND canonical.runtime_model = route.runtime_model\n" +
		"     AND canonical.upstream_model = route.upstream_model\n" +
		"    CROSS JOIN LATERAL jsonb_array_elements(route.capabilities) profile(value)\n" +
		"), aggregate_profiles AS (\n" +
		"    SELECT logical_model_id,\n" +
		"           jsonb_agg(profile ORDER BY profile::text) AS capabilities\n" +
		"    FROM distinct_profiles\n" +
		"    GROUP BY logical_model_id\n" +
		"), requested_models AS (\n" +
		"    SELECT DISTINCT logical_model_id\n" +
		"    FROM migration_000004_custom_bindings\n" +
		")\n" +
		"INSERT INTO model_routes (\n" +
		"    id, logical_model_id, provider, runtime_model, upstream_model, enabled,\n" +
		"    priority, weight, quota_bucket_key, quota_costs, capabilities\n" +
		")\n" +
		"SELECT 'custom.' || requested.logical_model_id,\n" +
		"       requested.logical_model_id,\n" +
		"       'custom',\n" +
		"       requested.logical_model_id,\n" +
		"       requested.logical_model_id,\n" +
		"       TRUE,\n" +
		"       10,\n" +
		"       1,\n" +
		"       'custom.unmetered',\n" +
		"       '{\"mode\":\"unmetered\"}'::jsonb,\n" +
		"       aggregate.capabilities\n" +
		"FROM requested_models requested\n" +
		"JOIN aggregate_profiles aggregate USING (logical_model_id)\n" +
		"ON CONFLICT (id) DO UPDATE SET\n" +
		"    logical_model_id = EXCLUDED.logical_model_id,\n" +
		"    provider = EXCLUDED.provider,\n" +
		"    runtime_model = EXCLUDED.runtime_model,\n" +
		"    upstream_model = EXCLUDED.upstream_model,\n" +
		"    quota_bucket_key = EXCLUDED.quota_bucket_key,\n" +
		"    quota_costs = EXCLUDED.quota_costs,\n" +
		"    capabilities = EXCLUDED.capabilities,\n" +
		"    updated_at = NOW();\n" +
		"\n" +
		"INSERT INTO account_model_routes (\n" +
		"    id, account_id, model_route_id, enabled, entitled, quota_bucket_key\n" +
		")\n" +
		"SELECT binding.account_id || ':custom.' || binding.logical_model_id,\n" +
		"       binding.account_id,\n" +
		"       'custom.' || binding.logical_model_id,\n" +
		"       TRUE,\n" +
		"       TRUE,\n" +
		"       'custom.unmetered'\n" +
		"FROM migration_000004_custom_bindings binding\n" +
		"JOIN model_routes route\n" +
		"  ON route.id = 'custom.' || binding.logical_model_id\n" +
		"ON CONFLICT (account_id, model_route_id) DO NOTHING;\n" +
		"\n" +
		"-- Freeze the invalid-route set independently of enabled state. A supported\n" +
		"-- canonical route may already be disabled by the administrator and must remain\n" +
		"-- present, disabled, with its entitlement state unchanged.\n" +
		"CREATE TEMP TABLE migration_000004_invalid_routes AS\n" +
		"SELECT route.id\n" +
		"FROM model_routes route\n" +
		"WHERE NOT EXISTS (\n" +
		"    SELECT 1\n" +
		"    FROM migration_000004_canonical_routes canonical\n" +
		"    WHERE canonical.id = route.id\n" +
		"      AND canonical.logical_model_id = route.logical_model_id\n" +
		"      AND canonical.provider = route.provider\n" +
		"      AND canonical.runtime_model = route.runtime_model\n" +
		"      AND canonical.upstream_model = route.upstream_model\n" +
		")\n" +
		"AND NOT (\n" +
		"    route.provider = 'custom'\n" +
		"    AND route.id = 'custom.' || route.logical_model_id\n" +
		"    AND route.runtime_model = route.logical_model_id\n" +
		"    AND route.upstream_model = route.logical_model_id\n" +
		"    AND EXISTS (\n" +
		"        SELECT 1\n" +
		"        FROM migration_000004_canonical_routes canonical\n" +
		"        WHERE canonical.logical_model_id = route.logical_model_id\n" +
		"    )\n" +
		");\n" +
		"\n" +
		"UPDATE account_model_routes binding\n" +
		"SET enabled = FALSE, entitled = FALSE, updated_at = NOW()\n" +
		"FROM migration_000004_invalid_routes invalid\n" +
		"WHERE binding.model_route_id = invalid.id;\n" +
		"\n" +
		"UPDATE model_routes route\n" +
		"SET enabled = FALSE, updated_at = NOW()\n" +
		"FROM migration_000004_invalid_routes invalid\n" +
		"WHERE route.id = invalid.id;\n" +
		"\n" +
		"DELETE FROM model_routes route\n" +
		"USING migration_000004_invalid_routes invalid\n" +
		"WHERE route.id = invalid.id\n" +
		"  AND NOT EXISTS (\n" +
		"      SELECT 1\n" +
		"      FROM dispatch_attempts attempt\n" +
		"      WHERE attempt.model_route_id = route.id\n" +
		"  );\n" +
		"\n" +
		"UPDATE logical_models\n" +
		"SET enabled = FALSE, updated_at = NOW()\n" +
		"WHERE id NOT IN (\n" +
		"    SELECT DISTINCT logical_model_id\n" +
		"    FROM migration_000004_canonical_routes\n" +
		");\n" +
		"\n" +
		"DELETE FROM logical_models logical\n" +
		"WHERE logical.enabled = FALSE\n" +
		"  AND logical.id NOT IN (\n" +
		"      SELECT DISTINCT logical_model_id\n" +
		"      FROM migration_000004_canonical_routes\n" +
		"  )\n" +
		"  AND NOT EXISTS (\n" +
		"      SELECT 1\n" +
		"      FROM model_routes route\n" +
		"      WHERE route.logical_model_id = logical.id\n" +
		"  );\n" +
		"\n" +
		"-- Retired provider rows with immutable quota history remain FK-safe tombstones,\n" +
		"-- but no retired plaintext credential or provider metadata is retained.\n" +
		"UPDATE provider_accounts\n" +
		"SET status = 'disabled',\n" +
		"    dead = TRUE,\n" +
		"    value = '',\n" +
		"    meta = '{}'::jsonb,\n" +
		"    account_email = '',\n" +
		"    account_display_name = '',\n" +
		"    updated_at = NOW()\n" +
		"WHERE pool NOT IN ('chatgpt','byteplus','adobe','runway','grok','oreate','custom');\n" +
		"\n" +
		"DELETE FROM provider_accounts account\n" +
		"WHERE account.pool NOT IN ('chatgpt','byteplus','adobe','runway','grok','oreate','custom')\n" +
		"  AND NOT EXISTS (\n" +
		"      SELECT 1\n" +
		"      FROM account_quota_buckets bucket\n" +
		"      JOIN quota_reservations reservation\n" +
		"        ON reservation.quota_bucket_id = bucket.id\n" +
		"      WHERE bucket.account_id = account.id\n" +
		"  );\n" +
		"\n" +
		"-- Adobe cookie refresh is the only retained refresh-profile workflow. Delete\n" +
		"-- retired provider/profile rows so their cookies and errors cannot leak through\n" +
		"-- an otherwise unused legacy table.\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF to_regclass('public.refresh_profiles') IS NOT NULL THEN\n" +
		"        EXECUTE $sql$\n" +
		"            DELETE FROM refresh_profiles\n" +
		"            WHERE pool <> 'adobe' OR kind <> 'adobe_cookie'\n" +
		"        $sql$;\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n" +
		"\n" +
		"-- event_logs.user_id was needed only for the one-time API credential mapping\n" +
		"-- in 000003. The 2API EventLog model no longer declares legacy user identity;\n" +
		"-- remove it (and older user_name variants) before dropping users.\n" +
		"ALTER TABLE event_logs\n" +
		"    DROP COLUMN IF EXISTS user_id,\n" +
		"    DROP COLUMN IF EXISTS user_name;\n" +
		"\n" +
		"-- The retained banned-word hit fields now mean API credential id/name. Rows\n" +
		"-- created by the legacy application contain ordinary-user identity, so clear\n" +
		"-- only those snapshots before the new application starts writing 2API hits.\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF to_regclass('public.banned_word_hits') IS NOT NULL THEN\n" +
		"        IF EXISTS (\n" +
		"            SELECT 1 FROM information_schema.columns\n" +
		"            WHERE table_schema = 'public'\n" +
		"              AND table_name = 'banned_word_hits'\n" +
		"              AND column_name = 'user_id'\n" +
		"        ) THEN\n" +
		"            EXECUTE 'UPDATE banned_word_hits SET user_id = ''''';\n" +
		"        END IF;\n" +
		"        IF EXISTS (\n" +
		"            SELECT 1 FROM information_schema.columns\n" +
		"            WHERE table_schema = 'public'\n" +
		"              AND table_name = 'banned_word_hits'\n" +
		"              AND column_name = 'user_name'\n" +
		"        ) THEN\n" +
		"            EXECUTE 'UPDATE banned_word_hits SET user_name = ''''';\n" +
		"        END IF;\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n" +
		"\n" +
		"-- Identity attribution and provider-account copying are complete. These four\n" +
		"-- legacy tables are not present in AutoMigrateModels and no runtime repository\n" +
		"-- reads them. Drop them in dependency order so retired user password hashes,\n" +
		"-- duplicate API-key hashes, obsolete model names, and duplicate account\n" +
		"-- credentials/PII cannot survive as a dormant rollback data plane.\n" +
		"DROP TABLE IF EXISTS api_keys;\n" +
		"DROP TABLE IF EXISTS users;\n" +
		"DROP TABLE IF EXISTS model_configs;\n" +
		"DROP TABLE IF EXISTS token_accounts;\n" +
		"\n" +
		"ALTER TABLE model_routes\n" +
		"    ADD CONSTRAINT model_routes_quota_costs_valid CHECK (\n" +
		"        quota_costs IS NOT NULL\n" +
		"        AND jsonb_typeof(quota_costs) = 'object'\n" +
		"        AND quota_costs->>'mode' IN ('metered', 'unknown', 'unmetered')\n" +
		"        AND (\n" +
		"            quota_costs->>'mode' = 'unmetered'\n" +
		"            OR NULLIF(BTRIM(quota_costs->>'unit'), '') IS NOT NULL\n" +
		"        )\n" +
		"        AND (\n" +
		"            quota_costs->>'mode' <> 'metered'\n" +
		"            OR quota_costs->>'calculator' IN (\n" +
		"                'fixed', 'per_second', 'byteplus_image', 'oreate_seedance'\n" +
		"            )\n" +
		"        )\n" +
		"    );\n" +
		"\n" +
		"DROP TABLE migration_000004_invalid_routes;\n" +
		"DROP TABLE migration_000004_custom_bindings;\n" +
		"DROP TABLE migration_000004_canonical_routes;\n",
	"000005_retire_unused_video_models.sql": "-- Retire the five video models removed from the closed public catalog.\n" +
		"-- Immutable dispatch history wins over physical deletion: a referenced route\n" +
		"-- remains as a disabled tombstone, while unused routes and logical models are\n" +
		"-- removed completely.\n" +
		"\n" +
		"UPDATE logical_models\n" +
		"SET enabled = FALSE, updated_at = NOW()\n" +
		"WHERE id IN (\n" +
		"    'luma-ray',\n" +
		"    'runway-gen-4-turbo',\n" +
		"    'runway-gen-4.5',\n" +
		"    'veo-3.1',\n" +
		"    'veo-3.1-lite'\n" +
		");\n" +
		"\n" +
		"UPDATE model_routes\n" +
		"SET enabled = FALSE, updated_at = NOW()\n" +
		"WHERE logical_model_id IN (\n" +
		"    'luma-ray',\n" +
		"    'runway-gen-4-turbo',\n" +
		"    'runway-gen-4.5',\n" +
		"    'veo-3.1',\n" +
		"    'veo-3.1-lite'\n" +
		");\n" +
		"\n" +
		"DELETE FROM account_model_routes\n" +
		"WHERE model_route_id IN (\n" +
		"    SELECT id\n" +
		"    FROM model_routes\n" +
		"    WHERE logical_model_id IN (\n" +
		"        'luma-ray',\n" +
		"        'runway-gen-4-turbo',\n" +
		"        'runway-gen-4.5',\n" +
		"        'veo-3.1',\n" +
		"        'veo-3.1-lite'\n" +
		"    )\n" +
		");\n" +
		"\n" +
		"DELETE FROM model_routes AS route\n" +
		"WHERE route.logical_model_id IN (\n" +
		"    'luma-ray',\n" +
		"    'runway-gen-4-turbo',\n" +
		"    'runway-gen-4.5',\n" +
		"    'veo-3.1',\n" +
		"    'veo-3.1-lite'\n" +
		")\n" +
		"AND NOT EXISTS (\n" +
		"    SELECT 1\n" +
		"    FROM dispatch_attempts AS attempt\n" +
		"    WHERE attempt.model_route_id = route.id\n" +
		");\n" +
		"\n" +
		"DELETE FROM logical_models AS logical\n" +
		"WHERE logical.id IN (\n" +
		"    'luma-ray',\n" +
		"    'runway-gen-4-turbo',\n" +
		"    'runway-gen-4.5',\n" +
		"    'veo-3.1',\n" +
		"    'veo-3.1-lite'\n" +
		")\n" +
		"AND NOT EXISTS (\n" +
		"    SELECT 1\n" +
		"    FROM model_routes AS route\n" +
		"    WHERE route.logical_model_id = logical.id\n" +
		");\n",
	"000006_adobe_arp_session.sql": "ALTER TABLE provider_accounts\n" +
		"    ADD COLUMN arp_session_token TEXT NOT NULL DEFAULT '';\n" +
		"\n" +
		"ALTER TABLE refresh_profiles\n" +
		"    ADD COLUMN arp_session_token TEXT NOT NULL DEFAULT '';\n",
	"000007_backfill_adobe_arp_sessions.sql": "-- Adobe's SherlockSdk creates the initial x-arp-session-id locally as padded\n" +
		"-- base64 of compact JSON containing a random v4 session UUID. Backfill legacy\n" +
		"-- Adobe rows so they can use the same base session immediately after upgrade.\n" +
		"UPDATE refresh_profiles\n" +
		"SET arp_session_token = encode(\n" +
		"    convert_to(format('{\"sid\":\"%s\"}', gen_random_uuid()::text), 'UTF8'),\n" +
		"    'base64'\n" +
		")\n" +
		"WHERE pool = 'adobe'\n" +
		"  AND kind = 'adobe_cookie'\n" +
		"  AND BTRIM(arp_session_token) = '';\n" +
		"\n" +
		"UPDATE provider_accounts AS account\n" +
		"SET arp_session_token = profile.arp_session_token\n" +
		"FROM refresh_profiles AS profile\n" +
		"WHERE account.pool = 'adobe'\n" +
		"  AND account.id = profile.id\n" +
		"  AND BTRIM(account.arp_session_token) = ''\n" +
		"  AND BTRIM(profile.arp_session_token) <> '';\n" +
		"\n" +
		"UPDATE provider_accounts\n" +
		"SET arp_session_token = encode(\n" +
		"    convert_to(format('{\"sid\":\"%s\"}', gen_random_uuid()::text), 'UTF8'),\n" +
		"    'base64'\n" +
		")\n" +
		"WHERE pool = 'adobe'\n" +
		"  AND BTRIM(arp_session_token) = '';\n",
	"000008_cascade_quota_reservations.sql": "-- Provider-account deletion cascades through account_quota_buckets. Reservations\n" +
		"-- belong to those buckets, so retaining a reservation while deleting its bucket\n" +
		"-- is impossible and previously made every used account undeletable.\n" +
		"ALTER TABLE quota_reservations\n" +
		"    DROP CONSTRAINT IF EXISTS quota_reservations_quota_bucket_id_fkey;\n" +
		"\n" +
		"ALTER TABLE quota_reservations\n" +
		"    ADD CONSTRAINT quota_reservations_quota_bucket_id_fkey\n" +
		"    FOREIGN KEY (quota_bucket_id)\n" +
		"    REFERENCES account_quota_buckets(id)\n" +
		"    ON DELETE CASCADE;\n",
	"000009_byteplus_login_profiles.sql": "ALTER TABLE refresh_profiles\n" +
		"    ADD COLUMN login_identity TEXT NOT NULL DEFAULT '',\n" +
		"    ADD COLUMN login_secret TEXT NOT NULL DEFAULT '';\n",
	"000010_oreate_session_identity.sql": "ALTER TABLE provider_accounts\n" +
		"    ADD COLUMN IF NOT EXISTS identity_hash VARCHAR(64) NOT NULL DEFAULT '';\n" +
		"\n" +
		"WITH ranked AS (\n" +
		"    SELECT id,\n" +
		"           ROW_NUMBER() OVER (\n" +
		"               PARTITION BY BTRIM(meta ->> 'ouid')\n" +
		"               ORDER BY success_total DESC, created_at ASC, id ASC\n" +
		"           ) AS identity_rank\n" +
		"    FROM provider_accounts\n" +
		"    WHERE pool = 'oreate'\n" +
		"      AND NULLIF(BTRIM(meta ->> 'ouid'), '') IS NOT NULL\n" +
		")\n" +
		"UPDATE provider_accounts AS account\n" +
		"SET identity_hash = ENCODE(SHA256(CONVERT_TO('oreate:' || BTRIM(account.meta ->> 'ouid'), 'UTF8')), 'hex')\n" +
		"FROM ranked\n" +
		"WHERE account.id = ranked.id\n" +
		"  AND ranked.identity_rank = 1\n" +
		"  AND account.identity_hash = '';\n" +
		"\n" +
		"CREATE UNIQUE INDEX IF NOT EXISTS ux_provider_accounts_oreate_identity\n" +
		"    ON provider_accounts (pool, identity_hash)\n" +
		"    WHERE pool = 'oreate' AND identity_hash <> '';\n",
	"000011_dola_video_provider.sql": "-- Add the Dola (Doubao international, dola.com) provider: one video route on\n" +
		"-- the canonical seedance-2.5 model, carried by website-cookie accounts.\n" +
		"\n" +
		"-- Widen the metered-calculator whitelist from 000004 to admit the Dola video\n" +
		"-- credit calculator before the route row lands.\n" +
		"ALTER TABLE model_routes DROP CONSTRAINT IF EXISTS model_routes_quota_costs_valid;\n" +
		"ALTER TABLE model_routes\n" +
		"    ADD CONSTRAINT model_routes_quota_costs_valid CHECK (\n" +
		"        quota_costs IS NOT NULL\n" +
		"        AND jsonb_typeof(quota_costs) = 'object'\n" +
		"        AND quota_costs->>'mode' IN ('metered', 'unknown', 'unmetered')\n" +
		"        AND (\n" +
		"            quota_costs->>'mode' = 'unmetered'\n" +
		"            OR NULLIF(BTRIM(quota_costs->>'unit'), '') IS NOT NULL\n" +
		"        )\n" +
		"        AND (\n" +
		"            quota_costs->>'mode' <> 'metered'\n" +
		"            OR quota_costs->>'calculator' IN (\n" +
		"                'fixed', 'per_second', 'byteplus_image', 'oreate_seedance', 'dola_video'\n" +
		"            )\n" +
		"        )\n" +
		"    );\n" +
		"\n" +
		"CREATE TEMP TABLE migration_0011_canonical_routes (\n" +
		"    id VARCHAR(191) PRIMARY KEY,\n" +
		"    logical_model_id VARCHAR(191) NOT NULL,\n" +
		"    provider VARCHAR(64) NOT NULL,\n" +
		"    runtime_model VARCHAR(255) NOT NULL,\n" +
		"    upstream_model VARCHAR(255) NOT NULL\n" +
		");\n" +
		"\n" +
		"INSERT INTO migration_0011_canonical_routes\n" +
		"    (id, logical_model_id, provider, runtime_model, upstream_model)\n" +
		"VALUES\n" +
		" ('video.seedance-2.5.dola','seedance-2.5','dola','dola-seedance-2.5','seedance_v2.5');\n" +
		"\n" +
		"INSERT INTO model_routes (\n" +
		"    id, logical_model_id, provider, runtime_model, upstream_model, enabled,\n" +
		"    priority, weight, quota_bucket_key, quota_costs, capabilities\n" +
		")\n" +
		"SELECT\n" +
		"    route.id,\n" +
		"    route.logical_model_id,\n" +
		"    route.provider,\n" +
		"    route.runtime_model,\n" +
		"    route.upstream_model,\n" +
		"    TRUE,\n" +
		"    80,\n" +
		"    1,\n" +
		"    'dola.video_credits',\n" +
		"    '{\"mode\":\"metered\",\"unit\":\"credits\",\"calculator\":\"dola_video\"}'::jsonb,\n" +
		"    '[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\",\"1:1\",\"4:3\",\"3:4\"],\"resolutions\":[\"720p\"],\"durations\":[\"5s\",\"10s\"],\"supports_audio_output\":true}]'::jsonb\n" +
		"FROM migration_0011_canonical_routes route\n" +
		"ON CONFLICT (id) DO UPDATE SET\n" +
		"    provider = EXCLUDED.provider,\n" +
		"    runtime_model = EXCLUDED.runtime_model,\n" +
		"    upstream_model = EXCLUDED.upstream_model,\n" +
		"    quota_bucket_key = EXCLUDED.quota_bucket_key,\n" +
		"    quota_costs = EXCLUDED.quota_costs,\n" +
		"    capabilities = EXCLUDED.capabilities,\n" +
		"    updated_at = NOW();\n" +
		"\n" +
		"-- Dola accounts dedupe on the hashed sessionid, mirroring the Oreate identity\n" +
		"-- index from 000010.\n" +
		"CREATE UNIQUE INDEX IF NOT EXISTS ux_provider_accounts_dola_identity\n" +
		"    ON provider_accounts (pool, identity_hash)\n" +
		"    WHERE pool = 'dola' AND identity_hash <> '';\n",
	"000012_dola_video_extended_durations.sql": "-- Extend the Dola video route with the 15s/30s durations the upstream accepts\n" +
		"-- directly (the web picker's 5s/10s cap is a frontend-only limit).\n" +
		"\n" +
		"UPDATE model_routes\n" +
		"SET capabilities = '[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\",\"1:1\",\"4:3\",\"3:4\"],\"resolutions\":[\"720p\"],\"durations\":[\"5s\",\"10s\",\"15s\",\"30s\"],\"supports_audio_output\":true}]'::jsonb,\n" +
		"    updated_at = NOW()\n" +
		"WHERE id = 'video.seedance-2.5.dola';\n",
	"000013_dola_seedance_family.sql": "-- Add Dola routes for the Seedance 2.0 family (std / fast / mini) and widen\n" +
		"-- the existing 2.5 route to accept a reference image (image-to-video first\n" +
		"-- frame). All Dola routes share the same daily-allowance bucket.\n" +
		"\n" +
		"CREATE TEMP TABLE migration_0013_canonical_routes (\n" +
		"    id VARCHAR(191) PRIMARY KEY,\n" +
		"    logical_model_id VARCHAR(191) NOT NULL,\n" +
		"    provider VARCHAR(64) NOT NULL,\n" +
		"    runtime_model VARCHAR(255) NOT NULL,\n" +
		"    upstream_model VARCHAR(255) NOT NULL\n" +
		");\n" +
		"\n" +
		"INSERT INTO migration_0013_canonical_routes\n" +
		"    (id, logical_model_id, provider, runtime_model, upstream_model)\n" +
		"VALUES\n" +
		" ('video.seedance-2.0.dola','seedance-2.0','dola','dola-seedance-2.0','seedance_v2.0_std'),\n" +
		" ('video.seedance-2.0-fast.dola','seedance-2.0-fast','dola','dola-seedance-2.0-fast','seedance_v2.0'),\n" +
		" ('video.seedance-2.0-mini.dola','seedance-2.0-mini','dola','dola-seedance-2.0-mini','seedance_v2.0_mini');\n" +
		"\n" +
		"INSERT INTO model_routes (\n" +
		"    id, logical_model_id, provider, runtime_model, upstream_model, enabled,\n" +
		"    priority, weight, quota_bucket_key, quota_costs, capabilities\n" +
		")\n" +
		"SELECT\n" +
		"    route.id,\n" +
		"    route.logical_model_id,\n" +
		"    route.provider,\n" +
		"    route.runtime_model,\n" +
		"    route.upstream_model,\n" +
		"    TRUE,\n" +
		"    70,\n" +
		"    1,\n" +
		"    'dola.video_credits',\n" +
		"    '{\"mode\":\"metered\",\"unit\":\"credits\",\"calculator\":\"dola_video\"}'::jsonb,\n" +
		"    '[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\",\"1:1\",\"4:3\",\"3:4\"],\"resolutions\":[\"720p\"],\"durations\":[\"5s\",\"10s\",\"15s\",\"30s\"],\"max_reference_images\":1,\"max_reference_media\":1,\"supports_audio_output\":true,\"reference_mode\":\"frame\"}]'::jsonb\n" +
		"FROM migration_0013_canonical_routes route\n" +
		"ON CONFLICT (id) DO UPDATE SET\n" +
		"    provider = EXCLUDED.provider,\n" +
		"    runtime_model = EXCLUDED.runtime_model,\n" +
		"    upstream_model = EXCLUDED.upstream_model,\n" +
		"    quota_bucket_key = EXCLUDED.quota_bucket_key,\n" +
		"    quota_costs = EXCLUDED.quota_costs,\n" +
		"    capabilities = EXCLUDED.capabilities,\n" +
		"    updated_at = NOW();\n" +
		"\n" +
		"-- Widen the existing 2.5 route to accept one reference image.\n" +
		"UPDATE model_routes\n" +
		"SET capabilities = '[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\",\"1:1\",\"4:3\",\"3:4\"],\"resolutions\":[\"720p\"],\"durations\":[\"5s\",\"10s\",\"15s\",\"30s\"],\"max_reference_images\":1,\"max_reference_media\":1,\"supports_audio_output\":true,\"reference_mode\":\"frame\"}]'::jsonb,\n" +
		"    updated_at = NOW()\n" +
		"WHERE id = 'video.seedance-2.5.dola';\n",
	"000014_dola_duration_cap_15s.sql": "-- Correct Dola video duration ceiling: Seedance 2.5 (and the 2.0 family on\n" +
		"-- Dola) only accept 4-15s. Earlier migrations advertised 30s, which the\n" +
		"-- upstream rejects with an opaque \"service busy\" (710022002) error. Rewrite\n" +
		"-- the duration list to the real 4..15 range while keeping every other\n" +
		"-- capability field intact.\n" +
		"UPDATE model_routes\n" +
		"SET capabilities = jsonb_set(\n" +
		"        capabilities,\n" +
		"        '{0,durations}',\n" +
		"        '[\"4s\",\"5s\",\"6s\",\"7s\",\"8s\",\"9s\",\"10s\",\"11s\",\"12s\",\"13s\",\"14s\",\"15s\"]'::jsonb\n" +
		"    ),\n" +
		"    updated_at = NOW()\n" +
		"WHERE provider = 'dola'\n" +
		"  AND jsonb_array_length(capabilities) > 0;\n",
	"000015_dola_image_provider.sql": "-- Add the Dola text-to-image route. Dola's website exposes a daily free image\n" +
		"-- allowance through the same chat transport as video, selected by the\n" +
		"-- SkillImageGen ability (ability_type 3) instead of SkillVideoGeneration (17).\n" +
		"-- This lands a new logical image model (dola-image) plus its single Dola route,\n" +
		"-- and widens the metered-calculator whitelist to admit the image credit\n" +
		"-- calculator.\n" +
		"\n" +
		"-- Widen the metered-calculator whitelist (last set in 000011) to include the\n" +
		"-- Dola image credit calculator before the route row lands.\n" +
		"ALTER TABLE model_routes DROP CONSTRAINT IF EXISTS model_routes_quota_costs_valid;\n" +
		"ALTER TABLE model_routes\n" +
		"    ADD CONSTRAINT model_routes_quota_costs_valid CHECK (\n" +
		"        quota_costs IS NOT NULL\n" +
		"        AND jsonb_typeof(quota_costs) = 'object'\n" +
		"        AND quota_costs->>'mode' IN ('metered', 'unknown', 'unmetered')\n" +
		"        AND (\n" +
		"            quota_costs->>'mode' = 'unmetered'\n" +
		"            OR NULLIF(BTRIM(quota_costs->>'unit'), '') IS NOT NULL\n" +
		"        )\n" +
		"        AND (\n" +
		"            quota_costs->>'mode' <> 'metered'\n" +
		"            OR quota_costs->>'calculator' IN (\n" +
		"                'fixed', 'per_second', 'byteplus_image', 'oreate_seedance', 'dola_video', 'dola_image'\n" +
		"            )\n" +
		"        )\n" +
		"    );\n" +
		"\n" +
		"-- The logical image model exposed to /v1 consumers. Kept enabled; provider ids\n" +
		"-- stay internal to the route.\n" +
		"INSERT INTO logical_models (id, kind, name, enabled, weight)\n" +
		"VALUES ('dola-image', 'image', 'Dola Image', TRUE, 0)\n" +
		"ON CONFLICT (id) DO UPDATE SET\n" +
		"    kind = EXCLUDED.kind,\n" +
		"    name = EXCLUDED.name,\n" +
		"    updated_at = NOW();\n" +
		"\n" +
		"CREATE TEMP TABLE migration_0015_canonical_routes (\n" +
		"    id VARCHAR(191) PRIMARY KEY,\n" +
		"    logical_model_id VARCHAR(191) NOT NULL,\n" +
		"    provider VARCHAR(64) NOT NULL,\n" +
		"    runtime_model VARCHAR(255) NOT NULL,\n" +
		"    upstream_model VARCHAR(255) NOT NULL\n" +
		");\n" +
		"\n" +
		"INSERT INTO migration_0015_canonical_routes\n" +
		"    (id, logical_model_id, provider, runtime_model, upstream_model)\n" +
		"VALUES\n" +
		" ('image.dola-image.dola','dola-image','dola','dola-image','');\n" +
		"\n" +
		"INSERT INTO model_routes (\n" +
		"    id, logical_model_id, provider, runtime_model, upstream_model, enabled,\n" +
		"    priority, weight, quota_bucket_key, quota_costs, capabilities\n" +
		")\n" +
		"SELECT\n" +
		"    route.id,\n" +
		"    route.logical_model_id,\n" +
		"    route.provider,\n" +
		"    route.runtime_model,\n" +
		"    route.upstream_model,\n" +
		"    TRUE,\n" +
		"    100,\n" +
		"    1,\n" +
		"    'dola.image_credits',\n" +
		"    '{\"mode\":\"metered\",\"unit\":\"credits\",\"calculator\":\"dola_image\"}'::jsonb,\n" +
		"    '[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\",\"1:1\",\"4:3\",\"3:4\"],\"resolutions\":[\"1K\"],\"reference_mode\":\"asset\"}]'::jsonb\n" +
		"FROM migration_0015_canonical_routes route\n" +
		"ON CONFLICT (id) DO UPDATE SET\n" +
		"    logical_model_id = EXCLUDED.logical_model_id,\n" +
		"    provider = EXCLUDED.provider,\n" +
		"    runtime_model = EXCLUDED.runtime_model,\n" +
		"    upstream_model = EXCLUDED.upstream_model,\n" +
		"    quota_bucket_key = EXCLUDED.quota_bucket_key,\n" +
		"    quota_costs = EXCLUDED.quota_costs,\n" +
		"    capabilities = EXCLUDED.capabilities,\n" +
		"    updated_at = NOW();\n",
	"000016_dola_image_account_bindings.sql": "-- Backfill account_model_routes bindings for existing Dola accounts onto the\n" +
		"-- new image route (image.dola-image.dola) added in 000015. Bindings are only\n" +
		"-- created at account-import time for the routes that existed then, so accounts\n" +
		"-- imported before the image route existed have no binding and the dispatcher\n" +
		"-- skips them (routeAccounts drops accounts without a binding row). This grants\n" +
		"-- every existing Dola account the image route, matching how bindAccountRoutes\n" +
		"-- would behave on a fresh import.\n" +
		"\n" +
		"INSERT INTO account_model_routes (id, account_id, model_route_id, enabled, entitled, quota_bucket_key)\n" +
		"SELECT a.id || ':' || r.id, a.id, r.id, TRUE, TRUE, r.quota_bucket_key\n" +
		"FROM provider_accounts a\n" +
		"JOIN model_routes r ON r.provider = a.pool\n" +
		"WHERE a.pool = 'dola'\n" +
		"  AND r.id = 'image.dola-image.dola'\n" +
		"ON CONFLICT (account_id, model_route_id) DO UPDATE SET\n" +
		"    enabled = TRUE,\n" +
		"    entitled = TRUE,\n" +
		"    quota_bucket_key = EXCLUDED.quota_bucket_key,\n" +
		"    updated_at = NOW();\n",
	"000017_dola_seedance25_only.sql": "-- Keep Dola limited to the single requested Seedance 2.5 video route.\n" +
		"-- Existing route rows are retained as disabled tombstones because dispatch\n" +
		"-- history references model_routes without ON DELETE CASCADE.\n" +
		"\n" +
		"UPDATE account_model_routes\n" +
		"SET enabled = FALSE,\n" +
		"    entitled = FALSE,\n" +
		"    cooldown_until = NULL,\n" +
		"    updated_at = NOW()\n" +
		"WHERE model_route_id IN (\n" +
		"    'image.dola-image.dola',\n" +
		"    'video.seedance-2.0.dola',\n" +
		"    'video.seedance-2.0-fast.dola',\n" +
		"    'video.seedance-2.0-mini.dola'\n" +
		");\n" +
		"\n" +
		"UPDATE model_routes\n" +
		"SET enabled = FALSE,\n" +
		"    updated_at = NOW()\n" +
		"WHERE provider = 'dola'\n" +
		"  AND id <> 'video.seedance-2.5.dola';\n" +
		"\n" +
		"UPDATE logical_models\n" +
		"SET enabled = FALSE,\n" +
		"    updated_at = NOW()\n" +
		"WHERE id = 'dola-image';\n" +
		"\n" +
		"UPDATE model_routes\n" +
		"SET enabled = TRUE,\n" +
		"    updated_at = NOW()\n" +
		"WHERE id = 'video.seedance-2.5.dola';\n" +
		"\n" +
		"INSERT INTO account_model_routes (\n" +
		"    id, account_id, model_route_id, enabled, entitled, quota_bucket_key\n" +
		")\n" +
		"SELECT\n" +
		"    account.id || ':' || route.id,\n" +
		"    account.id,\n" +
		"    route.id,\n" +
		"    TRUE,\n" +
		"    TRUE,\n" +
		"    route.quota_bucket_key\n" +
		"FROM provider_accounts AS account\n" +
		"JOIN model_routes AS route ON route.id = 'video.seedance-2.5.dola'\n" +
		"WHERE account.pool = 'dola'\n" +
		"ON CONFLICT (account_id, model_route_id) DO UPDATE SET\n" +
		"    enabled = TRUE,\n" +
		"    entitled = TRUE,\n" +
		"    quota_bucket_key = EXCLUDED.quota_bucket_key,\n" +
		"    cooldown_until = NULL,\n" +
		"    updated_at = NOW();\n",
	"000018_dola_daily_30s.sql": "-- Dola has two daily 30-second generations. These are local generation counts,\n" +
		"-- not estimated upstream credits. Preserve old ledgers as historical records.\n" +
		"UPDATE model_routes\n" +
		"SET quota_bucket_key = 'dola.video.daily',\n" +
		"    quota_costs = '{\"mode\":\"metered\",\"unit\":\"generations\",\"calculator\":\"dola_video\"}'::jsonb,\n" +
		"    capabilities = (SELECT jsonb_agg(jsonb_set(p, '{durations}', '[\"30s\"]'::jsonb))\n" +
		"                    FROM jsonb_array_elements(capabilities) p),\n" +
		"    updated_at = NOW()\n" +
		"WHERE provider = 'dola' AND id LIKE 'video.%';\n" +
		"\n" +
		"UPDATE account_model_routes\n" +
		"SET quota_bucket_key = 'dola.video.daily', updated_at = NOW()\n" +
		"WHERE model_route_id IN (SELECT id FROM model_routes WHERE provider = 'dola' AND id LIKE 'video.%');\n" +
		"\n" +
		"-- Some legacy installations do not yet have the retained operational log table.\n" +
		"CREATE TEMP TABLE dola_rollout_event_usage (account_id TEXT, event_id TEXT) ON COMMIT DROP;\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF to_regclass('public.event_logs') IS NOT NULL THEN\n" +
		"        EXECUTE $sql$\n" +
		"            INSERT INTO dola_rollout_event_usage\n" +
		"            SELECT to_jsonb(e)->>'account_id', e.id FROM event_logs e\n" +
		"            WHERE to_jsonb(e)->>'provider' = 'dola' AND to_jsonb(e)->>'kind' = 'video'\n" +
		"              AND to_jsonb(e)->>'status' IN ('success','pending')\n" +
		"              AND (to_jsonb(e)->>'ts')::timestamptz >= date_trunc('day', NOW() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'\n" +
		"        $sql$;\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n" +
		"\n" +
		"-- Carry today's recorded usage forward. Ambiguous old attempts remain charged;\n" +
		"-- an old quota refusal seals the day. Never replenish allowance during rollout.\n" +
		"WITH period AS (\n" +
		"    SELECT date_trunc('day', NOW() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS start_at,\n" +
		"           'dola.video.daily:' || to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS bucket_key\n" +
		"), usage AS (\n" +
		"    SELECT a.account_id, a.event_id\n" +
		"    FROM dispatch_attempts a JOIN model_routes r ON r.id = a.model_route_id, period p\n" +
		"    WHERE r.provider = 'dola' AND r.id LIKE 'video.%' AND a.started_at >= p.start_at\n" +
		"      AND (a.state IN ('created','submitting','accepted','unknown','succeeded')\n" +
		"           OR (a.state = 'failed' AND a.failure_class = 'temporary'))\n" +
		"    UNION\n" +
		"    SELECT account_id, event_id FROM dola_rollout_event_usage\n" +
		"), exhausted AS (\n" +
		"    SELECT DISTINCT a.account_id\n" +
		"    FROM dispatch_attempts a JOIN model_routes r ON r.id = a.model_route_id, period p\n" +
		"    WHERE r.provider = 'dola' AND a.started_at >= p.start_at AND a.failure_class = 'quota'\n" +
		"    UNION\n" +
		"    SELECT id FROM provider_accounts WHERE pool = 'dola' AND (status = 'quota' OR video_limited)\n" +
		")\n" +
		"INSERT INTO account_quota_buckets (id, account_id, bucket_key, unit, total, remaining, reserved, reset_at, refreshed_at)\n" +
		"SELECT a.id || ':' || p.bucket_key, a.id, p.bucket_key, 'generations', 2,\n" +
		"       CASE WHEN x.account_id IS NOT NULL THEN 0 ELSE GREATEST(0, 2 - COUNT(u.event_id)) END,\n" +
		"       0, p.start_at + INTERVAL '1 day', CASE WHEN x.account_id IS NOT NULL THEN NOW() ELSE NULL END\n" +
		"FROM provider_accounts a CROSS JOIN period p\n" +
		"LEFT JOIN usage u ON u.account_id = a.id\n" +
		"LEFT JOIN exhausted x ON x.account_id = a.id\n" +
		"WHERE a.pool = 'dola'\n" +
		"GROUP BY a.id, p.bucket_key, p.start_at, x.account_id\n" +
		"ON CONFLICT (account_id, bucket_key) DO NOTHING;\n",
	"000019_dola_public_video_model.sql": "-- Expose an explicit Dola-only model to OpenAI-compatible discovery clients.\n" +
		"-- Preserve the generic seedance-2.5 entry and its existing Dola fallback route.\n" +
		"INSERT INTO logical_models (id,kind,name,enabled)\n" +
		"VALUES ('dola-seedance-2.5','video','Dola Seedance 2.5',TRUE)\n" +
		"ON CONFLICT (id) DO NOTHING;\n" +
		"\n" +
		"WITH identity (id,logical_model_id,provider,runtime_model,upstream_model) AS (VALUES\n" +
		" ('video.dola-seedance-2.5.dola','dola-seedance-2.5','dola','dola-seedance-2.5','seedance_v2.5')\n" +
		")\n" +
		"INSERT INTO model_routes (id,logical_model_id,provider,runtime_model,upstream_model,\n" +
		"    enabled,priority,weight,quota_bucket_key,quota_costs,capabilities)\n" +
		"SELECT i.id,i.logical_model_id,i.provider,i.runtime_model,i.upstream_model,\n" +
		"    original.enabled,100,1,'dola.video.daily',\n" +
		"    '{\"mode\":\"metered\",\"unit\":\"generations\",\"calculator\":\"dola_video\"}'::jsonb,\n" +
		"    '[{\"operations\":[\"generation\"],\"ratios\":[\"16:9\",\"9:16\",\"1:1\",\"4:3\",\"3:4\"],\"resolutions\":[\"720p\"],\"durations\":[\"30s\"],\"supports_audio_output\":true}]'::jsonb\n" +
		"FROM identity i JOIN model_routes original ON original.id='video.seedance-2.5.dola'\n" +
		"ON CONFLICT (id) DO NOTHING;\n" +
		"\n" +
		"-- The two public entry points share each account's existing allowance. Copy\n" +
		"-- binding permissions/cooldowns without resetting quota or enabling disabled accounts.\n" +
		"INSERT INTO account_model_routes (id,account_id,model_route_id,enabled,entitled,\n" +
		"    quota_bucket_key,cooldown_until)\n" +
		"SELECT account_id || ':video.dola-seedance-2.5.dola',account_id,\n" +
		"    'video.dola-seedance-2.5.dola',enabled,entitled,'dola.video.daily',cooldown_until\n" +
		"FROM account_model_routes WHERE model_route_id='video.seedance-2.5.dola'\n" +
		"ON CONFLICT (account_id,model_route_id) DO NOTHING;\n",
	"000020_split_provider_public_models.sql": "-- Split merged multi-provider logical models into one public ID per provider.\n" +
		"-- Native route IDs stay stable so dispatch_attempts and account bindings keep\n" +
		"-- working. Retired merged IDs become tombstones when still referenced.\n" +
		"-- Runway and Custom are not part of this split: leftover Runway native routes\n" +
		"-- and custom.* bindings stay on the retired merged IDs and are disabled below.\n" +
		"\n" +
		"CREATE TEMP TABLE migration_000020_split (\n" +
		"    old_logical_id VARCHAR(191) NOT NULL,\n" +
		"    new_logical_id VARCHAR(191) NOT NULL,\n" +
		"    kind VARCHAR(32) NOT NULL,\n" +
		"    name VARCHAR(255) NOT NULL,\n" +
		"    native_route_id VARCHAR(191) NOT NULL\n" +
		");\n" +
		"\n" +
		"INSERT INTO migration_000020_split (old_logical_id,new_logical_id,kind,name,native_route_id) VALUES\n" +
		" ('gpt-image-2','chatgpt-gpt-image-2','image','ChatGPT GPT Image 2','image.gpt-image-2.chatgpt'),\n" +
		" ('gpt-image-2','byteplus-gpt-image-2','image','BytePlus GPT Image 2','image.gpt-image-2.byteplus'),\n" +
		" ('gpt-image-2','adobe-gpt-image-2','image','Adobe GPT Image 2','image.gpt-image-2.adobe'),\n" +
		" ('nano-banana-2','byteplus-nano-banana-2','image','BytePlus Nano Banana 2','image.nano-banana-2.byteplus'),\n" +
		" ('nano-banana-2','adobe-nano-banana-2','image','Adobe Nano Banana 2','image.nano-banana-2.adobe'),\n" +
		" ('nano-banana-pro','byteplus-nano-banana-pro','image','BytePlus Nano Banana Pro','image.nano-banana-pro.byteplus'),\n" +
		" ('nano-banana-pro','adobe-nano-banana-pro','image','Adobe Nano Banana Pro','image.nano-banana-pro.adobe'),\n" +
		" ('seedance-2.0','adobe-seedance-2.0','video','Adobe Seedance 2.0','video.seedance-2.0.adobe'),\n" +
		" ('seedance-2.0','oreate-seedance-2.0','video','Oreate Seedance 2.0','video.seedance-2.0.oreate'),\n" +
		" ('seedance-2.0-fast','adobe-seedance-2.0-fast','video','Adobe Seedance 2.0 Fast','video.seedance-2.0-fast.adobe'),\n" +
		" ('seedance-2.0-fast','oreate-seedance-2.0-fast','video','Oreate Seedance 2.0 Fast','video.seedance-2.0-fast.oreate'),\n" +
		" ('seedance-2.5','oreate-seedance-2.5','video','Oreate Seedance 2.5','video.seedance-2.5.oreate');\n" +
		"\n" +
		"INSERT INTO logical_models (id,kind,name,enabled,weight,generation_count,created_at,updated_at)\n" +
		"SELECT s.new_logical_id, s.kind, s.name, COALESCE(parent.enabled, TRUE), 0, 0,\n" +
		"       COALESCE(parent.created_at, NOW()), NOW()\n" +
		"FROM migration_000020_split s\n" +
		"LEFT JOIN logical_models parent ON parent.id = s.old_logical_id\n" +
		"ON CONFLICT (id) DO NOTHING;\n" +
		"\n" +
		"UPDATE model_routes AS route\n" +
		"SET logical_model_id = s.new_logical_id, updated_at = NOW()\n" +
		"FROM migration_000020_split s\n" +
		"WHERE route.id = s.native_route_id;\n" +
		"\n" +
		"UPDATE logical_models\n" +
		"SET enabled = FALSE, updated_at = NOW()\n" +
		"WHERE id IN (SELECT DISTINCT old_logical_id FROM migration_000020_split);\n" +
		"\n" +
		"UPDATE model_routes\n" +
		"SET enabled = FALSE, updated_at = NOW()\n" +
		"WHERE logical_model_id IN (SELECT DISTINCT old_logical_id FROM migration_000020_split);\n" +
		"\n" +
		"DELETE FROM account_model_routes\n" +
		"WHERE model_route_id IN (\n" +
		"    SELECT id FROM model_routes\n" +
		"    WHERE logical_model_id IN (SELECT DISTINCT old_logical_id FROM migration_000020_split)\n" +
		");\n" +
		"\n" +
		"DELETE FROM model_routes AS route\n" +
		"WHERE route.logical_model_id IN (SELECT DISTINCT old_logical_id FROM migration_000020_split)\n" +
		"AND NOT EXISTS (\n" +
		"    SELECT 1 FROM dispatch_attempts AS attempt WHERE attempt.model_route_id = route.id\n" +
		");\n" +
		"\n" +
		"DELETE FROM logical_models AS logical\n" +
		"WHERE logical.id IN (SELECT DISTINCT old_logical_id FROM migration_000020_split)\n" +
		"AND NOT EXISTS (\n" +
		"    SELECT 1 FROM model_routes AS route WHERE route.logical_model_id = logical.id\n" +
		");\n" +
		"\n" +
		"DROP TABLE migration_000020_split;\n",
	"000021_retire_oreate_runway_custom.sql": "-- Retire Oreate, Runway, and Custom. Delete leftover accounts and credentials.\n" +
		"-- Dispatch history stays; account_id is nulled by ON DELETE SET NULL. Routes\n" +
		"-- still referenced by dispatch_attempts remain as disabled tombstones.\n" +
		"\n" +
		"DO $migration$\n" +
		"BEGIN\n" +
		"    IF to_regclass('public.refresh_profiles') IS NOT NULL THEN\n" +
		"        DELETE FROM refresh_profiles\n" +
		"        WHERE pool IN ('oreate','runway','custom')\n" +
		"           OR id IN (SELECT id FROM provider_accounts WHERE pool IN ('oreate','runway','custom'));\n" +
		"    END IF;\n" +
		"    IF to_regclass('public.site_settings') IS NOT NULL THEN\n" +
		"        DELETE FROM site_settings\n" +
		"        WHERE key IN (\n" +
		"            'provider.oreate.enabled',\n" +
		"            'provider.runway.enabled',\n" +
		"            'provider.custom.enabled'\n" +
		"        );\n" +
		"    END IF;\n" +
		"END\n" +
		"$migration$;\n" +
		"\n" +
		"DELETE FROM provider_accounts\n" +
		"WHERE pool IN ('oreate','runway','custom');\n" +
		"\n" +
		"UPDATE model_routes\n" +
		"SET enabled = FALSE, updated_at = NOW()\n" +
		"WHERE provider IN ('oreate','runway','custom');\n" +
		"\n" +
		"UPDATE logical_models AS logical\n" +
		"SET enabled = FALSE, updated_at = NOW()\n" +
		"WHERE EXISTS (\n" +
		"    SELECT 1 FROM model_routes route\n" +
		"    WHERE route.logical_model_id = logical.id\n" +
		"      AND route.provider IN ('oreate','runway','custom')\n" +
		")\n" +
		"AND NOT EXISTS (\n" +
		"    SELECT 1 FROM model_routes route\n" +
		"    WHERE route.logical_model_id = logical.id\n" +
		"      AND route.provider NOT IN ('oreate','runway','custom')\n" +
		"      AND route.enabled = TRUE\n" +
		");\n" +
		"\n" +
		"UPDATE logical_models\n" +
		"SET enabled = FALSE, updated_at = NOW()\n" +
		"WHERE id IN (\n" +
		"    'oreate-seedance-2.0',\n" +
		"    'oreate-seedance-2.0-fast',\n" +
		"    'oreate-seedance-2.5',\n" +
		"    'seedance-2.0-mini',\n" +
		"    'seedance-1.5-pro',\n" +
		"    'runway-nano-banana-2',\n" +
		"    'runway-nano-banana-pro',\n" +
		"    'runway-gen-4.5',\n" +
		"    'runway-gen-4-turbo'\n" +
		");\n" +
		"\n" +
		"DELETE FROM account_model_routes\n" +
		"WHERE model_route_id IN (\n" +
		"    SELECT id FROM model_routes WHERE provider IN ('oreate','runway','custom')\n" +
		");\n" +
		"\n" +
		"DELETE FROM model_routes AS route\n" +
		"WHERE route.provider IN ('oreate','runway','custom')\n" +
		"AND NOT EXISTS (\n" +
		"    SELECT 1 FROM dispatch_attempts AS attempt WHERE attempt.model_route_id = route.id\n" +
		");\n" +
		"\n" +
		"DELETE FROM logical_models AS logical\n" +
		"WHERE logical.enabled = FALSE\n" +
		"  AND logical.id IN (\n" +
		"    'oreate-seedance-2.0',\n" +
		"    'oreate-seedance-2.0-fast',\n" +
		"    'oreate-seedance-2.5',\n" +
		"    'seedance-2.0-mini',\n" +
		"    'seedance-1.5-pro',\n" +
		"    'runway-nano-banana-2',\n" +
		"    'runway-nano-banana-pro',\n" +
		"    'runway-gen-4.5',\n" +
		"    'runway-gen-4-turbo'\n" +
		"  )\n" +
		"  AND NOT EXISTS (\n" +
		"      SELECT 1 FROM model_routes AS route WHERE route.logical_model_id = logical.id\n" +
		");\n",
}
