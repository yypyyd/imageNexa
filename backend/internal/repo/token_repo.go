package repo

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"backend/internal/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type TokenRepository struct {
	db *gorm.DB
}

func NewTokenRepository(db *gorm.DB) *TokenRepository {
	return &TokenRepository{db: db}
}

func (r *TokenRepository) List(ctx context.Context) ([]model.TokenAccount, error) {
	var items []model.TokenAccount
	if err := r.db.WithContext(ctx).
		Order("pool asc, created_at desc").
		Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (r *TokenRepository) ListByPool(ctx context.Context, pool string) ([]model.TokenAccount, error) {
	var items []model.TokenAccount
	if err := r.db.WithContext(ctx).
		Where("pool = ?", pool).
		Order("created_at desc").
		Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (r *TokenRepository) Get(ctx context.Context, pool, id string) (*model.TokenAccount, error) {
	var item model.TokenAccount
	if err := r.db.WithContext(ctx).
		First(&item, "pool = ? AND id = ?", pool, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// GetByPoolEmail finds an account in a pool by its account_email (the logical
// identity for import dedup). Returns (nil, nil) when none / email is blank.
func (r *TokenRepository) GetByPoolEmail(ctx context.Context, pool, email string) (*model.TokenAccount, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, nil
	}
	var item model.TokenAccount
	err := r.db.WithContext(ctx).
		Where("pool = ? AND account_email = ?", pool, email).
		First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *TokenRepository) Create(ctx context.Context, item *model.TokenAccount) error {
	return r.db.WithContext(ctx).Create(item).Error
}

func (r *TokenRepository) Update(ctx context.Context, pool, id string, patch map[string]any) (*model.TokenAccount, error) {
	patch["updated_at"] = time.Now()
	if err := r.db.WithContext(ctx).
		Model(&model.TokenAccount{}).
		Where("pool = ? AND id = ?", pool, id).
		Updates(patch).Error; err != nil {
		return nil, err
	}
	return r.Get(ctx, pool, id)
}

// UpdateMergingMeta atomically merges selected JSON keys while updating any
// accompanying scalar columns. This avoids replacing provider credentials or
// quota fields written concurrently from a stale in-memory account snapshot.
func (r *TokenRepository) UpdateMergingMeta(ctx context.Context, pool, id string, metaPatch map[string]any, patch map[string]any) error {
	raw, err := json.Marshal(metaPatch)
	if err != nil {
		return err
	}
	updates := make(map[string]any, len(patch)+2)
	for key, value := range patch {
		updates[key] = value
	}
	updates["meta"] = gorm.Expr("COALESCE(meta, '{}'::jsonb) || CAST(? AS jsonb)", string(raw))
	updates["updated_at"] = time.Now()
	return r.db.WithContext(ctx).
		Model(&model.TokenAccount{}).
		Where("pool = ? AND id = ?", pool, id).
		Updates(updates).Error
}

// ReserveQuota atomically pre-deducts `amount` from an account's cached image
// token balance under a row lock, so concurrent picks of the same near-empty
// account can never over-commit it. Returns:
//   - allowed=true, deducted=true: balance was known and ≥ amount → decremented.
//   - allowed=true, deducted=false: balance unknown → allowed without a hold
//     (benefit of the doubt; a post-render reconcile writes the real value).
//   - allowed=false: balance known and < amount → caller should fail over.
//
// RefundQuota releases a hold made with deducted=true when the render fails.
func (r *TokenRepository) ReserveQuota(ctx context.Context, pool, id string, amount int) (allowed, deducted bool, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item model.TokenAccount
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&item, "pool = ? AND id = ?", pool, id).Error; e != nil {
			return e
		}
		rem, known := metaInt(item.Meta, "cached_quota_remaining")
		if !known {
			allowed, deducted = true, false
			return nil
		}
		if rem < amount {
			allowed, deducted = false, false
			return nil
		}
		meta := cloneMeta(item.Meta)
		meta["cached_quota_remaining"] = rem - amount
		if e := tx.Model(&model.TokenAccount{}).
			Where("pool = ? AND id = ?", pool, id).
			Updates(map[string]any{"meta": meta, "updated_at": time.Now()}).Error; e != nil {
			return e
		}
		allowed, deducted = true, true
		return nil
	})
	return allowed, deducted, err
}

