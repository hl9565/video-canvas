package videoframe

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	composeAudioRate    = 48000
	composeMaxEdge      = 1920
	composeMaxPixels    = 1920 * 1088
	composeNearARTol    = 0.05
	composeSyncMinDelta = 0.001
	composeMaxFPS       = 240.0
	composeMinFPS       = 1.0
)

type rational struct {
	num int
	den int
}

type composeInfo struct {
	width    int
	height   int
	dar      float64
	fps      rational
	duration float64
	vStart   float64
	aStart   float64
	audio    bool
}

type composeTarget struct {
	width  int
	height int
	baseAR float64
	fps    rational
}

type probeOutput struct {
	Streams []probeStream `json:"streams"`
	Format  probeFormat   `json:"format"`
}

type probeStream struct {
	Type              string          `json:"codec_type"`
	Width             int             `json:"width"`
	Height            int             `json:"height"`
	SampleAspectRatio string          `json:"sample_aspect_ratio"`
	RFrameRate        string          `json:"r_frame_rate"`
	AvgFrameRate      string          `json:"avg_frame_rate"`
	Duration          string          `json:"duration"`
	StartTime         string          `json:"start_time"`
	NBFrames          string          `json:"nb_frames"`
	NBReadFrames      string          `json:"nb_read_frames"`
	SideDataList      []probeSideData `json:"side_data_list"`
	Tags              probeTags       `json:"tags"`
}

type probeSideData struct {
	SideDataType string  `json:"side_data_type"`
	Rotation     float64 `json:"rotation"`
}

type probeTags struct {
	Rotate string `json:"rotate"`
}

type probeFormat struct {
	Duration string `json:"duration"`
}

func parseComposeInfo(raw probeOutput) (composeInfo, error) {
	var info composeInfo
	var videoFound bool
	for _, s := range raw.Streams {
		if s.Type == "audio" && !info.audio {
			info.audio = true
			info.aStart = parseFiniteFloat(s.StartTime)
			continue
		}
		if s.Type == "video" && !videoFound && s.Width > 0 && s.Height > 0 {
			videoFound = true
			w, h, dar := canonicalDisplayGeometry(s)
			info.width, info.height, info.dar = w, h, dar
			info.fps = parseStreamFPS(s.RFrameRate, s.AvgFrameRate)
			info.duration = parseFiniteFloat(s.Duration)
			info.vStart = parseFiniteFloat(s.StartTime)
		}
	}
	if info.duration <= 0 {
		info.duration = parseFiniteFloat(raw.Format.Duration)
	}
	if !videoFound || info.width <= 0 || info.height <= 0 || info.duration <= 0 {
		return composeInfo{}, fmt.Errorf("%w: 视频没有有效的尺寸或时长", ErrDecode)
	}
	return info, nil
}

// canonicalDisplayGeometry computes post-rotation, non-shrinking square-pixel dimensions and DAR.
func canonicalDisplayGeometry(s probeStream) (int, int, float64) {
	w, h := s.Width, s.Height
	sarNum, sarDen := parsePositiveRatio(s.SampleAspectRatio, 1, 1)
	if isQuarterTurn(streamRotationDegrees(s)) {
		w, h = h, w
		sarNum, sarDen = sarDen, sarNum
	}
	dar := (float64(w) * float64(sarNum)) / (float64(h) * float64(sarDen))
	dispW, dispH := expandSAR(w, h, sarNum, sarDen)
	return dispW, dispH, dar
}

func expandSAR(w, h, sarNum, sarDen int) (int, int) {
	if sarNum > sarDen {
		scaledW := int(math.Round(float64(w) * float64(sarNum) / float64(sarDen)))
		return roundEven(scaledW), roundEven(h)
	}
	if sarNum < sarDen {
		scaledH := int(math.Round(float64(h) * float64(sarDen) / float64(sarNum)))
		return roundEven(w), roundEven(scaledH)
	}
	return roundEven(w), roundEven(h)
}

func streamRotationDegrees(s probeStream) float64 {
	for _, sd := range s.SideDataList {
		if sd.Rotation != 0 {
			return sd.Rotation
		}
	}
	if s.Tags.Rotate != "" {
		return parseFiniteFloat(s.Tags.Rotate)
	}
	return 0
}

func isQuarterTurn(deg float64) bool {
	if deg == 0 {
		return false
	}
	quarters := int(math.Round(math.Abs(deg)/90.0)) % 4
	return quarters == 1 || quarters == 3
}

func chooseComposeTarget(infos []composeInfo) composeTarget {
	anchor := infos[0]
	portrait := anchor.height > anchor.width
	bestW, bestH := anchor.width, anchor.height
	bestArea := bestW * bestH

	for _, info := range infos[1:] {
		sameOrient := (info.height > info.width) == portrait
		if !sameOrient || !nearAspectRatio(info.dar, anchor.dar) {
			continue
		}
		area := info.width * info.height
		if area > bestArea || (area == bestArea && info.width > bestW) {
			bestW, bestH, bestArea = info.width, info.height, area
		}
	}
	w, h := capComposeDimensions(bestW, bestH)
	return composeTarget{
		width:  w,
		height: h,
		baseAR: anchor.dar,
		fps:    chooseTargetFPS(infos),
	}
}

func nearAspectRatio(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	return math.Abs(a-b)/math.Max(a, b) <= composeNearARTol
}

