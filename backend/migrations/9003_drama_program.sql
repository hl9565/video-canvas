-- Fork PR3：集级节目单、静态帧与黑场。旧分镜默认保留视频模式。
ALTER TABLE drama_episodes
    ADD COLUMN program_json jsonb NOT NULL DEFAULT '{"v":1,"audio":[],"overlays":[]}'::jsonb,
    ADD CONSTRAINT ck_de_program CHECK (
        jsonb_typeof(program_json) = 'object'
        AND jsonb_exists(program_json, 'v')
        AND jsonb_exists(program_json, 'audio')
        AND jsonb_exists(program_json, 'overlays')
        AND program_json->'v' = '1'::jsonb
        AND jsonb_typeof(program_json->'audio') = 'array'
        AND jsonb_typeof(program_json->'overlays') = 'array'
    );

ALTER TABLE drama_segments
    ADD COLUMN visual_kind varchar(8) NOT NULL DEFAULT 'video',
    ADD COLUMN still_asset_id bigint,
    ADD COLUMN fx_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT ck_ds_visual_kind CHECK (visual_kind IN ('video','still','black')),
    ADD CONSTRAINT ck_ds_fx_obj CHECK (jsonb_typeof(fx_json) = 'object'),
    ADD CONSTRAINT ck_ds_visual CHECK (
        (visual_kind = 'video' AND still_asset_id IS NULL)
        OR (visual_kind = 'still' AND still_asset_id IS NOT NULL AND chosen_task_id IS NULL AND source_in_f = 0 AND source_out_f IS NOT NULL)
        OR (visual_kind = 'black' AND still_asset_id IS NULL AND chosen_task_id IS NULL AND source_in_f = 0 AND source_out_f IS NOT NULL)
    );
