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

// schedulingAccountColumns omits credential material. Admission reloads the
// chosen row with Get before any provider call.
var schedulingAccountColumns = []string{
	"id", "pool", "status", "fails", "fail_total", "upstream_fails", "success_total",
	"dead", "meta", "added_at", "last_used_at", "cached_quota_reset_after", "quota_recover_at",
	"image_limited", "video_limited", "account_email", "account_display_name",
	"weight", "concurrency", "created_at", "updated_at", "identity_hash",
}

func markSchedulingStubs(items []model.TokenAccount) []model.TokenAccount {
	for i := range items {
		items[i].SchedulingStub = true
		items[i].Value = ""
		items[i].ARPSessionToken = ""
	}
	return items
}

// ListSchedulingByPool loads pool rows for candidate ranking without cookies or
// session tokens. Empty-credential rows never enter the candidate set.
func (r *TokenRepository) ListSchedulingByPool(ctx context.Context, pool string) ([]model.TokenAccount, error) {
	var items []model.TokenAccount
	if err := r.db.WithContext(ctx).
		Select(schedulingAccountColumns).
		Where("pool = ? AND value <> ''", pool).
		Find(&items).Error; err != nil {
		return nil, err
	}
	return markSchedulingStubs(items), nil
}

// ListByIDs refreshes a queued request's candidates in one query. The caller
// preserves its scheduling order; database row order is deliberately irrelevant.
func (r *TokenRepository) ListByIDs(ctx context.Context, pool string, ids []string) ([]model.TokenAccount, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var items []model.TokenAccount
	err := r.db.WithContext(ctx).Where("pool = ? AND id IN ?", pool, ids).Find(&items).Error
	return items, err
}

// ListSchedulingByIDs refreshes queued candidates without credential material.
func (r *TokenRepository) ListSchedulingByIDs(ctx context.Context, pool string, ids []string) ([]model.TokenAccount, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var items []model.TokenAccount
	err := r.db.WithContext(ctx).
		Select(schedulingAccountColumns).
		Where("pool = ? AND id IN ? AND value <> ''", pool, ids).
		Find(&items).Error
	return markSchedulingStubs(items), err
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
func (r *TokenRepository) GetByIdentityHash(ctx context.Context, pool, identityHash string) (*model.TokenAccount, error) {
	identityHash = strings.TrimSpace(identityHash)
	if identityHash == "" {
		return nil, nil
	}
	var item model.TokenAccount
	err := r.db.WithContext(ctx).
		Where("pool = ? AND identity_hash = ?", pool, identityHash).
		First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

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

// UpsertOreateByIdentity inserts one Oreate credential or rotates the existing
// row with the same upstream identity. The unique identity index makes concurrent
// raw-cookie imports converge on one durable account.
func (r *TokenRepository) UpsertOreateByIdentity(ctx context.Context, item *model.TokenAccount, metaPatch map[string]any) (*model.TokenAccount, bool, error) {
	if item == nil || item.Pool != "oreate" || strings.TrimSpace(item.IdentityHash) == "" {
		return nil, false, errors.New("oreate identity is required")
	}
	if err := r.Create(ctx, item); err == nil {
		return item, true, nil
	} else if !errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil, false, err
	}
	existing, err := r.GetByIdentityHash(ctx, "oreate", item.IdentityHash)
	if err != nil || existing == nil {
		return nil, false, err
	}
	patch := map[string]any{
		"value":         item.Value,
		"status":        item.Status,
		"dead":          false,
		"fails":         0,
		"identity_hash": item.IdentityHash,
	}
	if strings.TrimSpace(item.AccountEmail) != "" {
		patch["account_email"] = item.AccountEmail
	}
	if err := r.UpdateMergingMeta(ctx, "oreate", existing.ID, metaPatch, patch); err != nil {
		return nil, false, err
	}
	updated, err := r.Get(ctx, "oreate", existing.ID)
	return updated, false, err
}

// UpsertDolaByIdentity inserts one Dola credential or rotates the existing row
// with the same sessionid identity, converging concurrent imports the same way
// the Oreate upsert does.
func (r *TokenRepository) UpsertDolaByIdentity(ctx context.Context, item *model.TokenAccount, metaPatch map[string]any) (*model.TokenAccount, bool, error) {
	if item == nil || item.Pool != "dola" || strings.TrimSpace(item.IdentityHash) == "" {
		return nil, false, errors.New("dola identity is required")
	}
	if err := r.Create(ctx, item); err == nil {
		return item, true, nil
	} else if !errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil, false, err
	}
	existing, err := r.GetByIdentityHash(ctx, "dola", item.IdentityHash)
	if err != nil || existing == nil {
		return nil, false, err
	}
	patch := map[string]any{
		"value":         item.Value,
		"status":        item.Status,
		"dead":          false,
		"fails":         0,
		"identity_hash": item.IdentityHash,
	}
	if err := r.UpdateMergingMeta(ctx, "dola", existing.ID, metaPatch, patch); err != nil {
		return nil, false, err
	}
	updated, err := r.Get(ctx, "dola", existing.ID)
	return updated, false, err
}

// RotateOreateSession atomically persists a browser-observed Cookie only while
// the account still contains the credential used to open that browser. A stale
// page therefore cannot overwrite a newer manual import.
func (r *TokenRepository) RotateOreateSession(ctx context.Context, id, expectedValue, replacementValue string, metaPatch map[string]any) (bool, error) {
	raw, err := json.Marshal(metaPatch)
	if err != nil {
		return false, err
	}
	result := r.db.WithContext(ctx).
		Model(&model.TokenAccount{}).
		Where("pool = ? AND id = ? AND value = ?", "oreate", id, expectedValue).
		Updates(map[string]any{
			"value":      replacementValue,
			"meta":       gorm.Expr("COALESCE(meta, '{}'::jsonb) || CAST(? AS jsonb)", string(raw)),
			"updated_at": time.Now(),
		})
	return result.RowsAffected == 1, result.Error
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

// UpdateMergingMetaIfGeneration fences asynchronous work started for an older
// credential import while atomically merging unrelated metadata.
func (r *TokenRepository) UpdateMergingMetaIfGeneration(ctx context.Context, pool, id, generationKey, expectedGeneration string, metaPatch map[string]any, patch map[string]any) (bool, error) {
	raw, err := json.Marshal(metaPatch)
	if err != nil {
		return false, err
	}
	updates := make(map[string]any, len(patch)+2)
	for key, value := range patch {
		updates[key] = value
	}
	updates["meta"] = gorm.Expr("COALESCE(meta, '{}'::jsonb) || CAST(? AS jsonb)", string(raw))
	updates["updated_at"] = time.Now()
	result := r.db.WithContext(ctx).
		Model(&model.TokenAccount{}).
		Where("pool = ? AND id = ? AND meta ->> ? = ?", pool, id, generationKey, expectedGeneration).
		Updates(updates)
	return result.RowsAffected == 1, result.Error
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
		if _, err := r.Update(ctx, t.Pool, t.ID, patch); err != nil {
			return recovered, err
		}
		recovered = append(recovered, *t)
	}
	return recovered, nil
}

// RollResetMarkers advances a stale (past) daily-reset marker to its next future
// occurrence — same time-of-day, +N whole days — for ACTIVE accounts of the given
// daily-reset pools, so the 恢复时间 column always shows the upcoming reset rather
// than yesterday's. Only active accounts are rolled: a 限额 account must keep its
// past marker so RecoverQuota can recover it (rolling it forward early would
// prevent recovery). Returns the number advanced.
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
		for !next.After(now) {
			next = next.Add(24 * time.Hour)
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
