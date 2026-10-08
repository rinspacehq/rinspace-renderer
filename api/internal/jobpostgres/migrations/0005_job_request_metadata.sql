ALTER TABLE rin_renderer.render_jobs
    ADD COLUMN request_metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT render_jobs_request_metadata_object
        CHECK (jsonb_typeof(request_metadata) = 'object');
