package service_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/service"
)

func TestPutDramaProgramNormalizesEmptyArrays(t *testing.T) {
	for _, program := range []string{`{"v":1}`, `{"v":1,"audio":null,"overlays":null}`, `{"v":1,"audio":[],"overlays":[]}`} {
		req, err := service.DecodePutDramaProgramReq(strings.NewReader(putProgramJSON(program) + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if req.ExpectedEpisodeID != 91 || req.ExpectedSegmentVersion == nil || *req.ExpectedSegmentVersion != 0 || req.Program.Audio == nil || req.Program.Overlays == nil {
			t.Fatalf("CAS 或空数组没有规范化：%+v", req)
		}
		stored, err := req.Program.ToDomain()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(stored)
		if err != nil || string(raw) != `{"v":1,"audio":[],"overlays":[]}` {
			t.Fatalf("空节目单不能按 DB 契约存储：%s / %v", raw, err)
		}
	}
}

func TestPutDramaProgramAnchoredCueKeepsBindingPrivate(t *testing.T) {
	program := fmt.Sprintf(`{"v":1,"audio":[{"id":"sfx-1","role":"sfx","asset_id":"9223372036854775807","anchor":{"segment_id":%q,"source_f":0},"length_f":24,"src_in_sample":100,"src_out_sample":200,"gain_db":12}]}`, idcodec.Encode(42))
	body := strings.TrimSuffix(putProgramJSON(program), "}") + `,"rebind_anchor_cue_ids":["sfx-1"]}`
	var req service.PutDramaProgramReq
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	stored, err := req.Program.ToDomain()
	if err != nil {
		t.Fatal(err)
	}
	cue := &stored.Audio[0]
	if cue.AssetID != math.MaxInt64 || cue.Anchor == nil || cue.Anchor.SegmentID != 42 || cue.BoundSource != nil || len(req.RebindAnchorCueIDs) != 1 || req.RebindAnchorCueIDs[0] != cue.ID {
		t.Fatalf("锚点、素材或重绑指令不正确：%+v / %v", cue, req.RebindAnchorCueIDs)
	}
	cue.BoundSource = &model.DramaProgramBoundSource{TaskID: 51, AssetID: 52, Sha256: strings.Repeat("a", 64)}
	private, err := json.Marshal(stored)
	if err != nil || !strings.Contains(string(private), `"bound_source":{"task_id":51,"asset_id":52,"sha256":`) {
		t.Fatalf("绑定应进入 DB JSON：%s / %v", private, err)
	}
	public, err := json.Marshal(service.DramaProgramViewFromDomain(stored))
	if err != nil || strings.Contains(string(public), "bound_source") || !strings.Contains(string(public), `"asset_id":9223372036854775807`) || !strings.Contains(string(public), fmt.Sprintf(`"segment_id":%q`, idcodec.Encode(42))) {
		t.Fatalf("公开节目单只应输出数字素材 ID、编码分镜 ID 和公开参数：%s / %v", public, err)
	}
}

func TestPutDramaProgramRejectsUnsupportedShape(t *testing.T) {
	const program = `{"v":1,"audio":[],"overlays":[]}`
	body := putProgramJSON(program)
	for _, tc := range []struct{ name, input string }{
		{"missing_episode", `{"expected_segment_version":0,"program":` + program + `}`},
		{"decimal_episode", strings.Replace(body, fmt.Sprintf("%q", idcodec.Encode(91)), `91`, 1)},
		{"missing_version", strings.Replace(body, `"expected_segment_version":0,`, "", 1)},
		{"null_version", strings.Replace(body, `"expected_segment_version":0`, `"expected_segment_version":null`, 1)},
		{"negative_version", strings.Replace(body, `"expected_segment_version":0`, `"expected_segment_version":-1`, 1)},
		{"missing_program", strings.TrimSuffix(body, `,"program":`+program+`}`) + `}`},
		{"null_program", putProgramJSON(`null`)},
		{"program_v2", putProgramJSON(`{"v":2}`)},
		{"unknown_top_level", strings.TrimSuffix(body, "}") + `,"unknown":true}`},
		{"unknown_program_field", putProgramJSON(`{"v":1,"unknown":true}`)},
		{"trailing_value", body + ` {}`},
		{"trailing_junk", body + ` extra`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.DecodePutDramaProgramReq(strings.NewReader(tc.input))
			assertWireProgramError(t, err, 91009)
		})
	}
	const overlayMessage = "当前版本暂不支持字幕与角标叠加，overlays 必须为空"
	if got := assertWireProgramError(t, json.Unmarshal([]byte(putProgramJSON(`{"v":1,"overlays":[{}]}`)), new(service.PutDramaProgramReq)), 91009); got != overlayMessage {
		t.Fatalf("非空 overlays 应返回固定提示：%s", got)
	}
}

func TestPutDramaProgramValidatesCueBounds(t *testing.T) {
	const cue = `"id":"cue-1","role":"bgm","asset_id":1,"start_f":0,"end_f":14400,"gain_db":-60,"src_in_sample":0,"duck_db":-8,"av_pts_offset_ms":-10000`
	for _, params := range []string{cue, strings.NewReplacer(`"bgm"`, `"room_tone"`, `"av_pts_offset_ms":-10000`, `"av_pts_offset_ms":10000`).Replace(cue)} {
		if err := decodeProgramCue(params); err != nil {
			t.Fatalf("应接受位于边界内的音频：%v", err)
		}
	}
	for _, tc := range []struct{ name, from, to string }{
		{"empty_id", `"cue-1"`, `""`}, {"unknown_role", `"bgm"`, `"music"`},
		{"legacy_dialogue_role", `"bgm"`, `"dialogue"`}, {"legacy_voiceover_role", `"bgm"`, `"voiceover"`},
		{"duck_role", `"bgm"`, `"sfx"`},
		{"duck_level", `"duck_db":-8`, `"duck_db":-7.99`}, {"missing_asset", `"asset_id":1,`, ``},
		{"asset_empty", `"asset_id":1`, `"asset_id":""`}, {"asset_null", `"asset_id":1`, `"asset_id":null`},
		{"asset_zero", `"asset_id":1`, `"asset_id":0`}, {"asset_negative", `"asset_id":1`, `"asset_id":-1`},
		{"asset_hex", `"asset_id":1`, `"asset_id":"0x2a"`}, {"asset_fraction", `"asset_id":1`, `"asset_id":1.5`},
		{"asset_overflow", `"asset_id":1`, `"asset_id":9223372036854775808`},
		{"too_long", `"end_f":14400`, `"end_f":14401`}, {"negative_start", `"start_f":0`, `"start_f":-1`},
		{"empty_range", `"end_f":14400`, `"end_f":0`}, {"missing_end", `"end_f":14400,`, ``},
		{"negative_sample", `"src_in_sample":0`, `"src_in_sample":-1`}, {"empty_sample_range", `"src_in_sample":0`, `"src_in_sample":1,"src_out_sample":1`},
		{"gain_low", `"gain_db":-60`, `"gain_db":-60.01`}, {"gain_high", `"gain_db":-60`, `"gain_db":12.01`},
		{"offset_high", `"av_pts_offset_ms":-10000`, `"av_pts_offset_ms":10001`}, {"offset_low", `"av_pts_offset_ms":-10000`, `"av_pts_offset_ms":-10001`},
		{"fade_negative", `"duck_db":-8`, `"duck_db":-8,"fade_in_f":-1`},
		{"fade_overflow", `"duck_db":-8`, `"duck_db":-8,"fade_in_f":10000,"fade_out_f":4401`},
		{"loop_narration", `"role":"bgm"`, `"role":"narration","loop":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertWireProgramError(t, decodeProgramCue(strings.Replace(cue, tc.from, tc.to, 1)), 91009)
		})
	}
}

func TestPutDramaProgramRejectsInvalidAnchors(t *testing.T) {
	segment := fmt.Sprintf(`%q`, idcodec.Encode(42))
	cue := `"id":"sfx-1","role":"sfx","asset_id":1,"anchor":{"segment_id":` + segment + `,"source_f":0},"length_f":24`
	for _, tc := range []struct{ name, input string }{
		{"private_binding", cue + `,"bound_source":{"task_id":1,"asset_id":2,"sha256":"x"}`},
		{"absolute_start", cue + `,"start_f":0`}, {"absolute_end", cue + `,"end_f":24`},
		{"not_sfx", strings.Replace(cue, `"sfx"`, `"narration"`, 1)},
		{"plain_segment_id", strings.Replace(cue, segment, `42`, 1)},
		{"negative_source_frame", strings.Replace(cue, `"source_f":0`, `"source_f":-1`, 1)},
		{"missing_source_frame", strings.Replace(cue, `,"source_f":0`, ``, 1)},
		{"unknown_anchor_field", strings.Replace(cue, `"source_f":0`, `"source_f":0,"unknown":0`, 1)},
		{"zero_length", strings.Replace(cue, `"length_f":24`, `"length_f":0`, 1)},
		{"missing_length", strings.Replace(cue, `,"length_f":24`, ``, 1)},
		{"too_long", strings.Replace(cue, `"length_f":24`, `"length_f":14401`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) { assertWireProgramError(t, decodeProgramCue(tc.input), 91009) })
	}
}

func TestDramaProgramWireLimits(t *testing.T) {
	cues := make([]string, 256)
	for i := range cues {
		cues[i] = fmt.Sprintf(`{"id":"cue-%d","role":"narration","asset_id":1,"start_f":0,"end_f":24}`, i)
	}
	decode := func(cues []string) error {
		return json.Unmarshal([]byte(putProgramJSON(`{"v":1,"audio":[`+strings.Join(cues, ",")+`]}`)), new(service.PutDramaProgramReq))
	}
	if err := decode(cues); err != nil {
		t.Fatalf("应接受 256 个音频条目：%v", err)
	}
	assertWireProgramError(t, decode(append(cues, `{"id":"cue-256","role":"narration","asset_id":1,"start_f":0,"end_f":24}`)), 91009)
	assertWireProgramError(t, decode([]string{cues[0], cues[0]}), 91009)
}

func TestDramaManifestV2UsesPublicIDsWithoutBinding(t *testing.T) {
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
	raw, err := json.Marshal(service.DramaManifestV2ViewFromDomain(manifest))
	if err != nil || strings.Contains(string(raw), "bound_source") {
		t.Fatalf("清单不可包含私有绑定：%s / %v", raw, err)
	}
	for _, want := range []string{
		fmt.Sprintf(`"segment_id":%q`, idcodec.Encode(42)),
		fmt.Sprintf(`"task_id":%q`, idcodec.Encode(51)),
		fmt.Sprintf(`"episode_id":%q`, idcodec.Encode(91)),
		`"v":2`, `"render_version":2`, `"asset_id":52`, `"asset_id":83`, `"still_asset_id":9223372036854775807`, `"visual_kind":"black"`, `"fx":{"fade_in_f":2}`, `"overlays":[]`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("清单缺少 %s：%s", want, raw)
		}
	}
}

func putProgramJSON(program string) string {
	return fmt.Sprintf(`{"expected_episode_id":%q,"expected_segment_version":0,"program":%s}`, idcodec.Encode(91), program)
}

func decodeProgramCue(cue string) error {
	return json.Unmarshal([]byte(putProgramJSON(`{"v":1,"audio":[{`+cue+`}]}`)), new(service.PutDramaProgramReq))
}

func assertWireProgramError(t *testing.T, err error, wantCode int) string {
	t.Helper()
	var programErr *model.DramaProgramError
	if wantCode != 91009 || !errors.As(err, &programErr) {
		t.Fatalf("期望 91009 节目单契约错误，实际 %v", err)
	}
	return programErr.Error()
}
