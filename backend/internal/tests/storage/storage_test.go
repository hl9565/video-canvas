package storage_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "video-canvas/internal/storage"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/config"
)

func newTestLocal(t *testing.T, baseURL string) *LocalStorage {
	t.Helper()
	l, err := NewLocal(config.LocalStorage{Dir: filepath.Join(t.TempDir(), "assets"), BaseURL: baseURL})
	if err != nil {
		t.Fatalf("创建本地存储失败：%v", err)
	}
	return l
}

func TestLocalVariantPublication(t *testing.T) {
	local := newTestLocal(t, "")
	key := "u1/original.png"
	if err := ValidateKey(".variants/u1/original.png"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("缓存命名空间必须对对象 API 隔离: %v", err)
	}
	put := func(data string) {
		t.Helper()
		if err := local.Put(t.Context(), key, strings.NewReader(data), int64(len(data)), "image/png"); err != nil {
			t.Fatal(err)
		}
	}
	put("original")
	src, err := local.StatSource(key)
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	tree := filepath.Join(filepath.Dir(filepath.Dir(src.Path)), ".variants", hash[:2], hash)
	final := filepath.Join(tree, "p2", fmt.Sprintf("%d-%d", src.Size, src.MTimeNano), "thumb.webp")
	write := func(dst io.Writer) error {
		_, err := io.WriteString(dst, "0123456789")
		return err
	}
	_, err = local.PublishVariant(key, "thumb", "webp", src, func(dst io.Writer) error {
		if err := write(dst); err != nil {
			return err
		}
		if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("原子发布前不应存在正式文件: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, hit, err := local.LookupVariant(key, "thumb", "webp"); err != nil || !hit {
		t.Fatalf("期望缓存命中: hit=%t err=%v", hit, err)
	}
	put("replacement has different size")
	if _, hit, err := local.LookupVariant(key, "thumb", "webp"); err != nil || hit {
		t.Fatalf("旧指纹不得命中: hit=%t err=%v", hit, err)
	}
	if _, err := local.PublishVariant(key, "thumb", "jpeg", src, write); err == nil {
		t.Fatal("旧指纹不得发布")
	}
	latest, err := local.StatSource(key)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := local.PublishVariant(key, "thumb", "webp", latest, func(dst io.Writer) error {
			close(started)
			<-release
			return write(dst)
		})
		finished <- err
	}()
	defer close(release)
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("生成过早结束: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("未开始写入临时文件")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- local.Delete(t.Context(), key) }()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete 不应等待编码")
	}
	release <- struct{}{}
	select {
	case err := <-finished:
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除后不能发布: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("生成没有退出")
	}
	if _, err := os.Stat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Delete 后不应残留缓存树: %v", err)
	}
}

