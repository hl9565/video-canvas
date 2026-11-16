package repository

import (
	"context"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// sortedUniqueUint64 对主键切片去重并严格升序排列，保证跨请求/并发批量的唯一无死锁加锁序。
func sortedUniqueUint64(ids []uint64) []uint64 {
	if len(ids) == 0 {
		return nil
	}
	out := make([]uint64, len(ids))
	copy(out, ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// lockOwnedByIDs 按 user_id + id ASC 批量加 FOR UPDATE 行锁，忽略重复或空输入。
func lockOwnedByIDs[T any](ctx context.Context, db *gorm.DB, userID uint64, ids []uint64) ([]T, error) {
	uniq := sortedUniqueUint64(ids)
	if len(uniq) == 0 {
		return nil, nil
	}
	var rows []T
	err := db.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND id IN ?", userID, uniq).
		Order("id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}
