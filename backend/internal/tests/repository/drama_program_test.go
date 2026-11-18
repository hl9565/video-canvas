package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/repository"
)

const storedProgramFixture = `{"v":1,"audio":[{"id":"voice","role":"narration","asset_id":40,"start_f":0,"end_f":12}],"overlays":[]}`

func TestDramaProgramRepositoryCASAndGenericEpisodeSave(t *testing.T) {
	ctx := context.Background()
	db := isolatedDB(t, &model.DramaEpisode{})
	repo := repository.NewDramaRepository(db)
	ep := model.DramaEpisode{UserID: 7, DramaID: 1, No: 1, Revision: 1, SegmentVersion: 4, Title: "original"}
	if err := repo.SaveEpisode(ctx, &ep, 0); err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][3]uint64{{8, 1, ep.ID}, {7, 2, ep.ID}, {7, 1, ep.ID + 1}} {
		err := repo.SaveEpisodeProgram(ctx, scope[0], scope[1], scope[2], 4, 1, json.RawMessage(storedProgramFixture), true)
		if !errors.Is(err, repository.ErrStateConflict) {
			t.Fatalf("incorrect scope: %v", err)
		}
	}
	if err := repo.SaveEpisodeProgram(ctx, 7, 1, ep.ID, 4, 1, json.RawMessage(storedProgramFixture), true); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveEpisodeProgram(ctx, 7, 1, ep.ID, 4, 1, json.RawMessage(storedProgramFixture), true); !errors.Is(err, repository.ErrStateConflict) {
		t.Fatalf("stale segment version: %v", err)
	}
	rows, err := repo.Episodes(ctx, 7, 1)
	if err != nil || len(rows) != 1 || rows[0].Revision != 2 || rows[0].Title != "original" || rows[0].EditSchema != 1 {
		t.Fatalf("CAS unexpectedly changed episode fields: %+v %v", rows, err)
	}
	// A freshly read revision and a stale program body must still preserve the separate Program field.
	current := rows[0]
	current.Title = "publication"
	current.ProgramJSON = []byte(`{"v":1,"audio":[],"overlays":[]}`)
	current.FinalAssetID = new(uint64(500))
	if err := repo.SaveEpisode(ctx, &current, current.Revision); err != nil {
		t.Fatal(err)
	}
	assertStoredDramaProgram(t, repo, 3)
	if err := repo.SaveEpisodeProgram(ctx, 7, 1, ep.ID, 5, 1, json.RawMessage(storedProgramFixture), false); err != nil {
		t.Fatal(err)
	}
	assertStoredDramaProgram(t, repo, 4)
}

func assertStoredDramaProgram(t *testing.T, repo *repository.DramaRepository, revision int64) {
	t.Helper()
	rows, err := repo.Episodes(context.Background(), 7, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read episode: %+v %v", rows, err)
	}
	got := rows[0]
	if !jsonEqual(got.ProgramJSON, storedProgramFixture) || got.SegmentVersion != 5 || got.Revision != revision ||
		got.Title != "publication" || got.FinalAssetID == nil || *got.FinalAssetID != 500 {
		t.Fatalf("program and publication must coexist: %+v program=%s", got, got.ProgramJSON)
	}
}