// RefundQuota atomically adds `amount` back to cached_quota_remaining (releasing a
// hold from a reservation whose render then failed). No-op if the balance is
// unknown. Row-locked like ReserveQuota.
func (r *TokenRepository) RefundQuota(ctx context.Context, pool, id string, amount int) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item model.TokenAccount
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&item, "pool = ? AND id = ?", pool, id).Error; e != nil {
			return e
		}
		rem, known := metaInt(item.Meta, "cached_quota_remaining")
		if !known {
			return nil
		}
		meta := cloneMeta(item.Meta)
		meta["cached_quota_remaining"] = rem + amount
		return tx.Model(&model.TokenAccount{}).
			Where("pool = ? AND id = ?", pool, id).
			Updates(map[string]any{"meta": meta, "updated_at": time.Now()}).Error
	})
}

// ReserveQuotaTracked atomically holds a decimal provider credit amount and
// records the aggregate in-flight hold in cached_quota_reserved. BytePlus uses
// tenths of a point, so all arithmetic is converted to fixed-point integers
// before touching JSON. Unknown balances remain eligible, but their holds are
// still tracked so the first upstream reconciliation can account for concurrent
// jobs that have not completed yet.
func (r *TokenRepository) ReserveQuotaTracked(ctx context.Context, pool, id string, amount float64) (allowed, held bool, err error) {
	amountTenths, ok := quotaTenths(amount)
	if !ok || amountTenths <= 0 {
		return false, false, errors.New("quota reservation amount must be positive")
	}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item model.TokenAccount
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&item, "pool = ? AND id = ?", pool, id).Error; e != nil {
			return e
		}
		remaining, known := metaTenths(item.Meta, "cached_quota_remaining")
		if known && remaining < amountTenths {
			allowed, held = false, false
			return nil
		}
		reserved, _ := metaTenths(item.Meta, "cached_quota_reserved")
		meta := cloneMeta(item.Meta)
		meta["cached_quota_reserved"] = canonicalTenths(reserved + amountTenths)
		if known {
			meta["cached_quota_remaining"] = canonicalTenths(remaining - amountTenths)
		}
		if e := tx.Model(&model.TokenAccount{}).
			Where("pool = ? AND id = ?", pool, id).
			Updates(map[string]any{"meta": meta, "updated_at": time.Now()}).Error; e != nil {
			return e
		}
		allowed, held = true, true
		return nil
	})
	return allowed, held, err
}

// RefundQuotaTracked releases a tracked hold after a failure known to have
// happened before billing. Only the amount actually present in the reservation
// ledger is restored, making duplicate cleanup calls harmless.
func (r *TokenRepository) RefundQuotaTracked(ctx context.Context, pool, id string, amount float64) error {
	amountTenths, ok := quotaTenths(amount)
	if !ok || amountTenths <= 0 {
		return errors.New("quota refund amount must be positive")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item model.TokenAccount
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&item, "pool = ? AND id = ?", pool, id).Error; e != nil {
			return e
		}
		reserved, _ := metaTenths(item.Meta, "cached_quota_reserved")
		released := amountTenths
		if released > reserved {
			released = reserved
		}
		meta := cloneMeta(item.Meta)
		meta["cached_quota_reserved"] = canonicalTenths(reserved - released)
		if remaining, known := metaTenths(item.Meta, "cached_quota_remaining"); known {
			meta["cached_quota_remaining"] = canonicalTenths(remaining + released)
		}
		return tx.Model(&model.TokenAccount{}).
			Where("pool = ? AND id = ?", pool, id).
			Updates(map[string]any{"meta": meta, "updated_at": time.Now()}).Error
	})
}

