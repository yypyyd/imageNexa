package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"backend/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type BannedWordRepository struct {
	db *gorm.DB
}

func NewBannedWordRepository(db *gorm.DB) *BannedWordRepository {
	return &BannedWordRepository{db: db}
}

func (r *BannedWordRepository) List(ctx context.Context) ([]model.BannedWord, error) {
	var items []model.BannedWord
	err := r.db.WithContext(ctx).Order("created_at DESC, hits DESC").Find(&items).Error
	return items, err
}

func (r *BannedWordRepository) Create(ctx context.Context, word string) (*model.BannedWord, error) {
	word = strings.TrimSpace(word)
	if word == "" {
		return nil, errors.New("违禁词不能为空")
	}
	item := &model.BannedWord{
		ID:        strings.ReplaceAll(uuid.NewString(), "-", "")[:32],
		Word:      word,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := r.db.WithContext(ctx).Create(item).Error; err != nil {
		return nil, errors.New("添加失败(可能已存在)")
	}
	return item, nil
}

// BulkCreate inserts the given words, skipping blanks and ones already in the
// table (case-insensitive). Returns how many were added vs skipped.
func (r *BannedWordRepository) BulkCreate(ctx context.Context, words []string) (added, skipped int, err error) {
	existing, err := r.List(ctx)
	if err != nil {
		return 0, 0, err
	}
	seen := make(map[string]bool, len(existing))
	for _, w := range existing {
		seen[strings.ToLower(w.Word)] = true
	}
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		key := strings.ToLower(w)
		if seen[key] {
			skipped++
			continue
		}
		item := &model.BannedWord{
			ID:        strings.ReplaceAll(uuid.NewString(), "-", "")[:32],
			Word:      w,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		if e := r.db.WithContext(ctx).Create(item).Error; e != nil {
			skipped++
			continue
		}
		seen[key] = true
		added++
	}
	return added, skipped, nil
}

func (r *BannedWordRepository) Delete(ctx context.Context, id string) (int64, error) {
	res := r.db.WithContext(ctx).Delete(&model.BannedWord{}, "id = ?", id)
	return res.RowsAffected, res.Error
}

// RecordHit bumps the word's block counter and appends a BannedWordHit row for
// the admin trigger list. userID/userName are request-identity snapshots only;
// there is no mutable legacy User record in the 2API product.
func (r *BannedWordRepository) RecordHit(ctx context.Context, wordID, word, userID, userName, prompt string) {
	_ = r.db.WithContext(ctx).Model(&model.BannedWord{}).Where("id = ?", wordID).
		UpdateColumn("hits", gorm.Expr("hits + 1")).Error
	_ = r.db.WithContext(ctx).Create(&model.BannedWordHit{
		ID:        strings.ReplaceAll(uuid.NewString(), "-", "")[:32],
		WordID:    wordID,
		Word:      word,
		UserID:    userID,
		UserName:  userName,
		Prompt:    prompt,
		CreatedAt: time.Now(),
	}).Error
}

// ListHits returns trigger records newest first, with pagination + total.
// query — server-side search over 违禁词 / 用户名 / 提示词 (跨页).
func (r *BannedWordRepository) ListHits(ctx context.Context, query string, limit, offset int) ([]model.BannedWordHit, int64, error) {
	var out []model.BannedWordHit
	var total int64
	q := r.db.WithContext(ctx).Model(&model.BannedWordHit{})
	if term := strings.TrimSpace(query); term != "" {
		like := "%" + term + "%"
		q = q.Where("(word ILIKE ? OR user_name ILIKE ? OR user_id ILIKE ? OR prompt ILIKE ?)", like, like, like, like)
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 50
	}
	err := q.Order("created_at desc").Limit(limit).Offset(offset).Find(&out).Error
	return out, total, err
}
