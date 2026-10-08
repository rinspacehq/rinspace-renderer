CREATE TABLE rin_renderer.render_job_wait_estimates (
    job_id uuid PRIMARY KEY REFERENCES rin_renderer.render_jobs(id) ON DELETE CASCADE,
    estimator_version text NOT NULL CHECK (estimator_version <> ''),
    estimated_start_at timestamptz NOT NULL,
    earliest_start_at timestamptz NOT NULL,
    latest_start_at timestamptz NOT NULL,
    confidence text NOT NULL CHECK (confidence IN ('low', 'medium', 'high')),
    sample_count integer NOT NULL CHECK (sample_count > 0),
    scope text NOT NULL CHECK (scope IN ('instance', 'cluster')),
    calculated_at timestamptz NOT NULL,
    actual_started_at timestamptz,
    central_error_ms bigint,
    interval_covered boolean,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (earliest_start_at <= estimated_start_at),
    CHECK (estimated_start_at <= latest_start_at),
    CHECK ((actual_started_at IS NULL) = (central_error_ms IS NULL)),
    CHECK ((actual_started_at IS NULL) = (interval_covered IS NULL))
);

CREATE INDEX render_job_wait_estimates_calibration_idx
    ON rin_renderer.render_job_wait_estimates (estimator_version, actual_started_at, job_id)
    WHERE actual_started_at IS NOT NULL;
