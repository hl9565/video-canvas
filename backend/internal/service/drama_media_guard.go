package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"video-canvas/internal/drama"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
)

// checkDramaMediaTasks 同一集的片段视频和分镜、合成共享互斥，不影响其他集与文本任务。
func checkDramaMediaTasks(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, eps []model.DramaEpisode, key string) error {
	for _, ep := range eps {
		if ep.LockedAt == nil {
			return errcode.ErrDramaStepNotReady.WithMsg("媒体任务只允许已锁定的集")
		}
		rows, err := tx.Segments(ctx, p.UserID, ep.ID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			tasks, err := tx.SegmentTasks(ctx, p.UserID, p.CanvasID, segmentNode(row.ID))
			if err != nil {
				return err
			}
			for _, task := range tasks {
				if slices.Contains(model.ActiveTaskStatuses, task.Status) && (key == "" || task.IdempotencyKey != key) {
					return errcode.ErrDramaJobBusy
				}
			}
		}
	}
	return nil
}

// guardDramaGeneration 在生成任务自身事务里锁短剧项目，防止两个片段同时穿透预检查。
func guardDramaGeneration(ctx context.Context, tx repository.GenerationTaskTx, task *model.GenerationTask) error {
	if task.IsTest || !strings.HasPrefix(task.NodeID, "seg:") {
		return nil
	}
	adapter, ok := tx.(interface{ DramaTx() repository.DramaTx })
	if !ok {
		return errcode.ErrInternal
	}
	dtx := adapter.DramaTx()
	sid, ok := idcodec.Decode(strings.TrimPrefix(task.NodeID, "seg:"))
	if !ok {
		return errcode.ErrInvalidParams
	}
	p, ep, row, err := lockSegment(ctx, dtx, task.UserID, sid)
	if err != nil {
		return segmentError(err)
	}
	if task.CanvasProjectID == nil || *task.CanvasProjectID != p.CanvasID || task.Kind != model.KindVideo {
		return errcode.ErrInvalidParams
	}
	if len(task.InputJSON) > 0 {
		var in map[string]any
		if err := json.Unmarshal(task.InputJSON, &in); err == nil {
			if err := checkSegmentTimeline(in, row.Seconds); err != nil {
				return err
			}
		}
	}
	jobs, err := dtx.Jobs(ctx, p.UserID, p.ID, true)
	if err != nil {
		return err
	}
	if dramaJobsConflict(jobs, &model.DramaJobReq{Step: "segment", Episodes: []int{ep.No}}) {
		return errcode.ErrDramaJobBusy
	}
	return checkDramaMediaTasks(ctx, dtx, p, []model.DramaEpisode{*ep}, task.IdempotencyKey)
}

func hasSegmentNode(nodes []string) bool {
	for _, nodeID := range nodes {
		if strings.HasPrefix(nodeID, "seg:") {
			return true
		}
	}
	return false
}

// checkSegmentTimeline 校验短剧片段提交的提示词时间轴是否超出最终生效的生成时长。
func checkSegmentTimeline(input map[string]any, fallbackSeconds int) error {
	prompt, _ := input["prompt"].(string)
	sec := inputDurationSeconds(input)
	if sec <= 0 {
		sec = fallbackSeconds
	}
	if sec <= 0 {
		return nil
	}
	if end, over := drama.TimelineOverrun(prompt, sec); over {
		return errcode.ErrTaskInput.WithMsg(fmt.Sprintf("%s提示词时间轴写到 %s 秒，超过生成时长 %d 秒，请调整提示词或重新转写",
			taskUserVisibleErrPrefix, strconv.FormatFloat(end, 'f', -1, 64), sec))
	}
	return nil
}

func inputDurationSeconds(input map[string]any) int {
	if input == nil {
		return 0
	}
	switch v := input[modelcfg.DurationParam].(type) {
	case int:
		return max(v, 0)
	case int64:
		return int(max(v, 0))
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 {
			return int(math.Round(v))
		}
	case json.Number:
		if f, err := v.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 {
			return int(math.Round(f))
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 {
			return int(math.Round(f))
		}
	}
	return 0
}
