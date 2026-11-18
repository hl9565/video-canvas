package service

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/repository"
)

type dramaJobIntent struct {
	req              model.DramaJobReq
	signature        string
	approvalID       *uint64
	serverDerivedKey bool
}

// newDramaJobIntent 在任何默认值覆盖前冻结客户端意图，保留内部审批键的命名空间。
func newDramaJobIntent(id uint64, approvalID *uint64, req *model.DramaJobReq) (*dramaJobIntent, error) {
	if err := validateDramaJobRequest(req); err != nil {
		return nil, err
	}
	// Agent 审批只授权生成，清空需要用户通过提交接口确认。
	if approvalID != nil && req.ConfirmClear {
		return nil, errcode.ErrInvalidParams
	}
	copyReq := *req
	copyReq.Episodes = canonicalDramaEpisodes(req.Episodes)
	compose := model.DramaComposeReq{}
	if req.Compose != nil {
		compose = *req.Compose
	}
	compose.Variants = canonicalDramaVariants(compose.Variants)
	copyReq.Compose = &compose
	// 沿用编码后的分集主键形态，保持已有请求签名可重放。
	sig, err := jsonhash.CanonicalSHA256(struct {
		DramaID                uint64                `json:"drama_id"`
		Step                   string                `json:"step"`
		Episodes               []int                 `json:"ordered_episodes"`
		Ratio                  string                `json:"ratio"`
		RawMaxSeconds          int                   `json:"raw_max_seconds"`
		ExpectedEpisodeID      *idcodec.ID           `json:"expected_episode_id,omitempty"`
		Compose                model.DramaComposeReq `json:"compose"`
		ConfirmClear           bool                  `json:"confirm_clear,omitempty"`
		ExpectedSegmentVersion *int64                `json:"expected_segment_version,omitempty"`
	}{
		DramaID: id, Step: req.Step, Episodes: copyReq.Episodes, Ratio: req.Ratio,
		RawMaxSeconds: req.MaxSeconds, ExpectedEpisodeID: (*idcodec.ID)(req.ExpectedEpisodeID), Compose: compose,
		ConfirmClear: req.ConfirmClear, ExpectedSegmentVersion: req.ExpectedSegmentVersion,
	})
	if err != nil {
		return nil, errcode.ErrInvalidParams
	}
	intent := &dramaJobIntent{req: copyReq, signature: sig, approvalID: approvalID}
	if approvalID != nil && req.Step == "compose" && copyReq.ComposeRequestKey == "" {
		intent.serverDerivedKey = true
		intent.req.ComposeRequestKey = "approval:" + strconv.FormatUint(*approvalID, 10)
	}
	return intent, nil
}

// validateDramaJobRequest 在查询前拒绝保留键和跨步骤参数；服务调用也执行 HTTP 同等校验。
func validateDramaJobRequest(req *model.DramaJobReq) error {
	if req == nil || len(req.ComposeRequestKey) > 96 || strings.HasPrefix(req.ComposeRequestKey, "approval:") {
		return errcode.ErrInvalidParams
	}
	if req.Step != "compose" && (req.ComposeRequestKey != "" || !emptyDramaCompose(req.Compose)) {
		return errcode.ErrInvalidParams
	}
	if req.ExpectedEpisodeID != nil && req.Step != "storyboard" && req.Step != "compose" {
		return errcode.ErrInvalidParams
	}
	if err := validateStoryboardClear(req); err != nil {
		return err
	}
	if req.Compose != nil {
		return validateDramaComposeRequest(req.Compose)
	}
	return nil
}

// validateStoryboardClear 片段版本只用于显式清空，必须同时提供目标身份。
func validateStoryboardClear(req *model.DramaJobReq) error {
	if !req.ConfirmClear && req.ExpectedSegmentVersion == nil {
		return nil
	}
	if req.Step != "storyboard" || !req.ConfirmClear || req.ExpectedEpisodeID == nil ||
		req.ExpectedSegmentVersion == nil || *req.ExpectedSegmentVersion < 0 {
		return errcode.ErrInvalidParams
	}
	return nil
}

// canonicalDramaEpisodes 规范集号顺序但不删除重复项，由既有范围校验拒绝重复。
func canonicalDramaEpisodes(values []int) []int {
	out := append([]int{}, values...)
	slices.Sort(out)
	return out
}

// canonicalDramaVariants 将变体集合固定排序，缺省仍为空以区别显式的新请求参数。
func canonicalDramaVariants(values []string) []string {
	out := append([]string{}, values...)
	slices.Sort(out)
	return out
}

// emptyDramaCompose 判断旧行能证明的空合成参数，不从当前项目配置还原。
func emptyDramaCompose(c *model.DramaComposeReq) bool {
	return c == nil || (c.SegmentVersion == nil && c.ManifestHash == "" && c.Size == "" && len(c.Variants) == 0)
}

