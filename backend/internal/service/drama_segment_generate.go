package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
)

// Generate 固定视频种类并在归属检查后优先核验重放，实时业务守卫由任务事务再次执行。
func (s *DramaSegmentService) Generate(ctx context.Context, userID, sid uint64, key string, req *model.GenerateDramaSegmentReq) (*model.GenerationTaskView, error) {
	// 1. 保留键在读取目标之前拒绝；归属读取不检查锁定、繁忙或时长。
	if req == nil || req.ModelID == "" || req.Input == nil || strings.HasPrefix(key, InternalDramaFrameRunKeyPrefix) {
		return nil, errcode.ErrInvalidParams
	}
	p, ep, row, err := s.segmentSource(ctx, userID, sid)
	if err != nil {
		return nil, err
	}
	createReq := segmentCreateRequest(p, sid, req)
	replay, err := s.tasks.CheckIdempotentReplay(ctx, userID, key, createReq)
	if err != nil || replay != nil {
		return replay, err
	}
	// 2. 未命中才验证当前集状态，防止后续状态漂移阻断合法重放。
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		lockedP, lockedEp, lockedRow, err := lockSegment(ctx, tx, userID, sid)
		if err != nil {
			return err
		}
		p, ep, row = lockedP, lockedEp, lockedRow
		if ep.LockedAt == nil {
			return errcode.ErrDramaStepNotReady.WithMsg("媒体任务只允许已锁定的集")
		}
		jobs, err := tx.Jobs(ctx, userID, p.ID, true)
		if err != nil {
			return err
		}
		if dramaJobsConflict(jobs, &model.DramaJobReq{Step: "segment", Episodes: []int{ep.No}}) {
			return errcode.ErrDramaJobBusy
		}
		return nil
	})
	if err != nil {
		return nil, segmentError(err)
	}
	if err := checkSegmentTimeline(req.Input, row.Seconds); err != nil {
		return nil, err
	}
	result, err := s.tasks.Create(ctx, userID, key, segmentCreateRequest(p, sid, req))
	if err != nil {
		return nil, err
	}
	return segmentCreatedTask(result, p, sid)
}

// Confirm 校验真实首帧、固定 I2V 能力和实时单镜报价，再以流程 CAS 冻结确认快照。
func (s *DramaFrameRunService) Confirm(ctx context.Context, userID, sid, rid uint64, req *model.ConfirmFrameRunReq) (*model.DramaSegmentFrameRunView, error) {
	// 1. 图像读取、模型解析与报价校验均在锁外执行。
	if req == nil || req.Revision < 1 || req.Input == nil || req.QuoteToken == "" {
		return nil, errcode.ErrInvalidParams
	}
	run, err := s.loadFrameRun(ctx, userID, sid, rid)
	if err != nil {
		return nil, err
	}
	if run.State != model.FrameRunStateFrameReady && run.State != model.FrameRunStateConfirmed {
		return nil, errcode.ErrDramaFrameRunConflict
	}
	input, err := s.frameConfirmInput(ctx, run, req)
	if err != nil {
		return nil, err
	}
	p, _, seg, err := s.segments.segmentSource(ctx, userID, sid)
	if err != nil {
		return nil, err
	}
	if req.SegmentRevision != nil && *req.SegmentRevision != seg.Revision {
		return nil, errcode.ErrCanvasConflict
	}
	if err := checkSegmentTimeline(input, seg.Seconds); err != nil {
		return nil, err
	}
	quoteReq := &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: s.i2vModelKey, CanvasID: idcodec.ID(p.CanvasID), NodeID: segmentNode(sid), Input: input}
	if _, err := s.tasks.VerifyQuoteToken(ctx, userID, quoteReq, req.QuoteToken, QuoteTaskOptions{SegmentID: sid, SegmentRevision: seg.Revision, SingleShotOnly: true}); err != nil {
		return nil, err
	}
	// 2. 再次锁定父链与流程，保证验价期间的换图、片段修改或取消均不会被覆盖。
	confirmed, err := s.saveFrameConfirmation(ctx, run, seg.Revision, input, req)
	if err != nil {
		return nil, err
	}
	return s.changedFrameRunView(ctx, confirmed)
}