func TestServeLocalVariantPolicies(t *testing.T) {
	local := newTestLocal(t, "")
	key := "u1/original.png"
	if err := local.Put(t.Context(), key, strings.NewReader("0123456789"), 10, "image/png"); err != nil {
		t.Fatal(err)
	}
	original, err := local.OpenLocal(key)
	if err != nil {
		t.Fatal(err)
	}
	src, err := local.StatSource(key)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := local.PublishVariant(key, "thumb", "webp", src, func(dst io.Writer) error {
		_, err := io.WriteString(dst, "0123456789")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cache, mime string
		file              *LocalFile
		policy            CachePolicy
	}{
		{"original", "public, max-age=31536000, immutable", "image/png", original, CachePolicy{MaxAge: 365 * 24 * time.Hour, Public: true, Immutable: true}},
		{"preview", "public, max-age=86400", "image/webp", preview, CachePolicy{MaxAge: 24 * time.Hour, Public: true}},
		{"fallback", "private, max-age=60", "image/png", original, CachePolicy{MaxAge: time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			serve := func(c *gin.Context) { ServeLocalFile(c, tc.file, tc.policy) }
			router.GET("/file.png", serve)
			router.HEAD("/file.png", serve)
			get := httptest.NewRequest(http.MethodGet, "/file.png", nil)
			get.Header.Set("Range", "bytes=2-4")
			rangeResp := httptest.NewRecorder()
			router.ServeHTTP(rangeResp, get)
			if rangeResp.Code != http.StatusPartialContent || rangeResp.Body.String() != "234" {
				t.Fatalf("Range: %d %q", rangeResp.Code, rangeResp.Body.String())
			}
			head := httptest.NewRecorder()
			router.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/file.png", nil))
			if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "10" {
				t.Fatalf("HEAD: %d %v %q", head.Code, head.Header(), head.Body.String())
			}
			for _, response := range []*httptest.ResponseRecorder{head, rangeResp} {
				hdr := response.Header()
				if hdr.Get("Cache-Control") != tc.cache || hdr.Get("Content-Type") != tc.mime || hdr.Get("X-Content-Type-Options") != "nosniff" {
					t.Fatalf("响应头不符: %v", hdr)
				}
			}
			conditional := httptest.NewRequest(http.MethodGet, "/file.png", nil)
			if tc.policy.Public {
				conditional.Header.Set("If-Modified-Since", head.Header().Get("Last-Modified"))
			} else {
				if head.Header().Get("Last-Modified") != "" {
					t.Fatalf("回退响应不应带 Last-Modified: %v", head.Header())
				}
				conditional.Header.Set("If-None-Match", head.Header().Get("ETag"))
			}
			fresh := httptest.NewRecorder()
			router.ServeHTTP(fresh, conditional)
			if fresh.Code != http.StatusNotModified || fresh.Header().Get("Cache-Control") != tc.cache {
				t.Fatalf("304: %d %v", fresh.Code, fresh.Header())
			}
		})
	}

	t.Run("同一兼容 URL 在变体与旧原图回退之间双向切换不误判 304", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		older := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
		if err := os.Chtimes(src.Path, older, older); err != nil {
			t.Fatal(err)
		}
		var serving *LocalFile
		var policy CachePolicy
		router := gin.New()
		router.GET("/file.png", func(c *gin.Context) { ServeLocalFile(c, serving, policy) })

		// 1. 先提供较新的变体
		serving, policy = preview, CachePolicy{MaxAge: 24 * time.Hour, Public: true}
		r1 := httptest.NewRecorder()
		router.ServeHTTP(r1, httptest.NewRequest(http.MethodGet, "/file.png", nil))
		if r1.Code != http.StatusOK || r1.Header().Get("ETag") == "" || r1.Header().Get("Last-Modified") == "" {
			t.Fatalf("变体首次响应异常: %d %v", r1.Code, r1.Header())
		}

		// 2. 切回较旧的原图回退：即使客户端带了较新变体的 If-Modified-Since 与 If-None-Match，也必须返回 200 原图
		serving, policy = original, CachePolicy{MaxAge: time.Minute}
		reqFallback := httptest.NewRequest(http.MethodGet, "/file.png", nil)
		reqFallback.Header.Set("If-Modified-Since", r1.Header().Get("Last-Modified"))
		reqFallback.Header.Set("If-None-Match", r1.Header().Get("ETag"))
		r2 := httptest.NewRecorder()
		router.ServeHTTP(r2, reqFallback)
		if r2.Code != http.StatusOK || r2.Body.String() != "0123456789" || r2.Header().Get("ETag") == r1.Header().Get("ETag") {
			t.Fatalf("切回回退原图不应误回 304: code=%d body=%q etag=%q", r2.Code, r2.Body.String(), r2.Header().Get("ETag"))
		}

		// 3. 再从回退原图切回变体：带着回退 ETag 也必须返回 200 变体
		serving, policy = preview, CachePolicy{MaxAge: 24 * time.Hour, Public: true}
		reqVariant := httptest.NewRequest(http.MethodGet, "/file.png", nil)
		reqVariant.Header.Set("If-None-Match", r2.Header().Get("ETag"))
		r3 := httptest.NewRecorder()
		router.ServeHTTP(r3, reqVariant)
		if r3.Code != http.StatusOK || r3.Body.String() != "0123456789" || r3.Header().Get("Content-Type") != "image/webp" {
			t.Fatalf("从回退切回变体不应误回 304: code=%d mime=%q", r3.Code, r3.Header().Get("Content-Type"))
		}
	})
}

