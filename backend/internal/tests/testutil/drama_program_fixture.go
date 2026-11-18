package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/service"
)

// Golden timeline size and shot indices shared by the integration suites.
const (
	GoldenFrames  = 708
	GoldenSamples = 1416000
	GoldenS3      = 2
	GoldenS5      = 4
	GoldenS6A     = 5
	GoldenS6B     = 6
	GoldenS7      = 7
)

// GoldenMedia associates source bytes with their asset row.
type GoldenMedia struct {
	Asset model.Asset
	Path  string
}

// GoldenShot describes one real segment and its optional video task.
type GoldenShot struct {
	Segment model.DramaSegment
	Task    *model.GenerationTask
	AssetID uint64
}

// GoldenProgram keeps the source assets, ten segment rows and authored cues together.
type GoldenProgram struct {
	Media   map[uint64]GoldenMedia
	Shots   []GoldenShot
	Program service.DramaProgramInput
}

// NewGoldenProgram synthesizes source media or loads the named inputs from an allowed local root.
func NewGoldenProgram(t *testing.T, user, episode, canvas uint64) *GoldenProgram {
	t.Helper()
	f := &GoldenProgram{Media: make(map[uint64]GoldenMedia)}
	dir, override := t.TempDir(), goldenOverrideRoot(t)
	for _, spec := range goldenMediaSpecs() {
		path := goldenSource(t, dir, override, spec)
		f.Media[spec.id] = GoldenMedia{Path: path, Asset: model.Asset{
			ID: spec.id, UserID: user, Kind: spec.kind, Sha256: FileSHA256(t, path),
			Source: model.AssetSourceUpload, StorageKey: spec.name,
		}}
	}
	f.Shots = goldenShots(t, user, episode, canvas)
	f.Program = goldenProgramInput(f.Shots[GoldenS3].Segment.ID)
	return f
}

type goldenMediaSpec struct {
	id         uint64
	kind, name string
	args       []string
}

func goldenMediaSpecs() []goldenMediaSpec {
	return []goldenMediaSpec{
		goldenVideo(121, "s1", 32, false), goldenVideo(122, "s2", 38, true),
		goldenVideo(123, "s3", 44, false), goldenVideo(124, "s4", 50, false),
		goldenVideo(125, "s5", 56, false), goldenVideo(133, "s7", 62, false),
		goldenVideo(134, "s8", 68, false), goldenVideo(135, "s3-replacement", 74, false),
		goldenVideo(140, "demo-trim", 32, false),
		goldenStill(141, "s6a", "testsrc2=s=320x240"), goldenStill(142, "s6b", "smptebars=s=320x240"),
		goldenWAV(151, "vo-3", "0.15*sin(2*PI*433*t)+0.04*sin(2*PI*719*t)", 186000),
		goldenWAV(152, "vo-4", "0.13*sin(2*PI*503*t)", 162000),
		goldenWAV(153, "bgm", "0.055*sin(2*PI*173*t)", 48000),
		goldenWAV(154, "room-tone", "0.035*sin(2*PI*271*t)", 48000),
		goldenWAV(155, "s3-contact", "if(lt(n,3600),0.08*cos(2*PI*1031*t),0)", 6000),
		goldenWAV(156, "vo-3-sfx", "0.045*sin(2*PI*887*t)", 48000),
	}
}

func goldenVideo(id uint64, name string, luma int, delayAudio bool) goldenMediaSpec {
	// 每帧的亮度不同；f42 和 f43 可以在重新编码后直接区分。
	visual := fmt.Sprintf("nullsrc=s=320x180:r=24:d=6,geq=lum='%d+N':cb=128:cr=128,setsar=1", luma)
	audio := "aevalsrc='if(lt(n,12000),0,0.16*cos(2*PI*317*t))':s=48000:d=6"
	args := []string{"-f", "lavfi", "-i", visual, "-f", "lavfi", "-i", audio}
	if delayAudio {
		args = append(args, "-af", "asetpts=PTS+0.125/TB")
	}
	args = append(args, "-ac", "2", "-c:a", "pcm_s16le", "-c:v", "libx264", "-threads:v", "1", "-pix_fmt", "yuv420p")
	return goldenMediaSpec{id, model.KindVideo, name + ".mkv", args}
}

func goldenStill(id uint64, name, source string) goldenMediaSpec {
	return goldenMediaSpec{id, model.KindImage, name + ".png",
		[]string{"-f", "lavfi", "-i", source, "-frames:v", "1", "-threads:v", "1", "-c:v", "png"}}
}

func goldenWAV(id uint64, name, expr string, samples int) goldenMediaSpec {
	return goldenMediaSpec{id, model.KindAudio, name + ".wav",
		[]string{"-f", "lavfi", "-i", "aevalsrc='" + expr + "|" + expr + "':s=48000",
			"-af", fmt.Sprintf("atrim=end_sample=%d", samples), "-c:a", "pcm_s16le"}}
}

