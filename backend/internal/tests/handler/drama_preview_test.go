package handler_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/config"
	"video-canvas/internal/handler"
	"video-canvas/internal/middleware"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

type previewHTTPRepo struct{ *httpDramaRepo }

func (r *previewHTTPRepo) Segments(context.Context, uint64, uint64) ([]model.DramaSegment, error) {
	chosen := uint64(20)
	return []model.DramaSegment{{BaseModel: model.BaseModel{ID: 5}, UserID: 1, EpisodeID: 10, ChosenTaskID: &chosen}}, nil
}
func (r *previewHTTPRepo) SegmentTasks(context.Context, uint64, uint64, string) ([]model.GenerationTask, error) {
	return []model.GenerationTask{{ID: 20, Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":30,"media_type":"video"}]`)}}, nil
}

type previewHTTPAssets struct{ service.DramaComposeAssets }

func (a previewHTTPAssets) Get(context.Context, uint64, uint64) (*model.Asset, error) {
	return &model.Asset{ID: 30, UserID: 1, Kind: model.KindVideo, Sha256: jsonhash.SHA256Hex([]byte("video"))}, nil
}
func (a previewHTTPAssets) Open(context.Context, uint64, uint64) (*provider.AssetFile, error) {
	return &provider.AssetFile{Asset: &model.Asset{ID: 30, UserID: 1, Kind: model.KindVideo}, Body: io.NopCloser(strings.NewReader("video"))}, nil
}

type previewHTTPProbes struct{}

func (previewHTTPProbes) Get(context.Context, uint64, string, int16) (*model.AssetMediaProbe, error) {
	return &model.AssetMediaProbe{NormalizedFrames24: 120}, nil
}
func (previewHTTPProbes) Upsert(context.Context, *model.AssetMediaProbe) error { return nil }

type previewHTTPComposer struct{}

func (previewHTTPComposer) Compose(context.Context, []string, string) error { return nil }
func (previewHTTPComposer) ComposeManifest(_ context.Context, in videoframe.ComposeManifestInput) error {
	return os.WriteFile(in.Outputs[0].Destination, []byte("preview-video-data"), 0600)
}

func previewHTTPRouter(t *testing.T) (*gin.Engine, *service.DramaService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &previewHTTPRepo{&httpDramaRepo{project: &model.DramaProject{BaseModel: model.BaseModel{ID: 7}, UserID: 1, CanvasID: 9}, episode: &model.DramaEpisode{BaseModel: model.BaseModel{ID: 10}, UserID: 1, DramaID: 7, No: 1}}}
	svc := service.NewDramaService(service.DramaDeps{Repo: repo, Assets: previewHTTPAssets{}, Probes: previewHTTPProbes{}, Compose: previewHTTPComposer{}, PreviewSecret: "http-preview-secret"})
	t.Cleanup(svc.ClosePreviews)
	h := handler.NewDramaHandler(svc)
	r := gin.New()
	r.GET("/api/v1/dramas/:id/episodes/:no/preview/:ticket/stream", h.StreamPreview)
	auth := r.Group("/api/v1", func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer test" {
			c.AbortWithStatus(401)
			return
		}
		user := uint(1)
		if c.GetHeader("X-Test-User") == "2" {
			user = 2
		}
		c.Set(middleware.CtxUserIDKey, user)
	})
	auth.GET("/dramas/:id/episodes/:no/manifest", h.Manifest)
	auth.POST("/dramas/:id/episodes/:no/preview", h.CreatePreview)
	auth.GET("/dramas/:id/episodes/:no/preview/:ticket", h.GetPreview)
	return r, svc
}

