-- A Renderer job can reach more than one terminal outcome: after a terminal
-- failure an operator may retry the same renderer_job_id, and the retry gets its
-- own terminal outcome. The completion outbox therefore needs a per-job
-- terminal ordinal instead of the fixed 1 that made every later outcome
-- collide with the first one. The ordinal is the attempt number for
-- attempt-driven outcomes and the next free ordinal for a queued cancellation.
--
-- The outbox already stores one row per (job_id, terminal_version); only the
-- literal-1 checks on event_id and terminal_version need widening. Historical
-- rows keep terminal_version = 1, so the widened constraint is satisfied
-- without rewriting data.
ALTER TABLE rin_renderer.renderer_completion_outbox
    DROP CONSTRAINT renderer_completion_outbox_event_id_check,
    ADD CONSTRAINT renderer_completion_outbox_event_id_check CHECK (
        event_id ~ '^renderer:[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}:completed:[1-9][0-9]*$'
    ) NOT VALID,
    DROP CONSTRAINT renderer_completion_outbox_terminal_version_check,
    ADD CONSTRAINT renderer_completion_outbox_terminal_version_check CHECK (
        terminal_version >= 1
    ) NOT VALID;

ALTER TABLE rin_renderer.renderer_completion_outbox
    VALIDATE CONSTRAINT renderer_completion_outbox_event_id_check;
ALTER TABLE rin_renderer.renderer_completion_outbox
    VALIDATE CONSTRAINT renderer_completion_outbox_terminal_version_check;