// frameConfirmInput 锁定唯一视频模型及真实首帧内容，输出可复现、显式时长和分辨率的 I2V 输入。
func (s *DramaFrameRunService) frameConfirmInput(ctx context.Context, run *model.DramaSegmentFrameRun, req *model.ConfirmFrameRunReq) (map[string]any, error) {
	if req.ModelID != "" && req.ModelID != s.i2vModelKey {
		return nil, errcode.ErrInvalidParams
	}
	snap, err := s.registry.Snapshot(ctx, s.i2vModelKey)
	if err != nil {
		// 模型下架使既有报价失效，沿用验价错误，不能将供应商哨兵暴露成内部故障。
		return nil, quoteSnapshotError(err)
	}
	if snap == nil || snap.Model.Key != s.i2vModelKey || snap.Model.Kind != modelcfg.KindVideo || !slices.Contains(snap.Model.Capabilities.Ops, modelcfg.OpI2V) ||
		!snap.Model.Capabilities.Refs.Image.On || snap.Model.Capabilities.Refs.Image.Max < 1 {
		return nil, errcode.ErrInvalidParams
	}
	if run.FrameAssetID == nil || req.FrameAssetID != nil && uint64(*req.FrameAssetID) != *run.FrameAssetID {
		return nil, errcode.ErrDramaFrameInvalid
	}
	asset, img, err := s.loadFrameImage(ctx, run.UserID, *run.FrameAssetID)
	if err != nil {
		return nil, err
	}
	if len(req.FrameSHA256) != 64 || asset.Sha256 != req.FrameSHA256 || !validDramaFrameRatio(img.Bounds().Dx(), img.Bounds().Dy()) {
		return nil, errcode.ErrDramaFrameInvalid
	}
	return canonicalFrameI2VInput(snap, *run.FrameAssetID, req.Input)
}

// canonicalFrameI2VInput 固定单图、单镜及 16:9，其他参数只保留模型验证后的有效值。
func canonicalFrameI2VInput(snap *provider.Snapshot, frameID uint64, raw map[string]any) (map[string]any, error) {
	prompt, _ := raw["prompt"].(string)
	resolution, _ := raw["resolution"].(string)
	duration := frameDuration(raw["duration"])
	if strings.TrimSpace(prompt) == "" || strings.TrimSpace(resolution) == "" || duration < 1 {
		return nil, errcode.ErrTaskInput
	}
	if op, ok := raw["op"]; ok && op != modelcfg.OpI2V {
		return nil, errcode.ErrInvalidParams
	}
	input := cloneMap(raw)
	input["op"], input["aspect_ratio"], input["images"], input["duration"] = modelcfg.OpI2V, "16:9", []uint64{frameID}, duration
	if fp := modelcfg.FanoutParam(snap.Model.Capabilities); fp != "" {
		input[fp] = 1
	}
	normalized, fieldErrors := modelcfg.ValidateInput(model.KindVideo, snap.Model.Capabilities, input)
	if len(fieldErrors) > 0 {
		return nil, errcode.ErrTaskInput.WithMsg(modelcfg.JoinFieldErrors(fieldErrors))
	}
	if frameDuration(normalized["duration"]) != duration || normalized["resolution"] != resolution {
		return nil, errcode.ErrTaskInput
	}
	if ratio, ok := normalized["aspect_ratio"]; ok && ratio != "16:9" {
		return nil, errcode.ErrTaskInput
	}
	normalized["duration"], normalized["aspect_ratio"] = duration, "16:9"
	return normalized, nil
}

