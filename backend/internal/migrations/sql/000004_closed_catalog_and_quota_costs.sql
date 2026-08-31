-- Every route has an explicit allowance-cost policy. Unknown means that the
-- provider exposes a balance but not a trustworthy per-request price; it is
-- refreshed after each attempt without inventing a one-credit reservation.
UPDATE model_routes
SET quota_costs = CASE
    WHEN provider = 'custom'
        THEN '{"mode":"unmetered"}'::jsonb
    WHEN provider = 'byteplus' AND id LIKE 'image.%'
        THEN '{"mode":"metered","unit":"points","calculator":"byteplus_image"}'::jsonb
    WHEN provider = 'oreate' AND id LIKE 'video.%'
        THEN '{"mode":"metered","unit":"points","calculator":"oreate_seedance"}'::jsonb
    WHEN provider = 'runway' AND id LIKE 'video.%'
        THEN '{"mode":"metered","unit":"credits","calculator":"per_second","per_second":5}'::jsonb
    WHEN provider = 'chatgpt' AND id LIKE 'image.%'
        THEN '{"mode":"metered","unit":"generations","calculator":"fixed","value":1}'::jsonb
    ELSE '{"mode":"unknown","unit":"credits"}'::jsonb
END,
updated_at = NOW();

-- Adobe's credits endpoint returns one account-wide Firefly allowance. Merge
-- the old image/video scheduler buckets without making that balance spendable
-- twice. The newest snapshot supplies the upstream value; every in-flight hold
-- across all three historical buckets remains held in the shared bucket.
WITH candidates AS (
    SELECT *
    FROM account_quota_buckets
    WHERE bucket_key IN ('adobe.image', 'adobe.video', 'adobe.credits')
), latest AS (
    SELECT DISTINCT ON (account_id)
           account_id, unit, total, remaining, reserved, reset_at, revision,
           refreshed_at, created_at, updated_at
    FROM candidates
    ORDER BY account_id, refreshed_at DESC NULLS LAST, updated_at DESC
), held AS (
    SELECT account_id, SUM(reserved) AS reserved
    FROM candidates
    GROUP BY account_id
)
INSERT INTO account_quota_buckets (
    id, account_id, bucket_key, unit, total, remaining, reserved, reset_at,
    revision, refreshed_at, created_at, updated_at
)
SELECT latest.account_id || ':adobe.credits', latest.account_id,
       'adobe.credits', latest.unit, latest.total,
       CASE WHEN latest.remaining IS NULL THEN NULL
            ELSE GREATEST(0, latest.remaining + latest.reserved - held.reserved)
       END,
       held.reserved, latest.reset_at, latest.revision + 1,
       latest.refreshed_at, latest.created_at, NOW()
FROM latest
JOIN held USING (account_id)
ON CONFLICT (account_id, bucket_key) DO UPDATE SET
    unit = EXCLUDED.unit,
    total = EXCLUDED.total,
    remaining = EXCLUDED.remaining,
    reserved = EXCLUDED.reserved,
    reset_at = EXCLUDED.reset_at,
    revision = EXCLUDED.revision,
    refreshed_at = EXCLUDED.refreshed_at,
    updated_at = NOW();

-- Do not assume the shared row uses the conventional account:key primary key.
-- Older/manual rows can have another id even though (account_id,bucket_key) is
-- unique; always redirect reservations to the actual conflict winner.
UPDATE quota_reservations reservation
SET quota_bucket_id = target.id,
    updated_at = NOW()
FROM account_quota_buckets source
JOIN account_quota_buckets target
  ON target.account_id = source.account_id
 AND target.bucket_key = 'adobe.credits'
WHERE reservation.quota_bucket_id = source.id
  AND source.bucket_key IN ('adobe.image', 'adobe.video');

DELETE FROM account_quota_buckets
WHERE bucket_key IN ('adobe.image', 'adobe.video');

UPDATE model_routes
SET quota_bucket_key = 'adobe.credits', updated_at = NOW()
WHERE provider = 'adobe';

UPDATE account_model_routes binding
SET quota_bucket_key = 'adobe.credits', updated_at = NOW()
FROM model_routes route
WHERE binding.model_route_id = route.id
  AND route.provider = 'adobe';

-- This exact route table is the executable built-in catalog. Matching only the
-- logical id is insufficient: a retired provider route can otherwise hide
-- under a canonical model and outrank its supported routes.
CREATE TEMP TABLE migration_000004_canonical_routes (
    id VARCHAR(191) PRIMARY KEY,
    logical_model_id VARCHAR(191) NOT NULL,
    provider VARCHAR(64) NOT NULL,
    runtime_model VARCHAR(255) NOT NULL,
    upstream_model VARCHAR(255) NOT NULL
);

