package videoframe

import "fmt"

// NormalizeManifestClips 将 EOF 明确为排他出点并校验源边界，供查询、预览与合成共用。
func NormalizeManifestClips(clips []ManifestClip) ([]ManifestClip, int32, error) {
	out := make([]ManifestClip, 0, len(clips))
	var total int64
	if len(clips) == 0 || len(clips) > 200 {
		return nil, 0, ErrInvalidManifest
	}
	for _, clip := range clips {
		end := clip.NormalizedFrames24
		if clip.SourceOutF != nil {
			end = *clip.SourceOutF
		}
		if clip.SourceInF < 0 || end <= clip.SourceInF || end > clip.NormalizedFrames24 || !validGainDB(clip.AudioGainDB) || clip.AvPtsOffsetMs < -10000 || clip.AvPtsOffsetMs > 10000 {
			return nil, 0, fmt.Errorf("%w: 片段 %d 的剪辑区间或音轨设置非法", ErrInvalidManifest, clip.Seq)
		}
		total += int64(end - clip.SourceInF)
		if total > 24*600 {
			return nil, 0, fmt.Errorf("%w: 总片长超过十分钟", ErrInvalidManifest)
		}
		clip.SourceOutF = &end
		out = append(out, clip)
	}
	return out, int32(total), nil
}
