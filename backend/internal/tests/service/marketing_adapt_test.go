package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/handler"

	"video-canvas/internal/llmgateway"
	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/service"
	"video-canvas/internal/service/stepfake"
)

const mktH3Key = "video-h3"

// useH3 登记一个提示词格式为 h3 的视频模型（其余能力同 video-a），并让转写调用按提示词现拼合格回复。
func (e *mktEnv) useH3() {
	snap := mktVideoSnapshot()
	snap.Model.Key = mktH3Key
	snap.Model.Capabilities.PromptFormat = "h3"
	e.models.Snapshots[mktH3Key] = snap
	e.caller.byText[mktAdaptStep] = func(prompt string) (string, error) { return h3Reply(prompt), nil }
}

// h3Scripts 建一个用 H3 出片的无参考项目，跑完第一轮，返回项目 id 和脚本。
func (e *mktEnv) h3Scripts() (idcodec.ID, []model.MarketingScriptView) {
	e.t.Helper()
	st := mktSettings()
	st.VideoModel = mktH3Key
	v := e.createProject(st)
	e.runRound(&v.Rounds[0])
	return v.ID, e.round(v.ID, v.Rounds[0].ID).Scripts
}

// adaptVars 取出转写提示词末尾的输入 JSON。
func adaptVars(prompt string) (refs []struct{ Label string }, shots []struct {
	No        int
	Cut       string
	Narration string
}) {
	i := strings.LastIndex(prompt, "```json\n")
	j := strings.LastIndex(prompt, "\n```")
	var vars struct {
		References []struct{ Label string } `json:"references"`
		Shots      []struct {
			No        int    `json:"no"`
			Cut       string `json:"cut"`
			Narration string `json:"narration"`
		} `json:"shots"`
	}
	_ = json.Unmarshal([]byte(prompt[i+len("```json\n"):j]), &vars)
	for _, s := range vars.Shots {
		shots = append(shots, struct {
			No        int
			Cut       string
			Narration string
		}{s.No, s.Cut, s.Narration})
	}
	return vars.References, shots
}

// h3Reply 按转写输入拼一份合格的 H3 六段稿：标签全用上，镜头与切点、旁白原文照搬。
func h3Reply(prompt string) string {
	refs, shots := adaptVars(prompt)
	labels := make([]string, 0, len(refs))
	for _, r := range refs {
		labels = append(labels, r.Label)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "subject_definitions:\n<Subject 1> is the product in %s.\n\n", strings.Join(labels, ", "))
	b.WriteString("summary:\n[reference generation] A product video of <Subject 1>.\n\n")
	b.WriteString("retention_analysis:\n<Subject 1> (appears in [Shot 1]): fully_preserved - shape retained.\n\n")
	b.WriteString("detailed_description:\nA realistic vertical product video.\n")
	for _, s := range shots {
		if s.Cut == "" {
			fmt.Fprintf(&b, "[Shot %d] A close-up of <Subject 1>. The narrator (S1) says: <d>[Chinese] %s</d>\n", s.No, s.Narration)
		} else {
			fmt.Fprintf(&b, "[Shot %d] At %s, the shot cuts to <Subject 1>. The narrator (S1) says: <d>[Chinese] %s</d>\n", s.No, s.Cut, s.Narration)
		}
	}
	b.WriteString("\noverall_soundscape:\nQuiet room tone.\n\nnon_diegetic_music:\nLight piano.")
	raw, _ := json.Marshal(map[string]string{"prompt": b.String()})
	return string(raw)
}

