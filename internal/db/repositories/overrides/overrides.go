// Package overrides is the data access for per-user limit overrides.
package overrides

import (
	"context"
	"errors"
	"time"

	"github.com/boson-chat/files/internal/db"
	"github.com/boson-chat/files/metrics"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Override replaces a class default for one quota key. Nil = default.
type Override struct {
	Owner        string `gorm:"primaryKey"`
	QuotaBytes   *int64
	MaxFileBytes *int64
	UpdatedAt    time.Time `gorm:"autoUpdateTime:false"`
	UpdatedBy    *string
}

func (Override) TableName() string { return "quota_overrides" }

type OverrideRepositoryImpl interface {
	// Get returns nil (no error) when owner has no override.
	Get(ctx context.Context, owner string) (*Override, error)
	Put(ctx context.Context, o *Override) error
	Delete(ctx context.Context, owner string) error
}

type OverrideRepository struct{ db *db.DB }

func NewOverrideRepository(d *db.DB) OverrideRepositoryImpl { return &OverrideRepository{db: d} }

func record(ctx context.Context, start time.Time, op string, errp *error) {
	err := *errp
	res := metrics.Success
	if err != nil {
		res = metrics.Error
	}
	metrics.GetAppMetrics().DatabaseMetric(ctx, time.Since(start).Seconds(), "quota_overrides", op, res)
}

func (r *OverrideRepository) Get(ctx context.Context, owner string) (_ *Override, err error) {
	defer record(ctx, time.Now(), "select", &err)
	var o Override
	err = r.db.Conn(ctx).Where("owner = ?", owner).Take(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *OverrideRepository) Put(ctx context.Context, o *Override) (err error) {
	defer record(ctx, time.Now(), "upsert", &err)
	return r.db.Conn(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "owner"}},
		DoUpdates: clause.AssignmentColumns([]string{"quota_bytes", "max_file_bytes", "updated_at", "updated_by"}),
	}).Create(o).Error
}

func (r *OverrideRepository) Delete(ctx context.Context, owner string) (err error) {
	defer record(ctx, time.Now(), "delete", &err)
	return r.db.Conn(ctx).Where("owner = ?", owner).Delete(&Override{}).Error
}
