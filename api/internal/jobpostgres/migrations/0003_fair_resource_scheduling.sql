CREATE TABLE rin_renderer.scheduler_priority_state (
    priority_class text PRIMARY KEY CHECK (priority_class IN ('publish', 'rebuild', 'migration')),
    dispatch_count bigint NOT NULL DEFAULT 0 CHECK (dispatch_count >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

INSERT INTO rin_renderer.scheduler_priority_state (priority_class)
VALUES ('publish'), ('rebuild'), ('migration');

CREATE TABLE rin_renderer.resource_allocations (
    id uuid PRIMARY KEY,
    resource_class text NOT NULL CHECK (resource_class IN (
        'document-light', 'document-latexml', 'math-node', 'texsvg', 'batch-migration'
    )),
    job_id uuid NOT NULL REFERENCES rin_renderer.render_jobs(id) ON DELETE CASCADE,
    attempt_id uuid REFERENCES rin_renderer.render_attempts(id) ON DELETE CASCADE,
    allocation_kind text NOT NULL CHECK (allocation_kind IN ('top-level', 'subwork')),
    work_key text,
    owner_id text NOT NULL CHECK (owner_id <> ''),
    lease_token_hash character(64) NOT NULL CHECK (lease_token_hash ~ '^[0-9a-f]{64}$'),
    lease_expires_at timestamptz NOT NULL,
    heartbeat_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (
        (allocation_kind = 'top-level' AND attempt_id IS NOT NULL AND work_key IS NULL)
        OR (allocation_kind = 'subwork' AND attempt_id IS NULL AND work_key IS NOT NULL AND work_key <> '')
    )
);

CREATE UNIQUE INDEX resource_allocations_attempt_idx
    ON rin_renderer.resource_allocations (attempt_id)
    WHERE attempt_id IS NOT NULL;

CREATE INDEX resource_allocations_capacity_idx
    ON rin_renderer.resource_allocations (resource_class, lease_expires_at, id);

CREATE INDEX resource_allocations_expiry_idx
    ON rin_renderer.resource_allocations (lease_expires_at, id);

CREATE INDEX resource_allocations_job_idx
    ON rin_renderer.resource_allocations (job_id, allocation_kind, id);