// SettleQuotaTracked commits a completed/possibly accepted generation's hold.
// When an upstream snapshot is available, cached remaining becomes upstream
// remaining minus every *other* in-flight hold. This prevents a late balance
// refresh from erasing reservations made by concurrent generations.
func (r *TokenRepository) SettleQuotaTracked(ctx context.Context, pool, id string, amount float64, upstreamRemaining *float64, metaPatch map[string]any) error {
	amountTenths, ok := quotaTenths(amount)
	if !ok || amountTenths < 0 {
		return errors.New("quota settlement amount must be non-negative")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item model.TokenAccount
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&item, "pool = ? AND id = ?", pool, id).Error; e != nil {
			return e
		}
		reserved, _ := metaTenths(item.Meta, "cached_quota_reserved")
		released := amountTenths
		if released > reserved {
			released = reserved
		}
		remainingReserved := reserved - released
		meta := cloneMeta(item.Meta)
		for key, value := range metaPatch {
			meta[key] = value
		}
		meta["cached_quota_reserved"] = canonicalTenths(remainingReserved)
		if upstreamRemaining != nil {
			upstreamTenths, valid := quotaTenths(*upstreamRemaining)
			if !valid {
				return errors.New("invalid upstream quota remaining")
			}
			available := upstreamTenths - remainingReserved
			if available < 0 {
				available = 0
			}
			meta["cached_quota_remaining"] = canonicalTenths(available)
		}
		return tx.Model(&model.TokenAccount{}).
			Where("pool = ? AND id = ?", pool, id).
			Updates(map[string]any{"meta": meta, "updated_at": time.Now()}).Error
	})
}

func quotaTenths(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return int64(math.Round(value * 10)), true
}

