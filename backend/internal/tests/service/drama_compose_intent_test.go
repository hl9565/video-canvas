package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/repository"
)

func composeReq(t *testing.T, extra string) *model.DramaJobReq {
	t.Helper()
	var req model.DramaJobReq
	if err := json.Unmarshal([]byte(`{"step":"compose","episodes":[1]`+extra+`}`), &req); err != nil {
		t.Fatal(err)
	}
	return &req
}

func (r *composeMemory) JobByComposeKey(_ context.Context, user, drama uint64, key string) (*model.DramaJob, error) {
	for _, j := range r.jobs {
		if j.UserID == user && j.DramaID == drama && j.Step == "compose" && j.ComposeRequestKey != nil && *j.ComposeRequestKey == key {
			return &j, nil
		}
	}
	return nil, repository.ErrNotFound
}

func TestDramaComposeIntent_NamespaceAndReplay(t *testing.T) {
	ctx := context.Background()
	for _, extra := range []string{`,"compose_request_key":"approval:55"`, `,"compose_request_key":"approval:other"`} {
		r := newComposeMemory()
		s := composeService(r, &composeFiles{r: r}, &composeProcessor{})
		_, err := s.CreateJob(ctx, 7, 1, composeReq(t, extra))
		dramaCheckCode(t, err, 10001)
		if len(r.jobs) != 0 {
			t.Fatal("非法键创建任务")
		}
	}
	r := newComposeMemory()
	s := composeService(r, &composeFiles{r: r}, &composeProcessor{})
	first, err := s.CreateAgentJob(ctx, 7, 1, 55, composeReq(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if r.jobs[len(r.jobs)-1].RequestIntentSig == "" || r.jobs[0].ComposeRequestKey == nil || *r.jobs[0].ComposeRequestKey != "approval:55" {
		t.Fatalf("未冻结意图和派生键: %+v", r.jobs[0])
	}
	r.episodes[0].SegmentVersion++
	r.segments[0].ChosenTaskID = nil
	again, err := s.CreateAgentJob(ctx, 7, 1, 55, composeReq(t, ""))
	if err != nil || again.ID != first.ID {
		t.Fatalf("同意图必须原样重放: %v", err)
	}
	for _, extra := range []string{`,"compose":{"size":"1080p"}`, `,"compose_request_key":"replacement"`, `,"ratio":"16:9"`} {
		_, err = s.CreateAgentJob(ctx, 7, 1, 55, composeReq(t, extra))
		dramaCheckCode(t, err, 90001)
	}
}

func TestDramaComposeIntent_LegacyFailClosed(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		r := newComposeMemory()
		approval := uint64(55)
		r.jobs = []model.DramaJob{{BaseModel: model.BaseModel{ID: 9}, UserID: 7, DramaID: 1, Step: "compose", Episodes: []int{1}, Status: "succeeded", ApprovalID: &approval, InputJSON: []byte(`{"episode_id":3,"segment_version":4,"asset_ids":[30]}`)}}
		if migrated {
			key := "approval:55"
			r.jobs[0].ComposeRequestKey = &key
		}
		s := composeService(r, &composeFiles{r: r}, &composeProcessor{})
		v, err := s.CreateAgentJob(context.Background(), 7, 1, 55, composeReq(t, `,"compose":{"segment_version":4}`))
		if err != nil || uint64(v.ID) != 9 {
			t.Fatalf("旧审批重放 %v", err)
		}
		for _, extra := range []string{`,"compose":{"size":"1080p"}`, `,"compose":{"segment_version":5}`, `,"compose_request_key":"new"`} {
			_, err = s.CreateAgentJob(context.Background(), 7, 1, 55, composeReq(t, extra))
			dramaCheckCode(t, err, 90001)
		}
	}
}

func (r *dramaMemory) JobByApproval(_ context.Context, user, approval uint64) (*model.DramaJob, error) {
	for _, j := range r.jobs {
		if j.UserID == user && j.ApprovalID != nil && *j.ApprovalID == approval {
			return &j, nil
		}
	}
	return nil, repository.ErrNotFound
}

func TestDramaJobIntent_AllStepsPersistAndRawSeconds(t *testing.T) {
	for _, step := range []string{"scripts", "breakdown", "asset_prompts", "storyboard"} {
		t.Run(step, func(t *testing.T) {
			r := newDramaMemory()
			storyboardReady(r)
			if step == "scripts" || step == "breakdown" {
				r.episodes[0].LockedAt = nil
			}
			s, _ := dramaTestService(r, &dramaTextStreamer{}, nil)
			if step == "asset_prompts" {
				r.jobs = []model.DramaJob{{BaseModel: model.BaseModel{ID: 1}, UserID: 7, DramaID: 1, Step: "asset_prompts", Status: "succeeded", Episodes: []int{1}, InputJSON: []byte(`[{"node_id":"asset-node"}]`)}}
			}
			baselineJobs := len(r.jobs)
			req := &model.DramaJobReq{Step: step, Episodes: []int{1}}
			first, err := s.CreateAgentJob(context.Background(), 7, 1, 101, req)
			if err != nil {
				t.Fatal(err)
			}
			if r.jobs[len(r.jobs)-1].RequestIntentSig == "" {
				t.Fatal("未保存全步骤请求意图")
			}
			frozen := r.credit.Frozen
			ledger := len(r.ledger)
			again, err := s.CreateAgentJob(context.Background(), 7, 1, 101, req)
			if err != nil || again.ID != first.ID {
				t.Fatalf("相同意图未重放: %v", err)
			}
			changed := *req
			changed.Ratio = "16:9"
			_, err = s.CreateAgentJob(context.Background(), 7, 1, 101, &changed)
			dramaCheckCode(t, err, 90001)
			if step == "storyboard" {
				changed = *req
				changed.MaxSeconds = 15
				_, err = s.CreateAgentJob(context.Background(), 7, 1, 101, &changed)
				dramaCheckCode(t, err, 90001)
			}
			if len(r.jobs) != baselineJobs+1 || r.credit.Frozen != frozen || len(r.ledger) != ledger {
				t.Fatal("重放冲突产生副作用")
			}
		})
	}
}

func TestDramaJobIntent_NonComposeKeyRejectedEveryEntrance(t *testing.T) {
	r := newDramaMemory()
	s, _ := dramaTestService(r, &dramaTextStreamer{}, nil)
	req := &model.DramaJobReq{Step: "scripts", ComposeRequestKey: "custom"}
	_, err := s.CreateJob(context.Background(), 7, 1, req)
	dramaCheckCode(t, err, 10001)
	_, err = s.CreateAgentJob(context.Background(), 7, 1, 1, req)
	dramaCheckCode(t, err, 10001)
	_, err = s.Estimate(context.Background(), 7, 1, req)
	dramaCheckCode(t, err, 10001)
}

func TestDramaComposeIntent_ApprovalAndKeyCannotBorrowJobs(t *testing.T) {
	for _, kind := range []string{"different-rows", "different-approval", "ordinary-as-approval", "approval-as-ordinary", "legacy-explicit-key"} {
		t.Run(kind, func(t *testing.T) {
			r := newComposeMemory()
			a, b := uint64(11), uint64(12)
			key := "custom"
			j := model.DramaJob{BaseModel: model.BaseModel{ID: 1}, UserID: 7, DramaID: 1, Step: "compose", Episodes: []int{1}, Status: "succeeded", ApprovalID: &a, ComposeRequestKey: &key, InputJSON: []byte(`{"segment_version":0}`)}
			switch kind {
			case "different-rows":
				other := j
				other.ID = 2
				other.ApprovalID = &b
				old := "other"
				j.ComposeRequestKey = &old
				r.jobs = append(r.jobs, other)
			case "different-approval":
				j.ApprovalID = &b
			case "ordinary-as-approval":
				j.ApprovalID = nil
			case "legacy-explicit-key":
				j.ComposeRequestKey = nil
			}
			r.jobs = append(r.jobs, j)
			s := composeService(r, &composeFiles{r: r}, &composeProcessor{})
			req := composeReq(t, `,"compose_request_key":"custom"`)
			var err error
			if kind == "approval-as-ordinary" {
				_, err = s.CreateJob(context.Background(), 7, 1, req)
			} else {
				_, err = s.CreateAgentJob(context.Background(), 7, 1, a, req)
			}
			dramaCheckCode(t, err, 90001)
		})
	}
}

func TestDramaJobIntent_LegacyEveryStepUsesOnlyFrozenFields(t *testing.T) {
	for _, step := range []string{"scripts", "breakdown", "asset_prompts", "storyboard"} {
		r := newDramaMemory()
		approval := uint64(200)
		r.jobs = []model.DramaJob{{BaseModel: model.BaseModel{ID: 8}, UserID: 7, DramaID: 1, Step: step, Episodes: []int{1}, Status: "succeeded", ApprovalID: &approval, InputJSON: []byte(`{"max_seconds":15}`)}}
		s, _ := dramaTestService(r, &dramaTextStreamer{}, nil)
		req := &model.DramaJobReq{Step: step, Episodes: []int{1}}
		v, err := s.CreateAgentJob(context.Background(), 7, 1, approval, req)
		if err != nil || uint64(v.ID) != 8 {
			t.Fatalf("%s 旧签名默认请求未重放 %v", step, err)
		}
		if step == "storyboard" {
			req.MaxSeconds = 15
			_, err = s.CreateAgentJob(context.Background(), 7, 1, approval, req)
			if err != nil {
				t.Fatal(err)
			}
			req.MaxSeconds = 16
		} else {
			req.MaxSeconds = 4
		}
		_, err = s.CreateAgentJob(context.Background(), 7, 1, approval, req)
		dramaCheckCode(t, err, 90001)
		req.MaxSeconds = 0
		req.Ratio = "16:9"
		_, err = s.CreateAgentJob(context.Background(), 7, 1, approval, req)
		dramaCheckCode(t, err, 90001)
		if len(r.jobs) != 1 || len(r.ledger) != 0 || r.credit.Frozen != 0 {
			t.Fatal("旧签名重放产生副作用")
		}
	}
}

func TestDramaJobIntent_ExpectedEpisodeIDValidationAndSignature(t *testing.T) {
	ctx := context.Background()
	r := newComposeMemory()
	s := composeService(r, &composeFiles{r: r}, &composeProcessor{})

	wrong := uint64(999)
	_, err := s.CreateJob(ctx, 7, 1, &model.DramaJobReq{Step: "scripts", Episodes: []int{1}, ExpectedEpisodeID: &wrong})
	dramaCheckCode(t, err, 10001)

	_, err = s.CreateJob(ctx, 7, 1, &model.DramaJobReq{Step: "compose", Episodes: []int{1}, ExpectedEpisodeID: &wrong, ComposeRequestKey: "cmp-wrong-ep"})
	dramaCheckCode(t, err, 91001)

	right := r.episodes[0].ID
	reqRight := &model.DramaJobReq{Step: "compose", Episodes: []int{1}, ExpectedEpisodeID: &right, ComposeRequestKey: "cmp-right-ep"}
	first, err := s.CreateJob(ctx, 7, 1, reqRight)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateJob(ctx, 7, 1, reqRight)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("相同 expected_episode_id 重放应命中幂等任务: %+v err=%v", replay, err)
	}
	otherEp := r.episodes[0].ID + 1
	_, err = s.CreateJob(ctx, 7, 1, &model.DramaJobReq{Step: "compose", Episodes: []int{1}, ExpectedEpisodeID: &otherEp, ComposeRequestKey: "cmp-right-ep"})
	dramaCheckCode(t, err, 90001)
}

