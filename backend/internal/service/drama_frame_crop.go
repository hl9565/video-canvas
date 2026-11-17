package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/draw"
	_ "image/gif"  // 注册首帧 GIF 解码器，取第一帧。
	_ "image/jpeg" // 注册首帧 JPEG 解码器。
	"image/png"
	"io"
	"math"
	"os/exec"
	"strings"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
)

const dramaFrameRatio = 16.0 / 9.0

// validDramaFrameRatio 使用解码得到的像素尺寸判断，不能信任历史素材的零值元数据。
func validDramaFrameRatio(width, height int) bool {
	return width >= 16 && height >= 16 && math.Abs(float64(width)/float64(height)-dramaFrameRatio)/dramaFrameRatio <= 0.025
}

const dramaFrameCanvasTTL = 30 * time.Minute

type dramaFrameCanvasClaims struct {
	Purpose   string `json:"purpose"`
	UserID    uint64 `json:"user_id"`
	RunID     uint64 `json:"run_id"`
	SegmentID uint64 `json:"segment_id"`
	CanvasID  uint64 `json:"canvas_id"`
	Expires   int64  `json:"expires"`
}

// signFrameCanvasTicket 将用途、用户、流程、片段及画布一并签名，不能替换其中任意目标。
func (s *DramaFrameRunService) signFrameCanvasTicket(run *model.DramaSegmentFrameRun) string {
	claims := dramaFrameCanvasClaims{Purpose: "drama-frame-canvas", UserID: run.UserID, RunID: run.ID, SegmentID: run.SegmentID,
		CanvasID: *run.OriginCanvasID, Expires: s.now().Add(dramaFrameCanvasTTL).Unix()}
	raw, _ := json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.ticketSecret)
	_, _ = mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyFrameCanvas 对 canvas 来源强制校验票据及源画布当前可读性。
