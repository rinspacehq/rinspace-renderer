DO $grants$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'rin_renderer_service') THEN
        EXECUTE 'GRANT USAGE ON SCHEMA rin_renderer TO rin_renderer_service';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA rin_renderer TO rin_renderer_service';
        EXECUTE 'GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA rin_renderer TO rin_renderer_service';
        EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA rin_renderer GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO rin_renderer_service';
        EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA rin_renderer GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO rin_renderer_service';
    END IF;
END
$grants$;
