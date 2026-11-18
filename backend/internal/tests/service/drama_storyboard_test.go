package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

const authoredStoryboardProgram = `{"v":1,"audio":[{"id":"voice","role":"narration","asset_id":30,"start_f":0,"end_f":12}],"overlays":[]}`

const storyboardValid = `{"segments":[{"seconds":15,"prompt":"@[村口](n_scene) @[陌生人](unknown)"}]}`

func storyboardRequest(t *testing.T, maxSeconds int, episodes []int) *model.DramaJobReq {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"step": "storyboard", "episodes": episodes, "max_seconds": maxSeconds})
	var req model.DramaJobReq
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	return &req
}

func storyboardReady(r *dramaMemory) {
	dramaAddEpisode(r, 1, "女子进入村口，与陌生人争执。")
	now := time.Now()
	r.episodes[0].LockedAt = &now
	r.project.Ratio = "9:16"
}

func TestDramaStoryboardPipeline(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		outputs              []string
		status               string
		code, calls, balance int
	}{
		{"成功", []string{storyboardValid}, "succeeded", 0, 1, 995},
		{"非法JSON重试", []string{`{bad`, storyboardValid}, "succeeded", 0, 2, 995},
		{"连续非法退款", []string{`{bad`, `{bad`}, "failed", 67008, 2, 1000},
		{"时长超限退款", []string{strings.ReplaceAll(storyboardValid, `"seconds":15`, `"seconds":16`), strings.ReplaceAll(storyboardValid, `"seconds":15`, `"seconds":16`)}, "failed", 67008, 2, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDramaMemory()
			storyboardReady(r)
			e := &dramaTextStreamer{outputs: tc.outputs}
			s, _ := dramaTestService(r, e, nil)
			job, err := s.CreateJob(context.Background(), 7, 1, storyboardRequest(t, 0, []int{1}))
			if err != nil {
				t.Fatal(err)
			}
			if r.episodes[0].StoryboardStatus != "generating" {
				t.Fatalf("queued status=%s", r.episodes[0].StoryboardStatus)
			}
			if err = s.HandleDrama(context.Background(), 7, 1, uint64(job.ID)); err != nil {
				t.Fatal(err)
			}
			if r.jobs[0].Status != tc.status || r.jobs[0].ErrorCode != tc.code || e.calls != tc.calls {
				t.Fatalf("job=%+v calls=%d", r.jobs[0], e.calls)
			}
			if r.credit.Balance != tc.balance || r.credit.Frozen != 0 {
				t.Fatalf("credit=%+v", r.credit)
			}
			wantStatus := "failed"
			if tc.status == "succeeded" {
				wantStatus = "ready"
			}
			if r.episodes[0].StoryboardStatus != wantStatus {
				t.Fatalf("episode=%+v", r.episodes[0])
			}
			if tc.status == "succeeded" {
				if len(r.segments) != 1 || r.segments[0].Seconds != 15 || r.segments[0].Seq != 0 || r.segments[0].Revision != 1 {
					t.Fatalf("segments=%+v", r.segments)
				}
				if strings.Contains(r.segments[0].Prompt, "unknown") || !strings.Contains(r.segments[0].Prompt, "@[村口](n_scene)") {
					t.Fatalf("saved prompt=%s", r.segments[0].Prompt)
				}
				if strings.Contains(e.prompts[0], "n_other") || strings.Contains(e.prompts[0], "不应输入") {
					t.Fatal("混入其他集资产")
				}
				if strings.Contains(e.prompts[0], "n_orphan") {
					t.Fatal("混入未分类资产（出现集数为 null）")
				}
				if !strings.Contains(string(r.jobs[0].Output), "陌生人") || strings.Contains(string(r.jobs[0].Output), "unknown") {
					t.Fatalf("引用未降级: %s", r.jobs[0].Output)
				}
				for _, required := range []string{"n_scene", "村口", "9:16", "写实", "max_seconds"} {
					if !strings.Contains(e.prompts[0], required) {
						t.Fatalf("输入缺少%s: %s", required, e.prompts[0])
					}
				}
			}
		})
	}
}

func TestDramaStoryboardPrerequisitesAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name          string
		max           int
		scope         []int
		locked, ready bool
		code          int
	}{
		{"未拆解", 15, []int{1}, false, true, 67006},
		{"剧本未就绪", 15, []int{1}, true, false, 67006},
		{"未指定单集", 15, nil, true, true, 10001},
		{"多集", 15, []int{1, 2}, true, true, 10001},
		{"低于上限边界", 3, []int{1}, true, true, 10001},
		{"超过上限边界", 61, []int{1}, true, true, 10001},
		{"默认上限", 0, []int{1}, true, true, 0},
		{"最小上限", 4, []int{1}, true, true, 0},
		{"最大上限", 60, []int{1}, true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDramaMemory()
			storyboardReady(r)
			if !tc.locked {
				r.episodes[0].LockedAt = nil
			}
			if !tc.ready {
				r.episodes[0].ScriptStatus = "empty"
			}
			s, _ := dramaTestService(r, &dramaTextStreamer{}, nil)
			req := storyboardRequest(t, tc.max, tc.scope)
			_, err := s.Estimate(context.Background(), 7, 1, req)
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				dramaCheckCode(t, err, tc.code)
			}
			_, err = s.CreateJob(context.Background(), 7, 1, req)
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				dramaCheckCode(t, err, tc.code)
			}
		})
	}
}

func TestDramaStoryboardHasTakes(t *testing.T) {
	t.Run("估价及提交", func(t *testing.T) {
		r := newDramaMemory()
		storyboardReady(r)
		r.hasTakes = true
		s, _ := dramaTestService(r, &dramaTextStreamer{}, nil)
		req := storyboardRequest(t, 15, []int{1})
		_, err := s.Estimate(context.Background(), 7, 1, req)
		dramaCheckCode(t, err, 67013)
		_, err = s.CreateJob(context.Background(), 7, 1, req)
		dramaCheckCode(t, err, 67013)
		if len(r.jobs) != 0 || r.credit.Frozen != 0 {
			t.Fatalf("拒绝仍冻结: %+v", r.credit)
		}
	})
	t.Run("模型执行期间新建take", func(t *testing.T) {
		r := newDramaMemory()
		storyboardReady(r)
		r.segments = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 99}, UserID: 7, EpisodeID: r.episodes[0].ID, Prompt: "旧片段", Seconds: 5, Revision: 1}}
		e := &dramaTextStreamer{outputs: []string{storyboardValid}, onCall: func() { r.hasTakes = true }}
		s, _ := dramaTestService(r, e, nil)
		j, err := s.CreateJob(context.Background(), 7, 1, storyboardRequest(t, 15, []int{1}))
		if err != nil {
			t.Fatal(err)
		}
		if err = s.HandleDrama(context.Background(), 7, 1, uint64(j.ID)); err != nil {
			t.Fatal(err)
		}
		if r.jobs[0].Status != "failed" || r.jobs[0].ErrorCode != 67013 || r.credit.Balance != 1000 || r.credit.Frozen != 0 {
			t.Fatalf("job=%+v credit=%+v", r.jobs[0], r.credit)
		}
		if len(r.segments) != 1 || r.segments[0].ID != 99 || r.segments[0].Prompt != "旧片段" {
			t.Fatalf("已有片段被覆盖: %+v", r.segments)
		}
	})
}

func TestDramaStoryboardCancelAndUpstreamFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cancel, old bool
		want        string
	}{{"取消空分镜", true, false, "empty"}, {"取消已有分镜", true, true, "ready"}, {"上游失败退款", false, false, "failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDramaMemory()
			storyboardReady(r)
			if tc.old {
				r.segments = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 99}, UserID: 7, EpisodeID: r.episodes[0].ID, Prompt: "旧片段", Seconds: 5, Revision: 1}}
			}
			e := &dramaTextStreamer{outputs: []string{storyboardValid}}
			if !tc.cancel {
				e.failAt = 1
			}
			s, _ := dramaTestService(r, e, nil)
			j, err := s.CreateJob(context.Background(), 7, 1, storyboardRequest(t, 15, []int{1}))
			if err != nil {
				t.Fatal(err)
			}
			if tc.cancel {
				e.onCall = func() {
					if _, err := s.CancelJob(context.Background(), 7, 1, uint64(j.ID)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err = s.HandleDrama(context.Background(), 7, 1, uint64(j.ID)); err != nil {
				t.Fatal(err)
			}
			if r.credit.Balance != 1000 || r.credit.Frozen != 0 || r.episodes[0].StoryboardStatus != tc.want {
				t.Fatalf("credit=%+v ep=%+v", r.credit, r.episodes[0])
			}
			if tc.cancel && r.jobs[0].Status != "canceled" {
				t.Fatalf("job=%+v", r.jobs[0])
			}
			if tc.old && (len(r.segments) != 1 || r.segments[0].ID != 99) {
				t.Fatalf("取消改了片段: %+v", r.segments)
			}
		})
	}
}

