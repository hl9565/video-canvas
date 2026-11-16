-- 0036_drama_edit.sql：短剧逐镜精裁、全入口批幂等意图与轮次执行代/租约

ALTER TABLE drama_segments
    ADD COLUMN source_in_f integer NOT NULL DEFAULT 0,
    ADD COLUMN source_out_f integer,
    ADD COLUMN mute_original boolean NOT NULL DEFAULT false,
    ADD COLUMN audio_gain_db numeric(5,2) NOT NULL DEFAULT 0,
    ADD COLUMN native_is_dialogue boolean NOT NULL DEFAULT false,
    ADD COLUMN av_pts_offset_ms integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT ck_ds_range CHECK (source_in_f >= 0 AND (source_out_f IS NULL OR source_out_f > source_in_f)),
    ADD CONSTRAINT ck_ds_gain CHECK (audio_gain_db BETWEEN -60 AND 12),
    ADD CONSTRAINT ck_ds_pts CHECK (av_pts_offset_ms BETWEEN -10000 AND 10000);

ALTER TABLE drama_episodes
    ADD COLUMN edit_schema smallint NOT NULL DEFAULT 0,
    ADD COLUMN final_compose_job_id bigint,
    ADD CONSTRAINT ck_de_edit_schema CHECK (edit_schema IN (0, 1));

ALTER TABLE drama_jobs
    ADD COLUMN compose_request_key varchar(96),
    ADD COLUMN request_intent_sig varchar(64) NOT NULL DEFAULT '';

UPDATE drama_jobs
SET compose_request_key = 'approval:' || approval_id::text
WHERE step = 'compose'
  AND approval_id IS NOT NULL
  AND compose_request_key IS NULL;

CREATE UNIQUE INDEX uk_drama_compose_request
    ON drama_jobs (user_id, drama_id, compose_request_key)
    WHERE step = 'compose' AND compose_request_key IS NOT NULL;

ALTER TABLE generation_tasks
    ADD COLUMN request_intent_sig varchar(64) NOT NULL DEFAULT '',
    ADD COLUMN request_batch_index smallint NOT NULL DEFAULT 0;

ALTER TABLE marketing_rounds
    ADD COLUMN execution_gen bigint NOT NULL DEFAULT 1;

ALTER TABLE marketing_step_runs
    ADD COLUMN round_gen bigint NOT NULL DEFAULT 1,
    ADD COLUMN cancel_requested boolean NOT NULL DEFAULT false,
    ADD COLUMN exec_token varchar(64) NOT NULL DEFAULT '',
    ADD COLUMN lease_until timestamptz;

CREATE TABLE asset_media_probes (
    asset_id bigint NOT NULL,
    sha256 char(64) NOT NULL,
    probe_version smallint NOT NULL,
    fps_num integer NOT NULL,
    fps_den integer NOT NULL,
    normalized_frames_24 integer NOT NULL,
    has_audio boolean NOT NULL DEFAULT false,
    audio_sample_rate integer NOT NULL DEFAULT 0,
    decoded_audio_samples bigint NOT NULL DEFAULT 0,
    v_start_us bigint NOT NULL DEFAULT 0,
    a_start_us bigint NOT NULL DEFAULT 0,
    probed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (asset_id, sha256, probe_version)
);
