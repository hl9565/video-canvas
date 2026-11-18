package middleware_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/config"
	"video-canvas/internal/middleware"
	"video-canvas/internal/pkg/logger"
)

func TestLoggerPreview_RedactsCredentialsForEveryStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	if err := logger.Init(config.Log{Level: "info", Format: "json", Filename: path, MaxSize: 1}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Init(config.Log{Level: "info", Format: "json"}) })
	gin.SetMode(gin.TestMode)
	for _, status := range []int{206, 401, 403} {
		r := gin.New()
		r.Use(middleware.Logger())
		r.GET("/stream", func(c *gin.Context) { c.Status(status) })
		w := httptest.NewRecorder()
		query := "token=PRIVATE_TOKEN&ToKeN=PRIVATE_CASE&sig=PRIVATE_SIG&signature=PRIVATE_SIGN&secret=PRIVATE_SECRET" +
			"&ticket=PRIVATE_TICKET&ws_ticket=PRIVATE_WS_TICKET&TiCkEt=PRIVATE_TICKET_CASE&ok=visible"
		r.ServeHTTP(w, httptest.NewRequest("GET", "/stream?"+query, nil))
	}
	logger.Sync()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE_") || strings.Count(string(raw), "ok=visible") != 3 {
		t.Fatalf("凭证泄露或正常查询丢失: %s", raw)
	}
}

func TestLoggerPreview_RedactsEntireMalformedQuery(t *testing.T) {
	read := accessLog(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.Logger())
	router.GET("/stream", func(c *gin.Context) { c.Status(200) })
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/stream?ok=visible&ticket=PRIVATE_TICKET&broken=%zz", nil))
	if query := accessLine(t, read())["query"]; query != "[REDACTED]" {
		t.Fatalf("异常编码应隐藏整串查询，实际为 %q", query)
	}
}