func TestDramaStoryboardExactDurationAndSummary(t *testing.T) {
	for _, seconds := range []int{4, 15, 60} {
		t.Run(fmt.Sprintf("%d秒", seconds), func(t *testing.T) {
			r := newDramaMemory()
			storyboardReady(r)
			e := &dramaTextStreamer{outputs: []string{strings.ReplaceAll(storyboardValid, `"seconds":15`, fmt.Sprintf(`"seconds":%d`, seconds))}}
			s, _ := dramaTestService(r, e, nil)
			j, err := s.CreateJob(context.Background(), 7, 1, storyboardRequest(t, seconds, []int{1}))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.HandleDrama(context.Background(), 7, 1, uint64(j.ID)); err != nil {
				t.Fatal(err)
			}
			if len(r.segments) != 1 {
				t.Fatalf("segments=%+v", r.segments)
			}
			task := uint64(77)
			r.segments[0].ChosenTaskID = &task
			composeJobID := uint64(888)
			r.episodes[0].EditSchema = 1
			r.episodes[0].SegmentVersion = 4
			r.episodes[0].FinalComposeJobID = &composeJobID
			view, err := s.Get(context.Background(), 7, 1)
			if err != nil {
				t.Fatal(err)
			}
			ep, err := s.GetEpisode(context.Background(), 7, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			for _, summary := range []model.DramaEpisodeSummary{view.Episodes[0], ep.DramaEpisodeSummary} {
				if summary.ID != idcodec.ID(r.episodes[0].ID) || summary.StoryboardStatus != "ready" || summary.SegmentCount != 1 || summary.SegmentSeconds != seconds || summary.DoneSegmentCount != 1 ||
					summary.EditSchema != 1 || summary.SegmentVersion != 4 || summary.FinalComposeJobID == nil || uint64(*summary.FinalComposeJobID) != composeJobID {
					t.Fatalf("summary=%+v", summary)
				}
				rawJSON, mErr := json.Marshal(summary)
				if mErr != nil {
					t.Fatal(mErr)
				}
				var decoded map[string]any
				if uErr := json.Unmarshal(rawJSON, &decoded); uErr != nil {
					t.Fatal(uErr)
				}
				if decoded["id"] != idcodec.Encode(r.episodes[0].ID) || decoded["edit_schema"] != float64(1) || decoded["segment_version"] != float64(4) || decoded["final_compose_job_id"] == nil {
					t.Fatalf("decoded summary json=%s", rawJSON)
				}
			}
		})
	}
}

func TestDramaStoryboardAtomicReplacementRetry(t *testing.T) {
	r := newDramaMemory()
	storyboardReady(r)
	r.segments = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 99}, UserID: 7, EpisodeID: r.episodes[0].ID, Prompt: "旧片段", Seconds: 5, Revision: 1}}
	failure := errors.New("写片段失败")
	r.replaceErr = failure
	e := &dramaTextStreamer{outputs: []string{storyboardValid, storyboardValid}}
	s, _ := dramaTestService(r, e, nil)
	j, err := s.CreateJob(context.Background(), 7, 1, storyboardRequest(t, 15, []int{1}))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.HandleDrama(context.Background(), 7, 1, uint64(j.ID)); !errors.Is(err, failure) {
		t.Fatalf("want repository failure, got %v", err)
	}
	if len(r.segments) != 1 || r.segments[0].ID != 99 || r.episodes[0].StoryboardStatus != "generating" || r.credit.Balance != 1000 || r.credit.Frozen != 5 {
		t.Fatalf("非原子变更: segments=%+v ep=%+v credit=%+v", r.segments, r.episodes[0], r.credit)
	}
	r.replaceErr = nil
	if err = s.HandleDrama(context.Background(), 7, 1, uint64(j.ID)); err != nil {
		t.Fatal(err)
	}
	if len(r.segments) != 1 || r.segments[0].ID == 99 || r.episodes[0].StoryboardStatus != "ready" || r.credit.Balance != 995 || r.credit.Frozen != 0 {
		t.Fatalf("恢复未完成: segments=%+v ep=%+v credit=%+v", r.segments, r.episodes[0], r.credit)
	}
}