// adaptLedger 数脚本的转写扣费流水。
func (e *mktEnv) adaptLedger(sc idcodec.ID) []model.CreditLedger {
	e.t.Helper()
	var rows []model.CreditLedger
	if err := e.db.Where("marketing_script_id = ?", uint64(sc)).Order("id").Find(&rows).Error; err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func TestMarketing_RenderH3_AdaptsOnceAndUsesAdaptation(t *testing.T) {
	env := newMktEnv(t, 300)
	env.useH3()
	_, scripts := env.h3Scripts()
	ctx := context.Background()
	sc := scripts[0]
	if sc.RenderCredits <= 20 {
		t.Fatalf("还没转写时，出片报价应含转写预估：%d", sc.RenderCredits)
	}
	before := env.credit().Balance
	got, err := env.render(sc.ID, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if n := env.caller.count(mktAdaptStep); n != 1 {
		t.Fatalf("应转写一次：%d", n)
	}
	var input map[string]any
	_ = json.Unmarshal(env.scriptTasks(sc.ID)[0].InputJSON, &input)
	prompt, _ := input["prompt"].(string)
	if !strings.HasPrefix(prompt, "subject_definitions:") || !strings.Contains(prompt, "<Picture 1>") || strings.Contains(prompt, "@图片") {
		t.Fatalf("出片应使用 H3 转写稿：%s", prompt)
	}
	ledger := env.adaptLedger(sc.ID)
	if len(ledger) != 2 || ledger[0].Type != model.LedgerFreeze || ledger[1].Type != model.LedgerSettle {
		t.Fatalf("转写应成对写冻结与结算流水：%+v", ledger)
	}
	if env.credit().Balance >= before {
		t.Fatalf("转写和出片都应扣费：%d → %d", before, env.credit().Balance)
	}
	if got.RenderCredits != 20 {
		t.Fatalf("已有有效转写稿，报价只剩出片费：%d", got.RenderCredits)
	}

	// 预览：h3 返回原稿（Seedance 写法）和未过期的转写稿。
	pv, err := env.svc.Prompt(ctx, env.user, uint64(sc.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if pv.Format != "h3" || !strings.Contains(pv.Prompt, "[分镜]") || pv.Adaptation == nil || pv.Adaptation.Stale || pv.Adaptation.Prompt != prompt {
		t.Fatalf("预览不对：%+v", pv)
	}
}

func TestMarketing_H3Adaptation_StaleAfterNarrationEditAndEditable(t *testing.T) {
	env := newMktEnv(t, 300)
	env.useH3()
	_, scripts := env.h3Scripts()
	ctx := context.Background()
	sc := scripts[0]
	if _, err := env.svc.Adapt(ctx, env.user, uint64(sc.ID), &model.AdaptMarketingScriptReq{ExpectedVersion: env.promptVersion(sc.ID, true)}); err != nil {
		t.Fatal(err)
	}
	// 手改转写稿：标记已修改，仍未过期。
	pv, err := env.svc.EditAdaptation(ctx, env.user, uint64(sc.ID), &model.EditMarketingAdaptationReq{ExpectedVersion: env.promptVersion(sc.ID, true), Format: "h3", Prompt: "手改过的稿"})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Adaptation == nil || !pv.Adaptation.Edited || pv.Adaptation.Stale || pv.Adaptation.Prompt != "手改过的稿" {
		t.Fatalf("手改：%+v", pv.Adaptation)
	}
	// 改旁白：原稿变了，转写稿过期。
	narr := make([]string, len(sc.Shots))
	for i, s := range sc.Shots {
		narr[i] = s.Narration
	}
	narr[0] = "换一句开场白"
	if _, err := env.svc.UpdateScript(ctx, env.user, uint64(sc.ID), &model.UpdateMarketingScriptReq{Narrations: narr, Revision: sc.Revision}); err != nil {
		t.Fatal(err)
	}
	pv, err = env.svc.Prompt(ctx, env.user, uint64(sc.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if pv.Adaptation == nil || !pv.Adaptation.Stale {
		t.Fatalf("改了旁白，转写稿应过期：%+v", pv.Adaptation)
	}
	// 出片时自动重新转写，用的是新旁白。
	if _, err := env.render(sc.ID, "k2"); err != nil {
		t.Fatal(err)
	}
	if n := env.caller.count(mktAdaptStep); n != 2 {
		t.Fatalf("过期后出片应重新转写：%d", n)
	}
	var input map[string]any
	_ = json.Unmarshal(env.scriptTasks(sc.ID)[0].InputJSON, &input)
	if !strings.Contains(input["prompt"].(string), "换一句开场白") {
		t.Fatalf("应用新旁白重新转写：%v", input["prompt"])
	}
}

func TestMarketing_H3Adaptation_FailureChargesNothing(t *testing.T) {
	env := newMktEnv(t, 300)
	env.useH3()
	env.caller.byText[mktAdaptStep] = func(string) (string, error) { return `{"prompt":"随便写写"}`, nil }
	_, scripts := env.h3Scripts()
	sc := scripts[0]
	_, err := env.render(sc.ID, "k1")
	var biz *errcode.Error
	if !errors.As(err, &biz) || biz.Code != errcode.ErrMarketingAdaptFailed.Code {
		t.Fatalf("两次都不合格应报转写失败：%v", err)
	}
	if n := env.caller.count(mktAdaptStep); n != 2 {
		t.Fatalf("不合格应带反馈重写一次：%d", n)
	}
	if len(env.adaptLedger(sc.ID)) != 0 || len(env.scriptTasks(sc.ID)) != 0 {
		t.Fatal("转写失败不扣费、不出片")
	}
}

func TestMarketing_H3Adaptation_UpstreamErrorIsShown(t *testing.T) {
	env := newMktEnv(t, 300)
	env.useH3()
	env.caller.byText[mktAdaptStep] = func(string) (string, error) {
		return "", &llmgateway.UpstreamError{Status: 429, Message: "You exceeded your current quota", Retryable: true}
	}
	_, scripts := env.h3Scripts()
	_, err := env.render(scripts[0].ID, "k1")
	var biz *errcode.Error
	if !errors.As(err, &biz) || biz.Code != errcode.ErrMarketingAdaptFailed.Code || !strings.Contains(biz.Msg, "You exceeded your current quota") {
		t.Fatalf("转写失败应带出上游原文：%v", err)
	}
}

func TestMarketing_RenderRoundH3_AdaptsAllInParallel(t *testing.T) {
	env := newMktEnv(t, 300)
	env.useH3()
	var mu sync.Mutex
	seen := map[string]bool{}
	env.caller.byText[mktAdaptStep] = func(prompt string) (string, error) {
		mu.Lock()
		seen[prompt] = true
		mu.Unlock()
		return h3Reply(prompt), nil
	}
	_, scripts := env.h3Scripts()
	r, err := env.svc.RenderRound(context.Background(), env.user, uint64(scripts[0].RoundID), m2RoundReq("all", scripts))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Skipped) != 0 || len(seen) != len(scripts) {
		t.Fatalf("每份脚本各转写一次并出片：skipped=%+v adapted=%d scripts=%d", r.Skipped, len(seen), len(scripts))
	}
	for _, sc := range scripts {
		if len(env.scriptTasks(sc.ID)) != 1 {
			t.Fatalf("脚本 %v 应出片", sc.ID)
		}
	}
}

// m2AdaptMemory 覆盖同步HTTP的版本与在途保护，不依赖数据库端口；锁竞争另见PG矩阵。
func m2AdaptMemory(t *testing.T) (*v21Memory, *mktEnv) {
	t.Helper()
	m := newV21Memory()
	m.p.ProductAssetIDs = []byte(`[1]`)
	m.p.Product = []byte(`{"name":"水杯","anchors":["透明杯身"]}`)
	m.r.Status, m.sc.Status, m.sc.Kind = "ready", "draft", marketing.KindCreate
	st := mktSettings()
	st.VideoModel = mktH3Key
	st.TextModel = "text-a"
	m.r.Settings = m2Raw(st)
	m.sc.Revision = 3
	m.sc.Shots = m2Raw([]marketing.ScriptShot{{Start: 0, End: 15, Scene: "展示水杯", Narration: "随身喝水"}})
	m.account.Frozen = 0
	e := &mktEnv{t: t, user: 7, models: stepfake.NewModels(), caller: newMktCaller()}
	e.useH3()
	e.svc = service.NewMarketingService(service.MarketingDeps{Repo: m, Models: e.models, Caller: e.caller, Credits: stepfake.Policy{Initial: 100}})
	return m, e
}

func TestMarketingV21_ForceVersionCASAndPromptVersions(t *testing.T) {
	m, e := m2AdaptMemory(t)
	ctx := context.Background()
	wrong := int64(7)
	_, err := e.svc.Adapt(ctx, 7, m.sc.ID, &model.AdaptMarketingScriptReq{ExpectedVersion: &wrong})
	wantMktCode(t, err, errcode.ErrMarketingConflict.Code)
	if e.caller.count(mktAdaptStep) != 0 || m.account.Balance != 100 {
		t.Fatal("错版本不得调用或收费")
	}
	zero := int64(0)
	pv, err := e.svc.Adapt(ctx, 7, m.sc.ID, &model.AdaptMarketingScriptReq{ExpectedVersion: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Adaptation == nil || pv.Adaptation.Version != 1 || pv.Version != 3 || pv.Adaptation.InputHash == "" {
		t.Fatalf("版本与指纹：%+v", pv)
	}
	_, err = e.svc.Adapt(ctx, 7, m.sc.ID, &model.AdaptMarketingScriptReq{ExpectedVersion: &zero})
	wantMktCode(t, err, errcode.ErrMarketingConflict.Code)
	if e.caller.count(mktAdaptStep) != 1 || m.account.Balance != 98 || len(m.ledgers) != 2 {
		t.Fatal("旧凭据重试不得再次调用或收费")
	}
	wrong = 2
	_, err = e.svc.EditPrompt(ctx, 7, m.sc.ID, &model.EditMarketingPromptReq{Format: "h3", Prompt: "陈旧编辑", ExpectedVersion: &wrong})
	wantMktCode(t, err, errcode.ErrMarketingConflict.Code)
	version := pv.Version
	pv, err = e.svc.EditPrompt(ctx, 7, m.sc.ID, &model.EditMarketingPromptReq{Format: "h3", Prompt: "手改原稿", ExpectedVersion: &version})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Version != 4 || !pv.Edited || !pv.Adaptation.Stale {
		t.Fatalf("原稿编辑必须推进内容版本并失效转写：%+v", pv)
	}
	av := pv.Adaptation.Version
	pv, err = e.svc.EditAdaptation(ctx, 7, m.sc.ID, &model.EditMarketingAdaptationReq{Format: "h3", Prompt: "用户手改稿", ExpectedVersion: &av})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Adaptation.Version != 2 {
		t.Fatal("手改转写应推进独立版本")
	}
	_, err = e.svc.EditAdaptation(ctx, 7, m.sc.ID, &model.EditMarketingAdaptationReq{Format: "h3", Prompt: "旧窗口覆盖", ExpectedVersion: &av})
	wantMktCode(t, err, errcode.ErrMarketingConflict.Code)
}

func TestMarketingV21_AdaptInputChangesWhenOnlyImagesChange(t *testing.T) {
	m, e := m2AdaptMemory(t)
	ctx := context.Background()
	version := m.sc.Revision
	_, err := e.svc.EditPrompt(ctx, 7, m.sc.ID, &model.EditMarketingPromptReq{Format: "h3", Prompt: "固定手改原稿", ExpectedVersion: &version})
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	before, err := e.svc.Adapt(ctx, 7, m.sc.ID, &model.AdaptMarketingScriptReq{ExpectedVersion: &zero})
	if err != nil {
		t.Fatal(err)
	}
	m.p.ProductAssetIDs = []byte(`[2]`)
	after, err := e.svc.Prompt(ctx, 7, m.sc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Prompt != before.Prompt || after.InputHash == before.InputHash || !after.Adaptation.Stale {
		t.Fatal("同数量替换商品图也必须改变实际输入指纹并标过期")
	}
	av := after.Adaptation.Version
	after, err = e.svc.Adapt(ctx, 7, m.sc.ID, &model.AdaptMarketingScriptReq{ExpectedVersion: &av})
	if err != nil {
		t.Fatal(err)
	}
	if after.Adaptation.Stale || after.Adaptation.Version != 2 || e.caller.count(mktAdaptStep) != 2 {
		t.Fatal("新输入应重新转写并推进版本")
	}
}

func TestMarketingV21_AdaptInFlightRejectsChangedContext(t *testing.T) {
	for _, change := range []string{"original", "narration", "settings", "generation", "cancel", "deleted", "adaptation"} {
		t.Run(change, func(t *testing.T) {
			m, e := m2AdaptMemory(t)
			e.caller.byText[mktAdaptStep] = func(prompt string) (string, error) {
				switch change {
				case "original":
					m.sc.PromptEdits = []byte(`{"h3":{"prompt":"刚保存的手改"}}`)
					m.sc.Revision++
				case "narration":
					m.sc.Shots = []byte(`[]`)
					m.sc.Revision++
				case "settings":
					m.r.Settings = []byte(`{"video_model":"video-h3","params":{"duration":10}}`)
				case "generation":
					m.r.ExecutionGen++
				case "cancel":
					m.r.Status = model.MarketingRoundCanceled
				case "deleted":
					m.p.DeletedAt.Valid = true
				case "adaptation":
					m.sc.Adaptations = []byte(`{"h3":{"prompt":"用户刚写的稿","version":1}}`)
				}
				return h3Reply(prompt), nil
			}
			zero := int64(0)
			_, err := e.svc.Adapt(context.Background(), 7, m.sc.ID, &model.AdaptMarketingScriptReq{ExpectedVersion: &zero})
			code := errcode.ErrMarketingConflict.Code
			if change == "deleted" {
				code = errcode.ErrMarketingNotFound.Code
			}
			wantMktCode(t, err, code)
			if m.account.Balance != 100 || len(m.ledgers) != 0 || m.writes != 0 {
				t.Fatal("在途失效不得覆盖或扣费")
			}
			if change == "adaptation" && !strings.Contains(string(m.sc.Adaptations), "用户刚写的稿") {
				t.Fatal("用户稿被覆盖")
			}
		})
	}
}

func m2Raw(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestMarketingV21_HTTPVersionCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, header, body string
		ok                 bool
	}{
		{"quoted", `"0"`, `{}`, true},
		{"plain", "0", `{}`, true},
		{"json", "", `{"expected_version":0}`, true},
		{"both", `"0"`, `{"expected_version":0}`, true},
		{"missing", "", `{}`, false},
		{"mismatch", `"1"`, `{"expected_version":0}`, false},
		{"single_quote", `"`, `{}`, false},
		{"double_quote", `""0""`, `{}`, false},
		{"weak", `W/"0"`, `{}`, false},
		{"multiple", `"0", "1"`, `{}`, false},
		{"negative", "-1", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, e := m2AdaptMemory(t)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("user_id", uint(7))
			c.Params = gin.Params{{Key: "id", Value: idcodec.Encode(m.sc.ID)}}
			c.Request = httptest.NewRequest("POST", "/marketing-scripts/4/adapt", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				c.Request.Header.Set("If-Match", tc.header)
			}
			handler.NewMarketingHandler(e.svc).Adapt(c)
			wantStatus, calls := 409, 0
			if tc.ok {
				wantStatus, calls = 200, 1
			}
			var body struct{ Code int }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != wantStatus || e.caller.count(mktAdaptStep) != calls {
				t.Fatalf("status/calls=%d/%d, body=%s", w.Code, e.caller.count(mktAdaptStep), w.Body.String())
			}
			if !tc.ok && (body.Code != 68009 || m.account.Balance != 100 || len(m.ledgers) != 0) {
				t.Fatal("非法CAS凭据调用了模型或扣费")
			}
		})
	}
}

func TestMarketing_RenderH3_RejectsBusyAndRewritingBeforeLLM(t *testing.T) {
	env := newMktEnv(t, 300)
	env.useH3()
	_, scripts := env.h3Scripts()
	// 1. 重写中：未调用转写模型即以 68006 拒绝。
	if err := env.db.Model(&model.MarketingScript{}).Where("id = ?", uint64(scripts[0].ID)).Update("status", model.MarketingScriptRewriting).Error; err != nil {
		t.Fatal(err)
	}
	_, err := env.render(scripts[0].ID, "rewriting-key")
	wantMktCode(t, err, errcode.ErrMarketingScriptBusy.Code)
	if env.caller.count(mktAdaptStep) != 0 {
		t.Fatalf("重写中不应调用转写模型：%d", env.caller.count(mktAdaptStep))
	}

	// 2. 已有在途出片任务：改用新幂等键再次出片必须在转写模型前返回 68006，旧键重放仍幂等。
	if _, err := env.render(scripts[1].ID, "first-render"); err != nil {
		t.Fatal(err)
	}
	if calls := env.caller.count(mktAdaptStep); calls != 1 {
		t.Fatalf("首次出片应转写一次：%d", calls)
	}
	tasks := env.scriptTasks(scripts[1].ID)
	if len(tasks) != 1 || len(tasks[0].RequestIntentSig) == 0 || len(tasks[0].RequestIntentSig) > 64 || !strings.HasPrefix(tasks[0].RequestIntentSig, "mkt:v1:") {
		t.Fatalf("营销请求签名格式/长度异常：%q", tasks[0].RequestIntentSig)
	}
	// 先置为过期以迫使未拦截请求一定会重新调 LLM。
	if err := env.db.Model(&model.MarketingScript{}).Where("id = ?", uint64(scripts[1].ID)).Update("adaptations", []byte(`{}`)).Error; err != nil {
		t.Fatal(err)
	}
	_, err = env.render(scripts[1].ID, "second-render")
	wantMktCode(t, err, errcode.ErrMarketingScriptBusy.Code)
	if calls := env.caller.count(mktAdaptStep); calls != 1 {
		t.Fatalf("已有在途任务时新幂等键不得调用转写模型：%d", calls)
	}
}
