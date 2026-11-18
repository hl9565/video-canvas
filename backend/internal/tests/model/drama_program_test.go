package model_test

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/jsonhash"
)

func TestDramaSegmentStillAssetPatch(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		set         bool
		id          uint64
	}{
		{"omitted", ``, false, 0},
		{"null", `,"still_asset_id":null`, true, 0},
		{"decimal", `,"still_asset_id":1`, true, 1},
		{"decimal_string", `,"still_asset_id":"9223372036854775807"`, true, math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req model.UpdateDramaSegmentReq
			if err := json.Unmarshal([]byte(`{"revision":1`+tc.field+`}`), &req); err != nil {
				t.Fatal(err)
			}
			id := req.StillAssetID
			if id.Set != tc.set || (tc.id == 0 && id.Value != nil) || (tc.id != 0 && (id.Value == nil || *id.Value != tc.id)) {
				t.Fatalf("素材 ID 三态不正确：%+v", id)
			}
		})
	}
	for _, invalid := range []string{`""`, `" "`, `0`, `"0"`, `-1`, `"-1"`, `1.5`, `"1.5"`, `1e2`, `"0x10"`, `9223372036854775808`, `"9223372036854775808"`} {
		t.Run(invalid, func(t *testing.T) {
			var req model.UpdateDramaSegmentReq
			if err := json.Unmarshal([]byte(`{"revision":1,"still_asset_id":`+invalid+`}`), &req); err == nil {
				t.Fatal("应拒绝非法 still_asset_id")
			}
		})
	}
}

