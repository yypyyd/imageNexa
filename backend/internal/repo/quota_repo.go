package repo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrQuotaUnavailable = errors.New("account quota is insufficient")

type QuotaRepository struct{ db *gorm.DB }

type OpenQuotaReservation struct {
	Reservation model.QuotaReservation
	Bucket      model.AccountQuotaBucket
	Attempt     *model.DispatchAttempt
}

func NewQuotaRepository(db *gorm.DB) *QuotaRepository { return &QuotaRepository{db: db} }

func durableID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(raw[:])
}

func quotaBucketID(accountID, bucketKey string) string { return accountID + ":" + bucketKey }

func (r *QuotaRepository) Get(ctx context.Context, accountID, bucketKey string) (*model.AccountQuotaBucket, error) {
	var item model.AccountQuotaBucket
	if err := r.db.WithContext(ctx).First(&item, "account_id = ? AND bucket_key = ?", accountID, bucketKey).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// GetMany returns the requested account/bucket snapshots with one database
// round trip. Missing rows remain unknown and therefore are absent from the map.
func (r *QuotaRepository) GetMany(ctx context.Context, accountIDs, bucketKeys []string) (map[string]model.AccountQuotaBucket, error) {
	out := make(map[string]model.AccountQuotaBucket)
	if len(accountIDs) == 0 || len(bucketKeys) == 0 {
		return out, nil
	}
	var items []model.AccountQuotaBucket
	if err := r.db.WithContext(ctx).
		Where("account_id IN ? AND bucket_key IN ?", accountIDs, bucketKeys).
		Find(&items).Error; err != nil {
		return nil, err
	}
	for _, item := range items {
		out[QuotaSnapshotKey(item.AccountID, item.BucketKey)] = item
	}
	return out, nil
}

func QuotaSnapshotKey(accountID, bucketKey string) string {
	return strings.TrimSpace(accountID) + "\x00" + strings.TrimSpace(bucketKey)
}

// ClaimRefreshDue schedules bounded background probes without changing the
// last authoritative RefreshedAt. UpdatedAt provides a five-minute probe retry
// delay, including failures, and row locks prevent two workers claiming a row.
func (r *QuotaRepository) ClaimRefreshDue(ctx context.Context, now time.Time, limit int) ([]model.AccountQuotaBucket, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var items []model.AccountQuotaBucket
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("bucket_key NOT LIKE ?", model.DolaDailyVideoBucket+":%").
			Where("updated_at <= ?", now.Add(-5*time.Minute)).
			Where("refreshed_at IS NULL OR refreshed_at <= ? OR (reset_at <= ? AND refreshed_at < reset_at)", now.Add(-15*time.Minute), now).
			Where(`EXISTS (SELECT 1 FROM provider_accounts a WHERE a.id = account_quota_buckets.account_id
				AND a.dead = false AND a.status IN ('active', 'quota') AND a.value <> '' AND a.pool <> 'custom')`).
			Order("updated_at ASC, id ASC").Limit(limit).Find(&items).Error; err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		return tx.Model(&model.AccountQuotaBucket{}).Where("id IN ?", ids).UpdateColumn("updated_at", now).Error
	})
	return items, err
}

func (r *QuotaRepository) ListByAccount(ctx context.Context, accountID string) ([]model.AccountQuotaBucket, error) {
	var items []model.AccountQuotaBucket
	if err := r.db.WithContext(ctx).Where("account_id = ?", accountID).Order("bucket_key asc").Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// UpsertSnapshot records a provider balance while preserving every other
// in-flight hold. Provider remaining normally includes those accepted-but-not-
// reconciled jobs, so schedulable remaining is snapshot - reserved.
func (r *QuotaRepository) UpsertSnapshot(ctx context.Context, accountID, bucketKey, unit string, total, upstreamRemaining *float64, resetAt *time.Time) (*model.AccountQuotaBucket, error) {
	accountID, bucketKey = strings.TrimSpace(accountID), strings.TrimSpace(bucketKey)
	if accountID == "" || bucketKey == "" {
		return nil, errors.New("account and quota bucket are required")
	}
	if unit == "" {
		unit = "credits"
	}
	var result model.AccountQuotaBucket
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item model.AccountQuotaBucket
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&item, "account_id = ? AND bucket_key = ?", accountID, bucketKey).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			item = model.AccountQuotaBucket{ID: quotaBucketID(accountID, bucketKey), AccountID: accountID, BucketKey: bucketKey, Unit: unit}
			if err = tx.Create(&item).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		item.Unit = unit
		// Partial provider snapshots are common. A missing total/reset field means
		// "not observed in this probe", not "erase the last authoritative value".
		if total != nil {
			item.Total = cloneFloat(total)
		}
		if resetAt != nil {
			item.ResetAt = resetAt
		}
		if upstreamRemaining != nil {
			available := *upstreamRemaining - item.Reserved
			if available < 0 {
				available = 0
			}
			item.Remaining = &available
		}
		now := time.Now()
		item.RefreshedAt, item.Revision, item.UpdatedAt = &now, item.Revision+1, now
		if err := tx.Save(&item).Error; err != nil {
			return err
		}
		result = item
		return nil
	})
	return &result, err
}

// Reserve atomically deducts available allowance and persists the hold before
// any provider request is sent. Unknown balances are allowed but still accrue a
// reservation, so the first refresh cannot erase concurrent work.
func (r *QuotaRepository) Reserve(ctx context.Context, eventID, attemptID, accountID, bucketKey string, amount float64) (*model.QuotaReservation, error) {
	return r.ReserveWithUnit(ctx, eventID, attemptID, accountID, bucketKey, "credits", amount)
}