INSERT INTO migration_000004_canonical_routes
    (id, logical_model_id, provider, runtime_model, upstream_model)
VALUES
 ('text.gpt-5-5-mini.chatgpt','gpt-5-5-mini','chatgpt','gpt-5-5-mini','gpt-5-5-mini'),
 ('text.gpt-5-5-thinking.chatgpt','gpt-5-5-thinking','chatgpt','gpt-5-5-thinking','gpt-5-5-thinking'),
 ('text.grok-4.5.grok','grok-4.5','grok','grok-4.5','grok-4.5'),
 ('text.grok-chat-fast.grok','grok-chat-fast','grok','grok-chat-fast','grok-chat-fast'),
 ('image.gpt-image-2.chatgpt','gpt-image-2','chatgpt','gpt-image-2','gpt-image-2'),
 ('image.gpt-image-2.byteplus','gpt-image-2','byteplus','lumina-gpt-image-2','6824519374061285743'),
 ('image.gpt-image-2.adobe','gpt-image-2','adobe','firefly-gpt-image-2',''),
 ('image.seedream-5.0-pro.byteplus','seedream-5.0-pro','byteplus','lumina-seedream-5.0-pro','7657401949175693322'),
 ('image.seedream-5.0-lite.byteplus','seedream-5.0-lite','byteplus','lumina-seedream-5.0-lite','7604761017696141358'),
 ('image.nano-banana-2.byteplus','nano-banana-2','byteplus','lumina-nano-banana-2','8162745039814627354'),
 ('image.nano-banana-2.runway','nano-banana-2','runway','nano-banana-2','nano-banana-2'),
 ('image.nano-banana-2.adobe','nano-banana-2','adobe','firefly-nano-banana-2',''),
 ('image.nano-banana-pro.byteplus','nano-banana-pro','byteplus','lumina-nano-banana-pro','8162745039814627353'),
 ('image.nano-banana-pro.runway','nano-banana-pro','runway','nano-banana-pro','nano-banana-pro'),
 ('image.nano-banana-pro.adobe','nano-banana-pro','adobe','firefly-nano-banana-pro',''),
 ('image.grok-imagine-image.grok','grok-imagine-image','grok','grok-imagine-image','grok-imagine-image'),
 ('video.veo-3.1.adobe','veo-3.1','adobe','gemini-veo31',''),
 ('video.veo-3.1-lite.adobe','veo-3.1-lite','adobe','gemini-veo31-lite',''),
 ('video.kling-3.adobe','kling-3','adobe','firefly-kling-3',''),
 ('video.kling-o3.adobe','kling-o3','adobe','firefly-kling-o3',''),
 ('video.runway-gen-4.5.adobe','runway-gen-4.5','adobe','firefly-runway-4.5',''),
 ('video.runway-gen-4-turbo.runway','runway-gen-4-turbo','runway','runway-gen4-turbo','gen4_turbo'),
 ('video.seedance-2.0.adobe','seedance-2.0','adobe','firefly-seedance-2',''),
 ('video.seedance-2.0.oreate','seedance-2.0','oreate','oreate-seedance-2.0','seedance-2.0'),
 ('video.seedance-2.0-fast.adobe','seedance-2.0-fast','adobe','firefly-seedance-2-fast',''),
 ('video.seedance-2.0-fast.oreate','seedance-2.0-fast','oreate','oreate-seedance-2.0-fast','seedance-2.0-fast'),
 ('video.seedance-2.0-mini.oreate','seedance-2.0-mini','oreate','oreate-seedance-2.0-mini','seedance-2.0-mini'),
 ('video.seedance-1.5-pro.oreate','seedance-1.5-pro','oreate','oreate-seedance-1.5-pro','seedance-1.5-pro'),
 ('video.seedance-2.5.oreate','seedance-2.5','oreate','oreate-seedance-2.5','seedance-2.5'),
 ('video.grok-imagine-video.grok','grok-imagine-video','grok','grok-video','grok-imagine-video'),
 ('video.luma-ray.adobe','luma-ray','adobe','firefly-ray',''),
 ('video.firefly-video.adobe','firefly-video','adobe','firefly-video','');