func TestDramaSegmentPatchStrictAndFX(t *testing.T) {
	const valid = `{"revision":1,"CHOSEN_TASK_ID":null,"visual_kind":"still","still_asset_id":"19","fx":{"fade_in_f":0,"fade_out_f":1,"flash_in_f":2,"flash_out_f":3,"native_fade_in_f":4,"native_fade_out_f":5,"zoom_start":1,"zoom_end":1.25,"shake_px":6,"crop_y":0,"seed":2147483647}}`
	var req model.UpdateDramaSegmentReq
	if err := json.Unmarshal([]byte(valid), &req); err != nil {
		t.Fatal(err)
	}
	if !req.ClearChosen || req.VisualKind == nil || *req.VisualKind != "still" || req.FX == nil || req.FX.NativeFadeOutF != 5 || req.FX.CropY == nil || *req.FX.CropY != 0 {
		t.Fatalf("视觉参数或 Stage B 清空行为丢失：%+v", req)
	}
	for _, invalid := range []string{`{"revision":1,"unknown":true}`, valid + ` {}`, `null`} {
		if err := req.UnmarshalJSON([]byte(invalid)); err == nil {
			t.Errorf("应拒绝 PATCH %s", invalid)
		}
	}
	for _, invalid := range []string{
		`"visual_kind":"image"`, `"fx":{"subtitle":"text"}`, `"fx":{"fade_in_f":-1}`, `"fx":{"flash_out_f":0.5}`,
		`"fx":{"native_fade_out_f":-1}`, `"fx":{"zoom_start":0.99}`, `"fx":{"zoom_end":1.26}`, `"fx":{"shake_px":6.01}`,
		`"fx":{"crop_y":1.01}`, `"fx":{"seed":-1}`, `"fx":{"seed":2147483648}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			assertDramaContractError(t, json.Unmarshal([]byte(`{"revision":1,`+invalid+`}`), &req), 91011)
		})
	}
	for _, nonFinite := range []float64{math.NaN(), math.Inf(-1), math.Inf(1)} {
		assertDramaContractError(t, (model.DramaSegmentFX{ZoomStart: &nonFinite}).Validate(), 91011)
	}
}

func TestDramaSegmentStageCWireIDs(t *testing.T) {
	defaultView, err := json.Marshal(model.DramaSegmentView{})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(defaultView, &raw); err != nil || string(raw["visual_kind"]) != `"video"` || raw["still_asset_id"] != nil {
		t.Fatalf("旧分镜视图应默认为 video：%s / %v", defaultView, err)
	}
	asset := uint64(math.MaxInt64)
	view, err := json.Marshal(model.DramaSegmentView{VisualKind: "still", StillAssetID: &asset})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(view, &raw); err != nil || string(raw["visual_kind"]) != `"still"` || string(raw["still_asset_id"]) != `9223372036854775807` {
		t.Fatalf("静帧素材编号必须输出为十进制数字：%s / %v", view, err)
	}
}

func TestDramaProgramDomainValidationAndLimits(t *testing.T) {
	const overlayMessage = "当前版本暂不支持字幕与角标叠加，overlays 必须为空"
	err := (model.DramaProgram{Version: 1, Overlays: []json.RawMessage{json.RawMessage(`{}`)}}).Validate()
	if got := assertDramaContractError(t, err, 91009); got != overlayMessage {
		t.Fatalf("非空 overlays 应返回固定提示：%s", got)
	}
	if err := model.ValidateDramaProgramTimeline(200, 14400); err != nil {
		t.Fatalf("应接受 200 个分镜、14400 帧：%v", err)
	}
	assertDramaContractError(t, model.ValidateDramaProgramTimeline(201, 14400), 91009)
	assertDramaContractError(t, model.ValidateDramaProgramTimeline(200, 14401), 91009)
}

func TestDramaEpisodeManifestV1JSONAndHashAreFrozen(t *testing.T) {
	manifest := model.DramaEpisodeManifest{
		Version: model.CurrentDramaManifestSchemaVersion, RenderVersion: model.CurrentDramaManifestRenderVersion,
		EpisodeID: 91, SegmentVersion: 7, FPS: 24, SampleRate: model.CurrentDramaManifestSampleRate,
		TotalFrames: 19, SkippedSeqs: []int{}, Program: model.DramaEpisodeProgram{Version: 1},
		Clips: []model.DramaManifestClip{{
			SegmentID: 42, TaskID: 51, AssetID: 52, Sha256: strings.Repeat("a", 64), SourceInF: 3, SourceOutF: 22,
			MasterOutF: 19, NormalizedFrames24: 30, HasAudio: true, DecodedAudioSamples: 60000, AStartUs: 1000,
			AudioGainDB: -6, NativeIsDialogue: true, AvPtsOffsetMs: -20,
		}},
	}
	const golden = `{"v":1,"render_version":1,"episode_id":91,"segment_version":7,"fps":24,"sample_rate":48000,"total_frames":19,"skipped_seqs":[],"clips":[{"segment_id":42,"task_id":51,"asset_id":52,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","seq":0,"source_in_f":3,"source_out_f":22,"master_in_f":0,"master_out_f":19,"normalized_frames_24":30,"has_audio":true,"decoded_audio_samples":60000,"v_start_us":0,"a_start_us":1000,"mute_original":false,"audio_gain_db":-6,"native_is_dialogue":true,"av_pts_offset_ms":-20}],"program":{"v":1,"audio":null,"overlays":null}}`
	raw, err := json.Marshal(manifest)
	if err != nil || string(raw) != golden {
		t.Fatalf("v1 JSON 发生变化：%s / %v", raw, err)
	}
	const goldenSHA = "c9e6da7933933b6b151b10fe5c4362ad5cdf908037b3249addcfc39a196e5560"
	if hash, err := jsonhash.CanonicalSHA256(manifest); err != nil || hash != goldenSHA {
		t.Fatalf("v1 SHA-256 发生变化：%s / %v", hash, err)
	}
}

func TestDramaManifestV2CanonicalJSONWithoutBinding(t *testing.T) {
	still := uint64(math.MaxInt64)
	program := model.DramaProgram{Version: 1, Audio: []model.DramaProgramAudioCue{{
		DramaProgramAudioSource: model.DramaProgramAudioSource{
			DramaProgramAudioParams: model.DramaProgramAudioParams{ID: "sfx-1", Role: model.DramaAudioSFX},
			AssetID:                 83, Anchor: &model.DramaProgramAnchor{SegmentID: 42, SourceF: 10},
		},
		BoundSource: &model.DramaProgramBoundSource{TaskID: 51, AssetID: 52, Sha256: strings.Repeat("a", 64)},
	}}}
	manifest := model.DramaEpisodeManifestV2{
		Version: model.CurrentDramaManifestV2SchemaVersion, RenderVersion: model.CurrentDramaManifestV2RenderVersion, EpisodeID: 91,
		Clips: []model.DramaManifestV2Clip{
			{SegmentID: 42, TaskID: 51, DramaManifestV2ClipParams: model.DramaManifestV2ClipParams{VisualKind: "video", AssetID: 52}},
			{SegmentID: 43, DramaManifestV2ClipParams: model.DramaManifestV2ClipParams{VisualKind: "still", StillAssetID: &still, FX: &model.DramaSegmentFX{FadeInF: 2}}},
			{SegmentID: 44, DramaManifestV2ClipParams: model.DramaManifestV2ClipParams{VisualKind: "black"}},
		},
		Program: program.ManifestProgram(),
	}
	raw, err := json.Marshal(manifest)
	if err != nil || strings.Contains(string(raw), "bound_source") {
		t.Fatalf("清单不可包含私有绑定：%s / %v", raw, err)
	}
	for _, want := range []string{
		`"segment_id":42`, `"task_id":51`, `"episode_id":91`,
		`"v":2`, `"render_version":2`, `"asset_id":52`, `"asset_id":83`, `"still_asset_id":9223372036854775807`, `"visual_kind":"black"`, `"fx":{"fade_in_f":2}`, `"overlays":[]`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("清单缺少 %s：%s", want, raw)
		}
	}
}

func assertDramaContractError(t *testing.T, err error, code int) string {
	t.Helper()
	switch code {
	case 91009:
		var got *model.DramaProgramError
		if !errors.As(err, &got) {
			t.Fatalf("期望节目单契约错误，实际 %v", err)
		}
		return got.Error()
	case 91011:
		var got *model.DramaVisualError
		if !errors.As(err, &got) {
			t.Fatalf("期望视觉契约错误，实际 %v", err)
		}
		return got.Error()
	default:
		t.Fatalf("未登记的契约测试错误码 %d：%v", code, err)
		return ""
	}
}