// ReserveWithUnit is Reserve with an explicit provider allowance unit. This is
// important for a newly observed bucket: points/generations must not silently
// become credits merely because the first operation was a reservation.
func (r *QuotaRepository) ReserveWithUnit(ctx context.Context, eventID, attemptID, accountID, bucketKey, unit string, amount float64) (*model.QuotaReservation, error) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return nil, errors.New("invalid quota amount")
	}
	unit = strings.TrimSpace(unit)
	if unit == "" {
		unit = "credits"
	}
	var result model.QuotaReservation
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var bucket model.AccountQuotaBucket
		if day, daily := model.DolaVideoBucketDay(bucketKey); daily {
			if unit != "generations" || amount != 1 {
				return errors.New("invalid Dola daily reservation")
			}
			total, remaining := float64(model.DolaDailyVideoLimit), float64(model.DolaDailyVideoLimit)
			reset := day.AddDate(0, 0, 1)
			initial := model.AccountQuotaBucket{ID: quotaBucketID(accountID, bucketKey), AccountID: accountID, BucketKey: bucketKey, Unit: unit, Total: &total, Remaining: &remaining, ResetAt: &reset}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; err != nil {
				return err
			}
		}
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&bucket, "account_id = ? AND bucket_key = ?", accountID, bucketKey).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			bucket = model.AccountQuotaBucket{ID: quotaBucketID(accountID, bucketKey), AccountID: accountID, BucketKey: bucketKey, Unit: unit}
			if err = tx.Create(&bucket).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if bucket.Remaining != nil && *bucket.Remaining+1e-9 < amount {
			return ErrQuotaUnavailable
		}
		if bucket.Remaining != nil {
			remaining := *bucket.Remaining - amount
			bucket.Remaining = &remaining
		}
		bucket.Reserved += amount
		bucket.Revision++
		if err := tx.Save(&bucket).Error; err != nil {
			return err
		}
		result = model.QuotaReservation{ID: durableID("qres_"), EventID: eventID, DispatchAttemptID: attemptID,
			QuotaBucketID: bucket.ID, Amount: amount, Status: "held"}
		return tx.Create(&result).Error
	})
	return &result, err
}

func (r *QuotaRepository) Settle(ctx context.Context, reservationID string, upstreamRemaining *float64) error {
	return r.finishReservation(ctx, reservationID, "settled", upstreamRemaining)
}

func (r *QuotaRepository) Release(ctx context.Context, reservationID string) error {
	return r.finishReservation(ctx, reservationID, "released", nil)
}

func (r *QuotaRepository) MarkUncertain(ctx context.Context, reservationID string) error {
	return r.db.WithContext(ctx).Model(&model.QuotaReservation{}).
		Where("id = ? AND status = ?", reservationID, "held").
		Updates(map[string]any{"status": "uncertain", "updated_at": time.Now()}).Error
}

func (r *QuotaRepository) ListOpen(ctx context.Context, olderThan time.Time, limit int) ([]OpenQuotaReservation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var reservations []model.QuotaReservation
	if err := r.db.WithContext(ctx).
		Where("status IN ? AND updated_at <= ?", []string{"held", "uncertain"}, olderThan).
		Order("updated_at asc").Limit(limit).Find(&reservations).Error; err != nil {
		return nil, err
	}
	out := make([]OpenQuotaReservation, 0, len(reservations))
	for _, reservation := range reservations {
		var bucket model.AccountQuotaBucket
		if err := r.db.WithContext(ctx).First(&bucket, "id = ?", reservation.QuotaBucketID).Error; err != nil {
			return nil, err
		}
		item := OpenQuotaReservation{Reservation: reservation, Bucket: bucket}
		if reservation.DispatchAttemptID != "" {
			var attempt model.DispatchAttempt
			err := r.db.WithContext(ctx).First(&attempt, "id = ?", reservation.DispatchAttemptID).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, err
			}
			if err == nil {
				item.Attempt = &attempt
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func (r *QuotaRepository) finishReservation(ctx context.Context, reservationID, target string, upstreamRemaining *float64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var reservation model.QuotaReservation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&reservation, "id = ?", reservationID).Error; err != nil {
			return err
		}
		if reservation.Status == target {
			return nil
		}
		if reservation.Status != "held" && reservation.Status != "uncertain" {
			return nil
		}
		var bucket model.AccountQuotaBucket
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&bucket, "id = ?", reservation.QuotaBucketID).Error; err != nil {
			return err
		}
		released := math.Min(bucket.Reserved, reservation.Amount)
		bucket.Reserved -= released
		if target == "released" && bucket.Remaining != nil && !dolaBucketExhausted(bucket) {
			remaining := *bucket.Remaining + released
			bucket.Remaining = &remaining
		} else if upstreamRemaining != nil {
			if _, daily := model.DolaVideoBucketDay(bucket.BucketKey); daily && *upstreamRemaining <= 0 {
				now := time.Now()
				bucket.RefreshedAt = &now // explicit upstream exhaustion seals this day, including later releases
			}
			remaining := *upstreamRemaining - bucket.Reserved
			if remaining < 0 {
				remaining = 0
			}
			bucket.Remaining = &remaining
		}
		bucket.Revision++
		if err := tx.Save(&bucket).Error; err != nil {
			return err
		}
		now := time.Now()
		return tx.Model(&reservation).Updates(map[string]any{"status": target, "settled_at": &now, "updated_at": now}).Error
	})
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func dolaBucketExhausted(bucket model.AccountQuotaBucket) bool {
	_, daily := model.DolaVideoBucketDay(bucket.BucketKey)
	return daily && bucket.RefreshedAt != nil
}