func TestDramaStoryboardBlocksAuthoredProgramAndVisualEdits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		program string
		row     model.DramaSegment
		code    int
	}{
		{name: "audio", program: authoredStoryboardProgram, code: 67006},
		{name: "overlays", program: `{"v":1,"audio":[],"overlays":[{}]}`, code: 67006},
		{name: "black", row: model.DramaSegment{VisualKind: "black"}, code: 67013},
		{name: "still", row: model.DramaSegment{VisualKind: "still", StillAssetID: new(uint64(30))}, code: 67013},
		{name: "FX", row: model.DramaSegment{VisualKind: "video", FXJSON: []byte(`{"fade_in_f":2}`)}, code: 67013},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newDramaMemory()
			storyboardReady(r)
			ep := &r.episodes[0]
			ep.ProgramJSON = []byte(tc.program)
			ep.StoryboardStatus = "ready"
			r.hasTakes = tc.program != "" // A program takes precedence even when both would block.
			tc.row.ID, tc.row.UserID, tc.row.EpisodeID = 20, 7, ep.ID
			r.segments = []model.DramaSegment{tc.row}
			s, _ := dramaTestService(r, &dramaTextStreamer{}, nil)
			req := storyboardRequest(t, 0, []int{1})
			_, err := s.Estimate(ctx, 7, 1, req)
			dramaCheckCode(t, err, tc.code)
			_, err = s.CreateJob(ctx, 7, 1, req)
			dramaCheckCode(t, err, tc.code)
			if tc.program != "" && !strings.Contains(err.Error(), "节目单") {
				t.Fatalf("missing program protection message: %v", err)
			}
			if len(r.jobs) != 0 || r.credit.Frozen != 0 || r.episodes[0].StoryboardStatus != "ready" {
				t.Fatalf("rejection changed jobs, credits or episode: %+v %+v %+v", r.jobs, r.credit, r.episodes[0])
			}
			if len(r.segments) != 1 || r.segments[0].ID != 20 {
				t.Fatalf("rejection changed clips: %+v", r.segments)
			}
		})
	}
}