-- Legacy custom accounts stored a comma-separated meta.models list. Missing or
-- blank meant "all models"; unknown/retired names are deliberately ignored.
-- Materialize the entitlement set before scrubbing the legacy credential table.
CREATE TEMP TABLE migration_000004_custom_bindings AS
SELECT account.id AS account_id, canonical.logical_model_id
FROM provider_accounts account
CROSS JOIN (
    SELECT DISTINCT logical_model_id
    FROM migration_000004_canonical_routes
) canonical
WHERE account.pool = 'custom'
  AND (
      NULLIF(BTRIM(COALESCE(account.meta->>'models', '')), '') IS NULL
      OR EXISTS (
          SELECT 1
          FROM regexp_split_to_table(account.meta->>'models', ',') requested(model_id)
          WHERE LOWER(BTRIM(requested.model_id)) = canonical.logical_model_id
      )
  );

-- A custom route exposes the union of the canonical model's complete native
-- capability profiles. Existing administrator-disabled custom routes stay
-- disabled; the upsert repairs only their immutable identity/policy fields.
WITH distinct_profiles AS (
    SELECT DISTINCT route.logical_model_id, profile.value AS profile
    FROM model_routes route
    JOIN migration_000004_canonical_routes canonical
      ON canonical.id = route.id
     AND canonical.logical_model_id = route.logical_model_id
     AND canonical.provider = route.provider
     AND canonical.runtime_model = route.runtime_model
     AND canonical.upstream_model = route.upstream_model
    CROSS JOIN LATERAL jsonb_array_elements(route.capabilities) profile(value)
), aggregate_profiles AS (
    SELECT logical_model_id,
           jsonb_agg(profile ORDER BY profile::text) AS capabilities
    FROM distinct_profiles
    GROUP BY logical_model_id
), requested_models AS (
    SELECT DISTINCT logical_model_id
    FROM migration_000004_custom_bindings
)
INSERT INTO model_routes (
    id, logical_model_id, provider, runtime_model, upstream_model, enabled,
    priority, weight, quota_bucket_key, quota_costs, capabilities
)
SELECT 'custom.' || requested.logical_model_id,
       requested.logical_model_id,
       'custom',
       requested.logical_model_id,
       requested.logical_model_id,
       TRUE,
       10,
       1,
       'custom.unmetered',
       '{"mode":"unmetered"}'::jsonb,
       aggregate.capabilities
FROM requested_models requested
JOIN aggregate_profiles aggregate USING (logical_model_id)
ON CONFLICT (id) DO UPDATE SET
    logical_model_id = EXCLUDED.logical_model_id,
    provider = EXCLUDED.provider,
    runtime_model = EXCLUDED.runtime_model,
    upstream_model = EXCLUDED.upstream_model,
    quota_bucket_key = EXCLUDED.quota_bucket_key,
    quota_costs = EXCLUDED.quota_costs,
    capabilities = EXCLUDED.capabilities,
    updated_at = NOW();

INSERT INTO account_model_routes (
    id, account_id, model_route_id, enabled, entitled, quota_bucket_key
)
SELECT binding.account_id || ':custom.' || binding.logical_model_id,
       binding.account_id,
       'custom.' || binding.logical_model_id,
       TRUE,
       TRUE,
       'custom.unmetered'
FROM migration_000004_custom_bindings binding
JOIN model_routes route
  ON route.id = 'custom.' || binding.logical_model_id
ON CONFLICT (account_id, model_route_id) DO NOTHING;

-- Freeze the invalid-route set independently of enabled state. A supported
-- canonical route may already be disabled by the administrator and must remain
-- present, disabled, with its entitlement state unchanged.
CREATE TEMP TABLE migration_000004_invalid_routes AS
SELECT route.id
FROM model_routes route
WHERE NOT EXISTS (
    SELECT 1
    FROM migration_000004_canonical_routes canonical
    WHERE canonical.id = route.id
      AND canonical.logical_model_id = route.logical_model_id
      AND canonical.provider = route.provider
      AND canonical.runtime_model = route.runtime_model
      AND canonical.upstream_model = route.upstream_model
)
AND NOT (
    route.provider = 'custom'
    AND route.id = 'custom.' || route.logical_model_id
    AND route.runtime_model = route.logical_model_id
    AND route.upstream_model = route.logical_model_id
    AND EXISTS (
        SELECT 1
        FROM migration_000004_canonical_routes canonical
        WHERE canonical.logical_model_id = route.logical_model_id
    )
);

UPDATE account_model_routes binding
SET enabled = FALSE, entitled = FALSE, updated_at = NOW()
FROM migration_000004_invalid_routes invalid
WHERE binding.model_route_id = invalid.id;

