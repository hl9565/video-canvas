package model_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin/binding"
	"gorm.io/gorm/schema"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
)

// TestUpdateDramaSegmentReqClearChosen 验证空采用值被规范为显式清空，非空 ID 仍供业务层判断冲突。
func TestUpdateDramaSegmentReqClearChosen(t *testing.T) {
	encoded := idcodec.Encode(42)
	for _, tc := range []struct {
		name  string
		body  string
		clear bool
		id    uint64
	}{
		{"缺失", `{"revision":1}`, false, 0},
		{"null", `{"revision":1,"chosen_task_id":null}`, true, 0},
		{"空字符串", `{"revision":1,"chosen_task_id":""}`, true, 0},
		{"显式清空", `{"revision":1,"clear_chosen":true}`, true, 0},
		{"null覆盖false", `{"revision":1,"chosen_task_id":null,"clear_chosen":false}`, true, 0},
		{"采用", `{"revision":1,"chosen_task_id":"` + encoded + `"}`, false, 42},
		{"冲突留业务层", `{"revision":1,"chosen_task_id":"` + encoded + `","clear_chosen":true}`, true, 42},
		{"重复键末值采用", `{"revision":1,"chosen_task_id":null,"chosen_task_id":"` + encoded + `"}`, false, 42},
		{"重复键末值清空", `{"revision":1,"chosen_task_id":"` + encoded + `","chosen_task_id":null}`, true, 0},
		{"大小写兼容", `{"revision":1,"CHOSEN_TASK_ID":null}`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req model.UpdateDramaSegmentReq
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			var id uint64
			if req.ChosenTaskID != nil {
				id = uint64(*req.ChosenTaskID)
			}
			if tc.id == 0 && req.ChosenTaskID != nil {
				t.Fatal("清空请求应规范为 nil ChosenTaskID")
			}
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["clear_chosen"] != tc.clear || id != tc.id {
				t.Fatalf("clear_chosen=%v id=%d，期望 %v/%d", fields["clear_chosen"], id, tc.clear, tc.id)
			}
		})
	}
}

