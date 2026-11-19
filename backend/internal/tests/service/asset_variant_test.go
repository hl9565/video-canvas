package service_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "video-canvas/internal/service"

	"video-canvas/internal/config"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/storage"
)

// fakeVariantResolver 实现 AssetVariantResolver：按 变体 返回预置结果，并记录收到的参数。
type fakeVariantResolver struct {
	targets  map[string]VariantTarget // key: 变体
	err      error
	gotStore uint64
	gotKey   string
	calls    int
}

func (f *fakeVariantResolver) Resolve(_ context.Context, storageID uint64, _ *storage.Handle, key, variant string) (VariantTarget, bool, error) {
	f.calls++
	f.gotStore, f.gotKey = storageID, key
	if f.err != nil {
		return VariantTarget{}, false, f.err
	}
	t, ok := f.targets[variant]
	return t, ok, nil
}

func TestAssetService_ResolveVariant(t *testing.T) {
	ctx := context.Background()
	seed := func(repo *fakeAssetRepo, kind string) {
		repo.rows[5] = &model.Asset{ID: 5, UserID: 1, Kind: kind, StorageID: 2, StorageKey: "u1/202610/a.bin", MimeType: kind + "/x"}
	}
	newSvc := func(kind string, res AssetVariantResolver) (*AssetService, *fakeAssetStore) {
		repo := newFakeAssetRepo()
		seed(repo, kind)
		store := newFakeAssetStore()
		svc := NewAssetService(repo, newFakeRegistry(2, objectHandle(2, store, 2*time.Hour)), config.Storage{})
		if res != nil {
			svc.SetVariantResolver(res)
		}
		return svc, store
	}
	const key = "u1/202610/a.bin"

	t.Run("有处理服务：302 到处理地址；公开读缓存一天，并把存储与 key 交给解析器", func(t *testing.T) {
		res := &fakeVariantResolver{targets: map[string]VariantTarget{"thumb": {URL: "https://assets.example.com/cdn-cgi/image/width=512/x"}}}
		svc, store := newSvc("image", res)
		tgt, err := svc.ResolveVariant(ctx, key, "thumb")
		if err != nil || tgt.RedirectURL != "https://assets.example.com/cdn-cgi/image/width=512/x" || tgt.Local != nil {
			t.Fatalf("实际 %+v err=%v", tgt, err)
		}
		if tgt.MaxAge != 24*time.Hour {
			t.Errorf("公开读地址永久稳定，应缓存 24h，实际 %v", tgt.MaxAge)
		}
		if res.gotStore != 2 || res.gotKey != key {
			t.Errorf("应把素材所在存储和 key 交给解析器：%d %q", res.gotStore, res.gotKey)
		}
		if store.urlCalls != 0 {
			t.Error("命中处理服务时不应再给原图签名")
		}
	})

	t.Run("带签名的处理地址：缓存不超过签名有效期的 1/4", func(t *testing.T) {
		res := &fakeVariantResolver{targets: map[string]VariantTarget{"thumb": {URL: "https://x/y?sig=1", Signed: true}}}
		svc, _ := newSvc("image", res)
		tgt, err := svc.ResolveVariant(ctx, key, "thumb")
		if err != nil || tgt.MaxAge != 30*time.Minute {
			t.Fatalf("ttl 2h 的 1/4 应为 30m：%+v err=%v", tgt, err)
		}
	})

	t.Run("视频封面", func(t *testing.T) {
		res := &fakeVariantResolver{targets: map[string]VariantTarget{"poster": {URL: "https://assets.example.com/cdn-cgi/media/x"}}}
		svc, _ := newSvc("video", res)
		tgt, err := svc.ResolveVariant(ctx, key, "poster")
		if err != nil || tgt.RedirectURL != "https://assets.example.com/cdn-cgi/media/x" {
			t.Fatalf("实际 %+v err=%v", tgt, err)
		}
	})

	t.Run("缩略图没有处理服务：回退原图，缓存压到 60 秒以内", func(t *testing.T) {
		svc, store := newSvc("image", &fakeVariantResolver{targets: map[string]VariantTarget{}})
		tgt, err := svc.ResolveVariant(ctx, key, "thumb")
		if err != nil || tgt.RedirectURL != "https://cdn.test/u1/202610/a.bin?ttl=2h0m0s" || store.urlCalls != 1 {
			t.Fatalf("应回退到原图的签名地址：%+v err=%v", tgt, err)
		}
		if tgt.MaxAge != 60*time.Second {
			t.Errorf("回退地址缓存应不超过 60s，以便处理服务发布后尽快生效，实际 %v", tgt.MaxAge)
		}
	})

	t.Run("缩略图回退到本地存储：直接提供文件", func(t *testing.T) {
		local, err := storage.NewLocal(config.LocalStorage{Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		repo := newFakeAssetRepo()
		repo.rows[5] = &model.Asset{ID: 5, UserID: 1, Kind: "image", StorageID: 1, StorageKey: key, MimeType: "image/png"}
		svc := NewAssetService(repo, newFakeRegistry(1, &storage.Handle{ID: 1, Provider: storage.ProviderLocal, Storage: local}), config.Storage{})
		tgt, err := svc.ResolveVariant(ctx, key, "thumb")
		if err != nil || tgt.Local == nil || tgt.MaxAge != time.Minute || tgt.CachePublic || tgt.CacheImmutable {
			t.Fatalf("实际 %+v err=%v", tgt, err)
		}
		if tgt, err := svc.ResolveVariantWithOptions(ctx, key, "thumb", VariantOptions{Strict: true}); tgt != nil || !errors.Is(err, errcode.ErrAssetNotFound) {
			t.Fatalf("严格模式不能返回原图: %+v %v", tgt, err)
		}
	})

	t.Run("没有配置解析器：和没有处理服务一样回退", func(t *testing.T) {
		svc, _ := newSvc("image", nil)
		if tgt, err := svc.ResolveVariant(ctx, key, "thumb"); err != nil || tgt.RedirectURL == "" {
			t.Fatalf("实际 %+v err=%v", tgt, err)
		}
	})

	t.Run("封面没有处理服务：404，不回退到几十 MB 的原视频", func(t *testing.T) {
		svc, _ := newSvc("video", &fakeVariantResolver{targets: map[string]VariantTarget{}})
		_, err := svc.ResolveVariant(ctx, key, "poster")
		if got := assetErrCode(t, err); got != errcode.ErrAssetNotFound.Code {
			t.Fatalf("实际 %d", got)
		}
	})

	t.Run("解析器报错：缩略图回退原图，封面 404，都不暴露内部原因", func(t *testing.T) {
		res := &fakeVariantResolver{err: errors.New("sign failed: secret xyz")}
		svc, _ := newSvc("image", res)
		if tgt, err := svc.ResolveVariant(ctx, key, "thumb"); err != nil || tgt.RedirectURL == "" {
			t.Fatalf("缩略图应回退：%+v err=%v", tgt, err)
		}
		svc2, _ := newSvc("video", res)
		_, err := svc2.ResolveVariant(ctx, key, "poster")
		if got := assetErrCode(t, err); got != errcode.ErrAssetNotFound.Code {
			t.Fatalf("封面应 404，实际 %d", got)
		}
	})

	t.Run("素材种类与变体不匹配：404，且不会去找处理服务", func(t *testing.T) {
		for _, tc := range []struct{ kind, variant string }{{"image", "poster"}, {"video", "thumb"}, {"audio", "thumb"}, {"audio", "poster"}} {
			res := &fakeVariantResolver{targets: map[string]VariantTarget{"thumb": {URL: "u"}, "poster": {URL: "u"}}}
			svc, _ := newSvc(tc.kind, res)
			_, err := svc.ResolveVariant(ctx, key, tc.variant)
			if got := assetErrCode(t, err); got != errcode.ErrAssetNotFound.Code || res.calls != 0 {
				t.Errorf("%s + %s：期望 404 且不调用解析器，实际 %d，调用 %d 次", tc.kind, tc.variant, got, res.calls)
			}
		}
	})

	t.Run("变体取值非法 / 素材不存在 / key 非法：统一 404", func(t *testing.T) {
		svc, _ := newSvc("image", &fakeVariantResolver{})
		for _, c := range []struct{ key, variant string }{{key, "huge"}, {key, ""}, {"u1/none.png", "thumb"}, {"../etc/passwd", "thumb"}} {
			_, err := svc.ResolveVariant(ctx, c.key, c.variant)
			if got := assetErrCode(t, err); got != errcode.ErrAssetNotFound.Code {
				t.Errorf("%q %q 应 404，实际 %d", c.key, c.variant, got)
			}
		}
	})

	t.Run("存储不可用：502 业务错误", func(t *testing.T) {
		repo := newFakeAssetRepo()
		seed(repo, "image")
		reg := newFakeRegistry(1)
		reg.getErr = errors.New("decrypt failed")
		svc := NewAssetService(repo, reg, config.Storage{})
		svc.SetVariantResolver(&fakeVariantResolver{})
		_, err := svc.ResolveVariant(ctx, key, "thumb")
		if got := assetErrCode(t, err); got != errcode.ErrStorageUnavailable.Code {
			t.Fatalf("实际 %d", got)
		}
	})
}

type localPreviewFake func(context.Context, string, io.Writer) error

func (f localPreviewFake) Format(_ context.Context) (videoframe.PreviewInfo, error) {
	return videoframe.PreviewInfo{Format: "webp", MIME: "image/webp"}, nil
}

func (f localPreviewFake) Encode(ctx context.Context, path string, dst io.Writer) (videoframe.PreviewInfo, error) {
	info, _ := f.Format(ctx)
	return info, f(ctx, path, dst)
}

type previewWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *previewWaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func awaitPreview[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("等待预览超时")
		var zero T
		return zero
	}
}

func seedLocalPreview(t *testing.T, local *storage.LocalStorage, key, contents string) {
	t.Helper()
	if err := local.Put(t.Context(), key, strings.NewReader(contents), int64(len(contents)), "image/png"); err != nil {
		t.Fatal(err)
	}
}

func newPreviewStorage(t *testing.T) (*storage.LocalStorage, string) {
	t.Helper()
	dir := t.TempDir()
	local, err := storage.NewLocal(config.LocalStorage{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return local, dir
}

func beginPreview(ctx context.Context, t *testing.T, svc *LocalVariantService, local *storage.LocalStorage, key string) <-chan error {
	t.Helper()
	waiter := &previewWaitingContext{Context: ctx, waiting: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := svc.Resolve(waiter, local, key, "thumb")
		result <- err
	}()
	select {
	case <-waiter.waiting:
		return result
	case err := <-result:
		t.Fatalf("请求未等候就返回: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("请求未进入调度")
	}
	return nil
}

func TestLocalVariantSharedGeneration(t *testing.T) {
	local, _ := newPreviewStorage(t)
	const key = "source.png"
	seedLocalPreview(t, local, key, "original")
	started, release := make(chan struct{}, 2), make(chan struct{})
	var calls atomic.Int32
	svc := NewLocalVariantService(t.Context(), localPreviewFake(func(ctx context.Context, _ string, dst io.Writer) error {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			_, err := io.WriteString(dst, "preview")
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	t.Cleanup(svc.Close)
	firstCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := beginPreview(firstCtx, t, svc, local, key)
	awaitPreview(t, started)
	second := beginPreview(t.Context(), t, svc, local, key)
	cancel()
	if err := awaitPreview(t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("首请求取消: %v", err)
	}
	// 大量热点重复请求取消退出，不影响共享任务与存活等待者
	for range 32 {
		burstCtx, burstCancel := context.WithCancel(t.Context())
		burst := beginPreview(burstCtx, t, svc, local, key)
		burstCancel()
		if err := awaitPreview(t, burst); !errors.Is(err, context.Canceled) {
			t.Fatalf("突发取消请求应立即返回 Canceled: %v", err)
		}
	}
	close(release)
	if err := awaitPreview(t, second); err != nil {
		t.Fatalf("另一个等待者应正常完成: %v", err)
	}
	file, err := svc.Resolve(t.Context(), local, key, "thumb")
	if err != nil || file.ContentType != "image/webp" || calls.Load() != 1 {
		t.Fatalf("缓存应复用编码结果: file=%+v calls=%d err=%v", file, calls.Load(), err)
	}
}

func TestLocalVariantQueueAndClose(t *testing.T) {
	local, dir := newPreviewStorage(t)
	started := make(chan struct{}, 66)
	var calls atomic.Int32
	svc := NewLocalVariantService(t.Context(), localPreviewFake(func(ctx context.Context, _ string, dst io.Writer) error {
		calls.Add(1)
		if _, err := io.WriteString(dst, "partial"); err != nil {
			return err
		}
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}))
	t.Cleanup(svc.Close)
	var results []<-chan error
	for index := range 66 {
		key := fmt.Sprintf("source-%d.png", index)
		seedLocalPreview(t, local, key, "original")
		results = append(results, beginPreview(t.Context(), t, svc, local, key))
		if index < 2 {
			awaitPreview(t, started)
		}
	}
	seedLocalPreview(t, local, "extra.png", "original")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := svc.Resolve(ctx, local, "extra.png", "thumb"); !errors.Is(err, errcode.ErrVariantUnavailable) {
		t.Fatalf("2 个运行 + 64 个等待时应立即 503: %v", err)
	}
	duplicate := beginPreview(t.Context(), t, svc, local, "source-0.png")
	closed := make(chan struct{})
	go func() { svc.Close(); close(closed) }()
	awaitPreview(t, closed)
	for _, result := range append(results, duplicate) {
		if err := awaitPreview(t, result); !errors.Is(err, errcode.ErrVariantUnavailable) {
			t.Fatalf("关闭应取消运行与排队: %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("关闭时不应启动排队编码: %d", calls.Load())
	}
	if _, err := svc.Resolve(t.Context(), local, "extra.png", "thumb"); !errors.Is(err, errcode.ErrVariantUnavailable) {
		t.Fatalf("关闭后应停止接单: %v", err)
	}
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Errorf("关闭后临时文件残留: %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalVariantFailureCooldown(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"decode failure", videoframe.ErrDecode, 1},
		{"oversized", videoframe.ErrTooLarge, 1},
		{"no encoder", videoframe.ErrUnavailable, 1},
		{"timeout", context.DeadlineExceeded, 2},
		{"busy", errcode.ErrVariantUnavailable, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, _ := newPreviewStorage(t)
			seedLocalPreview(t, local, "source.png", "original")
			var calls atomic.Int32
			svc := NewLocalVariantService(t.Context(), localPreviewFake(func(_ context.Context, _ string, _ io.Writer) error {
				calls.Add(1)
				return tc.err
			}))
			t.Cleanup(svc.Close)
			wantErr := tc.err
			if errors.Is(wantErr, context.DeadlineExceeded) {
				wantErr = errcode.ErrVariantUnavailable
			}
			for range 2 {
				if _, err := svc.Resolve(t.Context(), local, "source.png", "thumb"); !errors.Is(err, wantErr) {
					t.Fatalf("err=%v want=%v", err, wantErr)
				}
			}
			if got := int(calls.Load()); got != tc.want {
				t.Fatalf("编码次数 %d，期望 %d", got, tc.want)
			}
			seedLocalPreview(t, local, "source.png", "different fingerprint")
			_, _ = svc.Resolve(t.Context(), local, "source.png", "thumb")
			if got := int(calls.Load()); got != tc.want+1 {
				t.Fatalf("新指纹不应沿用旧失败: calls=%d", got)
			}
		})
	}
}

func TestAssetServiceLocalVariantPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, kind, variant string
		strict              bool
		encodeErr, want     error
		cache               time.Duration
	}{
		{"strict thumb success", "image", "thumb", true, nil, nil, 24 * time.Hour},
		{"poster success", "video", "poster", false, nil, nil, 24 * time.Hour},
		{"strict thumb invalid", "image", "thumb", true, videoframe.ErrDecode, errcode.ErrAssetNotFound, 0},
		{"poster invalid", "video", "poster", false, videoframe.ErrDecode, errcode.ErrAssetNotFound, 0},
		{"legacy thumb invalid", "image", "thumb", false, videoframe.ErrDecode, nil, time.Minute},
		{"strict thumb busy", "image", "thumb", true, context.DeadlineExceeded, errcode.ErrVariantUnavailable, 0},
		{"poster busy", "video", "poster", false, context.DeadlineExceeded, errcode.ErrVariantUnavailable, 0},
		{"legacy thumb busy", "image", "thumb", false, context.DeadlineExceeded, nil, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, _ := newPreviewStorage(t)
			const key = "local-source.bin"
			seedLocalPreview(t, local, key, "original")
			repo := newFakeAssetRepo()
			repo.rows[5] = &model.Asset{ID: 5, Kind: tc.kind, StorageID: 1, StorageKey: key, MimeType: tc.kind + "/original"}
			assets := NewAssetService(repo, newFakeRegistry(1, &storage.Handle{ID: 1, Provider: storage.ProviderLocal, Storage: local}), config.Storage{})
			svc := NewLocalVariantService(t.Context(), localPreviewFake(func(_ context.Context, _ string, dst io.Writer) error {
				if tc.encodeErr != nil {
					return tc.encodeErr
				}
				_, err := io.WriteString(dst, "encoded preview")
				return err
			}))
			t.Cleanup(svc.Close)
			assets.SetLocalVariantResolver(svc)
			target, err := assets.ResolveVariantWithOptions(t.Context(), key, tc.variant, VariantOptions{Strict: tc.strict})
			if tc.want != nil {
				if !errors.Is(err, tc.want) || target != nil {
					t.Fatalf("target=%+v err=%v want=%v", target, err, tc.want)
				}
				return
			}
			mime := tc.kind + "/original"
			if tc.encodeErr == nil {
				mime = "image/webp"
			}
			if err != nil || target.Local == nil || target.Local.ContentType != mime || target.MaxAge != tc.cache ||
				target.CachePublic != (tc.encodeErr == nil) || target.CacheImmutable {
				t.Fatalf("target=%+v err=%v", target, err)
			}
		})
	}
}