UPDATE model_routes route
SET enabled = FALSE, updated_at = NOW()
FROM migration_000004_invalid_routes invalid
WHERE route.id = invalid.id;

DELETE FROM model_routes route
USING migration_000004_invalid_routes invalid
WHERE route.id = invalid.id
  AND NOT EXISTS (
      SELECT 1
      FROM dispatch_attempts attempt
      WHERE attempt.model_route_id = route.id
  );

UPDATE logical_models
SET enabled = FALSE, updated_at = NOW()
WHERE id NOT IN (
    SELECT DISTINCT logical_model_id
    FROM migration_000004_canonical_routes
);

DELETE FROM logical_models logical
WHERE logical.enabled = FALSE
  AND logical.id NOT IN (
      SELECT DISTINCT logical_model_id
      FROM migration_000004_canonical_routes
  )
  AND NOT EXISTS (
      SELECT 1
      FROM model_routes route
      WHERE route.logical_model_id = logical.id
  );

-- Retired provider rows with immutable quota history remain FK-safe tombstones,
-- but no retired plaintext credential or provider metadata is retained.
UPDATE provider_accounts
SET status = 'disabled',
    dead = TRUE,
    value = '',
    meta = '{}'::jsonb,
    account_email = '',
    account_display_name = '',
    updated_at = NOW()
WHERE pool NOT IN ('chatgpt','byteplus','adobe','runway','grok','oreate','custom');

DELETE FROM provider_accounts account
WHERE account.pool NOT IN ('chatgpt','byteplus','adobe','runway','grok','oreate','custom')
  AND NOT EXISTS (
      SELECT 1
      FROM account_quota_buckets bucket
      JOIN quota_reservations reservation
        ON reservation.quota_bucket_id = bucket.id
      WHERE bucket.account_id = account.id
  );

-- Adobe cookie refresh is the only retained refresh-profile workflow. Delete
-- retired provider/profile rows so their cookies and errors cannot leak through
-- an otherwise unused legacy table.
DO $migration$
BEGIN
    IF to_regclass('public.refresh_profiles') IS NOT NULL THEN
        EXECUTE $sql$
            DELETE FROM refresh_profiles
            WHERE pool <> 'adobe' OR kind <> 'adobe_cookie'
        $sql$;
    END IF;
END
$migration$;

-- event_logs.user_id was needed only for the one-time API credential mapping
-- in 000003. The 2API EventLog model no longer declares legacy user identity;
-- remove it (and older user_name variants) before dropping users.
ALTER TABLE event_logs
    DROP COLUMN IF EXISTS user_id,
    DROP COLUMN IF EXISTS user_name;

-- The retained banned-word hit fields now mean API credential id/name. Rows
-- created by the legacy application contain ordinary-user identity, so clear
-- only those snapshots before the new application starts writing 2API hits.
DO $migration$
BEGIN
    IF to_regclass('public.banned_word_hits') IS NOT NULL THEN
        IF EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'public'
              AND table_name = 'banned_word_hits'
              AND column_name = 'user_id'
        ) THEN
            EXECUTE 'UPDATE banned_word_hits SET user_id = ''''';
        END IF;
        IF EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'public'
              AND table_name = 'banned_word_hits'
              AND column_name = 'user_name'
        ) THEN
            EXECUTE 'UPDATE banned_word_hits SET user_name = ''''';
        END IF;
    END IF;
END
$migration$;

-- Identity attribution and provider-account copying are complete. These four
-- legacy tables are not present in AutoMigrateModels and no runtime repository
-- reads them. Drop them in dependency order so retired user password hashes,
-- duplicate API-key hashes, obsolete model names, and duplicate account
-- credentials/PII cannot survive as a dormant rollback data plane.
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS model_configs;
DROP TABLE IF EXISTS token_accounts;

ALTER TABLE model_routes
    ADD CONSTRAINT model_routes_quota_costs_valid CHECK (
        quota_costs IS NOT NULL
        AND jsonb_typeof(quota_costs) = 'object'
        AND quota_costs->>'mode' IN ('metered', 'unknown', 'unmetered')
        AND (
            quota_costs->>'mode' = 'unmetered'
            OR NULLIF(BTRIM(quota_costs->>'unit'), '') IS NOT NULL
        )
        AND (
            quota_costs->>'mode' <> 'metered'
            OR quota_costs->>'calculator' IN (
                'fixed', 'per_second', 'byteplus_image', 'oreate_seedance'
            )
        )
    );

DROP TABLE migration_000004_invalid_routes;
DROP TABLE migration_000004_custom_bindings;
DROP TABLE migration_000004_canonical_routes;