func TestDramaStoryboardPreservesClipsWhenProgramIsAuthoredDuringGeneration(t *testing.T) {
	ctx := context.Background()
	r := newDramaMemory()
	storyboardReady(r)
	r.segments = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 20}, UserID: 7, EpisodeID: r.episodes[0].ID, Seconds: 5, Revision: 1}}
	streamer := &dramaTextStreamer{outputs: []string{storyboardValid}, onCall: func() {
		r.episodes[0].ProgramJSON = []byte(authoredStoryboardProgram)
	}}
	s, _ := dramaTestService(r, streamer, nil)
	job, err := s.CreateJob(ctx, 7, 1, storyboardRequest(t, 15, []int{1}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HandleDrama(ctx, 7, 1, uint64(job.ID)); err != nil {
		t.Fatal(err)
	}
	if r.jobs[0].Status != "failed" || r.jobs[0].ErrorCode != 67006 || r.credit.Balance != 1000 || r.credit.Frozen != 0 {
		t.Fatalf("job=%+v credit=%+v", r.jobs[0], r.credit)
	}
	if len(r.segments) != 1 || r.segments[0].ID != 20 || string(r.episodes[0].ProgramJSON) != authoredStoryboardProgram {
		t.Fatalf("clips or program changed: %+v %s", r.segments, r.episodes[0].ProgramJSON)
	}
}

// storyboardClearRepo 模拟真实替换递增版本、媒体互斥及任务插入失败后的事务回滚。
type storyboardClearRepo struct {
	*dramaMemory
	activeTask, activeRun bool
	enqueueErr            error
}

func (r *storyboardClearRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	return r.dramaMemory.WithTx(ctx, func(repository.DramaTx) error { return fn(r) })
}

func (r *storyboardClearRepo) ReplaceSegments(ctx context.Context, userID, episodeID uint64, rows []model.DramaSegment) error {
	if err := r.dramaMemory.ReplaceSegments(ctx, userID, episodeID, rows); err != nil {
		return err
	}
	r.episodes[0].SegmentVersion++
	return nil
}

func (r *storyboardClearRepo) SaveEpisode(ctx context.Context, ep *model.DramaEpisode, revision int64) error {
	ep.SegmentVersion = r.episodes[0].SegmentVersion
	return r.dramaMemory.SaveEpisode(ctx, ep, revision)
}

func (r *storyboardClearRepo) SegmentTasks(context.Context, uint64, uint64, string) ([]model.GenerationTask, error) {
	if r.activeTask {
		return []model.GenerationTask{{Status: model.TaskRunning}}, nil
	}
	return nil, nil
}

func (r *storyboardClearRepo) ActiveFrameRunsBySegments(context.Context, uint64, []uint64) ([]model.DramaSegmentFrameRun, error) {
	if r.activeRun {
		return []model.DramaSegmentFrameRun{{ID: 40, State: model.FrameRunStateVideoPending}}, nil
	}
	return nil, nil
}

func (r *storyboardClearRepo) InsertJob(ctx context.Context, job *model.DramaJob) error {
	if err := r.dramaMemory.InsertJob(ctx, job); err != nil {
		return err
	}
	return r.enqueueErr
}

func storyboardClearFixture(t *testing.T) (*storyboardClearRepo, *service.DramaService, *model.DramaJobReq) {
	t.Helper()
	r := &storyboardClearRepo{dramaMemory: newDramaMemory()}
	storyboardReady(r.dramaMemory)
	ep := &r.episodes[0]
	ep.SegmentVersion, ep.StoryboardStatus = 5, "ready"
	r.hasTakes = true
	r.segments = []model.DramaSegment{
		{BaseModel: model.BaseModel{ID: 20}, UserID: 7, EpisodeID: ep.ID, Seconds: 5, Revision: 1, VisualKind: "black"},
		{BaseModel: model.BaseModel{ID: 21}, UserID: 7, EpisodeID: ep.ID, Seconds: 5, Seq: 1, Revision: 1},
	}
	_, registry := dramaTestService(r.dramaMemory, &dramaTextStreamer{}, nil)
	s := service.NewDramaService(service.DramaDeps{Repo: r, Registry: registry})
	req := storyboardRequest(t, 15, []int{1})
	req.ConfirmClear = true
	req.ExpectedEpisodeID, req.ExpectedSegmentVersion = new(ep.ID), new(ep.SegmentVersion)
	return r, s, req
}

func TestDramaStoryboardConfirmedClear(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%v", empty), func(t *testing.T) {
			r, s, req := storyboardClearFixture(t)
			if empty {
				r.segments = nil
			}
			job, err := s.CreateJob(t.Context(), 7, 1, req)
			if err != nil || job.Status != "queued" || len(r.jobs) != 1 || r.credit.Frozen != 5 {
				t.Fatalf("提交失败: job=%+v err=%v credit=%+v", job, err, r.credit)
			}
			if len(r.segments) != 0 || r.episodes[0].SegmentVersion != 6 || r.episodes[0].StoryboardStatus != "generating" {
				t.Fatalf("清空未原子提交: segments=%+v ep=%+v", r.segments, r.episodes[0])
			}
			if _, err := s.CancelJob(t.Context(), 7, 1, uint64(job.ID)); err != nil {
				t.Fatal(err)
			}
			if r.episodes[0].StoryboardStatus != "empty" {
				t.Fatalf("已清空的集取消后应为空: %+v", r.episodes[0])
			}
			_, err = s.CreateJob(t.Context(), 7, 1, req)
			dramaCheckCode(t, err, 91001)
		})
	}
}