func TestDramaPreviewHTTP_CookieRangeAndIsolation(t *testing.T) {
	r, svc := previewHTTPRouter(t)
	base := "/api/v1/dramas/" + idcodec.Encode(7) + "/episodes/1"
	manifest, err := svc.ResolveEpisodeManifest(context.Background(), 1, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", base+"/preview", strings.NewReader(`{"segment_version":0,"manifest_hash":"`+manifest.ManifestHash+`","variant":"clean"}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("创建: %s", w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "vc_preview_sess" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Path != "/api/v1/dramas" {
		t.Fatalf("Cookie 不安全: %+v", cookies)
	}
	// 使用公开 service 状态接口轮询同一票据至就绪，不依赖 handler 私有实现。
	v, _, err := svc.CreateEpisodePreview(context.Background(), 1, 7, 1, &model.DramaPreviewReq{ManifestHash: manifest.ManifestHash, Variant: "clean"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitHTTPPreviewReady(t, svc, 1, 7, 1, v.Ticket)
	for _, cookie := range []*http.Cookie{nil, {Name: "vc_preview_sess", Value: "forged"}, cookies[0]} {
		req = httptest.NewRequest("GET", v.StreamURL, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		req.Header.Set("Range", "bytes=0-6")
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if cookie == cookies[0] {
			if w.Code != 206 || w.Body.String() != "preview" || w.Header().Get("Content-Range") != "bytes 0-6/18" {
				t.Fatalf("Range: %d %q %v", w.Code, w.Body.String(), w.Header())
			}
		} else if (w.Code != 401 && w.Code != 403) || w.Body.Len() != 0 {
			t.Fatalf("未鉴权泄露字节: %d %q", w.Code, w.Body.String())
		}
	}
	req = httptest.NewRequest("GET", base+"/preview/"+v.Ticket, nil)
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("X-Test-User", "2")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 404 || strings.Contains(w.Body.String(), "token=") {
		t.Fatalf("跨用户状态泄露: %s", w.Body.String())
	}
	for _, raw := range []string{`{}`, `{"manifest_hash":"` + manifest.ManifestHash + `","variant":"clean"}`, `{"segment_version":-1,"manifest_hash":"abc","variant":"clean"}`, `{"manifest_hash":"` + strings.Repeat("a", 64) + `","variant":"bad"}`} {
		req = httptest.NewRequest("POST", base+"/preview", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test")
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("参数未拒绝: %s", w.Body.String())
		}
	}
}

// previewCredential 使用公开 HMAC 契约构造已过期或他人身份的有效签名，区别于普通乱码负测。
func previewCredential(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte("vc-preview-v1:http-preview-secret"))
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func waitHTTPPreviewReady(t *testing.T, svc *service.DramaService, user, drama uint64, ep int, ticket string) *model.DramaPreviewView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, err := svc.GetEpisodePreview(context.Background(), user, drama, ep, ticket)
		if err != nil {
			t.Fatalf("轮询预览状态失败: %v", err)
		}
		if v.Status == "ready" {
			return v
		}
		if v.Status == "failed" {
			t.Fatalf("预览意外失败: %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待预览就绪超时: ticket=%s", ticket)
	return nil
}

func TestDramaPreviewHTTP_ExpiredAndOtherUserCredentialZeroBytesAndLogs(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "access.log")
	if err := logger.Init(config.Log{Level: "info", Format: "json", Filename: logPath, MaxSize: 1}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Init(config.Log{Level: "info", Format: "json"}) })
	r, svc := previewHTTPRouter(t)
	r.Use(middleware.Logger())
	// Gin 路由注册后附加的全局中间件不作用于既有路由；用外层 router 保证测试真实访问日志。
	outer := gin.New()
	outer.Use(middleware.Logger())
	outer.Any("/*path", func(c *gin.Context) { r.ServeHTTP(c.Writer, c.Request) })
	ctx := context.Background()
	m, err := svc.ResolveEpisodeManifest(ctx, 1, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	v, cookie, err := svc.CreateEpisodePreview(ctx, 1, 7, 1, &model.DramaPreviewReq{ManifestHash: m.ManifestHash, Variant: "clean"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitHTTPPreviewReady(t, svc, 1, 7, 1, v.Ticket)
	parsed, err := url.Parse(v.StreamURL)
	if err != nil {
		t.Fatal(err)
	}
	validToken := parsed.Query().Get("token")
	expiredToken := previewCredential(t, map[string]any{"u": 1, "d": 7, "e": 1, "t": v.Ticket, "x": time.Now().Add(-time.Minute).Unix(), "p": "stream"})
	otherCookie := previewCredential(t, map[string]any{"u": 2, "x": time.Now().Add(time.Hour).Unix(), "p": "session"})
	for _, tc := range []struct {
		cookie, token string
		status        int
	}{{cookie, validToken, 206}, {otherCookie, validToken, 403}, {cookie, expiredToken, 401}} {
		req := httptest.NewRequest("GET", parsed.Path+"?token="+tc.token, nil)
		req.AddCookie(&http.Cookie{Name: "vc_preview_sess", Value: tc.cookie})
		req.Header.Set("Range", "bytes=0-6")
		w := httptest.NewRecorder()
		outer.ServeHTTP(w, req)
		if w.Code != tc.status || tc.status != 206 && w.Body.Len() != 0 {
			t.Fatalf("凭证隔离失败 %d %q", w.Code, w.Body.String())
		}
	}
	logger.Sync()
	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logs), validToken) || strings.Contains(string(logs), expiredToken) {
		t.Fatalf("206/拒绝/过期日志泄露 token: %s", logs)
	}
}

type coldHTTPProbes struct {
	mu   sync.Mutex
	rows map[string]*model.AssetMediaProbe
}

func (p *coldHTTPProbes) Get(_ context.Context, _ uint64, sha string, _ int16) (*model.AssetMediaProbe, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if row := p.rows[sha]; row != nil {
		return row, nil
	}
	return nil, repository.ErrNotFound
}

func (p *coldHTTPProbes) Upsert(_ context.Context, row *model.AssetMediaProbe) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows[row.Sha256] = row
	return nil
}

func setupColdManifestRouter(t *testing.T, probeFn func(context.Context, string) (videoframe.NormalizedProbe, error)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &previewHTTPRepo{&httpDramaRepo{
		project: &model.DramaProject{BaseModel: model.BaseModel{ID: 7}, UserID: 1, CanvasID: 9},
		episode: &model.DramaEpisode{BaseModel: model.BaseModel{ID: 10}, UserID: 1, DramaID: 7, No: 1, EditSchema: 1, SegmentVersion: 1},
	}}
	svc := service.NewDramaService(service.DramaDeps{
		Repo: repo, Assets: previewHTTPAssets{}, Probes: &coldHTTPProbes{rows: map[string]*model.AssetMediaProbe{}},
		Compose: previewHTTPComposer{}, PreviewSecret: "http-preview-secret", ProbeVideo: probeFn,
	})
	t.Cleanup(svc.ClosePreviews)
	h := handler.NewDramaHandler(svc)
	r := gin.New()
	auth := r.Group("/api/v1", func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer test" {
			c.AbortWithStatus(401)
			return
		}
		c.Set(middleware.CtxUserIDKey, uint(1))
	})
	auth.GET("/dramas/:id/episodes/:no/manifest", h.Manifest)
	return r
}

