CREATE TABLE IF NOT EXISTS provider_accounts (
    id VARCHAR(64) PRIMARY KEY,
    pool VARCHAR(64) NOT NULL,
    value TEXT NOT NULL DEFAULT '',
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    fails INTEGER NOT NULL DEFAULT 0,
    fail_total INTEGER NOT NULL DEFAULT 0,
    upstream_fails INTEGER NOT NULL DEFAULT 0,
    success_total INTEGER NOT NULL DEFAULT 0,
    dead BOOLEAN NOT NULL DEFAULT FALSE,
    meta JSONB NOT NULL DEFAULT '{}'::jsonb,
    added_at TIMESTAMPTZ NULL,
    last_used_at TIMESTAMPTZ NULL,
    cached_quota_reset_after VARCHAR(128) NOT NULL DEFAULT '',
    quota_recover_at TIMESTAMPTZ NULL,
    image_limited BOOLEAN NOT NULL DEFAULT FALSE,
    video_limited BOOLEAN NOT NULL DEFAULT FALSE,
    account_email VARCHAR(255) NOT NULL DEFAULT '',
    account_display_name VARCHAR(255) NOT NULL DEFAULT '',
    weight INTEGER NOT NULL DEFAULT 0,
    concurrency INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT provider_accounts_status_valid CHECK (status IN ('active','disabled','quota','pending')),
    CONSTRAINT provider_accounts_concurrency_nonnegative CHECK (concurrency >= 0)
);

CREATE INDEX IF NOT EXISTS idx_provider_accounts_pool_status ON provider_accounts (pool, status);
CREATE INDEX IF NOT EXISTS idx_provider_accounts_last_used ON provider_accounts (last_used_at);

DO $migration$
BEGIN
    IF to_regclass('public.token_accounts') IS NOT NULL THEN
        EXECUTE $sql$
            INSERT INTO provider_accounts (
                id,pool,value,status,fails,fail_total,upstream_fails,success_total,dead,meta,
                added_at,last_used_at,cached_quota_reset_after,quota_recover_at,image_limited,
                video_limited,account_email,account_display_name,weight,concurrency,created_at,updated_at
            )
            SELECT id,pool,COALESCE(value,''),status,fails,fail_total,upstream_fails,success_total,dead,
                   COALESCE(meta,'{}'::jsonb),added_at,last_used_at,
                   COALESCE(cached_quota_reset_after,''),quota_recover_at,image_limited,
                   video_limited,COALESCE(account_email,''),COALESCE(account_display_name,''),
                   weight,concurrency,COALESCE(created_at,NOW()),
                   COALESCE(updated_at,created_at,NOW())
            FROM token_accounts
            WHERE pool IN ('chatgpt','byteplus','adobe','runway','grok','oreate','custom')
            ON CONFLICT (id) DO NOTHING
        $sql$;
    END IF;
END
$migration$;

CREATE TABLE IF NOT EXISTS logical_models (
    id VARCHAR(191) PRIMARY KEY,
    kind VARCHAR(32) NOT NULL,
    name VARCHAR(255) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    weight INTEGER NOT NULL DEFAULT 0,
    generation_count BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT logical_models_kind_valid CHECK (kind IN ('text','image','video'))
);

CREATE INDEX IF NOT EXISTS idx_logical_models_enabled_kind ON logical_models (enabled, kind);

CREATE TABLE IF NOT EXISTS model_routes (
    id VARCHAR(191) PRIMARY KEY,
    logical_model_id VARCHAR(191) NOT NULL REFERENCES logical_models(id) ON DELETE CASCADE,
    provider VARCHAR(64) NOT NULL,
    runtime_model VARCHAR(255) NOT NULL,
    upstream_model VARCHAR(255) NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    priority INTEGER NOT NULL DEFAULT 0,
    weight INTEGER NOT NULL DEFAULT 1,
    quota_bucket_key VARCHAR(128) NOT NULL DEFAULT '',
    quota_costs JSONB NOT NULL DEFAULT '{}'::jsonb,
    capabilities JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ux_route_identity UNIQUE (logical_model_id, provider, runtime_model),
    CONSTRAINT model_routes_weight_positive CHECK (weight > 0),
    CONSTRAINT model_routes_capabilities_array CHECK (jsonb_typeof(capabilities) = 'array')
);