func TestFrameRunModelsRegistered(t *testing.T) {
	for _, target := range []any{&model.DramaSegmentFrameRun{}, &model.DramaSegmentTakeLink{}} {
		found := false
		for _, row := range model.All() {
			found = found || reflect.TypeOf(row) == reflect.TypeOf(target)
		}
		if !found {
			t.Errorf("model.All() 缺少 %T", target)
		}
		s, err := schema.Parse(target, &sync.Map{}, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		if s.LookUpField("DeletedAt") != nil {
			t.Errorf("%T 不应软删", target)
		}
	}
}

func TestFrameRunGORMConstraintsAndIndexPredicates(t *testing.T) {
	for _, target := range []any{&model.DramaSegmentFrameRun{}, &model.DramaSegmentTakeLink{}} {
		s, err := schema.Parse(target, &sync.Map{}, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		for name, check := range s.ParseCheckConstraints() {
			if strings.Contains(check.Constraint, `\`) {
				t.Errorf("%s 的 GORM CHECK 包含无法执行的反斜杠：%s", name, check.Constraint)
			}
		}
		for _, index := range s.ParseIndexes() {
			if strings.Contains(index.Where, `\`) {
				t.Errorf("%s 的条件未正确反转义：%s", index.Name, index.Where)
			}
		}
	}
}

func TestFrameRunErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err    *errcode.Error
		code   int
		msg    string
		status int
	}{
		{errcode.ErrDramaFrameRunNotFound, 91004, "首帧控镜流程不存在", http.StatusNotFound},
		{errcode.ErrDramaFrameRunConflict, 91005, "首帧控镜状态已变更或存在进行中流程", http.StatusConflict},
		{errcode.ErrDramaFrameRunInconsistent, 91006, "首帧控镜任务归属或确认快照不一致", http.StatusConflict},
		{errcode.ErrDramaTakeQCRejected, 91007, "该候选已被驳回，无法直接采用", http.StatusConflict},
		{errcode.ErrDramaFrameInvalid, 91008, "首帧素材无效或尚未就绪", http.StatusBadRequest},
	} {
		if tc.err.Code != tc.code || tc.err.Msg != tc.msg || tc.err.HTTPStatus() != tc.status {
			t.Errorf("错误码契约：%+v，期望 %d/%s/%d", tc.err, tc.code, tc.msg, tc.status)
		}
	}
}

func TestUpdateDramaSegmentReqRejectsMalformedChosen(t *testing.T) {
	for _, raw := range []string{`42`, `true`, `{}`, `[]`, `"bad-id"`} {
		req := model.UpdateDramaSegmentReq{Revision: 7}
		if err := json.Unmarshal([]byte(`{"revision":1,"chosen_task_id":`+raw+`}`), &req); err == nil {
			t.Errorf("非法 ID 未被拒绝：%s", raw)
		}
		if req.Revision != 7 {
			t.Errorf("失败解析不应部分修改接收者：%+v", req)
		}
	}
	var req model.UpdateDramaSegmentReq
	if err := json.Unmarshal([]byte(`{"revision":0,"chosen_task_id":null}`), &req); err != nil {
		t.Fatal(err)
	}
	if err := binding.Validator.ValidateStruct(req); err == nil {
		t.Fatal("自定义 JSON 解析不能破坏 revision 的 binding 校验")
	}
}

func TestFrameRunRequestValidation(t *testing.T) {
	for _, req := range []any{
		model.CreateFrameRunReq{}, model.SetFrameRunFrameReq{}, model.ConfirmFrameRunReq{},
		model.SubmitFrameRunReq{}, model.CancelFrameRunReq{}, model.ImportDramaTakesReq{}, model.PatchDramaTakeQCReq{},
	} {
		if err := binding.Validator.ValidateStruct(req); err == nil {
			t.Errorf("%T 应拒绝空请求", req)
		}
		typ := reflect.TypeOf(req)
		for i := range typ.NumField() {
			field := typ.Field(i)
			if field.Tag.Get("binding") == "" || field.Tag.Get("label") == "" {
				t.Errorf("%s.%s 必须有 binding 和中文 label", typ.Name(), field.Name)
			}
		}
	}
	badQC := model.PatchDramaTakeQCReq{Revision: 1, QC: "pending"}
	if err := binding.Validator.ValidateStruct(badQC); err == nil {
		t.Fatal("qc=pending 不在 PR2 契约中")
	}
	tooLong := model.CreateFrameRunReq{RequestKey: strings.Repeat("a", 97), SegmentRevision: 1, FrameSource: model.FrameSourceExisting}
	if err := binding.Validator.ValidateStruct(tooLong); err == nil {
		t.Fatal("请求键不能超过 96 字符")
	}
}

func TestFrameRunSlice2DTOContract(t *testing.T) {
	tests := []struct {
		name, body string
		req        any
		valid      bool
	}{
		{"optional segment CAS", `{"revision":1,"frame_sha256":"` + strings.Repeat("a", 64) + `","input":{},"quote_token":"q"}`, &model.ConfirmFrameRunReq{}, true},
		{"missing frame hash", `{"revision":1,"segment_revision":1,"model_id":"v","input":{},"quote_token":"q"}`, &model.ConfirmFrameRunReq{}, false},
		{"single import", `{"revision":1,"source_canvas_id":"` + idcodec.Encode(9) + `","task_id":"` + idcodec.Encode(42) + `","adopt":true}`, &model.ImportDramaTakesReq{}, true},
		{"zero task in list", `{"revision":1,"source_canvas_id":"` + idcodec.Encode(9) + `","task_ids":[""]}`, &model.ImportDramaTakesReq{}, false},
		{"negative episode CAS", `{"revision":1,"qc":"accepted","expected_segment_version":-1}`, &model.PatchDramaTakeQCReq{}, false},
		{"oversize ticket", `{"revision":1,"frame_asset_id":"42","canvas_ticket":"` + strings.Repeat("x", 4097) + `"}`, &model.SetFrameRunFrameReq{}, false},
		{"decimal string asset id", `{"revision":1,"frame_asset_id":"42"}`, &model.SetFrameRunFrameReq{}, true},
		{"decimal number asset id", `{"revision":1,"frame_asset_id":42}`, &model.SetFrameRunFrameReq{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tc.body), tc.req); err != nil {
				t.Fatal(err)
			}
			err := binding.Validator.ValidateStruct(tc.req)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	var encodedAsset model.SetFrameRunFrameReq
	if err := json.Unmarshal([]byte(`{"revision":1,"frame_asset_id":"`+idcodec.Encode(42)+`"}`), &encodedAsset); err == nil {
		t.Fatal("AssetID 不应接受十六进制编码主键")
	}
	s, err := schema.Parse(&model.DramaSegment{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Relationships.Relations) != 0 {
		t.Fatal("片段不应持有流程/候选 ORM 关联")
	}
}