func (s *DramaFrameRunService) verifyFrameCanvas(ctx context.Context, run *model.DramaSegmentFrameRun, req *model.SetFrameRunFrameReq) error {
	if run.FrameSource != model.FrameSourceCanvas {
		return nil
	}
	if run.OriginCanvasID == nil || req.OriginCanvasID != nil && uint64(*req.OriginCanvasID) != *run.OriginCanvasID {
		return errcode.ErrDramaFrameInvalid
	}
	parts := strings.Split(req.CanvasTicket, ".")
	if len(parts) != 2 {
		return errcode.ErrDramaFrameInvalid
	}
	mac := hmac.New(sha256.New, s.ticketSecret)
	_, _ = mac.Write([]byte(parts[0]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return errcode.ErrDramaFrameInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errcode.ErrDramaFrameInvalid
	}
	var claims dramaFrameCanvasClaims
	if err := json.Unmarshal(body, &claims); err != nil || claims.Purpose != "drama-frame-canvas" || claims.UserID != run.UserID ||
		claims.RunID != run.ID || claims.SegmentID != run.SegmentID || claims.CanvasID != *run.OriginCanvasID || claims.Expires <= s.now().Unix() {
		return errcode.ErrDramaFrameInvalid
	}
	return s.checkFrameCanvas(ctx, run.UserID, *run.OriginCanvasID)
}

// checkFrameCanvas 不信任请求中的来源声明，重新检查画布归属及软删状态。
func (s *DramaFrameRunService) checkFrameCanvas(ctx context.Context, userID, canvasID uint64) error {
	if s.canvases == nil || canvasID == 0 {
		return errcode.ErrDramaFrameInvalid
	}
	canvas, err := s.canvases.GetByID(ctx, userID, canvasID)
	var business *errcode.Error
	if errors.Is(err, repository.ErrNotFound) || errors.As(err, &business) && business.HTTPStatus() == 404 {
		return errcode.ErrDramaFrameInvalid
	}
	if err != nil {
		return err
	}
	if canvas == nil || canvas.ID != canvasID || canvas.UserID != userID || canvas.DeletedAt.Valid {
		return errcode.ErrDramaFrameInvalid
	}
	return nil
}

// frameAssetError 将缺失或越权统一为首帧无效；存储和数据库故障保留以供重试。
func frameAssetError(err error) error {
	var business *errcode.Error
	if errors.Is(err, provider.ErrAssetNotFound) || errors.Is(err, repository.ErrNotFound) ||
		errors.As(err, &business) && business.Code == errcode.ErrAssetNotFound.Code {
		return errcode.ErrDramaFrameInvalid
	}
	return err
}

// loadFrameImage 在事务外校验素材归属并解码实际内容；所有打开的对象都在返回前关闭。
func (s *DramaFrameRunService) loadFrameImage(ctx context.Context, userID, assetID uint64) (*model.Asset, image.Image, error) {
	asset, err := s.assets.Get(ctx, userID, assetID)
	if err != nil {
		return nil, nil, frameAssetError(err)
	}
	if asset == nil || asset.ID != assetID || asset.UserID != userID || asset.Kind != model.KindImage {
		return nil, nil, errcode.ErrDramaFrameInvalid
	}
	file, err := s.assets.Open(ctx, userID, assetID)
	if err != nil {
		return nil, nil, frameAssetError(err)
	}
	if file == nil || file.Body == nil {
		return nil, nil, errcode.ErrDramaFrameInvalid
	}
	defer func() { _ = file.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(&assetSrcReader{ctx: ctx, r: file.Body}, defaultMaxUpload+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) == 0 || int64(len(data)) > defaultMaxUpload {
		return nil, nil, errcode.ErrDramaFrameInvalid
	}
	img, err := decodeDramaFrame(ctx, data)
	return asset, img, err
}

// decodeDramaFrame 原生 PNG/JPEG/GIF 损坏时直接拒绝，未知格式才交给有超时的 ffmpeg。
func decodeDramaFrame(ctx context.Context, data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if errors.Is(err, image.ErrFormat) {
		img, err = decodeDramaFrameFFmpeg(ctx, data)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || img == nil || img.Bounds().Dx() <= 0 || img.Bounds().Dy() <= 0 {
		return nil, errcode.ErrDramaFrameInvalid
	}
	return img, nil
}

// decodeDramaFrameFFmpeg 兼容 WebP/BMP 等格式；只处理 stdin 的单张图片，禁用外部协议。
func decodeDramaFrameFFmpeg(ctx context.Context, data []byte) (image.Image, error) {
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errcode.ErrDramaFrameInvalid
	}
	decodeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(decodeCtx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "pipe", "-f", "image2pipe", "-i", "pipe:0", "-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1")
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.Output()
	if err != nil {
		return nil, errcode.ErrDramaFrameInvalid
	}
	return png.Decode(bytes.NewReader(output))
}

// prepareFrameImage 保留容差内的原图，否则持久化精确 16:9 的偶数尺寸中心裁图。
func (s *DramaFrameRunService) prepareFrameImage(ctx context.Context, userID, assetID uint64) (*model.Asset, error) {
	asset, img, err := s.loadFrameImage(ctx, userID, assetID)
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	if validDramaFrameRatio(bounds.Dx(), bounds.Dy()) {
		return asset, nil
	}
	// 16k × 9k 同时为偶数要求 k 为偶数；不足 32×18 的图片不能无损裁出合格首帧。
	k := min(bounds.Dx()/16, bounds.Dy()/9) / 2 * 2
	if k < 2 {
		return nil, errcode.ErrDramaFrameInvalid
	}
	w, h := 16*k, 9*k
	start := bounds.Min.Add(image.Pt((bounds.Dx()-w)/2, (bounds.Dy()-h)/2))
	cropped := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(cropped, cropped.Bounds(), img, start, draw.Src)
	var out bytes.Buffer
	if err := png.Encode(&out, cropped); err != nil {
		return nil, err
	}
	return s.assets.SaveDerivedImage(ctx, userID, "drama-frame.png", "image/png", out.Bytes())
}
