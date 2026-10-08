-- Additive preview queue support. Ordinary workers remain safe because claiming
-- latex-pdf requires an explicit worker resource-class filter in application code.
ALTER TABLE rin_renderer.render_jobs
    DROP CONSTRAINT render_jobs_resource_class_check,
    ADD CONSTRAINT render_jobs_resource_class_check CHECK (resource_class IN (
        'document-light', 'document-latexml', 'math-node', 'texsvg', 'batch-migration',
        'latex-pdf'
    )) NOT VALID,
    DROP CONSTRAINT render_jobs_priority_class_check,
    ADD CONSTRAINT render_jobs_priority_class_check
        CHECK (priority_class IN ('publish', 'preview', 'rebuild', 'migration')) NOT VALID,
    ADD COLUMN superseded_by uuid REFERENCES rin_renderer.render_jobs(id),
    ADD CONSTRAINT render_jobs_superseded_state_check
        CHECK (superseded_by IS NULL OR state = 'canceled') NOT VALID;

ALTER TABLE rin_renderer.scheduler_priority_state
    DROP CONSTRAINT scheduler_priority_state_priority_class_check,
    ADD CONSTRAINT scheduler_priority_state_priority_class_check
        CHECK (priority_class IN ('publish', 'preview', 'rebuild', 'migration')) NOT VALID;

INSERT INTO rin_renderer.scheduler_priority_state (priority_class)
VALUES ('preview')
ON CONFLICT (priority_class) DO NOTHING;

ALTER TABLE rin_renderer.resource_allocations
    DROP CONSTRAINT resource_allocations_resource_class_check,
    ADD CONSTRAINT resource_allocations_resource_class_check CHECK (resource_class IN (
        'document-light', 'document-latexml', 'math-node', 'texsvg', 'batch-migration',
        'latex-pdf'
    )) NOT VALID;

-- Defense in depth for the cross-process queue invariants. CommitAdmission
-- supersedes an older queued preview before promoting the latest reservation.
CREATE UNIQUE INDEX render_jobs_latex_pdf_context_running_uidx
    ON rin_renderer.render_jobs (principal_id, owner_scope)
    WHERE resource_class = 'latex-pdf' AND state = 'running';

CREATE UNIQUE INDEX render_jobs_latex_pdf_context_queued_uidx
    ON rin_renderer.render_jobs (principal_id, owner_scope)
    WHERE resource_class = 'latex-pdf' AND state = 'queued' AND cancel_requested = false;

-- A trusted preview cache hit is scoped to the authenticated context and only
-- considers successful jobs whose private result artifact is still retained.
CREATE INDEX render_jobs_latex_pdf_content_cache_idx
    ON rin_renderer.render_jobs (
        principal_id, owner_scope, project_hash, options_hash, renderer_version,
        finished_at DESC, id
    )
    WHERE resource_class = 'latex-pdf' AND state = 'succeeded'
        AND result_artifact_id IS NOT NULL;

ALTER TABLE rin_renderer.render_jobs
    VALIDATE CONSTRAINT render_jobs_resource_class_check;
ALTER TABLE rin_renderer.render_jobs
    VALIDATE CONSTRAINT render_jobs_priority_class_check;
ALTER TABLE rin_renderer.render_jobs
    VALIDATE CONSTRAINT render_jobs_superseded_state_check;
ALTER TABLE rin_renderer.scheduler_priority_state
    VALIDATE CONSTRAINT scheduler_priority_state_priority_class_check;
ALTER TABLE rin_renderer.resource_allocations
    VALIDATE CONSTRAINT resource_allocations_resource_class_check;