// resolveDramaJobIntent 联合解析审批与普通幂等键；所有重放出口复用同一 fail-closed 校验。
func resolveDramaJobIntent(ctx context.Context, tx repository.DramaTx, user, id uint64, intent *dramaJobIntent) (*model.DramaJob, error) {
	var byApproval, byKey *model.DramaJob
	var err error
	if intent.approvalID != nil {
		byApproval, err = tx.JobByApproval(ctx, user, *intent.approvalID)
		if err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				return nil, err
			}
			byApproval = nil
		}
	}
	key := intent.req.ComposeRequestKey
	if intent.req.Step == "compose" && key != "" {
		byKey, err = tx.JobByComposeKey(ctx, user, id, key)
		if err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				return nil, err
			}
			byKey = nil
		}
	}
	if err := checkDramaJobKeys(id, intent, byApproval, byKey); err != nil {
		return nil, err
	}
	hit := byApproval
	if hit == nil {
		hit = byKey
	}
	if hit != nil && !matchesDramaJobIntent(hit, intent) {
		return nil, errcode.ErrIdempotencyConflict
	}
	return hit, nil
}

// checkDramaJobKeys 禁止跨审批借单、普通请求借审批单或两键分别指向不同任务。
func checkDramaJobKeys(id uint64, i *dramaJobIntent, a, b *model.DramaJob) error {
	conflict := errcode.ErrIdempotencyConflict
	if a != nil && a.DramaID != id {
		return errcode.ErrDramaNotFound
	}
	if a != nil && b != nil && a.ID != b.ID {
		return conflict
	}
	if b != nil {
		if i.approvalID == nil && b.ApprovalID != nil {
			return conflict
		}
		if i.approvalID != nil && (b.ApprovalID == nil || *b.ApprovalID != *i.approvalID) {
			return conflict
		}
	}
	if a != nil && i.req.Step == "compose" && i.req.ComposeRequestKey != "" {
		if a.ComposeRequestKey != nil && *a.ComposeRequestKey != i.req.ComposeRequestKey {
			return conflict
		}
		if a.ComposeRequestKey == nil && !i.serverDerivedKey {
			return conflict
		}
	}
	return nil
}

// matchesDramaJobIntent 老行仅按冻结字段证明相等，无法证明的非空字段一律冲突。
func matchesDramaJobIntent(hit *model.DramaJob, i *dramaJobIntent) bool {
	if hit.RequestIntentSig != "" {
		return hit.RequestIntentSig == i.signature
	}
	r := i.req
	if hit.Step != r.Step || !slices.Equal(canonicalDramaEpisodes(hit.Episodes), r.Episodes) || r.Ratio != "" {
		return false
	}
	switch r.Step {
	case "compose":
		var frozen dramaComposeInput
		if json.Unmarshal(hit.InputJSON, &frozen) != nil {
			return false
		}
		if r.ExpectedEpisodeID != nil && (frozen.EpisodeID == 0 || *r.ExpectedEpisodeID != frozen.EpisodeID) {
			return false
		}
		c := r.Compose
		return r.MaxSeconds == 0 && c.Size == "" && len(c.Variants) == 0 && c.ManifestHash == "" && (c.SegmentVersion == nil || *c.SegmentVersion == frozen.SegmentVersion)
	case "storyboard":
		if r.ExpectedEpisodeID != nil || r.ConfirmClear || r.ExpectedSegmentVersion != nil {
			return false
		}
		var frozen dramaStoryboardInput
		if json.Unmarshal(hit.InputJSON, &frozen) != nil {
			return false
		}
		return emptyDramaCompose(r.Compose) && (r.MaxSeconds == 0 || r.MaxSeconds == frozen.MaxSeconds)
	case "scripts", "breakdown", "asset_prompts":
		return r.MaxSeconds == 0 && emptyDramaCompose(r.Compose)
	}
	return false
}

// validDramaManifestHash 只接受完整 SHA-256 十六进制指纹。
func validDramaManifestHash(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil
}

// validateDramaComposeRequest 校验清单版本、档位与变体集合的白名单。
func validateDramaComposeRequest(c *model.DramaComposeReq) error {
	if c.SegmentVersion != nil && *c.SegmentVersion < 0 {
		return errcode.ErrInvalidParams
	}
	if c.Size != "" && c.Size != "720p" && c.Size != "1080p" {
		return errcode.ErrInvalidParams
	}
	if c.ManifestHash != "" && !validDramaManifestHash(c.ManifestHash) {
		return errcode.ErrInvalidParams
	}
	if len(c.Variants) > 2 {
		return errcode.ErrInvalidParams
	}
	seen := map[string]bool{}
	for _, v := range c.Variants {
		if (v != "clean" && v != "packaged") || seen[v] {
			return errcode.ErrInvalidParams
		}
		seen[v] = true
	}
	return nil
}
