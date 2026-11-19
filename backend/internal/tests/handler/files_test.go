package handler_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	. "video-canvas/internal/handler"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/config"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/service"
	"video-canvas/internal/storage"
)

// fakeFileResolver 实现 handler.FileResolver：按 key 返回预置的结果。
type fakeFileResolver struct {
	targets    map[string]*service.FileTarget
	variants   map[string]*service.FileTarget // key: "<key>?v=<variant>"
	err        error
	gotKey     string
	gotVariant string
	gotStrict  bool
	fileCalls  int
}

func (f *fakeFileResolver) ResolveVariantWithOptions(_ context.Context, key, variant string, opts service.VariantOptions) (*service.FileTarget, error) {
	f.gotKey, f.gotVariant, f.gotStrict = key, variant, opts.Strict
	if f.err != nil {
		return nil, f.err
	}
	t, ok := f.variants[key+"?v="+variant]
	if !ok {
		return nil, errcode.ErrAssetNotFound
	}
	return t, nil
}

func (f *fakeFileResolver) ResolveFile(ctx context.Context, key string) (*service.FileTarget, error) {
	f.gotKey = key
	f.fileCalls++
	if f.err != nil {
		return nil, f.err
	}
	t, ok := f.targets[key]
	if !ok {
		return nil, errcode.ErrAssetNotFound
	}
	return t, nil
}

func newFilesRouter(res *fakeFileResolver) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewFilesHandler(res)
	r.GET("/files/*filepath", h)
	r.HEAD("/files/*filepath", h)
	return r
}

func localFilesFixture(t *testing.T, key string) (*storage.LocalStorage, *storage.LocalFile) {
	t.Helper()
	local, err := storage.NewLocal(config.LocalStorage{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Put(t.Context(), key, strings.NewReader("0123456789"), 10, "image/png"); err != nil {
		t.Fatal(err)
	}
	file, err := local.OpenLocal(key)
	if err != nil {
		t.Fatal(err)
	}
	return local, file
}

func TestFilesHandler(t *testing.T) {
	t.Run("本地存储：直接提供文件，支持 Range", func(t *testing.T) {
		_, local := localFilesFixture(t, "u1/a.png")
		res := &fakeFileResolver{targets: map[string]*service.FileTarget{
			"u1/a.png": {Local: local, MaxAge: 365 * 24 * time.Hour, CachePublic: true, CacheImmutable: true},
		}}
		r := newFilesRouter(res)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/files/u1/a.png", nil)
		req.Header.Set("Range", "bytes=2-4")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusPartialContent || w.Body.String() != "234" || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("实际 %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
		}
		if res.gotKey != "u1/a.png" {
			t.Errorf("应把去掉前导斜杠的 key 交给 service，实际 %q", res.gotKey)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Fatalf("原图缓存策略不变: %q", got)
		}
	})

	t.Run("对象存储：302 跳转，限制缓存时长与来源信息", func(t *testing.T) {
		res := &fakeFileResolver{targets: map[string]*service.FileTarget{
			"u1/a.mp4": {RedirectURL: "https://bucket.oss-cn-hangzhou.aliyuncs.com/u1/a.mp4?X-Amz-Signature=abc", MaxAge: 30 * time.Minute},
		}}
		r := newFilesRouter(res)
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(method, "/files/u1/a.mp4", nil))
			if w.Code != http.StatusFound || w.Header().Get("Location") != "https://bucket.oss-cn-hangzhou.aliyuncs.com/u1/a.mp4?X-Amz-Signature=abc" {
				t.Fatalf("%s：%d %q", method, w.Code, w.Header().Get("Location"))
			}
			if w.Header().Get("Cache-Control") != "private, max-age=1800" || w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Errorf("%s：缓存与来源头不对：%v", method, w.Header())
			}
		}
	})

	t.Run("错误映射：不存在 404，存储不可用 502，未知错误 500", func(t *testing.T) {
		tests := []struct {
			name string
			res  *fakeFileResolver
			want int
		}{
			{"素材不存在", &fakeFileResolver{}, http.StatusNotFound},
			{"存储不可用", &fakeFileResolver{err: errcode.ErrStorageUnavailable}, http.StatusBadGateway},
			{"未知错误", &fakeFileResolver{err: errors.New("db down")}, http.StatusInternalServerError},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				newFilesRouter(tt.res).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/u1/none.png", nil))
				if w.Code != tt.want {
					t.Fatalf("实际 %d，期望 %d", w.Code, tt.want)
				}
				if w.Header().Get("Location") != "" {
					t.Error("错误响应不能带跳转")
				}
			})
		}
	})
}