CREATE INDEX IF NOT EXISTS idx_model_routes_logical_enabled ON model_routes (logical_model_id, enabled, priority DESC);
CREATE INDEX IF NOT EXISTS idx_model_routes_provider ON model_routes (provider, enabled);

CREATE TABLE IF NOT EXISTS account_model_routes (
    id VARCHAR(191) PRIMARY KEY,
    account_id VARCHAR(64) NOT NULL REFERENCES provider_accounts(id) ON DELETE CASCADE,
    model_route_id VARCHAR(191) NOT NULL REFERENCES model_routes(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    entitled BOOLEAN NOT NULL DEFAULT TRUE,
    quota_bucket_key VARCHAR(128) NOT NULL DEFAULT '',
    consecutive_fails INTEGER NOT NULL DEFAULT 0,
    success_total BIGINT NOT NULL DEFAULT 0,
    cooldown_until TIMESTAMPTZ NULL,
    last_failure_class VARCHAR(32) NOT NULL DEFAULT '',
    last_failure_at TIMESTAMPTZ NULL,
    last_success_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ux_account_route UNIQUE (account_id, model_route_id),
    CONSTRAINT account_model_routes_failures_nonnegative CHECK (consecutive_fails >= 0)
);

CREATE INDEX IF NOT EXISTS idx_account_model_routes_route_enabled ON account_model_routes (model_route_id, enabled, entitled);
CREATE INDEX IF NOT EXISTS idx_account_model_routes_cooldown ON account_model_routes (cooldown_until);

CREATE TABLE IF NOT EXISTS account_quota_buckets (
    id VARCHAR(191) PRIMARY KEY,
    account_id VARCHAR(64) NOT NULL REFERENCES provider_accounts(id) ON DELETE CASCADE,
    bucket_key VARCHAR(128) NOT NULL,
    unit VARCHAR(32) NOT NULL DEFAULT 'credits',
    total DOUBLE PRECISION NULL,
    remaining DOUBLE PRECISION NULL,
    reserved DOUBLE PRECISION NOT NULL DEFAULT 0,
    reset_at TIMESTAMPTZ NULL,
    revision BIGINT NOT NULL DEFAULT 0,
    refreshed_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ux_account_bucket UNIQUE (account_id, bucket_key),
    CONSTRAINT account_quota_buckets_nonnegative CHECK (
        (total IS NULL OR total >= 0) AND (remaining IS NULL OR remaining >= 0) AND reserved >= 0
    )
);

CREATE INDEX IF NOT EXISTS idx_account_quota_buckets_schedulable ON account_quota_buckets (bucket_key, remaining, reset_at);

CREATE TABLE IF NOT EXISTS dispatch_attempts (
    id VARCHAR(64) PRIMARY KEY,
    event_id VARCHAR(64) NOT NULL,
    model_route_id VARCHAR(191) NOT NULL REFERENCES model_routes(id),
    account_id VARCHAR(64) NULL REFERENCES provider_accounts(id) ON DELETE SET NULL,
    state VARCHAR(32) NOT NULL,
    failure_class VARCHAR(32) NOT NULL DEFAULT '',
    upstream_task_id VARCHAR(255) NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT dispatch_attempts_state_valid CHECK (state IN ('created','submitting','accepted','unknown','succeeded','failed')),
    CONSTRAINT dispatch_attempts_failure_valid CHECK (failure_class IN ('','auth','quota','entitlement','temporary','request','content'))
);

CREATE INDEX IF NOT EXISTS idx_dispatch_attempts_event ON dispatch_attempts (event_id, started_at);
CREATE INDEX IF NOT EXISTS idx_dispatch_attempts_account_state ON dispatch_attempts (account_id, state);

CREATE TABLE IF NOT EXISTS quota_reservations (
    id VARCHAR(64) PRIMARY KEY,
    event_id VARCHAR(64) NOT NULL,
    dispatch_attempt_id VARCHAR(64) NULL REFERENCES dispatch_attempts(id) ON DELETE SET NULL,
    quota_bucket_id VARCHAR(191) NOT NULL REFERENCES account_quota_buckets(id),
    amount DOUBLE PRECISION NOT NULL,
    status VARCHAR(32) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at TIMESTAMPTZ NULL,
    CONSTRAINT quota_reservations_amount_nonnegative CHECK (amount >= 0),
    CONSTRAINT quota_reservations_status_valid CHECK (status IN ('held','settled','released','uncertain'))
);

CREATE INDEX IF NOT EXISTS idx_quota_reservations_event ON quota_reservations (event_id);
CREATE INDEX IF NOT EXISTS idx_quota_reservations_open ON quota_reservations (quota_bucket_id, status)
    WHERE status IN ('held','uncertain');

INSERT INTO logical_models (id,kind,name,enabled,weight) VALUES
 ('gpt-5-5-mini','text','GPT-5.5 Mini',TRUE,0),
 ('gpt-5-5-thinking','text','GPT-5.5 Thinking',TRUE,0),
 ('grok-4.5','text','Grok 4.5',TRUE,0),
 ('grok-chat-fast','text','Grok Chat Fast',TRUE,0),
 ('gpt-image-2','image','GPT Image 2',TRUE,0),
 ('seedream-5.0-pro','image','Seedream 5.0 Pro',TRUE,0),
 ('seedream-5.0-lite','image','Seedream 5.0 Lite',TRUE,0),
 ('nano-banana-2','image','Nano Banana 2',TRUE,0),
 ('nano-banana-pro','image','Nano Banana Pro',TRUE,0),
 ('grok-imagine-image','image','Grok Imagine Image',TRUE,0),
 ('veo-3.1','video','Veo 3.1',TRUE,0),
 ('veo-3.1-lite','video','Veo 3.1 Lite',TRUE,0),
 ('kling-3','video','Kling 3',TRUE,0),
 ('kling-o3','video','Kling O3',TRUE,0),
 ('runway-gen-4.5','video','Runway Gen-4.5',TRUE,0),
 ('runway-gen-4-turbo','video','Runway Gen-4 Turbo',TRUE,0),
 ('seedance-2.0','video','Seedance 2.0',TRUE,0),
 ('seedance-2.0-fast','video','Seedance 2.0 Fast',TRUE,0),
 ('seedance-2.0-mini','video','Seedance 2.0 Mini',TRUE,0),
 ('seedance-1.5-pro','video','Seedance 1.5 Pro',TRUE,0),
 ('seedance-2.5','video','Seedance 2.5',TRUE,0),
 ('grok-imagine-video','video','Grok Imagine Video',TRUE,0),
 ('luma-ray','video','Luma Ray',TRUE,0),
 ('firefly-video','video','Firefly Video',TRUE,0)
ON CONFLICT (id) DO NOTHING;

-- Each capabilities value is an array of complete profiles. Request matching
-- must satisfy one profile; fields are never matched against independent unions.
INSERT INTO model_routes (id,logical_model_id,provider,runtime_model,upstream_model,priority,quota_bucket_key,capabilities) VALUES
 ('text.gpt-5-5-mini.chatgpt','gpt-5-5-mini','chatgpt','gpt-5-5-mini','gpt-5-5-mini',100,'chatgpt.text','[{"operations":["completion"]}]'),
 ('text.gpt-5-5-thinking.chatgpt','gpt-5-5-thinking','chatgpt','gpt-5-5-thinking','gpt-5-5-thinking',100,'chatgpt.text','[{"operations":["completion"]}]'),
 ('text.grok-4.5.grok','grok-4.5','grok','grok-4.5','grok-4.5',100,'grok.text','[{"operations":["completion"]}]'),
 ('text.grok-chat-fast.grok','grok-chat-fast','grok','grok-chat-fast','grok-chat-fast',100,'grok.text','[{"operations":["completion"]}]'),

 ('image.gpt-image-2.chatgpt','gpt-image-2','chatgpt','gpt-image-2','gpt-image-2',100,'chatgpt.image','[{"operations":["generation","edit"],"ratios":["1:1","16:9","9:16","4:3","3:4"],"resolutions":["1K"],"max_reference_images":6,"reference_mode":"asset"}]'),
 ('image.gpt-image-2.byteplus','gpt-image-2','byteplus','lumina-gpt-image-2','6824519374061285743',90,'byteplus.computing_points','[{"operations":["generation","edit"],"ratios":["1:1","16:9","9:16","4:3","3:4"],"resolutions":["1K","2K","4K"],"max_reference_images":14,"reference_mode":"asset"}]'),
 ('image.gpt-image-2.adobe','gpt-image-2','adobe','firefly-gpt-image-2','',80,'adobe.image','[{"operations":["generation","edit"],"ratios":["1:1","5:4","9:16","21:9","16:9","4:3","3:2","4:5","3:4","2:3"],"resolutions":["1K","2K","4K"],"max_reference_images":6,"reference_mode":"asset"}]'),
 ('image.seedream-5.0-pro.byteplus','seedream-5.0-pro','byteplus','lumina-seedream-5.0-pro','7657401949175693322',100,'byteplus.computing_points','[{"operations":["generation","edit"],"ratios":["1:1","16:9","9:16","4:3","3:4"],"resolutions":["1K","2K"],"max_reference_images":10,"reference_mode":"asset"}]'),
 ('image.seedream-5.0-lite.byteplus','seedream-5.0-lite','byteplus','lumina-seedream-5.0-lite','7604761017696141358',100,'byteplus.computing_points','[{"operations":["generation","edit"],"ratios":["1:1","16:9","9:16","4:3","3:4"],"resolutions":["2K"],"max_reference_images":10,"reference_mode":"asset"}]'),
 ('image.nano-banana-2.byteplus','nano-banana-2','byteplus','lumina-nano-banana-2','8162745039814627354',100,'byteplus.computing_points','[{"operations":["generation","edit"],"ratios":["1:1","16:9","9:16","4:3","3:4"],"resolutions":["1K","2K","4K"],"max_reference_images":14,"reference_mode":"asset"}]'),
 ('image.nano-banana-2.runway','nano-banana-2','runway','nano-banana-2','nano-banana-2',90,'runway.credits','[{"operations":["generation","edit"],"ratios":["1:1","1:4","1:8","2:3","3:2","3:4","4:1","4:3","4:5","5:4","8:1","9:16","16:9","21:9"],"resolutions":["1K","2K","4K"],"max_reference_images":6,"reference_mode":"asset"}]'),
 ('image.nano-banana-2.adobe','nano-banana-2','adobe','firefly-nano-banana-2','',80,'adobe.image','[{"operations":["generation","edit"],"ratios":["1:1","5:4","9:16","21:9","16:9","4:3","3:2","4:5","3:4","2:3"],"resolutions":["1K","2K","4K"],"max_reference_images":6,"reference_mode":"asset"}]'),
 ('image.nano-banana-pro.byteplus','nano-banana-pro','byteplus','lumina-nano-banana-pro','8162745039814627353',100,'byteplus.computing_points','[{"operations":["generation","edit"],"ratios":["1:1","16:9","9:16","4:3","3:4"],"resolutions":["1K","2K","4K"],"max_reference_images":14,"reference_mode":"asset"}]'),
 ('image.nano-banana-pro.runway','nano-banana-pro','runway','nano-banana-pro','nano-banana-pro',90,'runway.credits','[{"operations":["generation","edit"],"ratios":["1:1","1:4","1:8","2:3","3:2","3:4","4:1","4:3","4:5","5:4","8:1","9:16","16:9","21:9"],"resolutions":["1K","2K","4K"],"max_reference_images":6,"reference_mode":"asset"}]'),
 ('image.nano-banana-pro.adobe','nano-banana-pro','adobe','firefly-nano-banana-pro','',80,'adobe.image','[{"operations":["generation","edit"],"ratios":["1:1","5:4","9:16","21:9","16:9","4:3","3:2","4:5","3:4","2:3"],"resolutions":["1K","2K","4K"],"max_reference_images":6,"reference_mode":"asset"}]'),
 ('image.grok-imagine-image.grok','grok-imagine-image','grok','grok-imagine-image','grok-imagine-image',100,'grok.media','[{"operations":["generation"],"ratios":["2:3","3:2","1:1","9:16","16:9"],"resolutions":["1K"],"reference_mode":"asset"}]'),

 ('video.veo-3.1.adobe','veo-3.1','adobe','gemini-veo31','',100,'adobe.video','[{"operations":["generation"],"ratios":["16:9","9:16"],"resolutions":["720p","1080p"],"durations":["4s","6s","8s"],"max_reference_images":2,"supports_audio_output":true,"reference_mode":"frame"}]'),
 ('video.veo-3.1-lite.adobe','veo-3.1-lite','adobe','gemini-veo31-lite','',100,'adobe.video','[{"operations":["generation"],"ratios":["16:9","9:16"],"resolutions":["720p","1080p"],"durations":["4s","6s","8s"],"max_reference_images":2,"reference_mode":"frame"}]'),
 ('video.kling-3.adobe','kling-3','adobe','firefly-kling-3','',100,'adobe.video','[{"operations":["generation"],"ratios":["16:9","9:16"],"resolutions":["720p","1080p"],"durations":["3s","4s","5s","6s","7s","8s","9s","10s","11s","12s","13s","14s","15s"],"max_reference_images":1,"max_reference_videos":1,"supports_audio_output":true,"reference_mode":"frame"}]'),
 ('video.kling-o3.adobe','kling-o3','adobe','firefly-kling-o3','',100,'adobe.video','[{"operations":["generation"],"ratios":["16:9","9:16"],"resolutions":["720p","1080p"],"durations":["3s","4s","5s","6s","7s","8s","9s","10s","11s","12s","13s","14s","15s"],"max_reference_images":1,"max_reference_videos":1,"supports_audio_output":true,"reference_mode":"frame"}]'),
 ('video.runway-gen-4.5.adobe','runway-gen-4.5','adobe','firefly-runway-4.5','',100,'adobe.video','[{"operations":["generation"],"ratios":["16:9"],"resolutions":["720p"],"durations":["5s","8s","10s"],"max_reference_images":1,"reference_mode":"frame"}]'),
 ('video.runway-gen-4-turbo.runway','runway-gen-4-turbo','runway','runway-gen4-turbo','gen4_turbo',100,'runway.credits','[{"operations":["generation"],"ratios":["16:9","9:16","1:1","4:3","3:4","21:9"],"resolutions":["720p"],"durations":["5s","10s"],"max_reference_images":1,"reference_mode":"frame","requires_reference":true}]'),
 ('video.seedance-2.0.adobe','seedance-2.0','adobe','firefly-seedance-2','',100,'adobe.video','[{"operations":["generation"],"ratios":["16:9","9:16"],"resolutions":["480p","720p","1080p"],"durations":["4s","5s","6s","7s","8s","9s","10s","11s","12s","13s","14s","15s"],"max_reference_images":9,"max_reference_videos":3,"max_reference_audios":3,"max_reference_media":9,"supports_audio_output":true,"reference_mode":"asset"}]'),
 ('video.seedance-2.0.oreate','seedance-2.0','oreate','oreate-seedance-2.0','seedance-2.0',90,'oreate.points','[{"operations":["generation"],"ratios":["16:9","1:1","3:4","4:3","9:16","21:9"],"resolutions":["480p","720p","1080p"],"durations":["5s","10s"],"max_reference_images":9,"max_reference_videos":3,"max_reference_media":12,"supports_audio_output":true,"reference_mode":"asset"}]'),
 ('video.seedance-2.0-fast.adobe','seedance-2.0-fast','adobe','firefly-seedance-2-fast','',100,'adobe.video','[{"operations":["generation"],"ratios":["16:9","9:16"],"resolutions":["480p","720p","1080p"],"durations":["4s","5s","6s","7s","8s","9s","10s","11s","12s","13s","14s","15s"],"max_reference_images":9,"max_reference_videos":3,"max_reference_audios":3,"max_reference_media":9,"supports_audio_output":true,"reference_mode":"asset"}]'),
 ('video.seedance-2.0-fast.oreate','seedance-2.0-fast','oreate','oreate-seedance-2.0-fast','seedance-2.0-fast',90,'oreate.points','[{"operations":["generation"],"ratios":["16:9","1:1","3:4","4:3","9:16","21:9"],"resolutions":["480p","720p"],"durations":["5s","10s"],"max_reference_images":9,"max_reference_videos":3,"max_reference_media":12,"supports_audio_output":true,"reference_mode":"asset"}]'),
 ('video.seedance-2.0-mini.oreate','seedance-2.0-mini','oreate','oreate-seedance-2.0-mini','seedance-2.0-mini',100,'oreate.points','[{"operations":["generation"],"ratios":["16:9","1:1","3:4","4:3","9:16","21:9"],"resolutions":["480p","720p"],"durations":["5s","10s"],"max_reference_images":9,"max_reference_videos":3,"max_reference_media":12,"supports_audio_output":true,"reference_mode":"asset"}]'),
 ('video.seedance-1.5-pro.oreate','seedance-1.5-pro','oreate','oreate-seedance-1.5-pro','seedance-1.5-pro',100,'oreate.points','[{"operations":["generation"],"ratios":["16:9","1:1","3:4","4:3","9:16","21:9"],"resolutions":["480p","720p","1080p"],"durations":["5s","10s"],"max_reference_images":2,"max_reference_media":2,"supports_audio_output":true,"reference_mode":"frame"}]'),
 ('video.seedance-2.5.oreate','seedance-2.5','oreate','oreate-seedance-2.5','seedance-2.5',100,'oreate.points','[{"operations":["generation"],"ratios":["16:9","1:1","3:4","4:3","9:16","21:9"],"resolutions":["480p","720p"],"durations":["5s","10s","20s","30s"],"max_reference_images":9,"max_reference_videos":3,"max_reference_media":12,"supports_audio_output":true,"reference_mode":"asset"}]'),
 ('video.grok-imagine-video.grok','grok-imagine-video','grok','grok-video','grok-imagine-video',100,'grok.media','[{"operations":["generation"],"ratios":["2:3","3:2","1:1","9:16","16:9"],"resolutions":["720p"],"durations":["6s","10s"],"max_reference_images":6,"reference_mode":"asset"}]'),
 ('video.luma-ray.adobe','luma-ray','adobe','firefly-ray','',100,'adobe.video','[{"operations":["generation"],"ratios":["21:9","16:9","4:3","1:1","3:4","9:16","9:21"],"resolutions":["720p","1080p","4K"],"durations":["5s"],"max_reference_images":2,"max_reference_videos":1,"reference_mode":"frame"}]'),
 ('video.firefly-video.adobe','firefly-video','adobe','firefly-video','',100,'adobe.video','[{"operations":["generation"],"ratios":["16:9","1:1","9:16"],"resolutions":["540p","720p","1080p"],"durations":["5s"],"max_reference_images":2,"max_reference_videos":1,"reference_mode":"frame"}]')
ON CONFLICT (id) DO NOTHING;

-- Existing accounts initially inherit every route in their provider pool. A
-- later live entitlement refresh can disable only the rejected account-route.
INSERT INTO account_model_routes (id,account_id,model_route_id,enabled,entitled,quota_bucket_key)
SELECT a.id || ':' || r.id, a.id, r.id, TRUE, TRUE, r.quota_bucket_key
FROM provider_accounts a
JOIN model_routes r ON r.provider = a.pool
ON CONFLICT (account_id,model_route_id) DO NOTHING;

-- Seed one shared quota row per real provider allowance domain, never one row
-- per logical model. The JSON cache is only a migration snapshot; live refresh
-- replaces it after every generation.
INSERT INTO account_quota_buckets (id,account_id,bucket_key,unit,total,remaining,reserved,reset_at,revision,refreshed_at)
SELECT DISTINCT ON (a.id, r.quota_bucket_key)
       a.id || ':' || r.quota_bucket_key,
       a.id,
       r.quota_bucket_key,
       'credits',
       CASE WHEN (a.meta->>'cached_quota_total') ~ '^[0-9]+(\.[0-9]+)?$' THEN (a.meta->>'cached_quota_total')::double precision END,
       CASE WHEN (a.meta->>'cached_quota_remaining') ~ '^[0-9]+(\.[0-9]+)?$' THEN (a.meta->>'cached_quota_remaining')::double precision END,
       COALESCE(CASE WHEN (a.meta->>'cached_quota_reserved') ~ '^[0-9]+(\.[0-9]+)?$' THEN (a.meta->>'cached_quota_reserved')::double precision END,0),
       a.quota_recover_at,
       1,
       a.updated_at
FROM provider_accounts a
JOIN model_routes r ON r.provider = a.pool
WHERE r.quota_bucket_key <> ''
ON CONFLICT (account_id,bucket_key) DO NOTHING;

-- Merge legacy successful counters onto canonical ids without retaining any
-- removed public alias at runtime.
DO $migration$
BEGIN
    IF to_regclass('public.event_logs') IS NOT NULL THEN
        EXECUTE $sql$
            UPDATE logical_models lm SET generation_count = history.count
            FROM (
                SELECT canonical_id, COUNT(*)::bigint AS count
                FROM (
                    SELECT CASE model
                        WHEN 'lumina-gpt-image-2' THEN 'gpt-image-2'
                        WHEN 'firefly-gpt-image-2' THEN 'gpt-image-2'
                        WHEN 'lumina-seedream-5.0-pro' THEN 'seedream-5.0-pro'
                        WHEN 'lumina-seedream-5.0-lite' THEN 'seedream-5.0-lite'
                        WHEN 'lumina-nano-banana-2' THEN 'nano-banana-2'
                        WHEN 'firefly-nano-banana-2' THEN 'nano-banana-2'
                        WHEN 'lumina-nano-banana-pro' THEN 'nano-banana-pro'
                        WHEN 'firefly-nano-banana-pro' THEN 'nano-banana-pro'
                        WHEN 'gemini-veo31' THEN 'veo-3.1'
                        WHEN 'gemini-veo31-lite' THEN 'veo-3.1-lite'
                        WHEN 'firefly-kling-3' THEN 'kling-3'
                        WHEN 'firefly-kling-o3' THEN 'kling-o3'
                        WHEN 'firefly-runway-4.5' THEN 'runway-gen-4.5'
                        WHEN 'runway-gen4-turbo' THEN 'runway-gen-4-turbo'
                        WHEN 'firefly-seedance-2' THEN 'seedance-2.0'
                        WHEN 'oreate-seedance-2.0' THEN 'seedance-2.0'
                        WHEN 'firefly-seedance-2-fast' THEN 'seedance-2.0-fast'
                        WHEN 'oreate-seedance-2.0-fast' THEN 'seedance-2.0-fast'
                        WHEN 'oreate-seedance-2.0-mini' THEN 'seedance-2.0-mini'
                        WHEN 'oreate-seedance-1.5-pro' THEN 'seedance-1.5-pro'
                        WHEN 'oreate-seedance-2.5' THEN 'seedance-2.5'
                        WHEN 'grok-video' THEN 'grok-imagine-video'
                        WHEN 'firefly-ray' THEN 'luma-ray'
                        ELSE model
                    END AS canonical_id
                    FROM event_logs WHERE status = 'success'
                ) mapped
                WHERE canonical_id IN (SELECT id FROM logical_models)
                GROUP BY canonical_id
            ) history
            WHERE lm.id = history.canonical_id AND lm.generation_count = 0
        $sql$;
    END IF;
END
$migration$;

-- Provider accounts are a closed control-plane set. Run this after the routing
-- and quota foreign keys exist so cascades clean any manually pre-created
-- bindings/buckets while dispatch history safely keeps a NULL account id.
DELETE FROM provider_accounts
WHERE pool NOT IN ('chatgpt','byteplus','adobe','runway','grok','oreate','custom');
