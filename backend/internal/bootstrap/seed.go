package bootstrap

import (
	"context"

	"backend/internal/model"
	"gorm.io/gorm"
)

func seedDefaults(ctx context.Context, db *gorm.DB) error {
	defaults := []model.SiteSetting{
		{Key: "site.title", Value: "Vivid"},
		{Key: "site.logo", Value: ""},
		{Key: "site.subtitle", Value: ""},
		{Key: "contact.qq", Value: "1114639355"},
		{Key: "contact.qq_link", Value: "https://qm.qq.com/q/ItgCcNA7ac"},
		{Key: "contact.qq_group", Value: "1106849765"},
		{Key: "contact.qq_group_link", Value: "https://qm.qq.com/q/976LeMFoHu"},
		{Key: "contact.email", Value: "vividairun@gmail.com"},
		{Key: "contact.shop", Value: "https://pay.ldxp.cn/shop/chiyi"},
		{Key: "auth.open", Value: "true"},
		{Key: "auth.email_code", Value: "false"},
		{Key: "auth.allow_password_reset", Value: "false"},
		{Key: "auth.allowed_email_domains", Value: ""},
		{Key: "auth.code_ttl_seconds", Value: "600"},
		{Key: "smtp.host", Value: ""},
		{Key: "smtp.port", Value: "587"},
		{Key: "smtp.username", Value: ""},
		{Key: "smtp.password", Value: ""},
		{Key: "smtp.from_addr", Value: ""},
		{Key: "smtp.use_tls", Value: "true"},
		{Key: "proxy.url", Value: ""},
		{Key: "credits.checkin_enabled", Value: "true"},
		{Key: "credits.checkin_reward", Value: "3"},
		{Key: "credits.invite_enabled", Value: "true"},
		{Key: "credits.invite_reward", Value: "3"},
		{Key: "credits.cdk_redeem_enabled", Value: "true"},
		{Key: "pay.enabled", Value: "false"},
		{Key: "pay.api_base", Value: "https://pay.v8jisu.cn/api/pay"},
		{Key: "pay.methods", Value: "wxpay,alipay"},
		{Key: "pay.min_amount", Value: "1"},
		{Key: "pay.points_ratio", Value: "100"},
		{Key: "logs.retention_days", Value: "30"},
		{Key: "media.retention_days", Value: "30"},
	}
	for _, item := range defaults {
		var count int64
		if err := db.WithContext(ctx).Model(&model.SiteSetting{}).Where("key = ?", item.Key).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if err := db.WithContext(ctx).Create(&item).Error; err != nil {
			return err
		}
	}
	// Built-in Adobe video capabilities are catalog-owned (the admin form shows
	// them read-only). Backfill existing model rows after adding the media columns,
	// so an upgrade behaves the same as adding the model on a fresh install.
	if err := db.WithContext(ctx).Exec(`UPDATE model_configs SET
		max_reference_images = CASE id
			WHEN 'firefly-seedance-2' THEN 9 WHEN 'firefly-seedance-2-fast' THEN 9
			WHEN 'firefly-video' THEN 2
			ELSE max_reference_images END,
		max_reference_videos = CASE id
			WHEN 'firefly-kling-3' THEN 1 WHEN 'firefly-kling-o3' THEN 1
			WHEN 'firefly-seedance-2' THEN 3 WHEN 'firefly-seedance-2-fast' THEN 3
			WHEN 'firefly-ray' THEN 1 WHEN 'firefly-video' THEN 1 ELSE max_reference_videos END,
		max_reference_audios = CASE id
			WHEN 'firefly-seedance-2' THEN 3 WHEN 'firefly-seedance-2-fast' THEN 3 ELSE max_reference_audios END,
		max_reference_media = CASE id
			WHEN 'firefly-seedance-2' THEN 9 WHEN 'firefly-seedance-2-fast' THEN 9
			ELSE max_reference_media END,
		reference_mode = CASE id
			WHEN 'firefly-seedance-2' THEN 'asset' WHEN 'firefly-seedance-2-fast' THEN 'asset'
			ELSE reference_mode END,
		supports_audio_output = CASE id
			WHEN 'gemini-veo31' THEN true WHEN 'firefly-kling-3' THEN true
			WHEN 'firefly-kling-o3' THEN true WHEN 'firefly-seedance-2' THEN true
			WHEN 'firefly-seedance-2-fast' THEN true ELSE supports_audio_output END
		WHERE id IN ('gemini-veo31','firefly-kling-3','firefly-kling-o3','firefly-seedance-2',
			'firefly-seedance-2-fast','firefly-ray','firefly-video')`).Error; err != nil {
		return err
	}
	// Grok Imagine fast mode supports the same five aspect ratios exposed by
	// the current web client. Older installs advertised only 1:1, which caused
	// the gateway to snap every downstream size to a square and omit the ratio
	// from the upstream mediaGenInput payload.
	if err := db.WithContext(ctx).Exec(`UPDATE model_configs
		SET ratios = '["2:3","3:2","1:1","9:16","16:9"]'::jsonb,
			resolutions = '["1K"]'::jsonb
		WHERE id = 'grok-imagine-image' AND provider = 'grok'`).Error; err != nil {
		return err
	}
	// Grok video is a built-in provider capability. Older installations had the
	// entry only in the HTTP catalog, which meant /managed-models, the API model
	// list, and the account test dialog could not use it. Insert it once while
	// preserving any administrator-customized row/pricing on subsequent starts.
	if err := db.WithContext(ctx).Exec(`INSERT INTO model_configs
		(id, type, name, alias, provider, enabled, ratios, prices, resolutions,
		 image_to_image, duration_prices, prices_agent, duration_prices_agent,
		 durations, max_reference_images, max_reference_videos, max_reference_audios,
		 max_reference_media, supports_audio_output, reference_mode, upstream_model,
		 weight, generation_count, created_at, updated_at)
		VALUES ('grok-video', 'video', 'Grok Imagine video', '', 'grok', true,
		 '["2:3","3:2","1:1","9:16","16:9"]'::jsonb,
		 '{"720p":0}'::jsonb, '["720p"]'::jsonb, false,
		 '{"6s":10,"10s":15}'::jsonb, '{}'::jsonb, '{}'::jsonb,
		 '["6s","10s"]'::jsonb, 6, 0, 0, 0, false, 'asset', '',
			0, 0, now(), now()) ON CONFLICT (id) DO NOTHING`).Error; err != nil {
		return err
	}
	// BytePlus Lumina publishes these five image models. Namespaced public IDs
	// avoid collisions with the ChatGPT and Runway catalogs, while upstream_model
	// retains the numeric Lumina service ID used by the provider API.
	if err := db.WithContext(ctx).Exec(`INSERT INTO model_configs
		(id, type, name, alias, provider, enabled, ratios, prices, resolutions,
		 image_to_image, duration_prices, prices_agent, duration_prices_agent,
		 durations, max_reference_images, max_reference_videos, max_reference_audios,
		 max_reference_media, supports_audio_output, reference_mode, upstream_model,
		 weight, generation_count, created_at, updated_at)
		VALUES
		 ('lumina-seedream-5.0-pro', 'image', 'Seedream 5.0 Pro', '', 'byteplus', true,
		  '["1:1","16:9","9:16","4:3","3:4"]'::jsonb,
		  '{"1K":0,"2K":0}'::jsonb, '["1K","2K"]'::jsonb, true,
		  '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, '[]'::jsonb,
		  10, 0, 0, 0, false, 'asset', '7657401949175693322', 0, 0, now(), now()),
		 ('lumina-gpt-image-2', 'image', 'GPT Image 2 (Beta)', '', 'byteplus', true,
		  '["1:1","16:9","9:16","4:3","3:4"]'::jsonb,
		  '{"1K":0,"2K":0,"4K":0}'::jsonb, '["1K","2K","4K"]'::jsonb, true,
		  '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, '[]'::jsonb,
		  14, 0, 0, 0, false, 'asset', '6824519374061285743', 0, 0, now(), now()),
		 ('lumina-seedream-5.0-lite', 'image', 'Seedream 5.0 Lite', '', 'byteplus', true,
		  '["1:1","16:9","9:16","4:3","3:4"]'::jsonb,
		  '{"2K":0}'::jsonb, '["2K"]'::jsonb, true,
		  '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, '[]'::jsonb,
		  10, 0, 0, 0, false, 'asset', '7604761017696141358', 0, 0, now(), now()),
		 ('lumina-nano-banana-2', 'image', 'Nano Banana 2 (Beta)', '', 'byteplus', true,
		  '["16:9","9:16","4:3","3:4","1:1"]'::jsonb,
		  '{"1K":0,"2K":0,"4K":0}'::jsonb, '["1K","2K","4K"]'::jsonb, true,
		  '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, '[]'::jsonb,
		  14, 0, 0, 0, false, 'asset', '8162745039814627354', 0, 0, now(), now()),
		 ('lumina-nano-banana-pro', 'image', 'Nano Banana Pro (Beta)', '', 'byteplus', true,
		  '["16:9","9:16","4:3","3:4","1:1"]'::jsonb,
		  '{"1K":0,"2K":0,"4K":0}'::jsonb, '["1K","2K","4K"]'::jsonb, true,
		  '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, '[]'::jsonb,
		  14, 0, 0, 0, false, 'asset', '8162745039814627353', 0, 0, now(), now())
		ON CONFLICT (id) DO NOTHING`).Error; err != nil {
		return err
	}
	// Capability-only upgrade backfill. Do not overwrite operator-controlled
	// aliases, enablement, prices, weights, or counters on existing rows.
	if err := db.WithContext(ctx).Exec(`UPDATE model_configs AS m SET
		ratios = v.ratios::jsonb,
		resolutions = v.resolutions::jsonb,
		image_to_image = true,
		max_reference_images = v.max_images,
		max_reference_videos = 0,
		max_reference_audios = 0,
		max_reference_media = 0,
		supports_audio_output = false,
		reference_mode = 'asset',
		upstream_model = v.upstream_model,
		updated_at = now()
		FROM (VALUES
		 ('lumina-seedream-5.0-pro', '["1:1","16:9","9:16","4:3","3:4"]', '["1K","2K"]', 10, '7657401949175693322'),
		 ('lumina-gpt-image-2', '["1:1","16:9","9:16","4:3","3:4"]', '["1K","2K","4K"]', 14, '6824519374061285743'),
		 ('lumina-seedream-5.0-lite', '["1:1","16:9","9:16","4:3","3:4"]', '["2K"]', 10, '7604761017696141358'),
		 ('lumina-nano-banana-2', '["16:9","9:16","4:3","3:4","1:1"]', '["1K","2K","4K"]', 14, '8162745039814627354'),
		 ('lumina-nano-banana-pro', '["16:9","9:16","4:3","3:4","1:1"]', '["1K","2K","4K"]', 14, '8162745039814627353')
		) AS v(id, ratios, resolutions, max_images, upstream_model)
		WHERE m.id = v.id`).Error; err != nil {
		return err
	}
	// OreateAI Seedance capabilities mirror the authenticated account model and
	// scene configuration. Prices default to zero so deployments can set their
	// own retail pricing without seed updates overwriting it.
	if err := db.WithContext(ctx).Exec(`INSERT INTO model_configs
		(id, type, name, alias, provider, enabled, ratios, prices, resolutions,
		 image_to_image, duration_prices, prices_agent, duration_prices_agent,
		 durations, max_reference_images, max_reference_videos, max_reference_audios,
		 max_reference_media, supports_audio_output, reference_mode, upstream_model,
		 weight, generation_count, created_at, updated_at)
		VALUES
		 ('oreate-seedance-2.0-mini', 'video', 'Seedance 2.0 Mini', '', 'oreate', true,
		  '["16:9","1:1","3:4","4:3","9:16","21:9"]'::jsonb,
		  '{"480p":0,"720p":0}'::jsonb, '["480p","720p"]'::jsonb, false,
		  '{"5s":0,"10s":0}'::jsonb, '{}'::jsonb, '{}'::jsonb, '["5s","10s"]'::jsonb,
		  9, 3, 0, 12, true, 'asset', 'seedance-2.0-mini', 0, 0, now(), now()),
		 ('oreate-seedance-2.0-fast', 'video', 'Seedance 2.0 Fast', '', 'oreate', true,
		  '["16:9","1:1","3:4","4:3","9:16","21:9"]'::jsonb,
		  '{"480p":0,"720p":0}'::jsonb, '["480p","720p"]'::jsonb, false,
		  '{"5s":0,"10s":0}'::jsonb, '{}'::jsonb, '{}'::jsonb, '["5s","10s"]'::jsonb,
		  9, 3, 0, 12, true, 'asset', 'seedance-2.0-fast', 0, 0, now(), now()),
		 ('oreate-seedance-1.5-pro', 'video', 'Seedance 1.5 Pro', '', 'oreate', true,
		  '["16:9","1:1","3:4","4:3","9:16","21:9"]'::jsonb,
		  '{"480p":0,"720p":0,"1080p":0}'::jsonb, '["480p","720p","1080p"]'::jsonb, false,
		  '{"5s":0,"10s":0}'::jsonb, '{}'::jsonb, '{}'::jsonb, '["5s","10s"]'::jsonb,
		  2, 0, 0, 2, true, 'frame', 'seedance-1.5-pro', 0, 0, now(), now()),
		 ('oreate-seedance-2.0', 'video', 'Seedance 2.0', '', 'oreate', true,
		  '["16:9","1:1","3:4","4:3","9:16","21:9"]'::jsonb,
		  '{"480p":0,"720p":0,"1080p":0}'::jsonb, '["480p","720p","1080p"]'::jsonb, false,
		  '{"5s":0,"10s":0}'::jsonb, '{}'::jsonb, '{}'::jsonb, '["5s","10s"]'::jsonb,
		  9, 3, 0, 12, true, 'asset', 'seedance-2.0', 0, 0, now(), now()),
		 ('oreate-seedance-2.5', 'video', 'Seedance 2.5', '', 'oreate', true,
		  '["16:9","1:1","3:4","4:3","9:16","21:9"]'::jsonb,
		  '{"480p":0,"720p":0}'::jsonb, '["480p","720p"]'::jsonb, false,
		  '{"5s":0,"10s":0,"20s":0,"30s":0}'::jsonb, '{}'::jsonb, '{}'::jsonb,
		  '["5s","10s","20s","30s"]'::jsonb,
		  9, 3, 0, 12, true, 'asset', 'seedance-2.5', 0, 0, now(), now())
		ON CONFLICT (id) DO NOTHING`).Error; err != nil {
		return err
	}
	// Capability-only backfill for existing deployments. Operator-controlled
	// enablement, aliases, prices, weights, and counters are deliberately kept.
	if err := db.WithContext(ctx).Exec(`UPDATE model_configs AS m SET
		ratios = v.ratios::jsonb,
		resolutions = v.resolutions::jsonb,
		durations = v.durations::jsonb,
		max_reference_images = v.max_images,
		max_reference_videos = v.max_videos,
		max_reference_audios = 0,
		max_reference_media = v.max_media,
		supports_audio_output = true,
		reference_mode = v.reference_mode,
		upstream_model = v.upstream_model,
		updated_at = now()
		FROM (VALUES
		 ('oreate-seedance-2.0-mini', '["16:9","1:1","3:4","4:3","9:16","21:9"]', '["480p","720p"]', '["5s","10s"]', 9, 3, 12, 'asset', 'seedance-2.0-mini'),
		 ('oreate-seedance-2.0-fast', '["16:9","1:1","3:4","4:3","9:16","21:9"]', '["480p","720p"]', '["5s","10s"]', 9, 3, 12, 'asset', 'seedance-2.0-fast'),
		 ('oreate-seedance-1.5-pro', '["16:9","1:1","3:4","4:3","9:16","21:9"]', '["480p","720p","1080p"]', '["5s","10s"]', 2, 0, 2, 'frame', 'seedance-1.5-pro'),
		 ('oreate-seedance-2.0', '["16:9","1:1","3:4","4:3","9:16","21:9"]', '["480p","720p","1080p"]', '["5s","10s"]', 9, 3, 12, 'asset', 'seedance-2.0'),
		 ('oreate-seedance-2.5', '["16:9","1:1","3:4","4:3","9:16","21:9"]', '["480p","720p"]', '["5s","10s","20s","30s"]', 9, 3, 12, 'asset', 'seedance-2.5')
		) AS v(id, ratios, resolutions, durations, max_images, max_videos, max_media, reference_mode, upstream_model)
		WHERE m.id = v.id`).Error; err != nil {
		return err
	}
	// Older releases incorrectly treated a missing ACTIVE subscription as a
	// permanent Grok video restriction. Grok now exposes video to these sessions
	// as long as the media credit endpoint allows it; clear only that obsolete
	// provider-wide marker (the current liveness probe no longer writes it).
	if err := db.WithContext(ctx).Exec(`UPDATE token_accounts SET video_limited = false
		WHERE pool = 'grok' AND video_limited = true`).Error; err != nil {
		return err
	}
	// One-time backfill of the persistent per-model generation counter from
	// historical success logs, so the admin "次数" keeps its running total when we
	// switch it off the (retention-pruned) event_log. Only touches models still at
	// 0, so it never double-counts after the first run; increments take over next.
	if err := db.WithContext(ctx).Exec(
		`UPDATE model_configs m SET generation_count = COALESCE(
			(SELECT COUNT(*) FROM event_logs e WHERE e.model = m.id AND e.status = 'success'), 0)
		 WHERE m.generation_count = 0`).Error; err != nil {
		return err
	}
	// Same one-time backfill for the per-user generation counter.
	if err := db.WithContext(ctx).Exec(
		`UPDATE users u SET generation_count = COALESCE(
			(SELECT COUNT(*) FROM event_logs e WHERE e.user_id = u.id AND e.status = 'success'), 0)
		 WHERE u.generation_count = 0`).Error; err != nil {
		return err
	}
	// Seed the dashboard lifetime counters from logs ONCE (only when empty), so the
	// all-time cards start from real history then track forward via the hooks.
	var counterRows int64
	if err := db.WithContext(ctx).Model(&model.StatCounter{}).Count(&counterRows).Error; err == nil && counterRows == 0 {
		_ = db.WithContext(ctx).Exec(`INSERT INTO stat_counters (key, value, updated_at)
			SELECT 'total',   COUNT(*),                                   now() FROM event_logs
			UNION ALL SELECT 'success', COUNT(*) FILTER (WHERE status='success'), now() FROM event_logs
			UNION ALL SELECT 'failed',  COUNT(*) FILTER (WHERE status='failed'),  now() FROM event_logs
			UNION ALL SELECT 'image',   COUNT(*) FILTER (WHERE kind='image'),     now() FROM event_logs
			UNION ALL SELECT 'video',   COUNT(*) FILTER (WHERE kind='video'),     now() FROM event_logs
			UNION ALL SELECT 'api',     COUNT(*) FILTER (WHERE source='v1'),      now() FROM event_logs
			ON CONFLICT (key) DO NOTHING`).Error
	}
	return nil
}