// frameDuration 只接受显式正整数秒，不将小数或带单位字符串四舍五入成时长。
func frameDuration(value any) int {
	switch n := value.(type) {
	case int:
		return max(n, 0)
	case int64:
		return int(max(n, 0))
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 1 && n < float64(math.MaxInt) && math.Trunc(n) == n {
			return int(n)
		}
	case json.Number:
		if n, err := n.Int64(); err == nil {
			return int(max(n, 0))
		}
	}
	return 0
}

// saveFrameConfirmation 原子递增确认版本并固定内部视频键，旧版本不能重新获得同一个键。
func (s *DramaFrameRunService) saveFrameConfirmation(ctx context.Context, before *model.DramaSegmentFrameRun, segRev int64, input map[string]any, req *model.ConfirmFrameRunReq) (*model.DramaSegmentFrameRun, error) {
	raw, err := jsonhash.CanonicalJSON(input)
	if err != nil {
		return nil, err
	}
	hash := jsonhash.SHA256Hex(raw)
	var run *model.DramaSegmentFrameRun
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		_, _, seg, err := lockSegment(ctx, tx, before.UserID, before.SegmentID)
		if err != nil {
			return err
		}
		if seg.Revision != segRev {
			return errcode.ErrCanvasConflict
		}
		run, err = tx.FrameRun(ctx, before.UserID, before.ID, true)
		run, err = ownedFrameRun(run, before.SegmentID, err)
		if err != nil {
			return err
		}
		if run.State != model.FrameRunStateFrameReady && run.State != model.FrameRunStateConfirmed || run.Revision != req.Revision {
			return errcode.ErrDramaFrameRunConflict
		}
		if !sameUint64Ptr(run.FrameAssetID, before.FrameAssetID) {
			return errcode.ErrDramaFrameInvalid
		}
		run.ConfirmedVersion++
		run.State, run.ModelKey = model.FrameRunStateConfirmed, s.i2vModelKey
		run.ConfirmedInput, run.ConfirmedInputHash, run.QuoteToken = raw, &hash, &req.QuoteToken
		run.VideoIdempotencyKey = InternalDramaFrameRunKeyPrefix + idcodec.Encode(run.ID) + ":v" + strconv.FormatInt(run.ConfirmedVersion, 10)
		run.ErrorCode, run.ErrorMessage = "", ""
		if req.SegmentRevision != nil {
			run.SegmentRevision = *req.SegmentRevision
		}
		if req.AdoptIfEmpty != nil {
			run.AdoptIfEmpty = *req.AdoptIfEmpty
		}
		return tx.SaveFrameRun(ctx, run, req.Revision)
	})
	return run, segmentError(err)
}

// Submit 只提交已确认快照；已有任务优先幂等重放，报价过期也不会重新冻结积分。
func (s *DramaFrameRunService) Submit(ctx context.Context, userID, sid, rid uint64, req *model.SubmitFrameRunReq) (*model.DramaSegmentFrameRunView, *model.GenerationTaskView, error) {
	// 1. 已提交和已绑定使用原快照；尚未提交的确认先持久化 submitting。
	if req == nil || req.Revision < 1 || req.ConfirmedVersion < 1 {
		return nil, nil, errcode.ErrInvalidParams
	}
	run, err := s.loadFrameRun(ctx, userID, sid, rid)
	if err != nil {
		return nil, nil, err
	}
	if run.ConfirmedVersion != req.ConfirmedVersion {
		return nil, nil, errcode.ErrDramaFrameRunConflict
	}
	switch run.State {
	case model.FrameRunStateVideoPending, model.FrameRunStateBound:
	case model.FrameRunStateConfirmed, model.FrameRunStateSubmitting:
		run, err = s.beginFrameSubmit(ctx, userID, sid, rid, req)
		// 并发提交可能在无锁读取后先完成；锁内仍拒绝状态漂移，外层只重放已提交的同一确认版本。
		if err != nil && run != nil && run.ConfirmedVersion == req.ConfirmedVersion &&
			(run.State == model.FrameRunStateVideoPending || run.State == model.FrameRunStateBound) {
			var business *errcode.Error
			if errors.As(err, &business) && business.Code == errcode.ErrDramaFrameRunConflict.Code {
				err = nil
			}
		}
	default:
		err = errcode.ErrDramaFrameRunConflict
	}
	if err != nil {
		return nil, nil, err
	}
	// 2. 任务服务在独立事务中冻结积分和绑定；网络断线后的重入沿用相同内部键。
	task, err := s.createFrameVideo(ctx, run)
	if err != nil {
		return nil, nil, s.frameSubmitError(ctx, run, err)
	}
	current, err := s.loadFrameRun(ctx, userID, sid, rid)
	if err != nil {
		return nil, nil, err
	}
	if err := s.reconcileFrameRun(ctx, current); err != nil {
		return nil, nil, err
	}
	view, err := s.changedFrameRunView(ctx, current)
	return view, task, err
}

