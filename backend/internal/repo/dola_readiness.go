package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ClaimDolaReadiness fences probes across imports, workers and process restarts.
// Force retries are allowed by the explicit refresh action; disabled accounts
// stay disabled until explicitly re-enabled by an administrator.
func (r *TokenRepository) ClaimDolaReadiness(ctx context.Context, id string, force bool) (*model.TokenAccount, error) {
	var result *model.TokenAccount
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var a model.TokenAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&a, "id = ? AND pool = ?", id, "dola").Error; err != nil {
			return err
		}
		if a.Status == "disabled" || a.Dead || a.Value == "" {
			return nil
		}
		if a.Meta == nil {
			a.Meta = map[string]any{}
		}
		now := time.Now()
		lease, _ := a.Meta["dola_probe_until"].(string)
		until, _ := time.Parse(time.RFC3339, lease)
		if until.After(now) {
			return nil
		}
		if !force {
			if model.DolaAccountReady(a) {
				return nil
			}
			state, _ := a.Meta["dola_readiness"].(string)
			if state == "login_required" || state == "challenge" {
				return nil
			}
			retry, _ := a.Meta["dola_retry_at"].(string)
			at, _ := time.Parse(time.RFC3339, retry)
			if at.After(now) || state == "failed" {
				return nil
			}
		}
		if a.Status == "quota" {
			a.Meta["dola_resume_status"] = "quota"
		} else if a.Status != "pending" {
			a.Meta["dola_resume_status"] = "active"
		}
		generation, _ := a.Meta["dola_credential_generation"].(string)
		if generation == "" {
			a.Meta["dola_credential_generation"] = durableID("dg_")
		}
		attempts := 0
		if n, ok := a.Meta["dola_probe_attempts"].(float64); ok {
			attempts = int(n)
		}
		if force {
			attempts = 0
		}
		a.Meta["dola_probe_attempts"] = attempts + 1
		a.Meta["dola_probe_id"] = durableID("dp_")
		a.Meta["dola_probe_until"] = now.Add(5 * time.Minute).UTC().Format(time.RFC3339)
		a.Meta["dola_readiness"] = "checking"
		a.Meta["dola_readiness_version"] = ""
		a.Meta["dola_verified_generation"] = ""
		a.Meta["pending_check"] = true
		a.Status = "pending"
		if err := tx.Model(&a).Updates(map[string]any{"status": a.Status, "meta": a.Meta, "updated_at": now}).Error; err != nil {
			return err
		}
		result = &a
		return nil
	})
	return result, err
}

func (r *TokenRepository) FinishDolaReadiness(ctx context.Context, a model.TokenAccount, state, diagnostic string) (bool, error) {
	now := time.Now()
	status := "pending"
	meta := map[string]any{"pending_check": false, "dola_readiness": state, "dola_readiness_detail": diagnostic,
		"dola_checked_at": now.UTC().Format(time.RFC3339), "dola_probe_until": "", "dola_retry_at": "",
		"dola_readiness_version": "", "dola_verified_generation": ""}
	if state == "ready" {
		status = "active"
		if a.Meta["dola_resume_status"] == "quota" {
			status = "quota"
		}
		meta["dola_readiness_version"] = model.DolaReadinessVersion
		meta["dola_verified_generation"] = a.Meta["dola_credential_generation"]
	} else if state == "retry" {
		n, _ := a.Meta["dola_probe_attempts"].(int)
		if n >= 3 {
			meta["dola_readiness"] = "failed"
			meta["dola_readiness_detail"] = "自动验证已失败三次，请检查 Cookie 和代理后重新验证"
		} else {
			meta["dola_retry_at"] = now.Add(time.Duration(5*max(1, n)) * time.Minute).UTC().Format(time.RFC3339)
		}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return false, err
	}
	res := r.db.WithContext(ctx).Model(&model.TokenAccount{}).
		Where("id = ? AND pool = 'dola' AND status = 'pending' AND meta->>'dola_credential_generation' = ? AND meta->>'dola_probe_id' = ?", a.ID, a.Meta["dola_credential_generation"], a.Meta["dola_probe_id"]).
		Updates(map[string]any{"status": status, "meta": gorm.Expr("COALESCE(meta,'{}'::jsonb) || ?::jsonb", string(raw)), "updated_at": now})
	return res.RowsAffected == 1, res.Error
}

// Changing scheduling state cancels a stale verifier without changing quota.
func (r *TokenRepository) SetDolaEnabled(ctx context.Context, id string, enabled bool) error {
	status := "disabled"
	if enabled {
		status = "pending"
	}
	meta := map[string]any{"dola_probe_id": durableID("dp_"), "dola_probe_until": "", "dola_retry_at": "", "dola_probe_attempts": 0,
		"dola_readiness": "pending", "dola_readiness_detail": "", "dola_readiness_version": "", "dola_verified_generation": "", "pending_check": false}
	if err := r.UpdateMergingMeta(ctx, "dola", id, meta, map[string]any{"status": status, "dead": false}); err != nil {
		return err
	}
	return nil
}

func (r *TokenRepository) DolaReadinessCandidates(ctx context.Context) ([]model.TokenAccount, error) {
	var items []model.TokenAccount
	err := r.db.WithContext(ctx).Where("pool = 'dola' AND status IN ('active','pending','quota') AND dead = false").Order("updated_at ASC").Find(&items).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return items, err
}

// InvalidateDolaReadiness only suspends the credential that failed, fencing
// delayed requests against imports, disables and newer probes.
func (r *TokenRepository) InvalidateDolaReadiness(ctx context.Context, a model.TokenAccount, state, detail string) error {
	raw, err := json.Marshal(map[string]any{"dola_readiness": state, "dola_readiness_detail": detail,
		"dola_readiness_version": "", "dola_verified_generation": "", "dola_probe_until": "", "dola_probe_id": durableID("dp_"),
		"dola_retry_at": "", "dola_probe_attempts": 0, "pending_check": false})
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Model(&model.TokenAccount{}).
		Where("id = ? AND pool = 'dola' AND status = 'active' AND meta->>'dola_credential_generation' = ? AND COALESCE(meta->>'dola_probe_id','') = ?", a.ID, a.Meta["dola_credential_generation"], a.Meta["dola_probe_id"]).
		Updates(map[string]any{"status": "pending", "meta": gorm.Expr("COALESCE(meta,'{}'::jsonb) || ?::jsonb", string(raw)), "updated_at": time.Now()}).Error
}
