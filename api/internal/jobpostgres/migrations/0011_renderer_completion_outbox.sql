CREATE TABLE rin_renderer.renderer_completion_outbox (
    event_id text PRIMARY KEY
        CHECK (event_id ~ '^renderer:[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}:completed:1$'),
    job_id uuid NOT NULL REFERENCES rin_renderer.render_jobs(id) ON DELETE RESTRICT,
    schema_version text NOT NULL CHECK (schema_version = 'rin-renderer-completion/v2'),
    protocol_version text NOT NULL CHECK (protocol_version = 'v2'),
    terminal_version smallint NOT NULL CHECK (terminal_version = 1),
    request_id text NOT NULL CHECK (request_id ~ '^render-[0-9a-f]{32}$'),
    control_project_id text NOT NULL CHECK (control_project_id ~ '^(article|book|tag-wiki|pdf):[1-9][0-9]*$'),
    source_commit text NOT NULL CHECK (source_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    control_project_hash character(64) NOT NULL CHECK (control_project_hash ~ '^[0-9a-f]{64}$' AND control_project_hash !~ '^0+$'),
    renderer_project_hash character(64) NOT NULL CHECK (renderer_project_hash ~ '^[0-9a-f]{64}$' AND renderer_project_hash !~ '^0+$'),
    terminal_state text NOT NULL CHECK (terminal_state IN ('succeeded','failed','canceled')),
    result_reference text NOT NULL DEFAULT ''
        CHECK (result_reference = '' OR (length(result_reference) <= 2048 AND result_reference !~ '[[:cntrl:]]')),
    result_hash character(64) NOT NULL DEFAULT ''
        CHECK (result_hash = '' OR (result_hash ~ '^[0-9a-f]{64}$' AND result_hash !~ '^0+$')),
    failure_code text NOT NULL DEFAULT '' CHECK (failure_code IN (
        '', 'document_invalid', 'document_render_failed', 'document_render_timeout',
        'invalid_book_manifest', 'invalid_compatibility_result', 'invalid_project_source',
        'invalid_render_result', 'invalid_request_metadata', 'job_canceled',
        'result_storage_failed', 'source_artifact_unavailable', 'worker_lease_expired',
        'worker_shutting_down'
    )),
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','delivering','failed','delivered','dead_letter')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at timestamptz,
    lease_token_hash character(64)
        CHECK (lease_token_hash IS NULL OR (lease_token_hash ~ '^[0-9a-f]{64}$' AND lease_token_hash !~ '^0+$')),
    lease_until timestamptz,
    delivered_at timestamptz,
    dead_lettered_at timestamptz,
    last_error_code text NOT NULL DEFAULT ''
        CHECK (last_error_code = '' OR last_error_code ~ '^[a-z][a-z0-9_]{0,119}$'),
    occurred_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (job_id, terminal_version),
    CHECK ((lease_token_hash IS NULL) = (lease_until IS NULL)),
    CHECK ((state = 'delivered') = (delivered_at IS NOT NULL)),
    CHECK ((state = 'dead_letter') = (dead_lettered_at IS NOT NULL)),
    CHECK (
        (terminal_state = 'succeeded' AND result_reference <> '' AND result_hash <> '' AND failure_code = '')
        OR
        (terminal_state IN ('failed','canceled') AND result_reference = '' AND result_hash = '' AND failure_code <> '')
    )
);

CREATE INDEX renderer_completion_outbox_delivery_idx
    ON rin_renderer.renderer_completion_outbox (state, next_attempt_at, lease_until, occurred_at, event_id)
    WHERE state IN ('pending','delivering','failed');

CREATE INDEX renderer_completion_outbox_delivered_retention_idx
    ON rin_renderer.renderer_completion_outbox (delivered_at, event_id)
    WHERE state = 'delivered';

CREATE FUNCTION rin_renderer.validate_renderer_completion_outbox_job()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
DECLARE
    job_state text;
BEGIN
    SELECT state INTO job_state
    FROM rin_renderer.render_jobs
    WHERE id = NEW.job_id
    FOR KEY SHARE;
    IF job_state IS NULL OR job_state <> NEW.terminal_state THEN
        RAISE EXCEPTION USING
            ERRCODE = 'check_violation',
            MESSAGE = 'renderer completion outbox terminal state does not match its job';
    END IF;
    RETURN NEW;
END
$function$;

CREATE TRIGGER renderer_completion_outbox_terminal_job
    BEFORE INSERT ON rin_renderer.renderer_completion_outbox
    FOR EACH ROW EXECUTE FUNCTION rin_renderer.validate_renderer_completion_outbox_job();

DO $grants$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'rin_renderer_service') THEN
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON rin_renderer.renderer_completion_outbox TO rin_renderer_service';
    END IF;
END
$grants$;
