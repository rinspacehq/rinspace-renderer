CREATE TABLE rin_renderer.render_job_workloads (
    job_id uuid PRIMARY KEY REFERENCES rin_renderer.render_jobs(id) ON DELETE CASCADE,
    profile_version text NOT NULL CHECK (profile_version <> ''),
    profile_key text NOT NULL CHECK (profile_key <> ''),
    feature_stage text NOT NULL CHECK (feature_stage IN ('admission', 'analysis')),
    features jsonb NOT NULL CHECK (jsonb_typeof(features) = 'object'),
    refined_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((feature_stage = 'analysis') = (refined_at IS NOT NULL))
);

CREATE INDEX render_job_workloads_profile_idx
    ON rin_renderer.render_job_workloads (profile_version, profile_key, job_id);

ALTER TABLE rin_renderer.workload_profiles
    ADD COLUMN failure_count integer NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
    ADD COLUMN failure_counts jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(failure_counts) = 'object');
