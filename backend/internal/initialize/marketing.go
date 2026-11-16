package initialize

import (
	"context"
	"time"

	"go.uber.org/zap"

	"video-canvas/internal/pkg/logger"
)

const marketingRecoveryInterval = 15 * time.Second

// maintainMarketing 启动即扫描并定时回收租约；不依赖worker开关，退出前等待停止，旧NULL租约默认不回收。
func (a *App) maintainMarketing(ctx context.Context) {
	if a.marketing == nil {
		return
	}
	ticker := time.NewTicker(marketingRecoveryInterval)
	defer ticker.Stop()
	for {
		scanCtx, cancel := context.WithTimeout(ctx, marketingRecoveryInterval)
		err := a.marketing.RecoverUnsettledMarketingRuns(scanCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			logger.Warn("营销运行恢复失败", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
