package repository

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"video-canvas/internal/model"
)

// AssetMediaProbeRepository 管理素材规范 24fps 媒体探测缓存的持久化与幂等读取。
type AssetMediaProbeRepository struct {
	db *gorm.DB
}

// NewAssetMediaProbeRepository 构造素材探测缓存仓储。
func NewAssetMediaProbeRepository(db *gorm.DB) *AssetMediaProbeRepository {
	return &AssetMediaProbeRepository{db: db}
}

// Get 按 (asset_id, sha256, probe_version) 精确查询缓存；未命中返回 ErrNotFound。
func (r *AssetMediaProbeRepository) Get(ctx context.Context, assetID uint64, sha256 string, probeVersion int16) (*model.AssetMediaProbe, error) {
	var row model.AssetMediaProbe
	err := r.db.WithContext(ctx).
		Where("asset_id = ? AND sha256 = ? AND probe_version = ?", assetID, sha256, probeVersion).
		Take(&row).Error
	if err != nil {
		return nil, translate(err)
	}
	return &row, nil
}

// Upsert 按复合主键幂等写入探测结果；相同 (asset_id, sha256, probe_version) 并发写入静默忽略冲突。
func (r *AssetMediaProbeRepository) Upsert(ctx context.Context, probe *model.AssetMediaProbe) error {
	return r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "asset_id"}, {Name: "sha256"}, {Name: "probe_version"}},
			DoNothing: true,
		}).
		Create(probe).Error
}