func TestLocalVariantDualInstanceDeleteMutex(t *testing.T) {
	dir := t.TempDir()
	localA, err := NewLocal(config.LocalStorage{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	localB, err := NewLocal(config.LocalStorage{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	const key = "u1/shared.png"
	if err := localA.Put(t.Context(), key, strings.NewReader("original"), 8, "image/png"); err != nil {
		t.Fatal(err)
	}
	src, err := localA.StatSource(key)
	if err != nil {
		t.Fatal(err)
	}
	encoding, deleted := make(chan struct{}), make(chan struct{})
	go func() {
		<-encoding
		if err := localB.Delete(t.Context(), key); err != nil {
			t.Errorf("第二实例删除失败: %v", err)
		}
		close(deleted)
	}()
	if _, err := localA.PublishVariant(key, "thumb", "webp", src, func(dst io.Writer) error {
		close(encoding)
		<-deleted
		_, werr := io.WriteString(dst, "webp-bytes")
		return werr
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("另一实例已删除原文件，发布必须失败: %v", err)
	}
	if _, hit, err := localA.LookupVariant(key, "thumb", "webp"); !errors.Is(err, ErrNotFound) || hit {
		t.Fatalf("不能残留孤儿变体: hit=%v err=%v", hit, err)
	}
	// 并发交错竞态：双实例无外部同步屏障直接争夺关键区与 os.RemoveAll，任何一方先后顺序都不得在 Delete 完成后残留变体
	for idx := range 32 {
		raceKey := fmt.Sprintf("u1/race-%d.png", idx)
		if err := localA.Put(t.Context(), raceKey, strings.NewReader("original"), 8, "image/png"); err != nil {
			t.Fatal(err)
		}
		raceSrc, err := localA.StatSource(raceKey)
		if err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			<-ready
			done <- localB.Delete(t.Context(), raceKey)
		}()
		_, _ = localA.PublishVariant(raceKey, "thumb", "webp", raceSrc, func(dst io.Writer) error {
			close(ready)
			_, werr := io.WriteString(dst, "webp-bytes")
			return werr
		})
		if delErr := <-done; delErr != nil {
			t.Fatalf("迭代 %d 删除失败: %v", idx, delErr)
		}
		if _, hit, err := localA.LookupVariant(raceKey, "thumb", "webp"); !errors.Is(err, ErrNotFound) || hit {
			t.Fatalf("迭代 %d 存在跨实例竞争残留变体: hit=%v err=%v", idx, hit, err)
		}
	}
	if err := filepath.WalkDir(filepath.Join(dir, ".variants"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			t.Errorf("跨实例并发删除后磁盘仍残留变体文件: %s", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertNoTmpFiles(t, dir)
}

func TestLocalVariantCanceledPublication(t *testing.T) {
	const key = "original.png"
	local := newTestLocal(t, "")
	if err := local.Put(t.Context(), key, strings.NewReader("original"), 8, "image/png"); err != nil {
		t.Fatal(err)
	}
	src, err := local.StatSource(key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = local.PublishVariantContext(ctx, key, "thumb", "webp", src, func(dst io.Writer) error {
		_, err := io.WriteString(dst, "encoded data")
		cancel()
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("发布前取消: %v", err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	dir := filepath.Join(filepath.Dir(src.Path), ".variants", hash[:2], hash, "p2", fmt.Sprintf("%d-%d", src.Size, src.MTimeNano))
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("取消后不能残留缓存或临时文件: %v %v", entries, err)
	}
}

func TestValidateKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		ok   bool
	}{
		{"正常多级 key", "u1/202609/abc-123_x.png", true},
		{"单层 key", "a.mp4", true},
		{"空 key", "", false},
		{"绝对路径", "/etc/passwd", false},
		{"父目录穿越", "u1/../../etc/passwd", false},
		{"开头就是 ..", "../x", false},
		{"当前目录段", "u1/./x", false},
		{"反斜杠", `u1\x.png`, false},
		{"盘符", "C:/x.png", false},
		{"空路径段", "u1//x.png", false},
		{"末尾斜杠", "u1/x/", false},
		{"隐藏文件", "u1/.hidden", false},
		{"临时文件前缀", ".tmp-abc", false},
		{"空格", "u1/a b.png", false},
		{"中文", "u1/图片.png", false},
		{"百分号编码", "u1/%2e%2e/x", false},
		{"NUL 字节", "u1/a\x00.png", false},
		{"过长", strings.Repeat("a", 513), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateKey(tt.key)
			if tt.ok && err != nil {
				t.Fatalf("期望通过，实际：%v", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("期望 ErrInvalidKey，实际：%v", err)
			}
		})
	}
}

func TestLocalStorage_PutOpenDelete(t *testing.T) {
	l := newTestLocal(t, "")
	ctx := context.Background()

	// 1. 写入后能读回，多级目录自动创建
	if err := l.Put(ctx, "u1/202609/a.txt", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatalf("Put 失败：%v", err)
	}
	rc, err := l.Open(ctx, "u1/202609/a.txt")
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "hello" {
		t.Fatalf("读回内容不一致：%q", b)
	}

	// 2. 未知大小（-1）也能写入，且覆盖已有对象
	if err := l.Put(ctx, "u1/202609/a.txt", strings.NewReader("world!"), -1, ""); err != nil {
		t.Fatalf("覆盖写入失败：%v", err)
	}
	rc, _ = l.Open(ctx, "u1/202609/a.txt")
	b, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "world!" {
		t.Fatalf("覆盖后内容不一致：%q", b)
	}

	// 3. 删除后再读返回 ErrNotFound，重复删除不报错
	if err := l.Delete(ctx, "u1/202609/a.txt"); err != nil {
		t.Fatalf("Delete 失败：%v", err)
	}
	if _, err := l.Open(ctx, "u1/202609/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际：%v", err)
	}
	if err := l.Delete(ctx, "u1/202609/a.txt"); err != nil {
		t.Fatalf("重复删除不应报错：%v", err)
	}
}

func TestLocalStorage_PathTraversal(t *testing.T) {
	l := newTestLocal(t, "")
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(l.Dir()), "evil.txt")

	for _, key := range []string{"../evil.txt", "/abs/evil.txt", `..\evil.txt`, "u1/../../evil.txt"} {
		if err := l.Put(ctx, key, strings.NewReader("x"), 1, ""); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q) 期望 ErrInvalidKey，实际：%v", key, err)
		}
		if _, err := l.Open(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) 期望 ErrNotFound，实际：%v", key, err)
		}
		if err := l.Delete(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Delete(%q) 期望 ErrInvalidKey，实际：%v", key, err)
		}
		if _, err := l.URL(ctx, key, 0); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("URL(%q) 期望 ErrInvalidKey，实际：%v", key, err)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("存储目录之外出现了文件")
	}
}

// errAfterReader 读到一半返回错误，模拟上传中断。
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestLocalStorage_PutAtomic(t *testing.T) {
	ctx := context.Background()

	t.Run("写入中途失败不留下目标文件和临时文件", func(t *testing.T) {
		l := newTestLocal(t, "")
		boom := errors.New("boom")
		err := l.Put(ctx, "u1/a.bin", &errAfterReader{data: []byte("partial"), err: boom}, -1, "")
		if !errors.Is(err, boom) {
			t.Fatalf("期望透传读取错误，实际：%v", err)
		}
		if _, err := l.Open(ctx, "u1/a.bin"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("目标文件不应存在：%v", err)
		}
		assertNoTmpFiles(t, l.Dir())
	})

	t.Run("失败的覆盖写入保留旧内容", func(t *testing.T) {
		l := newTestLocal(t, "")
		if err := l.Put(ctx, "u1/a.bin", strings.NewReader("old"), 3, ""); err != nil {
			t.Fatal(err)
		}
		_ = l.Put(ctx, "u1/a.bin", &errAfterReader{data: []byte("new"), err: errors.New("boom")}, -1, "")
		rc, err := l.Open(ctx, "u1/a.bin")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(b) != "old" {
			t.Fatalf("旧内容被破坏：%q", b)
		}
		assertNoTmpFiles(t, l.Dir())
	})

	t.Run("声明长度与实际不符失败并清理", func(t *testing.T) {
		l := newTestLocal(t, "")
		err := l.Put(ctx, "u1/a.bin", strings.NewReader("abc"), 10, "")
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("期望 ErrUnexpectedEOF，实际：%v", err)
		}
		if _, err := l.Open(ctx, "u1/a.bin"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("目标文件不应存在：%v", err)
		}
		assertNoTmpFiles(t, l.Dir())
	})

	t.Run("ctx 已取消时不写入", func(t *testing.T) {
		l := newTestLocal(t, "")
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		err := l.Put(cctx, "u1/a.bin", strings.NewReader("abc"), 3, "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("期望 context.Canceled，实际：%v", err)
		}
		assertNoTmpFiles(t, l.Dir())
	})
}

func assertNoTmpFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), ".tmp-") {
			t.Errorf("残留临时文件：%s", p)
		}
		return nil
	})
}

