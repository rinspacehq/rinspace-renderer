ALTER TABLE rin_renderer.render_jobs
    ADD COLUMN declared_source_bytes bigint NOT NULL DEFAULT 0,
    ADD COLUMN reservation_expires_at timestamptz,
    ADD COLUMN admission_token_hash character(64);

ALTER TABLE rin_renderer.render_jobs
    ADD CONSTRAINT render_jobs_declared_source_bytes_check
        CHECK (declared_source_bytes >= 0),
    ADD CONSTRAINT render_jobs_admission_token_hash_check
        CHECK (admission_token_hash IS NULL OR admission_token_hash ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT render_jobs_uploading_reservation_check
        CHECK (
            state <> 'uploading' OR (
                source_artifact_id IS NULL
                AND queued_at IS NULL
                AND reservation_expires_at IS NOT NULL
                AND admission_token_hash IS NOT NULL
            )
        ),
    ADD CONSTRAINT render_jobs_non_uploading_reservation_check
        CHECK (
            state = 'uploading'
            OR (reservation_expires_at IS NULL AND admission_token_hash IS NULL)
        );

CREATE INDEX render_jobs_abandoned_reservation_idx
    ON rin_renderer.render_jobs (reservation_expires_at, id)
    WHERE state = 'uploading';
