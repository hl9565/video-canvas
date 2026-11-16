package router_test

import (
	"net/http/httptest"
	"testing"

	"video-canvas/internal/handler"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/router"
	"video-canvas/internal/service"
)

func TestRouter_DramaPreviewStreamOutsideBearerGroup(t *testing.T) {
	svc := service.NewDramaService(service.DramaDeps{})
	r := router.New("test", proxyJWTSecret, router.Handlers{Health: &handler.HealthHandler{}, Drama: handler.NewDramaHandler(svc)})
	base := "/api/v1/dramas/" + idcodec.Encode(7) + "/episodes/1"
	// 未携带 Bearer 仍须到达 stream 的凭证校验器：空正文 401，不能是 JWT envelope。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", base+"/preview/ticket/stream?token=invalid", nil))
	if w.Code != 401 || w.Body.Len() != 0 {
		t.Fatalf("stream 被 Bearer 拦截或泄露正文: %d %q", w.Code, w.Body.String())
	}
	for _, endpoint := range []string{base + "/manifest", base + "/preview/ticket"} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", endpoint, nil))
		if w.Code != 401 || w.Body.Len() == 0 {
			t.Fatalf("JWT 查询入口未保护: %s %d %q", endpoint, w.Code, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/files/vc-private-preview-ticket.mp4", nil))
	if w.Code != 404 {
		t.Fatalf("预览不应通过 files 暴露: %d", w.Code)
	}
}