func TestFilesHandler_Variant(t *testing.T) {
	cf := "https://assets.example.com/cdn-cgi/image/width=512/https://assets.example.com/u1/a.png"
	res := &fakeFileResolver{variants: map[string]*service.FileTarget{
		"u1/a.png?v=thumb": {RedirectURL: cf, MaxAge: 24 * time.Hour},
	}}
	r := newFilesRouter(res)

	t.Run("带 v 参数：交给 ResolveVariant，302 到处理地址并按 MaxAge 缓存", func(t *testing.T) {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/u1/a.png?v=thumb", nil))
		if w.Code != http.StatusFound || w.Header().Get("Location") != cf {
			t.Fatalf("实际 %d %q", w.Code, w.Header().Get("Location"))
		}
		if w.Header().Get("Cache-Control") != "private, max-age=86400" {
			t.Errorf("缓存头不对：%q", w.Header().Get("Cache-Control"))
		}
		if res.gotKey != "u1/a.png" || res.gotVariant != "thumb" || res.gotStrict || res.fileCalls != 0 {
			t.Errorf("应只走 ResolveVariant：key=%q variant=%q 原图调用 %d 次", res.gotKey, res.gotVariant, res.fileCalls)
		}
	})

	t.Run("没有 v 参数：仍走 ResolveFile，行为不变", func(t *testing.T) {
		res2 := &fakeFileResolver{targets: map[string]*service.FileTarget{"u1/a.png": {RedirectURL: "https://o/u1/a.png", MaxAge: time.Minute}}}
		w := httptest.NewRecorder()
		newFilesRouter(res2).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/u1/a.png", nil))
		if w.Code != http.StatusFound || res2.fileCalls != 1 || res2.gotVariant != "" {
			t.Fatalf("实际 %d 调用 %d", w.Code, res2.fileCalls)
		}
	})

	t.Run("封面没有处理服务：404，不带跳转", func(t *testing.T) {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/u1/a.mp4?v=poster", nil))
		if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("实际 %d %q", w.Code, w.Header().Get("Location"))
		}
	})

	t.Run("变体回退到本地文件：直接提供", func(t *testing.T) {
		_, local := localFilesFixture(t, "u1/a.png")
		res3 := &fakeFileResolver{variants: map[string]*service.FileTarget{"u1/a.png?v=thumb": {Local: local, MaxAge: time.Minute}}}
		w := httptest.NewRecorder()
		newFilesRouter(res3).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/u1/a.png?v=thumb", nil))
		if w.Code != http.StatusOK || w.Body.String() != "0123456789" || w.Header().Get("Cache-Control") != "private, max-age=60" {
			t.Fatalf("实际 %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("错误映射同原图：存储不可用 502", func(t *testing.T) {
		w := httptest.NewRecorder()
		newFilesRouter(&fakeFileResolver{err: errcode.ErrStorageUnavailable}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/u1/a.png?v=thumb", nil))
		if w.Code != http.StatusBadGateway {
			t.Fatalf("实际 %d", w.Code)
		}
	})
}

func TestFilesHandlerStrictLocalVariant(t *testing.T) {
	const key = "u1/a.png"
	const url = "/files/" + key + "?v=thumb&pv=2"
	for _, expected := range []*errcode.Error{errcode.ErrAssetNotFound, errcode.ErrVariantUnavailable} {
		res := &fakeFileResolver{err: expected}
		resp := httptest.NewRecorder()
		newFilesRouter(res).ServeHTTP(resp, httptest.NewRequest(http.MethodGet, url, nil))
		if !res.gotStrict || resp.Code != expected.HTTPStatus() || resp.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("严格变体错误: strict=%v code=%d headers=%v", res.gotStrict, resp.Code, resp.Header())
		}
	}
	local, _ := localFilesFixture(t, key)
	source, err := local.StatSource(key)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := local.PublishVariant(key, "thumb", "webp", source, func(dst io.Writer) error {
		_, err := io.WriteString(dst, "RIFFxxxxWEBP")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	res := &fakeFileResolver{variants: map[string]*service.FileTarget{
		key + "?v=thumb": {Local: preview, MaxAge: 24 * time.Hour, CachePublic: true},
	}}
	router := newFilesRouter(res)
	for _, tc := range []struct {
		method, byteRange string
		code              int
		body              string
	}{
		{http.MethodGet, "", http.StatusOK, "RIFFxxxxWEBP"},
		{http.MethodHead, "", http.StatusOK, ""},
		{http.MethodGet, "bytes=8-11", http.StatusPartialContent, "WEBP"},
	} {
		req := httptest.NewRequest(tc.method, url, nil)
		req.Header.Set("Range", tc.byteRange)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != tc.code || resp.Body.String() != tc.body || !res.gotStrict || resp.Header().Get("Content-Type") != "image/webp" ||
			resp.Header().Get("Cache-Control") != "public, max-age=86400" || resp.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s %q: code=%d body=%q headers=%v", tc.method, tc.byteRange, resp.Code, resp.Body, resp.Header())
		}
	}
	badRange := httptest.NewRequest(http.MethodGet, url, nil)
	badRange.Header.Set("Range", "bytes=200-300")
	rangeError := httptest.NewRecorder()
	router.ServeHTTP(rangeError, badRange)
	if rangeError.Code != http.StatusRequestedRangeNotSatisfiable || rangeError.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("无效 Range: code=%d headers=%v", rangeError.Code, rangeError.Header())
	}
	if err := local.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	missing := httptest.NewRecorder()
	router.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, url, nil))
	if missing.Code != http.StatusNotFound || missing.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("解析后文件被删: code=%d headers=%v", missing.Code, missing.Header())
	}
}
