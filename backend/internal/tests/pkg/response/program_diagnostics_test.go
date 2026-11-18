package response_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/response"
	"video-canvas/internal/service"
)

func TestDramaProgramErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err    *errcode.Error
		code   int
		status int
		msg    string
	}{
		{errcode.ErrDramaProgramInvalid, 91009, http.StatusBadRequest, "节目单参数或结构不合法"},
		{errcode.ErrDramaAnchorInvalid, 91010, http.StatusConflict, "节目锚点已失效"},
		{errcode.ErrDramaVisualInvalid, 91011, http.StatusBadRequest, "分镜视觉模式或特效参数不合法"},
		{errcode.ErrDramaMediaUnavailable, 91012, http.StatusConflict, "引用素材不可用或已变更"},
		{errcode.ErrDramaLoudnessInvalid, 91013, http.StatusUnprocessableEntity, "成片混音未达到响度或真峰交付标准"},
		{errcode.ErrDramaAssetInUse, 91014, http.StatusConflict, "素材正被剧集引用，无法执行此操作"},
	} {
		if tc.err.Code != tc.code || tc.err.HTTPStatus() != tc.status || tc.err.Msg != tc.msg {
			t.Errorf("错误码 %d：实际 %v / HTTP %d", tc.code, tc.err, tc.err.HTTPStatus())
		}
	}
}

func TestFailWithDramaProgramDiagnostics(t *testing.T) {
	sentinel := errcode.ErrDramaAnchorInvalid
	diagnostics := []service.DramaProgramDiagnostic{{CueID: "sfx-1", SegmentID: 42, Field: "anchor", Reason: "source_changed", Severity: "error"}}
	withData := sentinel.WithData(diagnostics)
	withMsg := sentinel.WithMsg("请重新绑定锚点")
	for _, err := range []*errcode.Error{withData.WithMsg(withMsg.Msg), withMsg.WithData(diagnostics)} {
		status, body := failResponse(t, fmt.Errorf("检查节目单: %w", err))
		if status != http.StatusConflict || body.Code != 91010 || body.Msg != "请重新绑定锚点" {
			t.Fatalf("诊断响应错误：HTTP %d / %+v", status, body)
		}
		want := fmt.Sprintf(`[{"cue_id":"sfx-1","segment_id":%q,"field":"anchor","reason":"source_changed","severity":"error"}]`, idcodec.Encode(42))
		if string(body.Data) != want {
			t.Fatalf("data = %s，期望 %s", body.Data, want)
		}
	}
	status, body := failResponse(t, sentinel)
	if status != http.StatusConflict || body.Code != 91010 || body.Msg != "节目锚点已失效" || body.Data != nil || withData.Msg != sentinel.Msg || withMsg.Data != nil {
		t.Fatalf("预定义错误或中间副本被修改：HTTP %d / %+v", status, body)
	}
}

func TestFailInternalOmitsData(t *testing.T) {
	status, body := failResponse(t, errors.New("内部实现细节"))
	if status != http.StatusInternalServerError || body.Code != 10000 || body.Msg != "服务内部错误" || body.Data != nil {
		t.Fatalf("内部错误应返回通用响应：HTTP %d / %+v", status, body)
	}
}

type errorResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func failResponse(t *testing.T, err error) (int, errorResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/program", nil)
	response.Fail(c, err)
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return w.Code, body
}