func metaTenths(meta datatypes.JSONMap, key string) (int64, bool) {
	if meta == nil {
		return 0, false
	}
	value, exists := meta[key]
	if !exists || value == nil {
		return 0, false
	}
	var number float64
	switch typed := value.(type) {
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case float64:
		number = typed
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return quotaTenths(number)
}

func canonicalTenths(value int64) any {
	if value%10 == 0 {
		return int(value / 10)
	}
	return float64(value) / 10
}

func cloneMeta(m datatypes.JSONMap) datatypes.JSONMap {
	out := datatypes.JSONMap{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func metaInt(m datatypes.JSONMap, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case json.Number:
		n, e := x.Int64()
		if e != nil {
			return 0, false
		}
		return int(n), true
	case string:
		n, e := strconv.Atoi(strings.TrimSpace(x))
		if e != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// TouchLastUsed stamps last_used_at at the moment a token is SELECTED, so the
// accounts view reflects an accurate "last used" time. Rotation order is driven
// by the in-memory strict round-robin cursor in the service layer (see
// V1Service.rotateRoundRobin), not by this timestamp.
func (r *TokenRepository) TouchLastUsed(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).
		Model(&model.TokenAccount{}).
		Where("id = ?", id).
		Update("last_used_at", time.Now()).Error
}

// IncrementFail bumps an account's failure counters by one. Used to attribute
// an abandoned (purged) generation's failure back to the account it was using,
// since that generation never reached the normal markTokenFailure path.
func (r *TokenRepository) IncrementFail(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).
		Model(&model.TokenAccount{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"fail_total": gorm.Expr("fail_total + 1"),
			"fails":      gorm.Expr("fails + 1"),
			"updated_at": time.Now(),
		}).Error
}

func (r *TokenRepository) Delete(ctx context.Context, pool, id string) (int64, error) {
	res := r.db.WithContext(ctx).
		Delete(&model.TokenAccount{}, "pool = ? AND id = ?", pool, id)
	return res.RowsAffected, res.Error
}

// DeleteByIDs removes accounts by id across pools (ids are globally unique),
// for bulk delete. Returns the number of rows removed.
func (r *TokenRepository) DeleteByIDs(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db.WithContext(ctx).Delete(&model.TokenAccount{}, "id IN ?", ids)
	return res.RowsAffected, res.Error
}

// leonardoDailyTokens is the free-tier daily allowance restored at each reset.
// A paid account's true balance is reconciled on its next successful render.
const leonardoDailyTokens = 150

// RecoverQuota reactivates quota-exhausted tokens whose reset time has passed.
// Reset source: cached_quota_reset_after (upstream marker) first, else the
// quota_recover_at fallback stamped when the token was marked quota-exhausted.
// Mirrors Python TokenPool.recover_quota; returns the count reactivated.
// RecoverQuota reactivates quota-exhausted tokens whose reset time has passed and
// returns the accounts it recovered, so the caller can re-sync their real balance
// (the providers only sync quota when accessed).
func (r *TokenRepository) RecoverQuota(ctx context.Context) ([]model.TokenAccount, error) {
	// Also pick up accounts that are only single-kind limited (image_limited /
	// video_limited) — those keep status "active" and would otherwise never have
	// their per-kind flag cleared. Adobe resets both kinds at once, so the shared
	// reset time gates recovery for all of them.
	var items []model.TokenAccount
	if err := r.db.WithContext(ctx).
		Where("status = ? OR image_limited = ? OR video_limited = ?", "quota", true, true).
		Find(&items).Error; err != nil {
		return nil, err
	}
	now := time.Now()
	var recovered []model.TokenAccount
	for i := range items {
		t := &items[i]
		// Runway's reset marker is the JWT expiry, not a quota-refresh time, and
		// there's no way to refresh a bare JWT. Oreate's marker is the expiry of
		// its current positive point bucket, not proof that a new grant arrived.
		// Both therefore require their provider-specific lifecycle instead of an
		// optimistic status flip here.
		if t.Pool == "runway" || t.Pool == "oreate" {
			continue
		}
		reset := parseResetMarker(t.CachedQuotaResetAfter)
		if reset == nil {
			reset = t.QuotaRecoverAt
		}
		if reset == nil || now.Before(*reset) {
			continue
		}
		patch := map[string]any{
			"fails":            0,
			"quota_recover_at": nil,
			"image_limited":    false,
			"video_limited":    false,
		}
		// Only flip status back to active if it was sunk to "quota" (both kinds
		// limited); a single-kind limit left status untouched.
		if t.Status == "quota" {
			patch["status"] = "active"
		}
		// Leonardo's free tokens fully renew at each daily reset — restore the
		// balance and advance the reset marker to the next 08:00 Beijing (== next
		// UTC midnight), so the account is immediately usable instead of stuck at a
		// stale 0. 积分号 (paid credits) renew monthly and are handled separately below.
		if t.Pool == "leonardo" || t.Pool == "krea" || t.Pool == "imagine" {
			meta := cloneMeta(t.Meta)
			if t.Pool == "leonardo" && isPaidLeonardo(t.Meta) {
				// 积分号:余额是点数,按上游 tokenRenewalDate 月度续期 —— 不能补成每日
				// 免费额度,也不能把恢复时间改成次日 08:00。丢掉过期的余额,让 sweep 的
				// Quota 探测写回上游真实点数与下一次续期时间。
				delete(meta, "cached_quota_remaining")
				meta["cached_quota_at"] = int(now.Unix())
				patch["meta"] = meta
				patch["cached_quota_reset_after"] = nextMonthlyReset(reset, now)
				if _, err := r.Update(ctx, t.Pool, t.ID, patch); err != nil {
					return recovered, err
				}
				recovered = append(recovered, *t)
				continue
			}
			if t.Pool == "leonardo" {
				meta["cached_quota_remaining"] = leonardoDailyTokens
			} else {
				// Krea/Imagine balances re-sync from upstream (billing-data / v1/credit)
				// on next probe — drop the stale value so the account isn't shown as
				// empty after reset.
				delete(meta, "cached_quota_remaining")
			}
			meta["cached_quota_at"] = int(now.Unix())
			patch["meta"] = meta
			patch["cached_quota_reset_after"] = time.Unix((now.Unix()/86400+1)*86400, 0).UTC().Format(time.RFC3339)
		}
		if _, err := r.Update(ctx, t.Pool, t.ID, patch); err != nil {
			return recovered, err
		}
		recovered = append(recovered, *t)
	}
	return recovered, nil
}

// isPaidLeonardo reports whether a Leonardo row is a 积分号 (paid credits, monthly
// renewal) rather than a free 普通号 whose tokens renew daily. The marker is
// written by the balance probe (applyLeonardoPlanMeta).
func isPaidLeonardo(meta datatypes.JSONMap) bool {
	if meta == nil {
		return false
	}
	paid, _ := meta["paid_account"].(bool)
	return paid
}

// nextMonthlyReset advances a past renewal marker by whole months until it is in
// the future (积分号 renew monthly). Falls back to +1 month from now when the old
// marker is unusable.
func nextMonthlyReset(reset *time.Time, now time.Time) string {
	next := now.AddDate(0, 1, 0)
	if reset != nil {
		next = *reset
		for !next.After(now) {
			next = next.AddDate(0, 1, 0)
		}
	}
	return next.UTC().Format(time.RFC3339)
}

// RollResetMarkers advances a stale (past) daily-reset marker to its next future
// occurrence — same time-of-day, +N whole days — for ACTIVE accounts of the given
// daily-reset pools, so the 恢复时间 column always shows the upcoming reset rather
// than yesterday's. Only active accounts are rolled: a 限额 account must keep its
// past marker so RecoverQuota can recover it (rolling it forward early would
// prevent recovery). Leonardo 积分号 are skipped — their renewal is monthly, so a
// daily roll would show a wrong date. Returns the number advanced.
func (r *TokenRepository) RollResetMarkers(ctx context.Context, pools []string) (int, error) {
	var items []model.TokenAccount
	if err := r.db.WithContext(ctx).
		Where("pool IN ? AND dead = ? AND status = ? AND image_limited = ? AND video_limited = ? AND cached_quota_reset_after <> ''",
			pools, false, "active", false, false).
		Find(&items).Error; err != nil {
		return 0, err
	}
	now := time.Now()
	n := 0
	for i := range items {
		t := &items[i]
		reset := parseResetMarker(t.CachedQuotaResetAfter)
		if reset == nil || !reset.Before(now) {
			continue // unparseable or already in the future
		}
		next := *reset
		if t.Pool == "leonardo" && isPaidLeonardo(t.Meta) {
			for !next.After(now) {
				next = next.AddDate(0, 1, 0)
			}
		} else {
			for !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
		}
		if _, err := r.Update(ctx, t.Pool, t.ID, map[string]any{
			"cached_quota_reset_after": next.UTC().Format(time.RFC3339),
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ExpireByReset marks accounts of a pool dead once their reset marker has passed.
// For runway the marker IS the JWT expiry and there's no refresh, so an expired
// token can only 401 — we proactively flip it to disabled+dead (the same end
// state a 401 would produce) instead of leaving a doomed account "active".
func (r *TokenRepository) ExpireByReset(ctx context.Context, pool string) (int, error) {
	var items []model.TokenAccount
	if err := r.db.WithContext(ctx).
		Where("pool = ? AND dead = ?", pool, false).
		Find(&items).Error; err != nil {
		return 0, err
	}
	now := time.Now()
	expired := 0
	for i := range items {
		t := &items[i]
		reset := parseResetMarker(t.CachedQuotaResetAfter)
		if reset == nil || now.Before(*reset) {
			continue
		}
		if _, err := r.Update(ctx, t.Pool, t.ID, map[string]any{
			"status": "disabled",
			"dead":   true,
		}); err != nil {
			return expired, err
		}
		expired++
	}
	return expired, nil
}

// parseResetMarker best-effort parses a quota reset marker into a time. Accepts
// epoch seconds (numeric string) or ISO-8601 (e.g. Adobe's available_until
// "2026-06-16T23:59:59.999Z"). Returns nil if unparseable.
func parseResetMarker(v string) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f > 946684800 {
		t := time.Unix(int64(f), 0)
		return &t
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.999Z07:00", "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t
		}
	}
	return nil
}
