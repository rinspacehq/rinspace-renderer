-- Additive Typst queue support. Ordinary workers remain safe because the
-- general document worker claims an explicit document class list that excludes
-- the Typst classes, and a Typst PDF job is only claimable by a worker holding
-- the matching resource-class leadership.
ALTER TABLE rin_renderer.render_jobs
    DROP CONSTRAINT render_jobs_content_kind_check,
    ADD CONSTRAINT render_jobs_content_kind_check
        CHECK (content_kind IN ('latex', 'markdown', 'typst')) NOT VALID,
    DROP CONSTRAINT render_jobs_resource_class_check,
    ADD CONSTRAINT render_jobs_resource_class_check CHECK (resource_class IN (
        'document-light', 'document-latexml', 'math-node', 'texsvg', 'batch-migration',
        'latex-pdf', 'document-typst', 'typst-pdf'
    )) NOT VALID;

ALTER TABLE rin_renderer.resource_allocations
    DROP CONSTRAINT resource_allocations_resource_class_check,
    ADD CONSTRAINT resource_allocations_resource_class_check CHECK (resource_class IN (
        'document-light', 'document-latexml', 'math-node', 'texsvg', 'batch-migration',
        'latex-pdf', 'document-typst', 'typst-pdf'
    )) NOT VALID;

-- Defense in depth for the cross-process queue invariants. Typst previews keep
-- their own one-running and one-queued-per-context invariant and never share a
-- pool with LaTeX previews.
CREATE UNIQUE INDEX render_jobs_typst_pdf_context_running_uidx
    ON rin_renderer.render_jobs (principal_id, owner_scope)
    WHERE resource_class = 'typst-pdf' AND state = 'running';

CREATE UNIQUE INDEX render_jobs_typst_pdf_context_queued_uidx
    ON rin_renderer.render_jobs (principal_id, owner_scope)
    WHERE resource_class = 'typst-pdf' AND state = 'queued' AND cancel_requested = false;

-- A trusted preview cache hit is scoped to the authenticated context and only
-- considers successful jobs whose private result artifact is still retained.
CREATE INDEX render_jobs_typst_pdf_content_cache_idx
    ON rin_renderer.render_jobs (
        principal_id, owner_scope, project_hash, options_hash, renderer_version,
        finished_at DESC, id
    )
    WHERE resource_class = 'typst-pdf' AND state = 'succeeded'
        AND result_artifact_id IS NOT NULL;

ALTER TABLE rin_renderer.render_jobs
    VALIDATE CONSTRAINT render_jobs_content_kind_check;
ALTER TABLE rin_renderer.render_jobs
    VALIDATE CONSTRAINT render_jobs_resource_class_check;
ALTER TABLE rin_renderer.resource_allocations
    VALIDATE CONSTRAINT resource_allocations_resource_class_check;