func goldenOverrideRoot(t *testing.T) *os.Root {
	t.Helper()
	path := os.Getenv("VC_PHASE1_FIXTURE_DIR")
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(os.PathSeparator) {
		t.Fatal("VC_PHASE1_FIXTURE_DIR must name a dedicated absolute local input directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func goldenSource(t *testing.T, dir string, override *os.Root, spec goldenMediaSpec) string {
	t.Helper()
	path := filepath.Join(dir, spec.name)
	if override != nil {
		// 仅接受固定命名的源素材；拒绝符号链接和任何成片文件。
		info, err := override.Lstat(spec.name)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("invalid override input %s: %v", spec.name, err)
		}
		data, err := override.ReadFile(spec.name)
		if err != nil {
			t.Fatal(err)
		}
		if !goldenSourceContainer(spec.kind, data) {
			t.Fatalf("override %s has the wrong source container", spec.name)
		}
		writeGoldenOverride(t, dir, spec.name, data)
		return path
	}
	args := append([]string{"-nostdin", "-v", "error", "-y"}, spec.args...)
	if out, err := exec.CommandContext(t.Context(), "ffmpeg", append(args, "-fflags", "+bitexact", path)...).CombinedOutput(); err != nil { //nolint:gosec // FFmpeg flags are fixed fixture data; the output is under t.TempDir().
		t.Fatalf("synthesize %s: %v: %s", spec.name, err, out)
	}
	return path
}

func goldenSourceContainer(kind string, data []byte) bool {
	switch kind {
	case model.KindVideo:
		return bytes.HasPrefix(data, []byte{0x1a, 0x45, 0xdf, 0xa3}) // Matroska; renamed MP4 exports are rejected.
	case model.KindImage:
		return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case model.KindAudio:
		return len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WAVE"
	default:
		return false
	}
}

func writeGoldenOverride(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// FileSHA256 checks the actual bytes rather than a path or metadata fingerprint.
func FileSHA256(t *testing.T, path string) string {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	data, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return jsonhash.SHA256Hex(data)
}

func goldenShots(t *testing.T, user, episode, canvas uint64) []GoldenShot {
	t.Helper()
	type spec struct {
		kind        string
		in, out     int32
		task, asset uint64
		fx          model.DramaSegmentFX
	}
	shots := []spec{
		{model.DramaVisualVideo, 0, 114, 101, 121, model.DramaSegmentFX{FadeInF: 10}},
		{model.DramaVisualVideo, 6, 120, 102, 122, model.DramaSegmentFX{}},
		{model.DramaVisualVideo, 2, 46, 103, 123, model.DramaSegmentFX{}},
		{model.DramaVisualVideo, 2, 54, 104, 124, model.DramaSegmentFX{}},
		{model.DramaVisualVideo, 0, 72, 105, 125, model.DramaSegmentFX{}},
		{model.DramaVisualStill, 0, 47, 0, 141, model.DramaSegmentFX{
			ZoomStart: new(1.02), ZoomEnd: new(1.14), ShakePx: new(2.6), FlashInF: 4, CropY: new(0.45), Seed: new(int32(601)),
		}},
		{model.DramaVisualStill, 0, 47, 0, 142, model.DramaSegmentFX{
			ZoomStart: new(1.0), ZoomEnd: new(1.12), ShakePx: new(3.4), FlashInF: 2, FlashOutF: 3, CropY: new(0.35), Seed: new(int32(602)),
		}},
		{model.DramaVisualVideo, 0, 81, 115, 133, model.DramaSegmentFX{}},
		{model.DramaVisualVideo, 0, 124, 116, 134, model.DramaSegmentFX{FadeOutF: 12}},
		{model.DramaVisualBlack, 0, 13, 0, 0, model.DramaSegmentFX{}},
	}
	out := make([]GoldenShot, 0, len(shots))
	for i, s := range shots {
		fx, err := json.Marshal(s.fx)
		if err != nil {
			t.Fatal(err)
		}
		row := model.DramaSegment{BaseModel: model.BaseModel{ID: uint64(101 + i)}, UserID: user, EpisodeID: episode,
			Seq: i, Revision: 1, Seconds: 5, VisualKind: s.kind, SourceInF: s.in, SourceOutF: new(s.out), FXJSON: fx}
		shot := GoldenShot{Segment: row, AssetID: s.asset}
		if s.task != 0 {
			shot.Segment.ChosenTaskID = new(s.task)
			shot.Segment.MuteOriginal = i != 1
			shot.Task = GoldenVideoTask(s.task, user, canvas, row.ID, s.asset)
		} else if s.kind == model.DramaVisualStill {
			shot.Segment.StillAssetID = new(s.asset)
		}
		out = append(out, shot)
	}
	out[1].Segment.NativeIsDialogue, out[1].Segment.AudioGainDB = true, 2
	return out
}

// GoldenVideoTask builds the completed candidate used by a fixture segment.
func GoldenVideoTask(id, user, canvas, segment, asset uint64) *model.GenerationTask {
	return &model.GenerationTask{ID: id, UserID: user, CanvasProjectID: new(canvas), NodeID: "seg:" + idcodec.Encode(segment),
		Kind: model.KindVideo, Status: model.TaskSucceeded,
		OutputJSON: []byte(fmt.Sprintf(`[{"asset_id":%d,"media_type":"video"}]`, asset))}
}

func goldenProgramInput(s3 uint64) service.DramaProgramInput {
	vo3 := goldenCue("vo_3", model.DramaAudioNarration, 151, 372, 465)
	vo3.FadeOutF = 2
	bgm := goldenCue("bgm", model.DramaAudioBGM, 153, 0, GoldenFrames)
	room := goldenCue("room_tone", model.DramaAudioRoomTone, 154, 0, GoldenFrames)
	bgm.Loop, room.Loop = true, true
	bgm.DuckDB, room.DuckDB = new(-8.0), new(-8.0)
	contact := service.DramaProgramAudioInput{AssetID: 155,
		DramaProgramAudioParams: model.DramaProgramAudioParams{ID: "contact", Role: model.DramaAudioSFX, LengthF: new(int32(3))},
		Anchor:                  &service.DramaProgramAnchorInput{SegmentID: idcodec.ID(s3), SourceF: new(int32(13))}}
	return service.DramaProgramInput{Version: 1, Overlays: []json.RawMessage{}, Audio: []service.DramaProgramAudioInput{
		contact, vo3, goldenCue("vo_4", model.DramaAudioNarration, 152, 490, 571), bgm, room,
		goldenCue("vo_3_sfx", model.DramaAudioSFX, 156, 410, 434),
	}}
}

func goldenCue(id string, role model.DramaProgramAudioRole, asset model.AssetID, in, out int32) service.DramaProgramAudioInput {
	return service.DramaProgramAudioInput{AssetID: asset, DramaProgramAudioParams: model.DramaProgramAudioParams{
		ID: id, Role: role, StartF: &in, EndF: &out,
	}}
}

// GoldenExtractor allows both export sizes to finish on slower CI runners.
func GoldenExtractor() *videoframe.Extractor {
	return videoframe.New(videoframe.Options{Timeout: 4 * time.Minute})
}

// GoldenVideoClip reads the real normalized probe for a candidate.
func GoldenVideoClip(t *testing.T, e *videoframe.Extractor, path string, in, out int32) videoframe.ProgramClip {
	t.Helper()
	p, err := e.ProbeNormalized24FPS(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	return videoframe.ProgramClip{
		ProgramVisualClip: videoframe.ProgramVisualClip{VisualKind: model.DramaVisualVideo, SourcePath: path,
			SourceInF: in, SourceOutF: out, NormalizedFrames24: p.NormalizedFrames24},
		HasAudio: p.HasAudio, DecodedAudioSamples: p.DecodedAudioSamples, VStartUs: p.VStartUs, AStartUs: p.AStartUs,
	}
}

// RenderInput maps all fixture rows to the public renderer contract.
func (f *GoldenProgram) RenderInput(t *testing.T, e *videoframe.Extractor) videoframe.ComposeProgramInput {
	t.Helper()
	var result videoframe.ComposeProgramInput
	for _, shot := range f.Shots {
		row := shot.Segment
		clip := videoframe.ProgramClip{ProgramVisualClip: videoframe.ProgramVisualClip{
			VisualKind: row.VisualKind, SourceOutF: *row.SourceOutF,
		}}
		switch row.VisualKind {
		case model.DramaVisualVideo:
			clip = GoldenVideoClip(t, e, f.Media[shot.AssetID].Path, row.SourceInF, *row.SourceOutF)
		case model.DramaVisualStill:
			clip.StillPath = f.Media[shot.AssetID].Path
		}
		fx, err := model.DecodeDramaSegmentFX(row.FXJSON)
		if err != nil {
			t.Fatal(err)
		}
		clip.FX, clip.MuteOriginal = &fx, row.MuteOriginal
		clip.AudioGainDB, clip.NativeIsDialogue = row.AudioGainDB, row.NativeIsDialogue
		result.Clips = append(result.Clips, clip)
	}
	for _, cue := range f.Program.Audio {
		result.Audio = append(result.Audio, f.renderCue(t, e, cue))
	}
	return result
}

func (f *GoldenProgram) renderCue(t *testing.T, e *videoframe.Extractor, cue service.DramaProgramAudioInput) videoframe.ProgramAudioCueSpec {
	t.Helper()
	path := f.Media[uint64(cue.AssetID)].Path
	p, err := e.ProbeProgramAudio(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	start, end := int32(239), int32(242) // 已解析的 S3 接触点；服务测试独立验证源锚点。
	if cue.Anchor == nil {
		start, end = *cue.StartF, *cue.EndF
	}
	return videoframe.ProgramAudioCueSpec{ID: cue.ID, Role: cue.Role, SourcePath: path,
		DecodedAudioSamples: p.DecodedAudioSamples, StartF: start, EndF: end, FadeOutF: cue.FadeOutF, Loop: cue.Loop, DuckDB: cue.DuckDB}
}
