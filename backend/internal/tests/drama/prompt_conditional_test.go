package drama_test

import (
	"strings"
	"testing"

	"video-canvas/internal/drama"
)

func TestPromptConditional_SilentReactionShot(t *testing.T) {
	sbPrompt, err := drama.Prompt("storyboard", map[string]any{
		"max_seconds": 10,
		"ratio":       "9:16",
		"style":       "真人都市电影",
		"script":      "林阳站在茶室窗边，静静看着屋外，沉默三秒。",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"仅当原稿剧情需要推进或动作衔接时直接从动作中段切入", "静默凝视", "首帧保持稳定静止", "2–3 秒", "横屏", "竖屏"} {
		if !strings.Contains(sbPrompt, want) {
			t.Fatalf("storyboard prompt missing conditional rule %q", want)
		}
	}

	source := "【画幅风格】9:16竖屏，真人都市电影。\n【场景】@[茶室](n_tea)\n【站位】@[林阳](n_lin)背靠窗边。\n【时间轴】0-5秒：近景、平视、固定镜头。@[林阳](n_lin)静默凝视窗外，肩线保持平稳，只在第3秒起眼睑微微一动，呼吸轻缓。声音：茶室内轻微钟摆声，无配乐。台词：无。"
	h3Prompt, err := drama.AdaptPrompt("h3", drama.AdaptInput{
		Source:  source,
		Seconds: 5,
		Ratio:   "9:16",
		Style:   "真人都市电影",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"仅当原稿该镜体现连续推进或动作衔接时切入动作中段", "静默凝视", "1–2 个清晰可控的核心动作节拍"} {
		if !strings.Contains(h3Prompt, want) {
			t.Fatalf("adapt_h3 prompt missing conditional motion rule %q", want)
		}
	}

	sbOut, err := drama.DecodeOutput("storyboard", []byte(`{"segments":[{"seconds":5,"prompt":"`+strings.ReplaceAll(source, "\n", `\n`)+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := sbOut.(drama.StoryboardOutput).ValidateMaxSeconds(10); err != nil {
		t.Fatalf("silent reaction storyboard should be valid: %v", err)
	}

	h3Out := "integrated_multimodal_description: [Shot 1] Live-action urban film close-up inside @[茶室](n_tea). @[林阳](n_lin) stands still beside the window in a static frame, keeping a calm posture with only a subtle blink and slow breath during a quiet pause.\n\noverall_soundscape: Soft interior clock ticking.\n\nnon_diegetic_music: No non-diegetic music."
	if _, err := drama.DecodeOutput("adapt", []byte(`{"prompt":`+jsonString(h3Out)+`}`)); err != nil {
		t.Fatal(err)
	}
	if err := drama.ValidateAdapt("h3", source, h3Out, 5); err != nil {
		t.Fatalf("silent reaction H3 adaptation should be valid: %v", err)
	}
}

func TestPromptConditional_ExplicitSourceMusic(t *testing.T) {
	sbPrompt, err := drama.Prompt("storyboard", map[string]any{"max_seconds": 10, "ratio": "16:9", "style": "国风三维"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"若原稿明确指定了背景音乐", "不删去原稿明确指定的配乐"} {
		if !strings.Contains(sbPrompt, want) {
			t.Fatalf("storyboard prompt missing conditional music rule %q", want)
		}
	}

	source := "【场景】@[垂钓台](n_scene)\n【角色】@[林阳](n_lin)\n【时间轴】0-5秒：中景，平视，缓推。@[林阳](n_lin)缓缓收起鱼竿。声音：溪水轻响，配乐自第1秒起进入低沉古筝独奏，情绪克制悠长。台词：无。"
	h3Prompt, err := drama.AdaptPrompt("h3", drama.AdaptInput{
		Source:  source,
		Seconds: 5,
		Ratio:   "16:9",
		Style:   "国风三维",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h3Prompt, "不得删去原稿已有的配乐需求") {
		t.Fatal("adapt_h3 prompt missing rule preserving explicit source music")
	}

	h3Out := "integrated_multimodal_description: [Shot 1] Chinese 3D CG medium shot at @[垂钓台](n_scene), slow push-in as @[林阳](n_lin) raises the fishing rod at a steady pace.\n\noverall_soundscape: Gentle stream water flowing over stones.\n\nnon_diegetic_music: Restrained solo guzheng enters at 00:01.000, carrying a slow, contemplative tone."
	if err := drama.ValidateAdapt("h3", source, h3Out, 5); err != nil {
		t.Fatalf("explicit music H3 adaptation should be valid: %v", err)
	}
}

func TestPromptConditional_DryIndoorDialogueAndAssetNeutrality(t *testing.T) {
	styles := drama.Styles()
	if len(styles.Genres) != 12 || len(styles.Styles) != 11 {
		t.Fatalf("expected 12 genres and 11 styles, got %d genres / %d styles", len(styles.Genres), len(styles.Styles))
	}
	styleByKey := map[string]string{}
	for _, s := range styles.Styles {
		styleByKey[s.Key] = s.Prompt
	}
	for _, key := range []string{"chinese_3d", "chinese_cyber_3d"} {
		p := styleByKey[key]
		if !strings.Contains(p, "骨相") || !strings.Contains(p, "侧逆光") {
			t.Fatalf("style %s missing anatomical proportion or directional lighting guidance: %s", key, p)
		}
		if strings.Contains(p, "湿发") {
			t.Fatalf("style %s must not demand wet hair: %s", key, p)
		}
	}
	if !strings.Contains(styleByKey["chinese_cyber_3d"], "仅在雨夜") {
		t.Fatalf("chinese_cyber_3d water reflection should be conditional, got %s", styleByKey["chinese_cyber_3d"])
	}

	assetPrompt, err := drama.Prompt("asset_prompts", map[string]any{"style": "国风三维"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"中性自然站姿", "基础正面照明", "不用戏剧强逆光", "画面中不出现人物", "不出现人物、手或肢体"} {
		if !strings.Contains(assetPrompt, want) {
			t.Fatalf("asset_prompts missing turnaround neutrality rule %q", want)
		}
	}

	source := "【场景】@[茶室](n_tea)\n【角色】@[林阳](n_lin)\n【时间轴】0-5秒：中近景。干燥晴朗的室内午后，@[林阳](n_lin)干爽整洁地坐在桌前轻声开口。林阳（平静）：「今天先谈到这里。」"
	h3Prompt, err := drama.AdaptPrompt("h3", drama.AdaptInput{Source: source, Seconds: 5, Ratio: "16:9", Style: "国风三维"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h3Prompt, "干燥室内常规场景不凭空加入湿发") {
		t.Fatal("adapt_h3 missing dry indoor guard against unprompted wet hair")
	}

	h3Out := "integrated_multimodal_description: [Shot 1] Chinese 3D CG medium close-up in @[茶室](n_tea) on a dry sunny afternoon. @[林阳](n_lin) sits neatly at a wooden table with clean dry hair and matte fabric robes. The young man with a calm voice (S1) says: <d>[Chinese] 今天先谈到这里。</d>\n\noverall_soundscape: Quiet indoor room tone.\n\nnon_diegetic_music: No non-diegetic music."
	if strings.Contains(strings.ToLower(h3Out), "wet hair") || strings.Contains(strings.ToLower(h3Out), "pores") {
		t.Fatal("dry indoor dialogue fixture must not contain wet hair or pores")
	}
	if err := drama.ValidateAdapt("h3", source, h3Out, 5); err != nil {
		t.Fatalf("dry indoor H3 adaptation should be valid: %v", err)
	}
}

func TestPromptConditional_NonPhotorealistic2DStyle(t *testing.T) {
	assetPrompt, err := drama.Prompt("asset_prompts", map[string]any{"style": "日系二次元"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(assetPrompt, "符合当前 `style` 的头身与骨相解剖结构") {
		t.Fatal("asset_prompts should scope anatomical structure cues to active style")
	}

	source := "【画幅风格】16:9，日系二次元赛璐珞。\n【场景】@[教室](n_room)\n【角色】@[小川](n_chuan)\n【时间轴】0-4秒：近景、固定镜头。@[小川](n_chuan)抬头望向黑板。声音：粉笔轻微摩擦声，无配乐。台词：无。"
	h3Prompt, err := drama.AdaptPrompt("h3", drama.AdaptInput{
		Source:  source,
		Seconds: 4,
		Ratio:   "16:9",
		Style:   "日系二次元",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h3Prompt, "非写实/2D赛璐珞画风只写线条、色块与平涂光影，不硬加写实皮肤毛孔") {
		t.Fatal("adapt_h3 missing guard against forcing 3D skin pores onto 2D styles")
	}

	h3Out := "integrated_multimodal_description: [Shot 1] 2D Japanese cel animation close-up inside @[教室](n_room) with crisp linework and clean cel-shaded flat tones. @[小川](n_chuan) looks up toward the chalkboard.\n\noverall_soundscape: Faint chalk scrape on the board.\n\nnon_diegetic_music: No non-diegetic music."
	if strings.Contains(strings.ToLower(h3Out), "skin pores") || strings.Contains(strings.ToLower(h3Out), "subsurface") {
		t.Fatal("2D style output must not force 3D skin micro-texture")
	}
	if err := drama.ValidateAdapt("h3", source, h3Out, 4); err != nil {
		t.Fatalf("2D style H3 adaptation should be valid: %v", err)
	}
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}
