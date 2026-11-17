-- Fork PR2：首帧控镜流程、候选导入与人工质检；历史任务与素材保持独立生命周期。
ALTER TABLE drama_segments
    ADD COLUMN chosen_cleared boolean NOT NULL DEFAULT false;

ALTER TABLE generation_tasks
    ADD COLUMN frame_run_id bigint,
    ADD COLUMN frame_run_confirm_rev bigint;

CREATE INDEX idx_task_frame_run ON generation_tasks (frame_run_id)
    WHERE frame_run_id IS NOT NULL;

CREATE TABLE drama_segment_frame_runs (
    id bigserial PRIMARY KEY,
    user_id bigint NOT NULL,
    segment_id bigint NOT NULL,
    request_key varchar(96) NOT NULL,
    request_hash char(64) NOT NULL,
    frame_source varchar(16) NOT NULL,
    state varchar(20) NOT NULL,
    segment_revision bigint NOT NULL,
    revision bigint NOT NULL DEFAULT 1,
    confirmed_version bigint NOT NULL DEFAULT 0,
    video_idempotency_key varchar(128) NOT NULL DEFAULT '',
    model_key varchar(128) NOT NULL DEFAULT '',
    origin_canvas_id bigint,
    frame_task_id bigint,
    frame_asset_id bigint,
    video_task_id bigint,
    adopt_if_empty boolean NOT NULL DEFAULT false,
    confirmed_input jsonb NOT NULL DEFAULT '{}'::jsonb,
    confirmed_input_hash char(64),
    quote_token text,
    submitting_at timestamptz,
    error_code varchar(64) NOT NULL DEFAULT '',
    error_message varchar(512) NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ck_dfr_state CHECK (state IN ('draft','frame_pending','frame_ready','confirmed','submitting','video_pending','bound','failed','cancelled')),
    CONSTRAINT ck_dfr_source CHECK (frame_source IN ('existing','generate','canvas')),
    CONSTRAINT ck_dfr_segment_rev CHECK (segment_revision > 0),
    CONSTRAINT ck_dfr_confirmed_obj CHECK (jsonb_typeof(confirmed_input) = 'object')
);

CREATE UNIQUE INDEX uk_dfr_user_req ON drama_segment_frame_runs (user_id, request_key);
CREATE UNIQUE INDEX uk_dfr_segment_active ON drama_segment_frame_runs (segment_id)
    WHERE state IN ('draft','frame_pending','frame_ready','confirmed','submitting','video_pending');
CREATE UNIQUE INDEX uk_dfr_video ON drama_segment_frame_runs (video_task_id)
    WHERE video_task_id IS NOT NULL;
CREATE INDEX ix_dfr_segment ON drama_segment_frame_runs (user_id, segment_id, id);
CREATE INDEX ix_dfr_recover ON drama_segment_frame_runs (state, updated_at)
    WHERE state IN ('frame_pending','submitting','video_pending');
CREATE INDEX ix_dfr_frame_task ON drama_segment_frame_runs (frame_task_id)
    WHERE frame_task_id IS NOT NULL;

CREATE TABLE drama_segment_take_links (
    user_id bigint NOT NULL,
    segment_id bigint NOT NULL,
    task_id bigint NOT NULL,
    source_canvas_id bigint NOT NULL,
    asset_id bigint NOT NULL,
    run_id bigint,
    origin varchar(16) NOT NULL,
    qc varchar(10) NOT NULL DEFAULT 'unreviewed',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (segment_id, task_id),
    CONSTRAINT ck_dst_origin CHECK (origin IN ('i2v','canvas_import','native')),
    CONSTRAINT ck_dst_qc CHECK (qc IN ('unreviewed','accepted','rejected'))
);

CREATE INDEX ix_dst_task ON drama_segment_take_links (user_id, task_id);
CREATE UNIQUE INDEX uk_dst_run ON drama_segment_take_links (run_id)
    WHERE run_id IS NOT NULL;
