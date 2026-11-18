package handler_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/handler"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/service"
)

func TestDramaVisualPatchHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, patch  string
		status, code int
		active       bool
	}{
		{"invalid kind", `"visual_kind":"gif"`, 400, 91011, false},
		{"invalid FX", `"fx":{"zoom_start":2}`, 400, 91011, false},
		{"invalid asset ID", `"still_asset_id":""`, 400, 91011, false},
		{"type error", `"edit":{"source_out_f":"13"}`, 400, 10001, false},
		{"missing image", `"visual_kind":"still","still_asset_id":1,"edit":{"source_out_f":13}`, 409, 91012, false},
		{"active run", `"visual_kind":"black","edit":{"source_out_f":13}`, 409, 91005, true},
		{"decimal string", `"visual_kind":"still","still_asset_id":"9007199254740993","edit":{"source_out_f":13}`, 200, 0, false},
		{"decimal integer", `"visual_kind":"still","still_asset_id":9007199254740993,"edit":{"source_out_f":13}`, 200, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, repo := segmentHTTP()
			repo.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 2}, UserID: 1, EpisodeID: 10, Revision: 1}}
			const assetID = 9007199254740993
			repo.Assets = map[uint64]model.Asset{assetID: {ID: assetID, UserID: 1, Kind: model.KindImage, Sha256: "image-hash"}}
			if tc.active {
				repo.FrameRuns = []model.DramaSegmentFrameRun{{UserID: 1, SegmentID: 2, State: model.FrameRunStateFrameReady}}
			}
			req := httptest.NewRequest("PATCH", "/drama-segments/"+idcodec.Encode(2), strings.NewReader(`{"revision":1,`+tc.patch+`}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			var body struct {
				Code int
				Data map[string]json.RawMessage
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != tc.status || body.Code != tc.code {
				t.Fatalf("status=%d body=%s err=%v", w.Code, w.Body, err)
			}
			if tc.code == 0 && (string(body.Data["still_asset_id"]) != "9007199254740993" || string(body.Data["source_frame_count"]) != "null") {
				t.Fatalf("unexpected visual wire fields: %s", w.Body)
			}
		})
	}
}

// TestDramaVisualBindKeepsProgramDiagnostics 直接覆盖通过通用绑定器返回的节目单业务诊断。
func TestDramaVisualBindKeepsProgramDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PUT("/program", func(c *gin.Context) {
		handler.BindJSON(c, new(service.PutDramaProgramReq))
	})
	status, body := characterCall(t, r, "PUT", "/program",
		`{"expected_episode_id":"`+idcodec.Encode(10)+`","expected_segment_version":0,"program":{"v":1,"audio":[],"overlays":[{}]}}`)
	if status != 400 || responseCode(t, body) != 91009 || responseData(t, body)["diagnostics"] == nil {
		t.Fatalf("status=%d body=%v", status, body)
	}
}