// beginFrameSubmit 用父链及流程锁保护媒体互斥，仅首次提交要求原流程 revision。
func (s *DramaFrameRunService) beginFrameSubmit(ctx context.Context, userID, sid, rid uint64, req *model.SubmitFrameRunReq) (*model.DramaSegmentFrameRun, error) {
	var run *model.DramaSegmentFrameRun
	err := s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, ep, _, err := lockSegment(ctx, tx, userID, sid)
		if err != nil {
			return err
		}
		run, err = tx.FrameRun(ctx, userID, rid, true)
		run, err = ownedFrameRun(run, sid, err)
		if err != nil {
			return err
		}
		if run.State != model.FrameRunStateConfirmed && run.State != model.FrameRunStateSubmitting || run.ConfirmedVersion != req.ConfirmedVersion ||
			run.ConfirmedInputHash == nil || run.QuoteToken == nil || run.VideoIdempotencyKey == "" {
			return errcode.ErrDramaFrameRunConflict
		}
		if err := checkFrameRunMedia(ctx, tx, p, ep, run.VideoIdempotencyKey); err != nil {
			return err
		}
		if run.State == model.FrameRunStateSubmitting {
			return nil
		}
		if run.Revision != req.Revision {
			return errcode.ErrCanvasConflict
		}
		now := s.now()
		run.State, run.SubmittingAt = model.FrameRunStateSubmitting, &now
		return tx.SaveFrameRun(ctx, run, req.Revision)
	})
	return run, segmentError(err)
}

// createFrameVideo 读取原始确认 JSON 时保留整数精度，跨重启重放时输入哈希和幂等意图不变。
func (s *DramaFrameRunService) createFrameVideo(ctx context.Context, run *model.DramaSegmentFrameRun) (*model.GenerationTaskView, error) {
	p, _, _, err := s.segments.segmentSource(ctx, run.UserID, run.SegmentID)
	if err != nil {
		return nil, err
	}
	var input map[string]any
	dec := json.NewDecoder(bytes.NewReader(run.ConfirmedInput))
	dec.UseNumber()
	if err := dec.Decode(&input); err != nil || input == nil || run.QuoteToken == nil || run.ConfirmedInputHash == nil {
		return nil, errcode.ErrDramaFrameRunInconsistent
	}
	hash, err := jsonhash.CanonicalSHA256(input)
	if err != nil || hash != *run.ConfirmedInputHash {
		return nil, errcode.ErrDramaFrameRunInconsistent
	}
	req := &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: run.ModelKey, CanvasID: idcodec.ID(p.CanvasID),
		NodeID: segmentNode(run.SegmentID), Input: input, QuoteToken: *run.QuoteToken}
	resp, err := s.tasks.CreateWithOptions(ctx, run.UserID, run.VideoIdempotencyKey, req,
		CreateTaskOptions{DramaRunBind: &DramaFrameRunBind{RunID: run.ID, ConfirmRev: run.ConfirmedVersion}})
	if err != nil {
		return nil, err
	}
	return frameCreatedTask(resp, req)
}