func TestLocalStorage_URL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"BaseURL 为空返回相对路径", "", "/files/u1/a.png"},
		{"带 BaseURL", "http://localhost:8080", "http://localhost:8080/files/u1/a.png"},
		{"BaseURL 末尾斜杠被去掉", "https://cdn.example.com/", "https://cdn.example.com/files/u1/a.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newTestLocal(t, tt.baseURL)
			got, err := l.URL(context.Background(), "u1/a.png", time.Hour)
			if err != nil || got != tt.want {
				t.Fatalf("期望 %q，实际 %q（err=%v）", tt.want, got, err)
			}
		})
	}
}

func TestFileServer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	l := newTestLocal(t, "")
	content := "0123456789"
	if err := l.Put(context.Background(), "u1/202609/v.mp4", strings.NewReader(content), int64(len(content)), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	// 存储目录同级放一个不该被访问到的文件
	secret := filepath.Join(filepath.Dir(l.Dir()), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	r.GET("/files/*filepath", FileServer(l))
	r.HEAD("/files/*filepath", FileServer(l))

	do := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	t.Run("正常读取带缓存头和类型", func(t *testing.T) {
		w := do(http.MethodGet, "/files/u1/202609/v.mp4", nil)
		if w.Code != http.StatusOK || w.Body.String() != content {
			t.Fatalf("状态码 %d，内容 %q", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Cache-Control"), "max-age") {
			t.Errorf("缺少 Cache-Control：%q", w.Header().Get("Cache-Control"))
		}
		if w.Header().Get("Content-Type") != "video/mp4" {
			t.Errorf("Content-Type 不对：%q", w.Header().Get("Content-Type"))
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("缺少 nosniff")
		}
		if w.Header().Get("Accept-Ranges") != "bytes" {
			t.Errorf("应声明支持 Range")
		}
	})
	t.Run("Range 请求返回 206", func(t *testing.T) {
		w := do(http.MethodGet, "/files/u1/202609/v.mp4", map[string]string{"Range": "bytes=2-5"})
		if w.Code != http.StatusPartialContent || w.Body.String() != "2345" {
			t.Fatalf("状态码 %d，内容 %q", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Content-Range"); got != "bytes 2-5/10" {
			t.Errorf("Content-Range 不对：%q", got)
		}
	})
	t.Run("HEAD 请求无 body", func(t *testing.T) {
		w := do(http.MethodHead, "/files/u1/202609/v.mp4", nil)
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Fatalf("状态码 %d，body 长度 %d", w.Code, w.Body.Len())
		}
	})
	t.Run("文件不存在 404", func(t *testing.T) {
		if w := do(http.MethodGet, "/files/u1/none.mp4", nil); w.Code != http.StatusNotFound {
			t.Fatalf("期望 404，实际 %d", w.Code)
		}
	})
	t.Run("目录 404", func(t *testing.T) {
		if w := do(http.MethodGet, "/files/u1/202609", nil); w.Code != http.StatusNotFound {
			t.Fatalf("期望 404，实际 %d", w.Code)
		}
	})
	t.Run("路径穿越 404 且不泄露文件", func(t *testing.T) {
		for _, p := range []string{"/files/../secret.txt", "/files/u1/../../secret.txt", "/files/..%2Fsecret.txt", "/files/%2e%2e/secret.txt", `/files/..%5Csecret.txt`} {
			w := do(http.MethodGet, p, nil)
			if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "secret") {
				t.Errorf("%s 泄露了文件：%d %q", p, w.Code, w.Body.String())
			}
		}
	})
}

func TestNewLocal_RequiresDir(t *testing.T) {
	if _, err := NewLocal(config.LocalStorage{}); err == nil {
		t.Fatal("缺目录应报错")
	}
	l, err := NewLocal(config.LocalStorage{Dir: filepath.Join(t.TempDir(), "d")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Dir()); err != nil {
		t.Errorf("目录不存在时应自动创建：%v", err)
	}
}