func TestDramaComposeIntent_StaleIdentityBeforeEligibility(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		episodes               []int
		replace, busy, missing bool
		code                   int
	}{
		{name: "replacement is unlocked", episodes: []int{1}, replace: true, code: 91001},
		{name: "replacement has active compose", episodes: []int{1}, replace: true, busy: true, code: 91001},
		{name: "missing requested number", episodes: []int{2}, code: 91001},
		{name: "no remaining episodes", episodes: []int{1}, missing: true, code: 91001},
		{name: "multiple episode numbers", episodes: []int{1, 2}, code: 10001},
		{name: "invalid episode number", episodes: []int{100001}, code: 10001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newComposeMemory()
			expected := r.episodes[0].ID
			if tc.replace {
				r.episodes[0].ID++
				r.episodes[0].LockedAt = nil
			}
			if tc.busy {
				r.jobs = []model.DramaJob{{UserID: 7, DramaID: 1, Step: "compose", Episodes: []int{1}, Status: "running"}}
			}
			if tc.missing {
				r.episodes = nil
			}
			s := composeService(r, &composeFiles{r: r}, &composeProcessor{})
			req := &model.DramaJobReq{Step: "compose", Episodes: tc.episodes, ExpectedEpisodeID: &expected}
			_, err := s.CreateJob(t.Context(), 7, 1, req)
			dramaCheckCode(t, err, tc.code)
			_, err = s.Estimate(t.Context(), 7, 1, req)
			dramaCheckCode(t, err, tc.code)
		})
	}
}

func TestDramaJobIntent_StoryboardClearCannotReuseAgentApproval(t *testing.T) {
	r := newDramaMemory()
	storyboardReady(r)
	s, _ := dramaTestService(r, &dramaTextStreamer{}, nil)
	ctx := t.Context()
	req := storyboardRequest(t, 15, []int{1})
	normal, err := s.CreateAgentJob(ctx, 7, 1, 101, req)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := *req
	confirmed.ConfirmClear = true
	confirmed.ExpectedEpisodeID = new(r.episodes[0].ID)
	confirmed.ExpectedSegmentVersion = new(int64)
	for _, approval := range []uint64{101, 102} {
		_, err := s.CreateAgentJob(ctx, 7, 1, approval, &confirmed)
		dramaCheckCode(t, err, 10001)
	}
	replay, err := s.CreateAgentJob(ctx, 7, 1, 101, req)
	if err != nil || replay.ID != normal.ID || len(r.jobs) != 1 || len(r.ledger) != 1 {
		t.Fatalf("清空请求影响普通审批重放: replay=%+v jobs=%+v err=%v", replay, r.jobs, err)
	}
}
