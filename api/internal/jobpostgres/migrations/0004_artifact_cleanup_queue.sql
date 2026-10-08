CREATE TABLE rin_renderer.artifact_cleanup_queue (
    id text PRIMARY KEY,
    sha256 character(64) NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    kind text NOT NULL CHECK (kind IN ('source', 'result', 'debug')),
    storage_key text NOT NULL CHECK (storage_key <> ''),
    byte_size bigint NOT NULL CHECK (byte_size >= 0),
    media_type text NOT NULL CHECK (media_type <> ''),
    schema_version text,
    expires_at timestamptz NOT NULL,
    staged_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX artifact_cleanup_queue_staged_idx
    ON rin_renderer.artifact_cleanup_queue (staged_at, id);
