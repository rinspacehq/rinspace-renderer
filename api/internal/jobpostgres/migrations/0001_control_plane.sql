CREATE TABLE rin_renderer.render_artifacts (
    id text PRIMARY KEY,
    sha256 character(64) NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    kind text NOT NULL CHECK (kind IN ('source', 'result', 'debug', 'cache', 'public-diagram')),
    visibility text NOT NULL CHECK (visibility IN ('private', 'public')),
    storage_key text NOT NULL UNIQUE CHECK (storage_key <> ''),
    byte_size bigint NOT NULL CHECK (byte_size >= 0),
    media_type text NOT NULL CHECK (media_type <> ''),
    schema_version text,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (visibility = 'private' OR kind = 'public-diagram')
);

CREATE INDEX render_artifacts_expiry_idx
    ON rin_renderer.render_artifacts (expires_at, id)
    WHERE expires_at IS NOT NULL;

CREATE TABLE rin_renderer.render_jobs (
    id uuid PRIMARY KEY,
    principal_id text NOT NULL CHECK (principal_id <> ''),
    owner_scope text NOT NULL DEFAULT ''::text,
    content_kind text NOT NULL CHECK (content_kind IN ('latex', 'markdown')),
    document_engine text NOT NULL CHECK (document_engine <> ''),
    resource_class text NOT NULL CHECK (resource_class IN (
        'document-light', 'document-latexml', 'math-node', 'texsvg', 'batch-migration'
    )),
    priority_class text NOT NULL CHECK (priority_class IN ('publish', 'rebuild', 'migration')),
    state text NOT NULL CHECK (state IN (
        'uploading', 'queued', 'running', 'succeeded', 'failed', 'canceled', 'expired'
    )),
    source_artifact_id text REFERENCES rin_renderer.render_artifacts(id),
    result_artifact_id text REFERENCES rin_renderer.render_artifacts(id),
    project_hash character(64) NOT NULL CHECK (project_hash ~ '^[0-9a-f]{64}$'),
    options_hash character(64) NOT NULL CHECK (options_hash ~ '^[0-9a-f]{64}$'),
    renderer_version text NOT NULL CHECK (renderer_version <> ''),
    max_attempts smallint NOT NULL DEFAULT 1 CHECK (max_attempts BETWEEN 1 AND 16),
    queued_at timestamptz,
    available_at timestamptz,
    started_at timestamptz,
    finished_at timestamptz,
    expires_at timestamptz NOT NULL,
    cancel_requested boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (state <> 'queued' OR (source_artifact_id IS NOT NULL AND queued_at IS NOT NULL)),
    CHECK (state NOT IN ('succeeded', 'failed', 'canceled', 'expired') OR finished_at IS NOT NULL),
    CHECK (available_at IS NULL OR queued_at IS NOT NULL)
);

CREATE INDEX render_jobs_queue_idx
    ON rin_renderer.render_jobs (priority_class, available_at, queued_at, id)
    WHERE state = 'queued' AND cancel_requested = false;

CREATE INDEX render_jobs_resource_queue_idx
    ON rin_renderer.render_jobs (resource_class, available_at, queued_at, id)
    WHERE state = 'queued' AND cancel_requested = false;

CREATE INDEX render_jobs_principal_active_idx
    ON rin_renderer.render_jobs (principal_id, owner_scope, state, queued_at)
    WHERE state IN ('uploading', 'queued', 'running');

CREATE INDEX render_jobs_expiry_idx
    ON rin_renderer.render_jobs (expires_at, id)
    WHERE state IN ('succeeded', 'failed', 'canceled', 'expired');

CREATE TABLE rin_renderer.render_attempts (
    id uuid PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES rin_renderer.render_jobs(id) ON DELETE CASCADE,
    attempt_no smallint NOT NULL CHECK (attempt_no > 0),
    worker_id text,
    lease_token_hash character(64) CHECK (
        lease_token_hash IS NULL OR lease_token_hash ~ '^[0-9a-f]{64}$'
    ),
    lease_expires_at timestamptz,
    heartbeat_at timestamptz,
    state text NOT NULL CHECK (state IN (
        'leased', 'running', 'succeeded', 'failed', 'canceled', 'expired'
    )),
    error_code text,
    duration_ms bigint CHECK (duration_ms IS NULL OR duration_ms >= 0),
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (job_id, attempt_no),
    CHECK ((lease_token_hash IS NULL) = (lease_expires_at IS NULL))
);

CREATE INDEX render_attempts_expired_lease_idx
    ON rin_renderer.render_attempts (lease_expires_at, job_id)
    WHERE state IN ('leased', 'running') AND lease_expires_at IS NOT NULL;

CREATE TABLE rin_renderer.render_job_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES rin_renderer.render_jobs(id) ON DELETE CASCADE,
    event_type text NOT NULL CHECK (event_type <> ''),
    stage text,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX render_job_events_job_idx
    ON rin_renderer.render_job_events (job_id, id);

CREATE TABLE rin_renderer.render_cache_entries (
    cache_key text PRIMARY KEY CHECK (cache_key <> ''),
    stage text NOT NULL CHECK (stage <> ''),
    artifact_id text NOT NULL REFERENCES rin_renderer.render_artifacts(id),
    schema_version text NOT NULL CHECK (schema_version <> ''),
    renderer_version text NOT NULL CHECK (renderer_version <> ''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_hit_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX render_cache_entries_expiry_idx
    ON rin_renderer.render_cache_entries (expires_at, cache_key);

CREATE TABLE rin_renderer.workload_profiles (
    profile_key text NOT NULL CHECK (profile_key <> ''),
    renderer_version text NOT NULL CHECK (renderer_version <> ''),
    sample_count integer NOT NULL DEFAULT 0 CHECK (sample_count >= 0),
    p50_ms bigint CHECK (p50_ms IS NULL OR p50_ms >= 0),
    p90_ms bigint CHECK (p90_ms IS NULL OR p90_ms >= 0),
    mean_ms bigint CHECK (mean_ms IS NULL OR mean_ms >= 0),
    samples_ms bigint[] NOT NULL DEFAULT '{}'::bigint[],
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (profile_key, renderer_version),
    CHECK (cardinality(samples_ms) <= 200),
    CHECK (0 <= ALL (samples_ms)),
    CHECK (sample_count >= cardinality(samples_ms))
);

CREATE TABLE rin_renderer.idempotency_keys (
    principal_id text NOT NULL CHECK (principal_id <> ''),
    owner_scope text NOT NULL DEFAULT ''::text,
    idempotency_key text NOT NULL CHECK (idempotency_key <> ''),
    request_hash character(64) NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    job_id uuid NOT NULL REFERENCES rin_renderer.render_jobs(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (principal_id, owner_scope, idempotency_key)
);

CREATE INDEX idempotency_keys_expiry_idx
    ON rin_renderer.idempotency_keys (expires_at, principal_id, owner_scope);
