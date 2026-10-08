ALTER TABLE rin_renderer.render_cache_entries
    ADD CONSTRAINT render_cache_entries_stage_v1_check
    CHECK (stage IN (
        'analysis', 'document-draft', 'math', 'diagram', 'code', 'page-finalizer', 'project-result'
    )) NOT VALID;

ALTER TABLE rin_renderer.render_cache_entries
    ADD CONSTRAINT render_cache_entries_key_v1_check
    CHECK (cache_key ~ '^rin-cache/v1/(analysis|document-draft|math|diagram|code|page-finalizer|project-result)/[0-9a-f]{64}$') NOT VALID;

ALTER TABLE rin_renderer.render_cache_entries
    ADD CONSTRAINT render_cache_entries_version_v1_check
    CHECK (renderer_version = 'rin-cache-key/v1') NOT VALID;

ALTER TABLE rin_renderer.render_cache_entries
    ADD CONSTRAINT render_cache_entries_positive_retention_check
    CHECK (expires_at > created_at) NOT VALID;

CREATE INDEX render_cache_entries_stage_hit_idx
    ON rin_renderer.render_cache_entries (stage, last_hit_at, cache_key);