func capComposeDimensions(w, h int) (int, int) {
	fw, fh := float64(w), float64(h)
	if maxEdge := math.Max(fw, fh); maxEdge > composeMaxEdge {
		scale := composeMaxEdge / maxEdge
		fw *= scale
		fh *= scale
	}
	if area := fw * fh; area > composeMaxPixels {
		scale := math.Sqrt(composeMaxPixels / area)
		fw *= scale
		fh *= scale
	}
	return roundEven(int(math.Round(fw))), roundEven(int(math.Round(fh)))
}

func chooseTargetFPS(infos []composeInfo) rational {
	if allNearFPS(infos, 24.0, 0.25) {
		if allExactRational(infos, 24000, 1001) {
			return rational{num: 24000, den: 1001}
		}
		return rational{num: 24, den: 1}
	}
	if allNearFPS(infos, 25.0, 0.2) {
		return rational{num: 25, den: 1}
	}
	return rational{num: 30, den: 1}
}

func allNearFPS(infos []composeInfo, nominal, tol float64) bool {
	for _, info := range infos {
		val := float64(info.fps.num) / float64(info.fps.den)
		if math.Abs(val-nominal) > tol {
			return false
		}
	}
	return true
}

func allExactRational(infos []composeInfo, num, den int) bool {
	for _, info := range infos {
		if info.fps.num != num || info.fps.den != den {
			return false
		}
	}
	return true
}

func buildVideoFilter(target composeTarget, info composeInfo) string {
	cover := (info.height > info.width) == (target.height > target.width) && nearAspectRatio(info.dar, target.baseAR)
	scaledW, scaledH := planScaledDimensions(target, info.dar, cover)
	steps := []string{
		fmt.Sprintf("scale=%d:%d:flags=lanczos", scaledW, scaledH),
		"setsar=1",
	}
	if scaledW != target.width || scaledH != target.height {
		if cover {
			steps = append(steps, fmt.Sprintf("crop=%d:%d", target.width, target.height))
		} else {
			steps = append(steps, fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2", target.width, target.height))
		}
	}
	steps = append(steps, "setpts=PTS-STARTPTS", fmt.Sprintf("fps=%d/%d", target.fps.num, target.fps.den), "format=yuv420p")
	return strings.Join(steps, ",")
}

func planScaledDimensions(target composeTarget, dar float64, cover bool) (int, int) {
	if dar <= 0 {
		return target.width, target.height
	}
	targetAR := float64(target.width) / float64(target.height)
	if cover {
		if dar >= targetAR {
			w := max(target.width, roundEven(int(math.Round(float64(target.height)*dar))))
			return w, target.height
		}
		h := max(target.height, roundEven(int(math.Round(float64(target.width)/dar))))
		return target.width, h
	}
	if dar >= targetAR {
		h := min(target.height, roundEven(int(math.Round(float64(target.width)/dar))))
		return target.width, h
	}
	w := min(target.width, roundEven(int(math.Round(float64(target.height)*dar))))
	return w, target.height
}

func buildAudioFilter(info composeInfo, needSamples int64) (filter string, useSilence bool) {
	if !info.audio {
		return fmt.Sprintf("atrim=0:end_sample=%d", needSamples), true
	}
	offset := info.aStart - info.vStart
	if offset >= info.duration {
		return fmt.Sprintf("atrim=0:end_sample=%d", needSamples), true
	}
	parts := []string{
		"asetpts=PTS-STARTPTS",
		fmt.Sprintf("aresample=%d", composeAudioRate),
		"aformat=sample_fmts=s16:channel_layouts=stereo",
	}
	if offset > composeSyncMinDelta {
		delayMs := int64(math.Round(offset * 1000.0))
		parts = append(parts, fmt.Sprintf("adelay=%d|%d", delayMs, delayMs))
	} else if offset < -composeSyncMinDelta {
		parts = append(parts, "atrim=start="+strconv.FormatFloat(-offset, 'f', 6, 64), "asetpts=PTS-STARTPTS")
	}
	parts = append(parts, fmt.Sprintf("apad=whole_len=%d", needSamples), fmt.Sprintf("atrim=0:end_sample=%d", needSamples))
	return strings.Join(parts, ","), false
}

func cumulativeSamplesDelta(cumFrames int64, fps rational, prevSamples *int64) int64 {
	targetTotal := int64(math.Round(float64(cumFrames) * float64(composeAudioRate) * float64(fps.den) / float64(fps.num)))
	delta := max(targetTotal-*prevSamples, 1)
	*prevSamples = targetTotal
	return delta
}

func parseStreamFPS(rRate, avgRate string) rational {
	for _, raw := range []string{rRate, avgRate} {
		num, den := parsePositiveRatio(raw, 0, 0)
		if num <= 0 || den <= 0 {
			continue
		}
		v := float64(num) / float64(den)
		if v >= composeMinFPS && v <= composeMaxFPS {
			return rational{num: num, den: den}
		}
	}
	return rational{num: 24, den: 1}
}

func parsePositiveRatio(raw string, defNum, defDen int) (int, int) {
	parts := strings.FieldsFunc(strings.TrimSpace(raw), func(r rune) bool { return r == ':' || r == '/' })
	if len(parts) != 2 {
		return defNum, defDen
	}
	num, err1 := strconv.Atoi(parts[0])
	den, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || num <= 0 || den <= 0 {
		return defNum, defDen
	}
	return num, den
}

func parseFiniteFloat(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func roundEven(n int) int {
	if n < 2 {
		return 2
	}
	if n%2 != 0 {
		return n + 1
	}
	return n
}