func TestDramaStoryboardConfirmedClearRejectsChangesAndRollsBack(t *testing.T) {
	queueFailure := errors.New("分镜队列不可用")
	for _, tc := range []struct {
		name   string
		change func(*storyboardClearRepo)
		code   int
	}{
		{"anchored cue saved after precheck", func(r *storyboardClearRepo) {
			r.episodes[0].ProgramJSON = []byte(strings.Replace(authoredStoryboardProgram, `"end_f":12`, `"anchor":{"segment_id":20,"source_f":0},"length_f":12`, 1))
			r.episodes[0].SegmentVersion++
		}, 67006},
		{"segment edit advanced version", func(r *storyboardClearRepo) { r.episodes[0].SegmentVersion++ }, 91001},
		{"episode replaced", func(r *storyboardClearRepo) { r.episodes[0].ID++ }, 91001},
		{"episode removed", func(r *storyboardClearRepo) { r.episodes = nil }, 91001},
		{"active job", func(r *storyboardClearRepo) {
			r.jobs = []model.DramaJob{{UserID: 7, DramaID: 1, Step: "compose", Episodes: []int{1}, Status: "running"}}
		}, 67004},
		{"active frame run", func(r *storyboardClearRepo) { r.activeRun = true }, 67004},
		{"active media task", func(r *storyboardClearRepo) { r.activeTask = true }, 67004},
		{"queue insertion failed", func(r *storyboardClearRepo) { r.enqueueErr = queueFailure }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, req := storyboardClearFixture(t)
			tc.change(r)
			before, eps, jobs := slices.Clone(r.segments), slices.Clone(r.episodes), len(r.jobs)
			_, err := s.CreateJob(t.Context(), 7, 1, req)
			if tc.code != 0 {
				dramaCheckCode(t, err, tc.code)
			} else if !errors.Is(err, queueFailure) {
				t.Fatalf("应返回入队失败: %v", err)
			}
			if tc.code == 67006 && !strings.Contains(err.Error(), "请先清空节目单，再重新生成分镜") {
				t.Fatalf("未提示节目单保护: %v", err)
			}
			if !reflect.DeepEqual(before, r.segments) || len(r.jobs) != jobs || r.credit.Frozen != 0 ||
				!slices.EqualFunc(eps, r.episodes, func(a, b model.DramaEpisode) bool {
					return a.SegmentVersion == b.SegmentVersion && a.StoryboardStatus == b.StoryboardStatus &&
						string(a.ProgramJSON) == string(b.ProgramJSON)
				}) {
				t.Fatalf("失败修改了片段、集状态或任务: %+v %+v %+v", r.segments, r.episodes, r.jobs)
			}
		})
	}
}

// TestDramaStoryboardConfirmedClearPG16 验证实际事务内的软删、版本号、队列失败回滚及历史任务保留。
func TestDramaStoryboardConfirmedClearPG16(t *testing.T) {
	db, repo := newProgramPG16(t)
	const episodeID uint64 = 3
	mustTakeSQL(t, db.Model(&model.DramaEpisode{}).Where("id = ?", episodeID).
		Updates(map[string]any{"script_md": "正文", "script_status": "ready", "storyboard_status": "ready"}))
	fixture := newComposeMemory()
	seedComposePGTask(&fixture.tasks[0])
	for _, row := range []any{&fixture.segments[0], &fixture.segments[1], &fixture.tasks[0]} {
		mustTakeSQL(t, db.Create(row))
	}
	_, registry := dramaTestService(fixture.dramaMemory, &dramaTextStreamer{}, nil)
	svc := service.NewDramaService(service.DramaDeps{Repo: repo, Registry: registry})
	ctx := t.Context()
	req := &model.DramaJobReq{Step: "storyboard", Episodes: []int{1}, MaxSeconds: 15,
		ConfirmClear: true, ExpectedEpisodeID: new(episodeID), ExpectedSegmentVersion: new(int64(4))}
	assertState := func(live int64, version int64, status string, jobs int64) {
		t.Helper()
		var ep model.DramaEpisode
		var visible, allRows, taskCount, jobCount int64
		mustTakeSQL(t, db.First(&ep, episodeID))
		mustTakeSQL(t, db.Model(&model.DramaSegment{}).Where("episode_id = ?", episodeID).Count(&visible))
		mustTakeSQL(t, db.Unscoped().Model(&model.DramaSegment{}).Where("episode_id = ?", episodeID).Count(&allRows))
		mustTakeSQL(t, db.Model(&model.GenerationTask{}).Count(&taskCount))
		mustTakeSQL(t, db.Model(&model.DramaJob{}).Count(&jobCount))
		if visible != live || ep.SegmentVersion != version || ep.StoryboardStatus != status ||
			allRows != 2 || taskCount != 1 || jobCount != jobs {
			t.Fatalf("片段或任务状态不一致: ep=%+v live=%d all=%d tasks=%d jobs=%d", ep, visible, allRows, taskCount, jobCount)
		}
	}
	// 未配置队列模拟业务任务记录已插入后入队失败。
	if _, err := svc.CreateJob(ctx, 7, 1, req); err == nil || !strings.Contains(err.Error(), "队列未配置") {
		t.Fatalf("应因无队列而回滚: %v", err)
	}
	assertState(2, 4, "ready", 0)
	queue := &slice2cQueue{}
	repo.SetEnqueuer(queue)
	job, err := svc.CreateJob(ctx, 7, 1, req)
	if err != nil || job.Status != "queued" || queue.calls.Load() != 1 {
		t.Fatalf("入队失败: %+v %v", job, err)
	}
	assertState(0, 5, "generating", 1)
}
