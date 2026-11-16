package service

import (
	"strings"

	"video-canvas/internal/provider"
)

func resolvePluginRawSize(snap *provider.Snapshot, effective map[string]any) string {
	if s, _ := effective["size"].(string); s != "" {
		if m := sizePattern.FindStringSubmatch(strings.TrimSpace(s)); len(m) == 3 {
			return m[1] + "x" + m[2]
		}
	}
	if snap == nil {
		return ""
	}
	ratio, _ := effective["aspect_ratio"].(string)
	res := strings.ToLower(strings.TrimSpace(asString(effective["resolution"])))
	switch snap.Plugin.Key {
	case "sora":
		high := res == "1080p" || res == "high"
		if ratio == "16:9" {
			if high {
				return "1792x1024"
			}
			return "1280x720"
		}
		if ratio == "9:16" {
			if high {
				return "1024x1792"
			}
			return "720x1280"
		}
	case "alibaba-video":
		return resolveAlibabaLegacySize(snap.Model.UpstreamModel, ratio, strings.ToUpper(res))
	case "colab-gpu-studio":
		return resolveColabStudioSize(snap, effective)
	}
	return ""
}

var colabRef2vDims = map[string][2]string{
	"720P":  {"864", "480"},
	"1080P": {"960", "544"},
	"4K":    {"1024", "576"},
}

var colabStandardSizes = map[string][2]string{
	"16:9": {"864x480", "1024x576"},
	"9:16": {"480x864", "576x1024"},
	"Auto": {"480x864", "576x1024"},
	"4:3":  {"768x576", "768x576"},
	"3:4":  {"576x768", "576x768"},
	"1:1":  {"640x640", "768x768"},
	"21:9": {"1024x448", "1024x448"},
}

func resolveColabStudioSize(snap *provider.Snapshot, effective map[string]any) string {
	ver := strings.TrimSpace(snap.Plugin.Version)
	if ver != "" && !strings.HasPrefix(ver, "1.") {
		return ""
	}
	upstream := strings.TrimSpace(snap.Model.UpstreamModel)
	if upstream == "" {
		upstream = "minimax-h3-i2v"
	}
	ar := strings.TrimSpace(asString(effective["aspect_ratio"]))
	if ar == "" {
		ar = "Auto"
	}
	tier := strings.ToUpper(strings.TrimSpace(asString(effective["resolution"])))
	if tier == "" {
		tier = resolveColabDefaultTier(snap.Channel.Settings)
		if tier == "" {
			return ""
		}
	}
	if strings.Contains(upstream, "ref2v") && (ar == "16:9" || ar == "9:16" || ar == "Auto") {
		dims, ok := colabRef2vDims[tier]
		if !ok {
			return ""
		}
		if ar == "16:9" {
			return dims[0] + "x" + dims[1]
		}
		return dims[1] + "x" + dims[0]
	}
	pair, ok := colabStandardSizes[ar]
	if !ok {
		return "864x480"
	}
	if tier == "1080P" || tier == "4K" {
		return pair[1]
	}
	return pair[0]
}

func resolveColabDefaultTier(settings map[string]any) string {
	if settings == nil {
		return "720P"
	}
	raw, exists := settings["default_resolution"]
	if !exists || raw == nil {
		return "720P"
	}
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	if s == "" {
		return "720P"
	}
	switch s {
	case "720P", "1080P", "4K":
		return s
	default:
		return ""
	}
}

func resolveAlibabaLegacySize(upstream, ratio, resUpper string) string {
	if !strings.Contains(upstream, "t2v") || (!strings.Contains(upstream, "2.6") && !strings.Contains(upstream, "2.5") && !strings.Contains(upstream, "2.2") && !strings.Contains(upstream, "2.1")) {
		return ""
	}
	if ratio == "" {
		ratio = "16:9"
	}
	if resUpper == "" {
		resUpper = "1080P"
	}
	table := map[string]map[string]string{
		"480P":  {"16:9": "832x480", "9:16": "480x832", "1:1": "624x624"},
		"720P":  {"16:9": "1280x720", "9:16": "720x1280", "1:1": "960x960", "4:3": "1088x832", "3:4": "832x1088"},
		"1080P": {"16:9": "1920x1080", "9:16": "1080x1920", "1:1": "1440x1440", "4:3": "1632x1248", "3:4": "1248x1632"},
	}
	return table[resUpper][ratio]
}
