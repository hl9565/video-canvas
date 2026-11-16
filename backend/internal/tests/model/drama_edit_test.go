package model_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
)

// TestNullableInt32Unmarshal 验证 PATCH 编辑框的三态字段能严格区分缺失、显式 null、整数和非法值。
func TestNullableInt32Unmarshal(t *testing.T) {
	type patchBody struct {
		SourceOutF model.NullableInt32 `json:"source_out_f"`
	}

	var missing patchBody
	if err := json.Unmarshal([]byte(`{}`), &missing); err != nil {
		t.Fatalf("缺失字段不应报错：%v", err)
	}
	if missing.SourceOutF.Set || missing.SourceOutF.Value != nil {
		t.Fatalf("缺失字段应保持 Set=false, Value=nil，实际 %+v", missing.SourceOutF)
	}

	var explicitNull patchBody
	if err := json.Unmarshal([]byte(`{"source_out_f":null}`), &explicitNull); err != nil {
		t.Fatalf("显式 null 不应报错：%v", err)
	}
	if !explicitNull.SourceOutF.Set || explicitNull.SourceOutF.Value != nil {
		t.Fatalf("显式 null 应置 Set=true, Value=nil，实际 %+v", explicitNull.SourceOutF)
	}

	var value patchBody
	if err := json.Unmarshal([]byte(`{"source_out_f":43}`), &value); err != nil {
		t.Fatalf("整数值不应报错：%v", err)
	}
	if !value.SourceOutF.Set || value.SourceOutF.Value == nil || *value.SourceOutF.Value != 43 {
		t.Fatalf("整数值应解析为 43，实际 %+v", value.SourceOutF)
	}

	raw, err := json.Marshal(value.SourceOutF)
	if err != nil || string(raw) != "43" {
		t.Fatalf("有值序列化应为 43，实际 %q err=%v", raw, err)
	}
	rawNull, err := json.Marshal(explicitNull.SourceOutF)
	if err != nil || string(rawNull) != "null" {
		t.Fatalf("null 值序列化应为 null，实际 %q err=%v", rawNull, err)
	}

	for _, bad := range []string{`{"source_out_f":"43"}`, `{"source_out_f":1.5}`, `{"source_out_f":true}`, `{"source_out_f":{}}`} {
		var target patchBody
		if err := json.Unmarshal([]byte(bad), &target); err == nil {
			t.Fatalf("非法 JSON %s 应当拒绝", bad)
		}
	}
}

// TestModelAllContainsAssetMediaProbe 验证新探测缓存表已注册进 model.All，可供 schema 校验覆盖。
func TestModelAllContainsAssetMediaProbe(t *testing.T) {
	found := false
	for _, m := range model.All() {
		if _, ok := m.(*model.AssetMediaProbe); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("model.All() 缺少 *model.AssetMediaProbe")
	}
}

// TestDramaEditErrorCodes 验证 Phase 2 PR1 新增错误码与 HTTP 状态码。
func TestDramaEditErrorCodes(t *testing.T) {
	cases := []struct {
		name   string
		err    *errcode.Error
		code   int
		status int
	}{
		{"ErrDramaManifestStale", errcode.ErrDramaManifestStale, 67028, http.StatusConflict},
		{"ErrDramaQuoteExpired", errcode.ErrDramaQuoteExpired, 67029, http.StatusConflict},
		{"ErrDramaEditInvalid", errcode.ErrDramaEditInvalid, 67030, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if tc.err == nil {
			t.Fatalf("%s 未定义", tc.name)
		}
		if tc.err.Code != tc.code || tc.err.HTTPStatus() != tc.status {
			t.Fatalf("%s = (%d, %d)，期望 (%d, %d)", tc.name, tc.err.Code, tc.err.HTTPStatus(), tc.code, tc.status)
		}
	}
}

// TestDramaEpisodeProgramDefault 验证集级包结构的默认版本契约。
func TestDramaEpisodeProgramDefault(t *testing.T) {
	p := model.DramaEpisodeProgram{Version: 1}
	if p.Version != 1 {
		t.Fatalf("期望 Version=1，实际 %d", p.Version)
	}
}
