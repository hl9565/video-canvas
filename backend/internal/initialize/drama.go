package initialize

import (
	"context"
	"time"

	"go.uber.org/zap"

	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/repository"
)

const (
	dramaFrameRunRecoveryInterval = 15 * time.Second
	dramaFrameRunPageSize         = 100
	dramaFrameRunMaxPagesPerTick  = 5
)

// maintainDramaFrameRuns 启动即按游标轮转恢复未完成流程；慢记录或长任务前缀不会饿死后续队列。
func (a *App) maintainDramaFrameRuns(ctx context.Context) {
	if a.dramaFrameRuns == nil {
		return
	}
	ticker := time.NewTicker(dramaFrameRunRecoveryInterval)
	defer ticker.Stop()
	var cursor repository.FrameRunRecoveryFilter
	for {
		scanCtx, cancel := context.WithTimeout(ctx, dramaFrameRunRecoveryInterval)
		for range dramaFrameRunMaxPagesPerTick {
			next, hasMore, err := a.dramaFrameRuns.RecoverUnsettledDramaFrameRunsPage(scanCtx, cursor, dramaFrameRunPageSize)
			if err != nil && ctx.Err() == nil {
				logger.Warn("短剧首帧流程恢复失败", zap.Error(err))
			}
			if !hasMore {
				cursor = repository.FrameRunRecoveryFilter{}
				break
			}
			cursor = next
			if scanCtx.Err() != nil {
				break
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