func doManifestHTTP(r *gin.Engine) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/dramas/"+idcodec.Encode(7)+"/episodes/1/manifest", nil)
	req.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestDramaManifestHTTP_ColdWarmupAcceptedThenReadyAndFailureSurfaced(t *testing.T) {
	t.Run("cold_202_dedup_then_200_ready", func(t *testing.T) {
		gate := make(chan struct{})
		entered := make(chan struct{}, 2)
		var probeCalls atomic.Int32
		r := setupColdManifestRouter(t, func(ctx context.Context, _ string) (videoframe.NormalizedProbe, error) {
			probeCalls.Add(1)
			entered <- struct{}{}
			select {
			case <-gate:
				return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
			case <-ctx.Done():
				return videoframe.NormalizedProbe{}, ctx.Err()
			}
		})

		start := time.Now()
		w1 := doManifestHTTP(r)
		if w1.Code != http.StatusAccepted || time.Since(start) > 250*time.Millisecond {
			t.Fatalf("冷素材 GET /manifest 应立即返回 202: code=%d cost=%v body=%s", w1.Code, time.Since(start), w1.Body.String())
		}
		if cc := w1.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("202 响应缺少 Cache-Control: no-store，实际 %q", cc)
		}
		var prepEnv struct {
			Code int `json:"code"`
			Data struct {
				Status         string   `json:"status"`
				SegmentVersion int      `json:"segment_version"`
				SkippedSeqs    []int    `json:"skipped_seqs"`
				Diagnostics    []string `json:"diagnostics"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w1.Body.Bytes(), &prepEnv); err != nil || prepEnv.Code != 0 || prepEnv.Data.Status != "preparing" || prepEnv.Data.SegmentVersion != 1 || prepEnv.Data.SkippedSeqs == nil || prepEnv.Data.Diagnostics == nil {
			t.Fatalf("202 载荷不符合预期: err=%v body=%s", err, w1.Body.String())
		}

		// 等待后台进入探测，再次并发 GET 仍应立即返回 202 且去重。
		<-entered
		if w2 := doManifestHTTP(r); w2.Code != http.StatusAccepted {
			t.Fatalf("在途预热期间重复 GET 应返回 202: code=%d body=%s", w2.Code, w2.Body.String())
		}

		close(gate)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			w := doManifestHTTP(r)
			if w.Code == http.StatusOK {
				var okEnv struct {
					Code int `json:"code"`
					Data struct {
						ManifestHash string `json:"manifest_hash"`
						Manifest     struct {
							V     int              `json:"v"`
							Clips []map[string]any `json:"clips"`
						} `json:"manifest"`
					} `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &okEnv); err != nil || okEnv.Code != 0 || okEnv.Data.ManifestHash == "" || okEnv.Data.Manifest.V != 1 || len(okEnv.Data.Manifest.Clips) != 1 {
					t.Fatalf("200 清单响应不符合预期: err=%v body=%s", err, w.Body.String())
				}
				if got := probeCalls.Load(); got != 1 {
					t.Fatalf("并发请求未去重，预期 1 次探测，实际 %d", got)
				}
				return
			}
			if w.Code != http.StatusAccepted {
				t.Fatalf("预热完成前轮询出现非预期状态: code=%d body=%s", w.Code, w.Body.String())
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatal("等待 GET /manifest 转 200 OK 超时")
	})

	t.Run("background_failure_surfaced_then_retry_succeeds", func(t *testing.T) {
		var shouldFail atomic.Bool
		shouldFail.Store(true)
		r := setupColdManifestRouter(t, func(context.Context, string) (videoframe.NormalizedProbe, error) {
			if shouldFail.Load() {
				return videoframe.NormalizedProbe{}, errors.New("probe boom")
			}
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
		})

		if w := doManifestHTTP(r); w.Code != http.StatusAccepted {
			t.Fatalf("首次请求应返回 202: code=%d body=%s", w.Code, w.Body.String())
		}

		deadline := time.Now().Add(3 * time.Second)
		surfaced := false
		for time.Now().Before(deadline) {
			w := doManifestHTTP(r)
			if w.Code == http.StatusAccepted {
				time.Sleep(15 * time.Millisecond)
				continue
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("后台探测失败后应返回 400 错误，实际 %d body=%s", w.Code, w.Body.String())
			}
			surfaced = true
			break
		}
		if !surfaced {
			t.Fatal("后台探测失败未在后续 GET /manifest 中显露")
		}

		shouldFail.Store(false)
		if w := doManifestHTTP(r); w.Code != http.StatusAccepted {
			t.Fatalf("失败消费后重试应重新返回 202 preparing: code=%d body=%s", w.Code, w.Body.String())
		}
		deadline = time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			w := doManifestHTTP(r)
			if w.Code == http.StatusOK {
				return
			}
			if w.Code != http.StatusAccepted {
				t.Fatalf("恢复后重试失败: code=%d body=%s", w.Code, w.Body.String())
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatal("错误恢复后等待 200 OK 超时")
	})
}
