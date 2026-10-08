ALTER TABLE rin_renderer.render_job_workloads
    ADD COLUMN admission_profile_key text;

UPDATE rin_renderer.render_job_workloads
SET admission_profile_key = profile_key
WHERE admission_profile_key IS NULL;

ALTER TABLE rin_renderer.render_job_workloads
    ALTER COLUMN admission_profile_key SET NOT NULL,
    ADD CONSTRAINT render_job_workloads_admission_profile_key_nonempty
        CHECK (admission_profile_key <> '');

CREATE INDEX render_job_workloads_admission_profile_idx
    ON rin_renderer.render_job_workloads (profile_version, admission_profile_key, job_id);