// frameSubmitError 报价失效保留首帧供重新确认，余额不足结束流程；不确定的断线保留 submitting 等待恢复。
func (s *DramaFrameRunService) frameSubmitError(ctx context.Context, run *model.DramaSegmentFrameRun, cause error) error {
	var business *errcode.Error
	if !errors.As(cause, &business) || run.State != model.FrameRunStateSubmitting {
		return cause
	}
	var err error
	switch {
	case business.Code == errcode.ErrDramaQuoteExpired.Code:
		err = s.commitFrameObservation(ctx, run, func(row *model.DramaSegmentFrameRun) {
			row.State, row.SubmittingAt, row.QuoteToken = model.FrameRunStateFrameReady, nil, nil
		})
	case business.HTTPStatus() == 402:
		code, message := frameRunFailure(cause)
		err = s.failFrameObservation(ctx, run, code, message)
	}
	if err != nil {
		return err
	}
	return cause
}

// Quote 无锁读取片段归属与版本，报价令牌绑定该版本和精裁单镜约束。
func (s *DramaSegmentService) Quote(ctx context.Context, userID, sid uint64, req *model.GenerateDramaSegmentReq) (*model.TaskQuoteView, error) {
	// 1. 请求结构与生成入口一致；报价不要求集已锁定，也不冻结积分。
	if req == nil || req.ModelID == "" || req.Input == nil {
		return nil, errcode.ErrInvalidParams
	}
	p, ep, row, err := s.segmentSource(ctx, userID, sid)
	if err != nil {
		return nil, err
	}
	return s.tasks.Quote(ctx, userID, &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: req.ModelID, CanvasID: idcodec.ID(p.CanvasID), NodeID: segmentNode(sid), Input: req.Input}, QuoteTaskOptions{SegmentID: sid, SegmentRevision: row.Revision, SingleShotOnly: ep.EditSchema == 1})
}

// segmentSource 无锁读取存活的段、集和项目，统一隐藏越权资源。
func (s *DramaSegmentService) segmentSource(ctx context.Context, userID, sid uint64) (*model.DramaProject, *model.DramaEpisode, *model.DramaSegment, error) {
	row, err := s.repo.Segment(ctx, userID, sid, false)
	if err != nil {
		return nil, nil, nil, segmentError(err)
	}
	ep, err := s.repo.SegmentEpisode(ctx, userID, row.EpisodeID)
	if err != nil {
		return nil, nil, nil, segmentError(err)
	}
	p, err := s.repo.Project(ctx, userID, ep.DramaID, false)
	if err != nil {
		return nil, nil, nil, segmentError(err)
	}
	return p, ep, row, nil
}

// segmentCreateRequest 将服务端认可的归属与客户端报价一起传给任务服务。
func segmentCreateRequest(p *model.DramaProject, sid uint64, req *model.GenerateDramaSegmentReq) *model.CreateGenerationTaskReq {
	return &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: req.ModelID, CanvasID: idcodec.ID(p.CanvasID), NodeID: segmentNode(sid), Input: req.Input, QuoteToken: req.QuoteToken}
}

// segmentCreatedTask 将单项错误恢复为标准业务错误，并检查返回任务的归属。
func segmentCreatedTask(result *model.CreateGenerationTaskResp, project *model.DramaProject, sid uint64) (*model.GenerationTaskView, error) {
	if result == nil || len(result.Items) != 1 {
		return nil, errors.New("片段任务创建未返回唯一结果")
	}
	item := result.Items[0]
	if item.Error != nil {
		return nil, errcode.New(item.Error.Code, item.Error.Message, item.Error.Status)
	}
	if item.Task == nil {
		return nil, errors.New("片段任务创建结果缺少任务")
	}
	if item.Task.NodeID != segmentNode(sid) || item.Task.CanvasProjectID == nil || uint64(*item.Task.CanvasProjectID) != project.CanvasID || item.Task.Kind != model.KindVideo {
		return nil, errcode.ErrIdempotencyConflict
	}
	return item.Task, nil
}
