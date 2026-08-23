package repo

import (
	"context"
	"sort"
	"strings"
	"time"

	"backend/internal/model"

	"gorm.io/gorm"
)

type ShowcaseRepository struct {
	db *gorm.DB
}

func NewShowcaseRepository(db *gorm.DB) *ShowcaseRepository {
	return &ShowcaseRepository{db: db}
}

// PublicFileSet returns the set of image keys referenced by any showcase item
// (normalized, no leading slash). The media-prune sweep uses it to never delete
// a file the homepage still shows, regardless of how old the file is.
func (r *ShowcaseRepository) PublicFileSet(ctx context.Context) (map[string]struct{}, error) {
	var images []string
	if err := r.db.WithContext(ctx).
		Model(&model.ShowcaseItem{}).
		Where("image <> ''").
		Pluck("image", &images).Error; err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(images))
	for _, img := range images {
		n := strings.TrimLeft(strings.TrimSpace(img), "/")
		if n != "" {
			set[n] = struct{}{}
		}
	}
	return set, nil
}

func (r *ShowcaseRepository) Grouped(ctx context.Context) (map[string][]model.ShowcaseItem, error) {
	var items []model.ShowcaseItem
	if err := r.db.WithContext(ctx).Find(&items).Error; err != nil {
		return nil, err
	}

	grouped := map[string][]model.ShowcaseItem{
		"hero":  {},
		"bento": {},
		"work":  {},
	}
	for _, item := range items {
		grouped[item.Kind] = append(grouped[item.Kind], item)
	}
	for kind := range grouped {
		sort.Slice(grouped[kind], func(i, j int) bool {
			return grouped[kind][i].Weight > grouped[kind][j].Weight
		})
	}
	return grouped, nil
}

func (r *ShowcaseRepository) Create(ctx context.Context, item *model.ShowcaseItem) error {
	return r.db.WithContext(ctx).Create(item).Error
}

func (r *ShowcaseRepository) Update(ctx context.Context, entryID string, patch map[string]any) (*model.ShowcaseItem, error) {
	patch["updated_at"] = time.Now()
	if err := r.db.WithContext(ctx).Model(&model.ShowcaseItem{}).Where("id = ?", entryID).Updates(patch).Error; err != nil {
		return nil, err
	}
	var item model.ShowcaseItem
	if err := r.db.WithContext(ctx).First(&item, "id = ?", entryID).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *ShowcaseRepository) Delete(ctx context.Context, entryID string) (int64, error) {
	res := r.db.WithContext(ctx).Delete(&model.ShowcaseItem{}, "id = ?", entryID)
	return res.RowsAffected, res.Error
}
