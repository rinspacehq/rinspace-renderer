package jobpostgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

func TestLoadMigrations(t *testing.T) {
	items, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(items) != 14 {
		t.Fatalf("migration count = %d, want 14", len(items))
	}
	if items[0].version != 1 || items[0].name != "control_plane" {
		t.Fatalf("migration identity = (%d, %q)", items[0].version, items[0].name)
	}
	if items[1].version != 2 || items[1].name != "admission_reservations" {
		t.Fatalf("second migration identity = (%d, %q)", items[1].version, items[1].name)
	}
	if items[2].version != 3 || items[2].name != "fair_resource_scheduling" {
		t.Fatalf("third migration identity = (%d, %q)", items[2].version, items[2].name)
	}
	if items[3].version != 4 || items[3].name != "artifact_cleanup_queue" {
		t.Fatalf("fourth migration identity = (%d, %q)", items[3].version, items[3].name)
	}
	if items[4].version != 5 || items[4].name != "job_request_metadata" {
		t.Fatalf("fifth migration identity = (%d, %q)", items[4].version, items[4].name)
	}
	if items[5].version != 6 || items[5].name != "workload_sampling" {
		t.Fatalf("sixth migration identity = (%d, %q)", items[5].version, items[5].name)
	}
	if items[6].version != 7 || items[6].name != "wait_estimation" {
		t.Fatalf("seventh migration identity = (%d, %q)", items[6].version, items[6].name)
	}
	if items[7].version != 8 || items[7].name != "content_cache_index" {
		t.Fatalf("eighth migration identity = (%d, %q)", items[7].version, items[7].name)
	}
	if items[8].version != 9 || items[8].name != "runtime_role_grants" {
		t.Fatalf("ninth migration identity = (%d, %q)", items[8].version, items[8].name)
	}
	if items[9].version != 10 || items[9].name != "admission_eta_profiles" {
		t.Fatalf("tenth migration identity = (%d, %q)", items[9].version, items[9].name)
	}
	if items[10].version != 11 || items[10].name != "renderer_completion_outbox" {
		t.Fatalf("eleventh migration identity = (%d, %q)", items[10].version, items[10].name)
	}
	if items[11].version != 12 || items[11].name != "latex_pdf_preview_queue" {
		t.Fatalf("twelfth migration identity = (%d, %q)", items[11].version, items[11].name)
	}
	if items[12].version != 13 || items[12].name != "typst_queue" {
		t.Fatalf("thirteenth migration identity = (%d, %q)", items[12].version, items[12].name)
	}
	if items[13].version != 14 || items[13].name != "renderer_completion_terminal_version" {
		t.Fatalf("fourteenth migration identity = (%d, %q)", items[13].version, items[13].name)
	}
	if len(items[0].checksum) != 64 || strings.Trim(items[0].checksum, "0123456789abcdef") != "" {
		t.Fatalf("migration checksum is not lowercase SHA-256: %q", items[0].checksum)
	}
}

func TestPostgresMigrationAndRepository(t *testing.T) {
	dataSourceName := os.Getenv("RIN_RENDERER_TEST_DATABASE_URL")
	if dataSourceName == "" {
		t.Skip("RIN_RENDERER_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	repository, err := Open(ctx, dataSourceName)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	// This test database is created solely for this CI job. The public probe verifies that Renderer
	// migrations remain additive and do not mutate application-owned schemas.
	if _, err := repository.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS rin_renderer CASCADE`); err != nil {
		t.Fatalf("reset isolated renderer schema: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS public.rinspace_compatibility_probe (
			id integer PRIMARY KEY,
			value text NOT NULL
		);
		INSERT INTO public.rinspace_compatibility_probe (id, value)
		VALUES (1, 'preserve-me')
		ON CONFLICT (id) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("prepare compatibility probe: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		DO $role$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'rin_renderer_service') THEN
				CREATE ROLE rin_renderer_service NOLOGIN;
			END IF;
		END
		$role$`); err != nil {
		t.Fatalf("prepare isolated runtime role: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `CREATE SCHEMA rin_renderer AUTHORIZATION rin_renderer_service`); err != nil {
		t.Fatalf("prepare mismatched renderer schema owner: %v", err)
	}
	if err := repository.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "does not match migration role") {
		t.Fatalf("Migrate() with mismatched schema owner error = %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		DROP SCHEMA rin_renderer CASCADE;
		CREATE SCHEMA rin_renderer AUTHORIZATION CURRENT_USER`); err != nil {
		t.Fatalf("prepare control-plane-provisioned renderer schema: %v", err)
	}

	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("repeat Migrate() error = %v", err)
	}
	if err := repository.Ready(ctx); err != nil {
		t.Fatalf("repository readiness: %v", err)
	}
	if err := repository.VerifyMigrations(ctx); err != nil {
		t.Fatalf("verify current migrations: %v", err)
	}

	var migrationCount, tableCount int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM rin_renderer.schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if migrationCount != 14 {
		t.Fatalf("migration count = %d, want 14", migrationCount)
	}
	if err := repository.db.QueryRowContext(ctx, `
		SELECT count(*)
		FROM information_schema.tables
		WHERE table_schema = 'rin_renderer'`).Scan(&tableCount); err != nil {
		t.Fatalf("count renderer tables: %v", err)
	}
	if tableCount != 14 {
		t.Fatalf("renderer table count = %d, want 14", tableCount)
	}
	var probe string
	if err := repository.db.QueryRowContext(ctx, `SELECT value FROM public.rinspace_compatibility_probe WHERE id = 1`).Scan(&probe); err != nil {
		t.Fatalf("read compatibility probe: %v", err)
	}
	if probe != "preserve-me" {
		t.Fatalf("compatibility probe = %q", probe)
	}
	var rendererSchema, rendererTable, completionTable, applicationTable bool
	if err := repository.db.QueryRowContext(ctx, `
		SELECT
			has_schema_privilege('rin_renderer_service', 'rin_renderer', 'USAGE'),
			has_table_privilege('rin_renderer_service', 'rin_renderer.render_jobs', 'SELECT,INSERT,UPDATE,DELETE'),
			has_table_privilege('rin_renderer_service', 'rin_renderer.renderer_completion_outbox', 'SELECT,INSERT,UPDATE,DELETE'),
			has_table_privilege('rin_renderer_service', 'public.rinspace_compatibility_probe', 'SELECT')`).Scan(
		&rendererSchema, &rendererTable, &completionTable, &applicationTable,
	); err != nil {
		t.Fatalf("read runtime role grants: %v", err)
	}
	if !rendererSchema || !rendererTable || !completionTable || applicationTable {
		t.Fatalf("runtime grants schema=%v renderer_table=%v completion_table=%v application_table=%v", rendererSchema, rendererTable, completionTable, applicationTable)
	}

	wantIndexes := []string{
		"artifact_cleanup_queue_staged_idx",
		"idempotency_keys_expiry_idx",
		"render_artifacts_expiry_idx",
		"render_attempts_expired_lease_idx",
		"render_cache_entries_expiry_idx",
		"render_cache_entries_stage_hit_idx",
		"renderer_completion_outbox_delivered_retention_idx",
		"renderer_completion_outbox_delivery_idx",
		"render_job_events_job_idx",
		"render_jobs_expiry_idx",
		"render_jobs_abandoned_reservation_idx",
		"render_jobs_principal_active_idx",
		"render_jobs_queue_idx",
		"render_jobs_resource_queue_idx",
		"render_job_workloads_admission_profile_idx",
		"render_job_workloads_profile_idx",
		"resource_allocations_attempt_idx",
		"resource_allocations_capacity_idx",
		"resource_allocations_expiry_idx",
		"resource_allocations_job_idx",
	}
	rows, err := repository.db.QueryContext(ctx, `
		SELECT indexname
		FROM pg_indexes
		WHERE schemaname = 'rin_renderer' AND indexname = ANY($1::text[])
		ORDER BY indexname`, stringsToPostgresArray(wantIndexes))
	if err != nil {
		t.Fatalf("query renderer indexes: %v", err)
	}
	var gotIndexes []string
	for rows.Next() {
		var index string
		if err := rows.Scan(&index); err != nil {
			rows.Close()
			t.Fatalf("scan renderer index: %v", err)
		}
		gotIndexes = append(gotIndexes, index)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close renderer index rows: %v", err)
	}
	sort.Strings(wantIndexes)
	if strings.Join(gotIndexes, ",") != strings.Join(wantIndexes, ",") {
		t.Fatalf("renderer indexes = %v, want %v", gotIndexes, wantIndexes)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	completionJob, err := repository.CreateJob(ctx, Job{
		ID: "77777777-7777-4777-8777-777777777777", PrincipalID: "rinspace-publish",
		OwnerScope: "article:777", ContentKind: "markdown", DocumentEngine: "unified",
		ResourceClass: "document-light", PriorityClass: "publish", State: "failed",
		ProjectHash: strings.Repeat("7", 64), OptionsHash: strings.Repeat("8", 64),
		RendererVersion: "test-version", MaxAttempts: 1, FinishedAt: timePointer(now),
		ExpiresAt: now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create completion test job: %v", err)
	}
	completionInput := CompletionOutboxInput{
		JobID: completionJob.ID, RequestID: "render-77777777777777777777777777777777",
		ControlProjectID: "article:777", SourceCommit: strings.Repeat("a", 40),
		ControlProjectHash: strings.Repeat("b", 64), RendererProjectHash: strings.Repeat("7", 64),
		TerminalState: "failed", FailureCode: "document_render_failed", OccurredAt: now, TerminalVersion: 1,
	}
	insertCompletion := func(input CompletionOutboxInput) error {
		tx, beginErr := repository.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()
		if insertErr := insertCompletionOutboxTx(ctx, tx, input); insertErr != nil {
			return insertErr
		}
		return tx.Commit()
	}
	if err := insertCompletion(completionInput); err != nil {
		t.Fatalf("insert completion outbox: %v", err)
	}
	if err := insertCompletion(completionInput); err != nil {
		t.Fatalf("repeat completion outbox insert: %v", err)
	}
	if health, err := repository.CompletionHealth(ctx, now); err != nil || health.State != "healthy" ||
		health.Pending != 1 || health.Failed != 0 || health.Delivering != 0 || health.Dead != 0 || health.LastAcceptedAt != nil {
		t.Fatalf("pending completion health = %#v, %v", health, err)
	}
	conflictingCompletion := completionInput
	conflictingCompletion.FailureCode = "document_render_timeout"
	if err := insertCompletion(conflictingCompletion); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting completion outbox insert error = %v", err)
	}
	unversionedCompletion := completionInput
	unversionedCompletion.TerminalVersion = 0
	if err := insertCompletion(unversionedCompletion); err == nil || !strings.Contains(err.Error(), "terminal version is invalid") {
		t.Fatalf("unversioned completion outbox insert error = %v", err)
	}

	firstToken := strings.Repeat("1", 64)
	firstDelivery, err := repository.ClaimCompletion(ctx, ClaimCompletionInput{
		LeaseTokenHash: firstToken, Now: now.Add(time.Second), LeaseExpiresAt: now.Add(31 * time.Second),
	})
	if err != nil || firstDelivery.EventID != "renderer:77777777-7777-4777-8777-777777777777:completed:1" ||
		firstDelivery.AttemptCount != 1 || firstDelivery.FailureCode != completionInput.FailureCode || firstDelivery.ResultHash != "" {
		t.Fatalf("first completion claim = %#v, %v", firstDelivery, err)
	}
	if _, err := repository.ClaimCompletion(ctx, ClaimCompletionInput{
		LeaseTokenHash: strings.Repeat("2", 64), Now: now.Add(2 * time.Second), LeaseExpiresAt: now.Add(32 * time.Second),
	}); !errors.Is(err, ErrNoCompletionDelivery) {
		t.Fatalf("active completion lease was reclaimed early: %v", err)
	}
	restartedRepository, err := Open(ctx, dataSourceName)
	if err != nil {
		t.Fatalf("reopen Renderer repository for completion recovery: %v", err)
	}
	defer restartedRepository.Close()
	if err := restartedRepository.VerifyMigrations(ctx); err != nil {
		t.Fatalf("verify migrations after Renderer repository restart: %v", err)
	}
	secondToken := strings.Repeat("3", 64)
	secondDelivery, err := restartedRepository.ClaimCompletion(ctx, ClaimCompletionInput{
		LeaseTokenHash: secondToken, Now: now.Add(32 * time.Second), LeaseExpiresAt: now.Add(62 * time.Second),
	})
	if err != nil || secondDelivery.AttemptCount != 2 {
		t.Fatalf("reclaimed completion = %#v, %v", secondDelivery, err)
	}
	if err := restartedRepository.AckCompletion(ctx, secondDelivery.EventID, firstToken, now.Add(33*time.Second)); !errors.Is(err, ErrCompletionLeaseLost) {
		t.Fatalf("stale completion token acknowledgement error = %v", err)
	}
	state, err := restartedRepository.FailCompletion(ctx, secondDelivery.EventID, secondToken, now.Add(33*time.Second), "control_plane_unavailable", 2)
	if err != nil || state != "dead_letter" {
		t.Fatalf("dead-letter completion = %q, %v", state, err)
	}
	if health, err := repository.CompletionHealth(ctx, now.Add(33*time.Second)); err != nil ||
		health.State != "degraded" || health.Dead != 1 || health.OldestAgeSeconds != 33 {
		t.Fatalf("dead-letter completion health = %#v, %v", health, err)
	}
	if err := restartedRepository.ReplayCompletion(ctx, secondDelivery.EventID, now.Add(34*time.Second)); err != nil {
		t.Fatalf("replay completion: %v", err)
	}
	thirdToken := strings.Repeat("4", 64)
	thirdDelivery, err := repository.ClaimCompletion(ctx, ClaimCompletionInput{
		LeaseTokenHash: thirdToken, Now: now.Add(35 * time.Second), LeaseExpiresAt: now.Add(65 * time.Second),
	})
	if err != nil || thirdDelivery.AttemptCount != 1 {
		t.Fatalf("replayed completion claim = %#v, %v", thirdDelivery, err)
	}
	if err := repository.AckCompletion(ctx, thirdDelivery.EventID, thirdToken, now.Add(36*time.Second)); err != nil {
		t.Fatalf("ack replayed completion: %v", err)
	}
	if health, err := repository.CompletionHealth(ctx, now.Add(36*time.Second)); err != nil ||
		health.State != "healthy" || health.Pending != 0 || health.Delivering != 0 || health.Dead != 0 ||
		health.LastAcceptedAt == nil || !health.LastAcceptedAt.Equal(now.Add(36*time.Second)) {
		t.Fatalf("accepted completion health = %#v, %v", health, err)
	}
	// A retried job publishes its later terminal outcome under its own ordinal
	// instead of colliding with the first one, and that ordinal survives the
	// duplicate and conflict checks the first outcome already passed.
	retriedCompletion := completionInput
	retriedCompletion.TerminalVersion = 2
	retriedCompletion.FailureCode = "document_render_timeout"
	retriedCompletion.OccurredAt = now.Add(37 * time.Second)
	if err := insertCompletion(retriedCompletion); err != nil {
		t.Fatalf("insert retried completion outbox: %v", err)
	}
	if err := insertCompletion(retriedCompletion); err != nil {
		t.Fatalf("repeat retried completion outbox insert: %v", err)
	}
	conflictingRetried := retriedCompletion
	conflictingRetried.FailureCode = "document_invalid"
	if err := insertCompletion(conflictingRetried); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting retried completion outbox insert error = %v", err)
	}
	retriedToken := strings.Repeat("5", 64)
	retriedDelivery, err := repository.ClaimCompletion(ctx, ClaimCompletionInput{
		LeaseTokenHash: retriedToken, Now: now.Add(38 * time.Second), LeaseExpiresAt: now.Add(68 * time.Second),
	})
	if err != nil || retriedDelivery.EventID != "renderer:77777777-7777-4777-8777-777777777777:completed:2" ||
		retriedDelivery.TerminalVersion != 2 || retriedDelivery.FailureCode != "document_render_timeout" {
		t.Fatalf("retried completion claim = %#v, %v", retriedDelivery, err)
	}
	if err := repository.AckCompletion(ctx, retriedDelivery.EventID, retriedToken, now.Add(39*time.Second)); err != nil {
		t.Fatalf("ack retried completion: %v", err)
	}
	if cleaned, err := repository.CleanupDeliveredCompletions(ctx, now.Add(time.Hour), 10); err != nil || cleaned != 0 {
		t.Fatalf("completion cleanup before job expiry = %d, %v", cleaned, err)
	}
	if _, err := repository.db.ExecContext(ctx, `UPDATE rin_renderer.render_jobs SET state='expired' WHERE id=$1::uuid`, completionJob.ID); err != nil {
		t.Fatalf("expire completion test job: %v", err)
	}
	if cleaned, err := repository.CleanupDeliveredCompletions(ctx, now.Add(time.Hour), 10); err != nil || cleaned != 2 {
		t.Fatalf("completion cleanup after delivery and expiry = %d, %v", cleaned, err)
	}
	var forbiddenCompletionColumns int
	if err := repository.db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema='rin_renderer' AND table_name='renderer_completion_outbox'
		  AND column_name ~ '(callback|url|payload|body|source_body|source_payload|credential|secret|raw_error)'`).Scan(&forbiddenCompletionColumns); err != nil {
		t.Fatalf("inspect completion outbox columns: %v", err)
	}
	if forbiddenCompletionColumns != 0 {
		t.Fatalf("completion outbox exposes %d forbidden fields", forbiddenCompletionColumns)
	}
	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_jobs WHERE id=$1::uuid`, completionJob.ID); err != nil {
		t.Fatalf("delete completion test job: %v", err)
	}

	artifact, err := repository.CreateArtifact(ctx, Artifact{
		ID:         "render-jobs/v1/source/11111111-1111-4111-8111-111111111111/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SHA256:     strings.Repeat("a", 64),
		Kind:       "source",
		Visibility: "private",
		StorageKey: "render-jobs/v1/source/11111111-1111-4111-8111-111111111111/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ByteSize:   42,
		MediaType:  "application/zip",
		ExpiresAt:  timePointer(now.Add(24 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	testPostgresAtomicTerminalCompletions(t, ctx, repository, artifact, now.Add(10*time.Minute))
	queuedAt := now
	job, err := repository.CreateJob(ctx, Job{
		ID:               "11111111-1111-4111-8111-111111111111",
		PrincipalID:      "principal-a",
		OwnerScope:       "book-260",
		ContentKind:      "latex",
		DocumentEngine:   "latexml",
		ResourceClass:    "document-latexml",
		PriorityClass:    "publish",
		State:            "queued",
		SourceArtifactID: artifact.ID,
		ProjectHash:      strings.Repeat("b", 64),
		OptionsHash:      strings.Repeat("c", 64),
		RendererVersion:  "test-version",
		RequestMetadata:  json.RawMessage(`{"sourceName":"book260.zip","entrypoint":"main.tex"}`),
		MaxAttempts:      2,
		QueuedAt:         &queuedAt,
		AvailableAt:      &queuedAt,
		ExpiresAt:        now.Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	gotJob, err := repository.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotJob.PrincipalID != job.PrincipalID || gotJob.SourceArtifactID != artifact.ID || gotJob.State != "queued" {
		t.Fatalf("repository job mismatch: %#v", gotJob)
	}
	var requestMetadata map[string]string
	if err := json.Unmarshal(gotJob.RequestMetadata, &requestMetadata); err != nil || requestMetadata["sourceName"] != "book260.zip" || requestMetadata["entrypoint"] != "main.tex" {
		t.Fatalf("repository job metadata = %s, %v", gotJob.RequestMetadata, err)
	}
	if _, err := repository.Job(ctx, "22222222-2222-4222-8222-222222222222"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Job() error = %v, want ErrNotFound", err)
	}

	assertDatabaseRejects(t, repository.db, ctx, `
		INSERT INTO rin_renderer.render_jobs (
			id, principal_id, content_kind, document_engine, resource_class, priority_class,
			state, project_hash, options_hash, renderer_version, expires_at
		) VALUES (
			'22222222-2222-4222-8222-222222222222', 'principal-a', 'latex', 'latexml',
			'document-latexml', 'publish', 'unknown', $1, $2, 'test-version', $3
		)`, strings.Repeat("d", 64), strings.Repeat("e", 64), now.Add(time.Hour))

	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_attempts (id, job_id, attempt_no, state)
		VALUES ('33333333-3333-4333-8333-333333333333', $1::uuid, 1, 'running')`, job.ID); err != nil {
		t.Fatalf("insert first attempt: %v", err)
	}
	assertDatabaseRejects(t, repository.db, ctx, `
		INSERT INTO rin_renderer.render_attempts (id, job_id, attempt_no, state)
		VALUES ('44444444-4444-4444-8444-444444444444', $1::uuid, 1, 'running')`, job.ID)

	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.idempotency_keys (
			principal_id, owner_scope, idempotency_key, request_hash, job_id, expires_at
		) VALUES ('principal-a', 'book-260', 'request-1', $1, $2::uuid, $3)`,
		strings.Repeat("f", 64), job.ID, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("insert first idempotency key: %v", err)
	}
	assertDatabaseRejects(t, repository.db, ctx, `
		INSERT INTO rin_renderer.idempotency_keys (
			principal_id, owner_scope, idempotency_key, request_hash, job_id, expires_at
		) VALUES ('principal-a', 'book-260', 'request-1', $1, $2::uuid, $3)`,
		strings.Repeat("f", 64), job.ID, now.Add(24*time.Hour))
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.idempotency_keys (
			principal_id, owner_scope, idempotency_key, request_hash, job_id, expires_at
		) VALUES ('principal-b', 'book-260', 'request-1', $1, $2::uuid, $3)`,
		strings.Repeat("f", 64), job.ID, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("principal-scoped idempotency key: %v", err)
	}

	admissionLimits := AdmissionLimits{GlobalNonTerminal: 10, PrincipalQueued: 8, PreviewQueued: 20, PrivateSourceBytes: 100}
	reservationInput := testAdmissionInput("55555555-5555-4555-8555-555555555555", "admission-a", "book-a", 3, now)
	reservation, err := repository.ReserveAdmission(ctx, reservationInput, admissionLimits)
	if err != nil {
		t.Fatalf("ReserveAdmission() error = %v", err)
	}
	if reservation.Reused || !reservation.Uploading || reservation.Job.State != "uploading" {
		t.Fatalf("new reservation = %#v", reservation)
	}
	reused, err := repository.ReserveAdmission(ctx, reservationInput, admissionLimits)
	if err != nil {
		t.Fatalf("reused ReserveAdmission() error = %v", err)
	}
	if !reused.Reused || !reused.Uploading || reused.Job.ID != reservation.Job.ID {
		t.Fatalf("reused reservation = %#v", reused)
	}
	conflicting := reservationInput
	conflicting.RequestHash = strings.Repeat("e", 64)
	if _, err := repository.ReserveAdmission(ctx, conflicting, admissionLimits); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting ReserveAdmission() error = %v", err)
	}

	sourceBody := []byte("abc")
	sourceHash := testDigest(sourceBody)
	sourceExpiresAt := now.Add(24 * time.Hour)
	sourceArtifactID := "render-jobs/v1/source/" + reservation.Job.ID + "/" + sourceHash
	queuedJob, err := repository.CommitAdmission(ctx, CommitAdmissionInput{
		JobID:              reservation.Job.ID,
		AdmissionTokenHash: reservationInput.AdmissionTokenHash,
		Artifact: Artifact{
			ID:            sourceArtifactID,
			SHA256:        sourceHash,
			Kind:          "source",
			Visibility:    "private",
			StorageKey:    sourceArtifactID,
			ByteSize:      int64(len(sourceBody)),
			MediaType:     "application/zip",
			SchemaVersion: "rin-project-archive/v1",
			ExpiresAt:     &sourceExpiresAt,
		},
		Workload: reservationInput.Workload,
		QueuedAt: now,
	})
	if err != nil {
		t.Fatalf("CommitAdmission() error = %v", err)
	}
	if queuedJob.State != "queued" || queuedJob.SourceArtifactID != sourceArtifactID || queuedJob.ReservationExpiresAt != nil {
		t.Fatalf("queued admission job = %#v", queuedJob)
	}
	admissionWorkload, err := repository.Workload(ctx, queuedJob.ID)
	if err != nil || admissionWorkload.FeatureStage != "admission" || !strings.Contains(admissionWorkload.ProfileKey, "/latex/latexml/unknown/") {
		t.Fatalf("admission workload = %#v, %v", admissionWorkload, err)
	}
	reused, err = repository.ReserveAdmission(ctx, reservationInput, admissionLimits)
	if err != nil || !reused.Reused || reused.Uploading || reused.Job.State != "queued" {
		t.Fatalf("accepted idempotent reservation = %#v, %v", reused, err)
	}
	var queuedEventCount int
	if err := repository.db.QueryRowContext(ctx, `
		SELECT count(*) FROM rin_renderer.render_job_events
		WHERE job_id = $1::uuid AND event_type = 'queued' AND stage = 'admission'`,
		queuedJob.ID).Scan(&queuedEventCount); err != nil || queuedEventCount != 1 {
		t.Fatalf("queued admission events = %d, %v", queuedEventCount, err)
	}

	principalLimited := testAdmissionInput("66666666-6666-4666-8666-666666666666", "admission-a", "book-a", 1, now)
	if _, err := repository.ReserveAdmission(ctx, principalLimited, AdmissionLimits{
		GlobalNonTerminal: 100, PrincipalQueued: 1, PreviewQueued: 20, PrivateSourceBytes: 100,
	}); !errors.Is(err, ErrPrincipalQuota) {
		t.Fatalf("principal-limited ReserveAdmission() error = %v", err)
	}
	globalLimited := testAdmissionInput("77777777-7777-4777-8777-777777777777", "admission-b", "book-b", 1, now)
	if _, err := repository.ReserveAdmission(ctx, globalLimited, AdmissionLimits{
		GlobalNonTerminal: 2, PrincipalQueued: 8, PreviewQueued: 20, PrivateSourceBytes: 100,
	}); !errors.Is(err, ErrGlobalCapacity) {
		t.Fatalf("global-limited ReserveAdmission() error = %v", err)
	}
	storageLimited := testAdmissionInput("88888888-8888-4888-8888-888888888888", "admission-c", "book-c", 2, now)
	if _, err := repository.ReserveAdmission(ctx, storageLimited, AdmissionLimits{
		GlobalNonTerminal: 100, PrincipalQueued: 8, PreviewQueued: 20, PrivateSourceBytes: 46,
	}); !errors.Is(err, ErrStorageCapacity) {
		t.Fatalf("storage-limited ReserveAdmission() error = %v", err)
	}

	abortInput := testAdmissionInput("99999999-9999-4999-8999-999999999999", "admission-d", "book-d", 1, now)
	if _, err := repository.ReserveAdmission(ctx, abortInput, admissionLimits); err != nil {
		t.Fatalf("reserve abort candidate: %v", err)
	}
	if err := repository.AbortAdmission(ctx, abortInput.Job.ID, abortInput.AdmissionTokenHash); err != nil {
		t.Fatalf("AbortAdmission() error = %v", err)
	}
	if _, err := repository.Job(ctx, abortInput.Job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("aborted Job() error = %v", err)
	}

	abandonedInput := testAdmissionInput("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "admission-e", "book-e", 1, now)
	abandonedInput.ReservationExpiresAt = now.Add(time.Minute)
	if _, err := repository.ReserveAdmission(ctx, abandonedInput, admissionLimits); err != nil {
		t.Fatalf("reserve abandoned candidate: %v", err)
	}
	cleaned, err := repository.CleanupAbandonedAdmissions(ctx, now.Add(2*time.Minute), 1)
	if err != nil || cleaned != 1 {
		t.Fatalf("CleanupAbandonedAdmissions() = %d, %v", cleaned, err)
	}
	if _, err := repository.Job(ctx, abandonedInput.Job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleaned Job() error = %v", err)
	}

	// The advisory admission lock makes count-and-reserve atomic: with one remaining global slot,
	// exactly one concurrent reservation succeeds.
	concurrentInputs := []AdmissionReservationInput{
		testAdmissionInput("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "concurrent-a", "", 1, now),
		testAdmissionInput("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "concurrent-b", "", 1, now),
	}
	var wait sync.WaitGroup
	concurrentErrors := make([]error, len(concurrentInputs))
	for index := range concurrentInputs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, concurrentErrors[index] = repository.ReserveAdmission(ctx, concurrentInputs[index], AdmissionLimits{
				GlobalNonTerminal: 3, PrincipalQueued: 8, PreviewQueued: 20, PrivateSourceBytes: 100,
			})
		}(index)
	}
	wait.Wait()
	successes, capacityFailures := 0, 0
	for index, concurrentErr := range concurrentErrors {
		switch {
		case concurrentErr == nil:
			successes++
			if err := repository.AbortAdmission(ctx, concurrentInputs[index].Job.ID, concurrentInputs[index].AdmissionTokenHash); err != nil {
				t.Fatalf("abort concurrent reservation: %v", err)
			}
		case errors.Is(concurrentErr, ErrGlobalCapacity):
			capacityFailures++
		default:
			t.Fatalf("concurrent ReserveAdmission() error = %v", concurrentErr)
		}
	}
	if successes != 1 || capacityFailures != 1 {
		t.Fatalf("concurrent reservations = %d success, %d capacity", successes, capacityFailures)
	}

	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_attempts WHERE job_id = $1::uuid`, job.ID); err != nil {
		t.Fatalf("clear constraint-test attempt: %v", err)
	}
	leader, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatalf("AcquireLeadership() error = %v", err)
	}
	if secondLeader, err := repository.AcquireLeadership(ctx); !errors.Is(err, ErrLeadershipUnavailable) {
		if secondLeader != nil {
			secondLeader.Close()
		}
		t.Fatalf("second AcquireLeadership() error = %v", err)
	}
	firstClaimTime := now.Add(time.Minute)
	firstLease, err := repository.ClaimNext(ctx, leader, ClaimInput{
		AttemptID:      "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		WorkerID:       "worker-a",
		LeaseTokenHash: strings.Repeat("5", 64),
		Now:            firstClaimTime,
		LeaseExpiresAt: firstClaimTime.Add(time.Minute),
		Policy:         testSchedulingPolicy(),
	})
	if err != nil {
		t.Fatalf("first ClaimNext() error = %v", err)
	}
	if firstLease.Job.ID != job.ID || firstLease.Job.State != "running" || firstLease.AttemptNo != 1 {
		t.Fatalf("first lease = %#v", firstLease)
	}
	if err := repository.Heartbeat(ctx, firstLease.AttemptID, strings.Repeat("6", 64), firstClaimTime.Add(time.Second), firstClaimTime.Add(time.Minute)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-token Heartbeat() error = %v", err)
	}
	if err := repository.Heartbeat(ctx, firstLease.AttemptID, strings.Repeat("5", 64), firstClaimTime.Add(time.Second), firstClaimTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if _, err := repository.ClaimNext(ctx, leader, ClaimInput{
		AttemptID: "ffffffff-ffff-4fff-8fff-ffffffffffff", WorkerID: "worker-blocked",
		LeaseTokenHash: strings.Repeat("8", 64), Now: firstClaimTime.Add(2 * time.Second),
		LeaseExpiresAt: firstClaimTime.Add(time.Minute), Policy: testSchedulingPolicy(),
	}); !errors.Is(err, ErrNoEligibleJob) {
		t.Fatalf("resource-limited ClaimNext() error = %v", err)
	}
	lightQueuedAt := firstClaimTime.Add(2 * time.Second)
	lightJob, err := repository.CreateJob(ctx, Job{
		ID: "15151515-1515-4515-8515-151515151515", PrincipalID: "short-markdown",
		ContentKind: "markdown", DocumentEngine: "unified", ResourceClass: "document-light",
		PriorityClass: "publish", State: "queued", SourceArtifactID: artifact.ID,
		ProjectHash: strings.Repeat("a", 64), OptionsHash: strings.Repeat("b", 64),
		RendererVersion: "test-version", MaxAttempts: 1, QueuedAt: &lightQueuedAt,
		AvailableAt: &lightQueuedAt, ExpiresAt: lightQueuedAt.Add(8 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create short Markdown job: %v", err)
	}
	lightLease, err := repository.ClaimNext(ctx, leader, ClaimInput{
		AttemptID: "16161616-1616-4616-8616-161616161616", WorkerID: "worker-light",
		LeaseTokenHash: strings.Repeat("d", 64), Now: firstClaimTime.Add(3 * time.Second),
		LeaseExpiresAt: firstClaimTime.Add(time.Minute), Policy: testSchedulingPolicy(),
	})
	if err != nil {
		t.Fatalf("independent-class ClaimNext() error = %v", err)
	}
	if lightLease.Job.ID != lightJob.ID {
		t.Fatalf("heavy LaTeX slot blocked short Markdown: %#v", lightLease.Job)
	}
	refineAt := firstClaimTime.Add(3500 * time.Millisecond)
	refinedWorkload, err := repository.RefineWorkload(ctx, RefineWorkloadInput{
		AttemptID: lightLease.AttemptID, LeaseTokenHash: strings.Repeat("d", 64), Now: refineAt,
		Refinement: WorkloadRefinement{
			DocumentEngine: "unified", DocumentClass: "book", FileCount: 4,
			PageCount: 12, MathCount: 20, DiagramCount: 2, CodeBlockCount: 3,
		},
	})
	if err != nil || refinedWorkload.FeatureStage != "analysis" || !strings.Contains(refinedWorkload.ProfileKey, "/markdown/unified/book/") || !strings.HasSuffix(refinedWorkload.ProfileKey, "/diagrams-present") ||
		refinedWorkload.AdmissionProfileKey == "" || refinedWorkload.AdmissionProfileKey == refinedWorkload.ProfileKey {
		t.Fatalf("refined workload = %#v, %v", refinedWorkload, err)
	}
	resultExpiresAt := firstClaimTime.Add(24 * time.Hour)
	lightResult := Artifact{
		ID:     "render-jobs/v1/result/15151515-1515-4515-8515-151515151515/" + strings.Repeat("e", 64) + ".json",
		SHA256: strings.Repeat("e", 64), Kind: "result", Visibility: "private",
		StorageKey: "render-jobs/v1/result/15151515-1515-4515-8515-151515151515/" + strings.Repeat("e", 64) + ".json",
		ByteSize:   99, MediaType: "application/json", SchemaVersion: "rin-render-result/v1",
		ExpiresAt: &resultExpiresAt,
	}
	finishedLightJob, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: lightLease.AttemptID, LeaseTokenHash: strings.Repeat("d", 64),
		Now: firstClaimTime.Add(4 * time.Second), Succeeded: true, ResultArtifact: &lightResult,
	})
	if err != nil {
		t.Fatalf("finish short Markdown job: %v", err)
	}
	if finishedLightJob.ResultArtifactID != lightResult.ID {
		t.Fatalf("finished result artifact = %#v", finishedLightJob)
	}
	lightProfile, err := repository.WorkloadProfile(ctx, refinedWorkload.ProfileKey, lightJob.RendererVersion)
	if err != nil || lightProfile.SampleCount != 1 || len(lightProfile.SamplesMS) != 1 || lightProfile.SamplesMS[0] != 1000 || lightProfile.FailureCount != 0 || lightProfile.P50MS == nil || *lightProfile.P50MS != 1000 {
		t.Fatalf("successful workload profile = %#v, %v", lightProfile, err)
	}
	admissionProfile, err := repository.WorkloadProfile(ctx, refinedWorkload.AdmissionProfileKey, lightJob.RendererVersion)
	if err != nil || admissionProfile.SampleCount != 1 || len(admissionProfile.SamplesMS) != 1 || admissionProfile.SamplesMS[0] != 1000 || admissionProfile.P50MS == nil || *admissionProfile.P50MS != 1000 {
		t.Fatalf("successful admission ETA profile = %#v, %v", admissionProfile, err)
	}
	storedLightResult, err := repository.Artifact(ctx, lightResult.ID)
	if err != nil || !sameArtifact(storedLightResult, lightResult) {
		t.Fatalf("atomic result artifact = %#v, %v", storedLightResult, err)
	}
	failureQueuedAt := firstClaimTime.Add(4 * time.Second)
	failureJob, err := repository.CreateJob(ctx, Job{
		ID: "17171717-1717-4717-8717-171717171717", PrincipalID: "invalid-markdown",
		ContentKind: "markdown", DocumentEngine: "unified", ResourceClass: "document-light",
		PriorityClass: "publish", State: "queued", SourceArtifactID: artifact.ID,
		ProjectHash: strings.Repeat("c", 64), OptionsHash: strings.Repeat("d", 64),
		RendererVersion: "test-version", MaxAttempts: 1, QueuedAt: &failureQueuedAt,
		AvailableAt: &failureQueuedAt, ExpiresAt: failureQueuedAt.Add(8 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create terminal failure job: %v", err)
	}
	failureLease, err := repository.ClaimNext(ctx, leader, ClaimInput{
		AttemptID: "18181818-1818-4818-8818-181818181818", WorkerID: "worker-failure",
		LeaseTokenHash: strings.Repeat("f", 64), Now: firstClaimTime.Add(5 * time.Second),
		LeaseExpiresAt: firstClaimTime.Add(time.Minute), Policy: testSchedulingPolicy(),
	})
	if err != nil || failureLease.Job.ID != failureJob.ID {
		t.Fatalf("claim terminal failure job = %#v, %v", failureLease, err)
	}
	failureResult := Artifact{
		ID:     "render-jobs/v1/result/17171717-1717-4717-8717-171717171717/" + strings.Repeat("f", 64) + ".json",
		SHA256: strings.Repeat("f", 64), Kind: "result", Visibility: "private",
		StorageKey: "render-jobs/v1/result/17171717-1717-4717-8717-171717171717/" + strings.Repeat("f", 64) + ".json",
		ByteSize:   77, MediaType: "application/json", SchemaVersion: "rin-stored-render-failure/v1",
		ExpiresAt: &resultExpiresAt,
	}
	terminalFailureJob, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: failureLease.AttemptID, LeaseTokenHash: strings.Repeat("f", 64),
		Now: firstClaimTime.Add(6 * time.Second), ErrorCode: "invalid_project_source", ResultArtifact: &failureResult,
	})
	if err != nil || terminalFailureJob.State != "failed" || terminalFailureJob.ResultArtifactID != failureResult.ID {
		t.Fatalf("atomic terminal failure = %#v, %v", terminalFailureJob, err)
	}
	failureWorkload, err := repository.Workload(ctx, failureJob.ID)
	if err != nil {
		t.Fatalf("failure workload: %v", err)
	}
	failureProfile, err := repository.WorkloadProfile(ctx, failureWorkload.ProfileKey, failureJob.RendererVersion)
	// The failure shares the admission-time profile with the earlier successful light job. Since
	// completion now samples both admission and refined profiles, preserve that duration sample
	// while adding the classified failure count.
	if err != nil || failureProfile.SampleCount != 1 || failureProfile.FailureCount != 1 || failureProfile.FailureCounts["invalid"] != 1 ||
		len(failureProfile.SamplesMS) != 1 || failureProfile.SamplesMS[0] != 1000 || failureProfile.P50MS == nil || *failureProfile.P50MS != 1000 {
		t.Fatalf("classified failure profile = %#v, %v", failureProfile, err)
	}
	storedFailureResult, err := repository.Artifact(ctx, failureResult.ID)
	if err != nil || !sameArtifact(storedFailureResult, failureResult) {
		t.Fatalf("atomic failure artifact = %#v, %v", storedFailureResult, err)
	}
	var jobCountBeforeSubwork int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM rin_renderer.render_jobs`).Scan(&jobCountBeforeSubwork); err != nil {
		t.Fatalf("count jobs before subwork: %v", err)
	}
	subworkOne := SubworkAllocationInput{
		AllocationID: "12121212-1212-4212-8212-121212121212", ResourceClass: "texsvg",
		JobID: firstLease.Job.ID, WorkKey: "diagram-1", OwnerID: "worker-diagram",
		LeaseTokenHash: strings.Repeat("8", 64), Now: firstClaimTime.Add(2 * time.Second),
		LeaseExpiresAt: firstClaimTime.Add(time.Minute), Capacities: testSchedulingPolicy().Resources,
	}
	subworkTwo := subworkOne
	subworkTwo.AllocationID = "13131313-1313-4313-8313-131313131313"
	subworkTwo.WorkKey = "diagram-2"
	if _, err := repository.AcquireSubwork(ctx, subworkOne); err != nil {
		t.Fatalf("first AcquireSubwork() error = %v", err)
	}
	if _, err := repository.AcquireSubwork(ctx, subworkTwo); err != nil {
		t.Fatalf("second AcquireSubwork() error = %v", err)
	}
	subworkThree := subworkOne
	subworkThree.AllocationID = "14141414-1414-4414-8414-141414141414"
	subworkThree.WorkKey = "diagram-3"
	if _, err := repository.AcquireSubwork(ctx, subworkThree); !errors.Is(err, ErrResourceCapacity) {
		t.Fatalf("capacity AcquireSubwork() error = %v", err)
	}
	if err := repository.HeartbeatSubwork(ctx, subworkOne.AllocationID, strings.Repeat("9", 64), firstClaimTime.Add(3*time.Second), firstClaimTime.Add(time.Minute)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-token HeartbeatSubwork() error = %v", err)
	}
	if err := repository.HeartbeatSubwork(ctx, subworkOne.AllocationID, subworkOne.LeaseTokenHash, firstClaimTime.Add(3*time.Second), firstClaimTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("HeartbeatSubwork() error = %v", err)
	}
	if err := repository.ReleaseSubwork(ctx, subworkOne.AllocationID, subworkOne.LeaseTokenHash); err != nil {
		t.Fatalf("ReleaseSubwork() error = %v", err)
	}
	if _, err := repository.AcquireSubwork(ctx, subworkThree); err != nil {
		t.Fatalf("replacement AcquireSubwork() error = %v", err)
	}
	if err := repository.ReleaseSubwork(ctx, subworkTwo.AllocationID, subworkTwo.LeaseTokenHash); err != nil {
		t.Fatalf("release second subwork: %v", err)
	}
	if err := repository.ReleaseSubwork(ctx, subworkThree.AllocationID, subworkThree.LeaseTokenHash); err != nil {
		t.Fatalf("release third subwork: %v", err)
	}
	var jobCountAfterSubwork int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM rin_renderer.render_jobs`).Scan(&jobCountAfterSubwork); err != nil {
		t.Fatalf("count jobs after subwork: %v", err)
	}
	if jobCountAfterSubwork != jobCountBeforeSubwork {
		t.Fatalf("subwork changed user-visible job count: %d -> %d", jobCountBeforeSubwork, jobCountAfterSubwork)
	}
	retryResult := Artifact{
		ID:     "render-jobs/v1/result/11111111-1111-4111-8111-111111111111/" + strings.Repeat("9", 64) + ".json",
		SHA256: strings.Repeat("9", 64), Kind: "result", Visibility: "private",
		StorageKey: "render-jobs/v1/result/11111111-1111-4111-8111-111111111111/" + strings.Repeat("9", 64) + ".json",
		ByteSize:   55, MediaType: "application/json", SchemaVersion: "rin-stored-render-failure/v1",
		ExpiresAt: &resultExpiresAt,
	}
	retriedJob, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: firstLease.AttemptID, LeaseTokenHash: strings.Repeat("5", 64),
		Now: firstClaimTime.Add(2 * time.Second), Retryable: true,
		ErrorCode: "worker_transient", RetryAvailableAt: firstClaimTime.Add(15 * time.Second), ResultArtifact: &retryResult,
	})
	if err != nil {
		t.Fatalf("retryable FinishAttempt() error = %v", err)
	}
	if retriedJob.State != "queued" || retriedJob.AvailableAt == nil || !retriedJob.AvailableAt.Equal(firstClaimTime.Add(15*time.Second)) {
		t.Fatalf("retried job = %#v", retriedJob)
	}
	if _, err := repository.Artifact(ctx, retryResult.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry failure artifact should not be attached: %v", err)
	}
	if _, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: firstLease.AttemptID, LeaseTokenHash: strings.Repeat("5", 64),
		Now: firstClaimTime.Add(3 * time.Second), Succeeded: true,
	}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("duplicate FinishAttempt() error = %v", err)
	}

	secondClaimTime := firstClaimTime.Add(20 * time.Second)
	secondLease, err := repository.ClaimNext(ctx, leader, ClaimInput{
		AttemptID:      "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		WorkerID:       "worker-b",
		LeaseTokenHash: strings.Repeat("7", 64),
		Now:            secondClaimTime,
		LeaseExpiresAt: secondClaimTime.Add(time.Minute),
		Policy:         testSchedulingPolicy(),
	})
	if err != nil {
		t.Fatalf("second ClaimNext() error = %v", err)
	}
	if secondLease.Job.ID != job.ID || secondLease.AttemptNo != 2 {
		t.Fatalf("second lease = %#v", secondLease)
	}
	if err := leader.Close(); err != nil {
		t.Fatalf("close first leadership: %v", err)
	}
	replacement, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatalf("replacement AcquireLeadership() error = %v", err)
	}
	recoveryTime := secondLease.LeaseExpiresAt.Add(time.Second)
	recovered, err := repository.RecoverExpiredLeases(ctx, replacement, recoveryTime, recoveryTime.Add(15*time.Second), 10)
	if err != nil || recovered != 1 {
		t.Fatalf("RecoverExpiredLeases() = %d, %v", recovered, err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatalf("close replacement leadership: %v", err)
	}
	failedJob, err := repository.Job(ctx, job.ID)
	if err != nil || failedJob.State != "failed" || failedJob.FinishedAt == nil {
		t.Fatalf("lease-exhausted job = %#v, %v", failedJob, err)
	}
	if err := repository.Heartbeat(ctx, secondLease.AttemptID, strings.Repeat("7", 64), recoveryTime, recoveryTime.Add(time.Minute)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale Heartbeat() error = %v", err)
	}
	if _, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: secondLease.AttemptID, LeaseTokenHash: strings.Repeat("7", 64),
		Now: recoveryTime, Succeeded: true,
	}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale FinishAttempt() error = %v", err)
	}
	var attemptStates string
	if err := repository.db.QueryRowContext(ctx, `
		SELECT string_agg(state, ',' ORDER BY attempt_no)
		FROM rin_renderer.render_attempts WHERE job_id = $1::uuid`, job.ID).Scan(&attemptStates); err != nil {
		t.Fatalf("read recovered attempt states: %v", err)
	}
	if attemptStates != "failed,expired" {
		t.Fatalf("attempt states = %q", attemptStates)
	}

	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_jobs`); err != nil {
		t.Fatalf("reset jobs for cancellation tests: %v", err)
	}
	cancelQueuedAt := recoveryTime.Add(time.Minute)
	queuedCancelJob, err := repository.CreateJob(ctx, Job{
		ID: "17171717-1717-4717-8717-171717171717", PrincipalID: "cancel-queued",
		ContentKind: "markdown", DocumentEngine: "unified", ResourceClass: "document-light",
		PriorityClass: "publish", State: "queued", SourceArtifactID: artifact.ID,
		ProjectHash: strings.Repeat("a", 64), OptionsHash: strings.Repeat("b", 64),
		RendererVersion: "cancel-version", MaxAttempts: 1, QueuedAt: &cancelQueuedAt,
		AvailableAt: &cancelQueuedAt, ExpiresAt: cancelQueuedAt.Add(8 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create queued cancellation job: %v", err)
	}
	cancelWorkload, err := AdmissionWorkload(WorkloadAdmission{
		ContentKind: queuedCancelJob.ContentKind, DocumentEngine: queuedCancelJob.DocumentEngine,
		ResourceClass: queuedCancelJob.ResourceClass, PriorityClass: queuedCancelJob.PriorityClass,
		ProjectBytes: queuedCancelJob.DeclaredSourceBytes,
	})
	if err != nil {
		t.Fatalf("build queued cancellation workload: %v", err)
	}
	cancelWorkload.JobID = queuedCancelJob.ID
	cancelWorkloadTx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin queued cancellation workload: %v", err)
	}
	if err := upsertJobWorkload(ctx, cancelWorkloadTx, cancelWorkload); err != nil {
		cancelWorkloadTx.Rollback()
		t.Fatalf("store queued cancellation workload: %v", err)
	}
	if err := cancelWorkloadTx.Commit(); err != nil {
		t.Fatalf("commit queued cancellation workload: %v", err)
	}
	queuedCancellation, err := repository.RequestCancellation(ctx, queuedCancelJob.ID, cancelQueuedAt.Add(time.Second))
	if err != nil || queuedCancellation.Job.State != "canceled" || queuedCancellation.WorkerMustStop ||
		queuedCancellation.Job.FinishedAt == nil {
		t.Fatalf("queued RequestCancellation() = %#v, %v", queuedCancellation, err)
	}
	if _, err := repository.WorkloadProfile(ctx, cancelWorkload.ProfileKey, queuedCancelJob.RendererVersion); !errors.Is(err, ErrNotFound) {
		t.Fatalf("canceled job contributed a duration profile: %v", err)
	}

	runningQueuedAt := cancelQueuedAt.Add(2 * time.Second)
	runningCancelJob, err := repository.CreateJob(ctx, Job{
		ID: "18181818-1818-4818-8818-181818181818", PrincipalID: "cancel-running",
		ContentKind: "latex", DocumentEngine: "latexml", ResourceClass: "document-latexml",
		PriorityClass: "publish", State: "queued", SourceArtifactID: artifact.ID,
		ProjectHash: strings.Repeat("a", 64), OptionsHash: strings.Repeat("b", 64),
		RendererVersion: "test-version", MaxAttempts: 2, QueuedAt: &runningQueuedAt,
		AvailableAt: &runningQueuedAt, ExpiresAt: runningQueuedAt.Add(8 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create running cancellation job: %v", err)
	}
	cancelLeader, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatalf("acquire cancellation leadership: %v", err)
	}
	runningCancelLease, err := repository.ClaimNext(ctx, cancelLeader, ClaimInput{
		AttemptID: "19191919-1919-4919-8919-191919191919", WorkerID: "worker-cancel",
		LeaseTokenHash: strings.Repeat("e", 64), Now: runningQueuedAt.Add(time.Second),
		LeaseExpiresAt: runningQueuedAt.Add(time.Minute), Policy: testSchedulingPolicy(),
	})
	if err != nil || runningCancelLease.Job.ID != runningCancelJob.ID {
		t.Fatalf("claim running cancellation job = %#v, %v", runningCancelLease, err)
	}
	runningCancellation, err := repository.RequestCancellation(ctx, runningCancelJob.ID, runningQueuedAt.Add(2*time.Second))
	if err != nil || !runningCancellation.WorkerMustStop || !runningCancellation.Job.CancelRequested ||
		runningCancellation.Job.State != "running" {
		t.Fatalf("running RequestCancellation() = %#v, %v", runningCancellation, err)
	}
	var boundedAttemptLease, boundedResourceLease time.Time
	if err := repository.db.QueryRowContext(ctx, `
		SELECT attempts.lease_expires_at, allocations.lease_expires_at
		FROM rin_renderer.render_attempts AS attempts
		JOIN rin_renderer.resource_allocations AS allocations ON allocations.attempt_id = attempts.id
		WHERE attempts.id = $1::uuid`, runningCancelLease.AttemptID).Scan(&boundedAttemptLease, &boundedResourceLease); err != nil ||
		!boundedAttemptLease.Equal(runningQueuedAt.Add(12*time.Second)) || !boundedResourceLease.Equal(boundedAttemptLease) {
		t.Fatalf("running cancellation lease bounds = attempt %v resource %v, %v", boundedAttemptLease, boundedResourceLease, err)
	}
	if err := repository.Heartbeat(ctx, runningCancelLease.AttemptID, strings.Repeat("e", 64), runningQueuedAt.Add(3*time.Second), runningQueuedAt.Add(time.Minute)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Heartbeat() extended a canceled job lease: %v", err)
	}
	requested, err := repository.CancellationRequested(ctx, runningCancelLease.AttemptID, strings.Repeat("e", 64), runningQueuedAt.Add(3*time.Second))
	if err != nil || !requested {
		t.Fatalf("CancellationRequested() = %v, %v", requested, err)
	}
	if _, err := repository.ConfirmCancellation(ctx, runningCancelLease.AttemptID, strings.Repeat("f", 64), runningQueuedAt.Add(4*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-token ConfirmCancellation() error = %v", err)
	}
	// The worker may only observe cancellation on its next heartbeat, after the bounded
	// lease has expired. The same token may still confirm while its attempt and job are
	// running; recovery clears the token and continues to fence stale confirmations.
	requested, err = repository.CancellationRequested(ctx, runningCancelLease.AttemptID, strings.Repeat("e", 64), runningQueuedAt.Add(13*time.Second))
	if err != nil || !requested {
		t.Fatalf("expired CancellationRequested() = %v, %v", requested, err)
	}
	canceledJob, err := repository.ConfirmCancellation(ctx, runningCancelLease.AttemptID, strings.Repeat("e", 64), runningQueuedAt.Add(13*time.Second))
	if err != nil || canceledJob.State != "canceled" || canceledJob.FinishedAt == nil {
		t.Fatalf("ConfirmCancellation() = %#v, %v", canceledJob, err)
	}
	if err := cancelLeader.Close(); err != nil {
		t.Fatalf("close cancellation leadership: %v", err)
	}
	var canceledResources int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM rin_renderer.resource_allocations WHERE job_id = $1::uuid`, runningCancelJob.ID).Scan(&canceledResources); err != nil || canceledResources != 0 {
		t.Fatalf("canceled job resources = %d, %v", canceledResources, err)
	}

	shutdownQueuedAt := runningQueuedAt.Add(10 * time.Second)
	shutdownJob, err := repository.CreateJob(ctx, Job{
		ID: "20202020-2020-4020-8020-202020202020", PrincipalID: "shutdown-worker",
		ContentKind: "markdown", DocumentEngine: "unified", ResourceClass: "document-light",
		PriorityClass: "rebuild", State: "queued", SourceArtifactID: artifact.ID,
		ProjectHash: strings.Repeat("a", 64), OptionsHash: strings.Repeat("b", 64),
		RendererVersion: "test-version", MaxAttempts: 2, QueuedAt: &shutdownQueuedAt,
		AvailableAt: &shutdownQueuedAt, ExpiresAt: shutdownQueuedAt.Add(8 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create shutdown interruption job: %v", err)
	}
	shutdownLeader, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatalf("acquire shutdown leadership: %v", err)
	}
	shutdownLease, err := repository.ClaimNext(ctx, shutdownLeader, ClaimInput{
		AttemptID: "21212121-2121-4121-8121-212121212121", WorkerID: "worker-shutdown",
		LeaseTokenHash: strings.Repeat("1", 64), Now: shutdownQueuedAt.Add(time.Second),
		LeaseExpiresAt: shutdownQueuedAt.Add(time.Minute), Policy: testSchedulingPolicy(),
	})
	if err != nil || shutdownLease.Job.ID != shutdownJob.ID {
		t.Fatalf("claim shutdown job = %#v, %v", shutdownLease, err)
	}
	interruptAt := shutdownQueuedAt.Add(2 * time.Second)
	interrupted, err := repository.InterruptActiveLeases(ctx, shutdownLeader, interruptAt, 10)
	if err != nil || interrupted != 1 {
		t.Fatalf("InterruptActiveLeases() = %d, %v", interrupted, err)
	}
	if err := repository.Heartbeat(ctx, shutdownLease.AttemptID, strings.Repeat("1", 64), interruptAt, interruptAt.Add(time.Minute)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("interrupted Heartbeat() error = %v", err)
	}
	if _, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: shutdownLease.AttemptID, LeaseTokenHash: strings.Repeat("1", 64),
		Now: interruptAt, Succeeded: true,
	}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("interrupted FinishAttempt() error = %v", err)
	}
	recovered, err = repository.RecoverExpiredLeases(ctx, shutdownLeader, interruptAt, interruptAt.Add(15*time.Second), 10)
	if err != nil || recovered != 1 {
		t.Fatalf("recover interrupted shutdown lease = %d, %v", recovered, err)
	}
	interruptedJob, err := repository.Job(ctx, shutdownJob.ID)
	if err != nil || interruptedJob.State != "queued" {
		t.Fatalf("interrupted shutdown job = %#v, %v", interruptedJob, err)
	}
	var interruptedCode string
	if err := repository.db.QueryRowContext(ctx, `SELECT error_code FROM rin_renderer.render_attempts WHERE id = $1::uuid`, shutdownLease.AttemptID).Scan(&interruptedCode); err != nil || interruptedCode != "shutdown_interrupted" {
		t.Fatalf("interrupted attempt code = %q, %v", interruptedCode, err)
	}
	if err := shutdownLeader.Close(); err != nil {
		t.Fatalf("close shutdown leadership: %v", err)
	}

	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_jobs`); err != nil {
		t.Fatalf("reset jobs for fair scheduling: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `UPDATE rin_renderer.scheduler_priority_state SET dispatch_count = 0`); err != nil {
		t.Fatalf("reset priority state: %v", err)
	}
	createFairJob := func(number int, priority string, contentKind string, resourceClass string, queued time.Time) {
		t.Helper()
		identifier := fmt.Sprintf("%08x-0000-4000-8000-%012x", number, number)
		if _, err := repository.CreateJob(ctx, Job{
			ID: identifier, PrincipalID: "fair-" + identifier, ContentKind: contentKind,
			DocumentEngine: "unified", ResourceClass: resourceClass, PriorityClass: priority,
			State: "queued", SourceArtifactID: artifact.ID, ProjectHash: strings.Repeat("a", 64),
			OptionsHash: strings.Repeat("b", 64), RendererVersion: "test-version", MaxAttempts: 1,
			QueuedAt: &queued, AvailableAt: &queued, ExpiresAt: queued.Add(8 * 24 * time.Hour),
		}); err != nil {
			t.Fatalf("create fair job %d: %v", number, err)
		}
	}
	fairQueuedAt := now.Add(3 * time.Hour)
	jobNumber := 100
	for count := 0; count < 120; count++ {
		createFairJob(jobNumber, "publish", "markdown", "document-light", fairQueuedAt)
		jobNumber++
	}
	for count := 0; count < 45; count++ {
		createFairJob(jobNumber, "rebuild", "markdown", "document-light", fairQueuedAt)
		jobNumber++
	}
	for count := 0; count < 15; count++ {
		createFairJob(jobNumber, "migration", "latex", "document-latexml", fairQueuedAt)
		jobNumber++
	}
	fairLeader, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatalf("acquire fair scheduler leadership: %v", err)
	}
	fairCounts := map[string]int{}
	for dispatch := 0; dispatch < 120; dispatch++ {
		claimTime := fairQueuedAt.Add(time.Duration(dispatch+1) * time.Second)
		attemptID := fmt.Sprintf("%08x-1111-4111-8111-%012x", dispatch+1, dispatch+1)
		lease, err := repository.ClaimNext(ctx, fairLeader, ClaimInput{
			AttemptID: attemptID, WorkerID: "fair-worker", LeaseTokenHash: strings.Repeat("a", 64),
			Now: claimTime, LeaseExpiresAt: claimTime.Add(time.Minute), Policy: testSchedulingPolicy(),
		})
		if err != nil {
			t.Fatalf("fair ClaimNext(%d) error = %v", dispatch, err)
		}
		fairCounts[lease.Job.PriorityClass]++
		if _, err := repository.FinishAttempt(ctx, FinishInput{
			AttemptID: lease.AttemptID, LeaseTokenHash: strings.Repeat("a", 64),
			Now: claimTime.Add(time.Second), Succeeded: true,
		}); err != nil {
			t.Fatalf("finish fair dispatch %d: %v", dispatch, err)
		}
	}
	if fairCounts["publish"] < 78 || fairCounts["publish"] > 82 ||
		fairCounts["rebuild"] < 28 || fairCounts["rebuild"] > 32 ||
		fairCounts["migration"] < 8 || fairCounts["migration"] > 12 {
		t.Fatalf("weighted fair load counts = %v, want approximately 80/30/10", fairCounts)
	}
	if err := fairLeader.Close(); err != nil {
		t.Fatalf("close fair scheduler leadership: %v", err)
	}

	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_jobs`); err != nil {
		t.Fatalf("reset jobs for aging test: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `UPDATE rin_renderer.scheduler_priority_state SET dispatch_count = 0`); err != nil {
		t.Fatalf("reset priority state for aging test: %v", err)
	}
	for count := 0; count < 10; count++ {
		createFairJob(200+count, "publish", "markdown", "document-light", fairQueuedAt)
	}
	createFairJob(299, "migration", "latex", "document-latexml", fairQueuedAt.Add(-50*time.Minute))
	agingLeader, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatalf("acquire aging scheduler leadership: %v", err)
	}
	agingClaimTime := fairQueuedAt.Add(time.Second)
	agedLease, err := repository.ClaimNext(ctx, agingLeader, ClaimInput{
		AttemptID: "abababab-abab-4bab-8bab-abababababab", WorkerID: "aging-worker",
		LeaseTokenHash: strings.Repeat("c", 64), Now: agingClaimTime,
		LeaseExpiresAt: agingClaimTime.Add(time.Minute), Policy: testSchedulingPolicy(),
	})
	if err != nil {
		t.Fatalf("aging ClaimNext() error = %v", err)
	}
	if agedLease.Job.PriorityClass != "migration" || agedLease.Job.ContentKind != "latex" {
		t.Fatalf("aged job was starved by fresh Markdown: %#v", agedLease.Job)
	}
	if err := agingLeader.Close(); err != nil {
		t.Fatalf("close aging scheduler leadership: %v", err)
	}

	cleanupNow := agingClaimTime.Add(30 * 24 * time.Hour)
	if _, err := repository.db.ExecContext(ctx, `
		DELETE FROM rin_renderer.render_artifacts AS artifacts
		WHERE artifacts.visibility = 'private' AND artifacts.id <> $1
			AND NOT EXISTS (SELECT 1 FROM rin_renderer.render_jobs AS jobs
				WHERE jobs.source_artifact_id = artifacts.id OR jobs.result_artifact_id = artifacts.id)
			AND NOT EXISTS (SELECT 1 FROM rin_renderer.render_cache_entries AS cache
				WHERE cache.artifact_id = artifacts.id)`, artifact.ID); err != nil {
		t.Fatalf("isolate cleanup artifact fixtures: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `UPDATE rin_renderer.render_artifacts SET expires_at = $2 WHERE id = $1`, artifact.ID, cleanupNow.Add(-time.Hour)); err != nil {
		t.Fatalf("age active source artifact: %v", err)
	}
	debugArtifact := Artifact{
		ID:     "render-jobs/v1/debug/30303030-3030-4030-8030-303030303030/worker.log",
		SHA256: strings.Repeat("3", 64), Kind: "debug", Visibility: "private",
		StorageKey: "render-jobs/v1/debug/30303030-3030-4030-8030-303030303030/worker.log",
		ByteSize:   3, MediaType: "text/plain", ExpiresAt: timePointer(cleanupNow.Add(-time.Hour)),
	}
	if _, err := repository.CreateArtifact(ctx, debugArtifact); err != nil {
		t.Fatalf("create expired debug artifact: %v", err)
	}
	publicArtifact := Artifact{
		ID:     "diagrams/v1/svg-sha256/55/" + strings.Repeat("5", 64) + ".svg",
		SHA256: strings.Repeat("5", 64), Kind: "public-diagram", Visibility: "public",
		StorageKey: "diagrams/v1/svg-sha256/55/" + strings.Repeat("5", 64) + ".svg",
		ByteSize:   6, MediaType: "image/svg+xml", ExpiresAt: timePointer(cleanupNow.Add(-time.Hour)),
	}
	if _, err := repository.CreateArtifact(ctx, publicArtifact); err != nil {
		t.Fatalf("create public cleanup sentinel: %v", err)
	}
	staged, err := repository.StageExpiredPrivateArtifacts(ctx, cleanupNow, 10)
	if err != nil || staged != 1 {
		t.Fatalf("StageExpiredPrivateArtifacts() = %d, %v", staged, err)
	}
	pendingDeletes, err := repository.PendingArtifactDeletions(ctx, 10)
	if err != nil || len(pendingDeletes) != 1 || pendingDeletes[0].ID != debugArtifact.ID {
		t.Fatalf("PendingArtifactDeletions() = %#v, %v", pendingDeletes, err)
	}
	if err := repository.AckArtifactDeletion(ctx, debugArtifact.ID); err != nil {
		t.Fatalf("AckArtifactDeletion() error = %v", err)
	}
	var activeSourceRows int
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM rin_renderer.render_artifacts WHERE id = $1`, artifact.ID).Scan(&activeSourceRows); err != nil || activeSourceRows != 1 {
		t.Fatalf("active source cleanup protection = %d, %v", activeSourceRows, err)
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM rin_renderer.render_artifacts WHERE id = $1`, publicArtifact.ID).Scan(&activeSourceRows); err != nil || activeSourceRows != 1 {
		t.Fatalf("public artifact cleanup protection = %d, %v", activeSourceRows, err)
	}

	terminalFinished := cleanupNow.Add(-2 * time.Hour)
	terminalJob, err := repository.CreateJob(ctx, Job{
		ID: "31313131-3131-4131-8131-313131313131", PrincipalID: "cleanup-terminal",
		ContentKind: "markdown", DocumentEngine: "unified", ResourceClass: "document-light",
		PriorityClass: "rebuild", State: "failed", ProjectHash: strings.Repeat("a", 64),
		OptionsHash: strings.Repeat("b", 64), RendererVersion: "test-version", MaxAttempts: 1,
		FinishedAt: &terminalFinished, ExpiresAt: fairQueuedAt.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("create expired terminal job: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.idempotency_keys (
			principal_id, owner_scope, idempotency_key, request_hash, job_id, expires_at)
		VALUES ('cleanup-terminal', '', 'cleanup-key', $1, $2::uuid, $3)`, strings.Repeat("4", 64), terminalJob.ID, cleanupNow.Add(-time.Hour)); err != nil {
		t.Fatalf("create expired cleanup metadata: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `INSERT INTO rin_renderer.render_job_events (job_id, event_type) VALUES ($1::uuid, 'old-event')`, terminalJob.ID); err != nil {
		t.Fatalf("create cleanup event: %v", err)
	}
	expiredJobs, err := repository.ExpireTerminalJobs(ctx, cleanupNow, 10)
	if err != nil || expiredJobs != 1 {
		t.Fatalf("ExpireTerminalJobs() = %d, %v", expiredJobs, err)
	}
	terminalJob, err = repository.Job(ctx, terminalJob.ID)
	if err != nil || terminalJob.State != "expired" {
		t.Fatalf("expired terminal job = %#v, %v", terminalJob, err)
	}
	metadataCleanup, err := repository.CleanupExpiredMetadata(ctx, cleanupNow, cleanupNow, 10)
	if err != nil || metadataCleanup.Idempotency != 1 || metadataCleanup.Events < 1 {
		t.Fatalf("CleanupExpiredMetadata() = %#v, %v", metadataCleanup, err)
	}
	authorizedJob, err := repository.AuthorizedJob(ctx, terminalJob.ID, terminalJob.PrincipalID, terminalJob.OwnerScope)
	if err != nil || authorizedJob.State != "expired" {
		t.Fatalf("AuthorizedJob() = %#v, %v", authorizedJob, err)
	}
	if _, err := repository.AuthorizedJob(ctx, terminalJob.ID, "another-principal", terminalJob.OwnerScope); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-principal AuthorizedJob() error = %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage) VALUES ($1::uuid, 'status_probe', 'test')`, terminalJob.ID); err != nil {
		t.Fatalf("create authorized event probe: %v", err)
	}
	events, err := repository.AuthorizedEvents(ctx, terminalJob.ID, terminalJob.PrincipalID, terminalJob.OwnerScope, 0, 10)
	foundStatusProbe := false
	for _, event := range events {
		foundStatusProbe = foundStatusProbe || event.Type == "status_probe"
	}
	if err != nil || !foundStatusProbe {
		t.Fatalf("AuthorizedEvents() = %#v, %v", events, err)
	}
	if _, err := repository.AuthorizedEvents(ctx, terminalJob.ID, terminalJob.PrincipalID, "another-owner", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner AuthorizedEvents() error = %v", err)
	}
	queueTime := cleanupNow.Add(time.Minute)
	queueProbe, err := repository.CreateJob(ctx, Job{
		ID: "32323232-3232-4232-8232-323232323232", PrincipalID: "queue-probe", OwnerScope: "book-probe",
		ContentKind: "markdown", DocumentEngine: "unified", ResourceClass: "document-light",
		PriorityClass: "publish", State: "queued", SourceArtifactID: artifact.ID,
		ProjectHash: strings.Repeat("a", 64), OptionsHash: strings.Repeat("b", 64),
		RendererVersion: "test-version", MaxAttempts: 2, QueuedAt: &queueTime,
		AvailableAt: &queueTime, ExpiresAt: cleanupNow.Add(8 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create queue API probe: %v", err)
	}
	estimationPolicy := EstimationPolicy{MinimumSamples: 20, Scheduling: testSchedulingPolicy()}
	jobQueue, err := repository.AuthorizedJobQueue(ctx, queueProbe.ID, queueProbe.PrincipalID, queueProbe.OwnerScope, cleanupNow, estimationPolicy)
	if err != nil || jobQueue.Scope != "instance" || jobQueue.QueuedProjects < 1 || jobQueue.JobsAheadEstimate < 0 {
		t.Fatalf("AuthorizedJobQueue() = %#v, %v", jobQueue, err)
	}
	if _, err := repository.AuthorizedJobQueue(ctx, queueProbe.ID, queueProbe.PrincipalID, "another-owner", cleanupNow, estimationPolicy); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner AuthorizedJobQueue() error = %v", err)
	}
	queueSummary, err := repository.Queue(ctx, cleanupNow)
	if err != nil || queueSummary.Scope != "instance" || queueSummary.ActiveProjects < 1 {
		t.Fatalf("Queue() = %#v, %v", queueSummary, err)
	}
	resourceUsage, err := repository.ResourceUsage(ctx, cleanupNow)
	if err != nil {
		t.Fatalf("ResourceUsage() = %#v, %v", resourceUsage, err)
	}
	for resourceClass, count := range resourceUsage {
		if count < 0 || (resourceClass != "document-light" && resourceClass != "document-latexml" &&
			resourceClass != "math-node" && resourceClass != "texsvg" && resourceClass != "batch-migration") {
			t.Fatalf("ResourceUsage() unsafe entry = %q:%d", resourceClass, count)
		}
	}
	readArtifact, err := repository.Artifact(ctx, artifact.ID)
	if err != nil || readArtifact.Visibility != "private" {
		t.Fatalf("Artifact() = %#v, %v", readArtifact, err)
	}
	testPostgresWaitEstimation(t, ctx, repository, artifact, cleanupNow.Add(10*time.Minute))
	testPostgresSupportOperations(t, ctx, repository, cleanupNow.Add(20*time.Minute))
	testPostgresCacheIndex(t, ctx, repository, cleanupNow.Add(30*time.Minute))

	if _, err := repository.db.ExecContext(ctx, `
		UPDATE rin_renderer.schema_migrations SET checksum = $1 WHERE version = 1`, strings.Repeat("0", 64)); err != nil {
		t.Fatalf("tamper migration checksum: %v", err)
	}
	if err := repository.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "history mismatch") {
		t.Fatalf("Migrate() after checksum tamper error = %v", err)
	}
}

func TestPostgresLatexPDFPreviewQueueSemantics(t *testing.T) {
	dataSourceName := os.Getenv("RIN_RENDERER_TEST_DATABASE_URL")
	if dataSourceName == "" {
		t.Skip("RIN_RENDERER_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	repository, err := Open(ctx, dataSourceName)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS rin_renderer CASCADE`); err != nil {
		t.Fatalf("reset isolated preview schema: %v", err)
	}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("migrate preview schema: %v", err)
	}

	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	limits := AdmissionLimits{GlobalNonTerminal: 100, PrincipalQueued: 8, PreviewQueued: 20, PrivateSourceBytes: 1 << 20}
	firstInput := testPreviewAdmissionInput("80000000-0000-4000-8000-000000000001", "preview-first", "preview-context-a", "a", now)
	first, err := repository.ReserveAdmission(ctx, firstInput, limits)
	if err != nil {
		t.Fatalf("reserve first preview: %v", err)
	}
	conflict := firstInput
	conflict.RequestHash = strings.Repeat("f", 64)
	if _, err := repository.ReserveAdmission(ctx, conflict, limits); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("preview idempotency conflict = %v", err)
	}
	secondInput := testPreviewAdmissionInput("80000000-0000-4000-8000-000000000002", "preview-second", "preview-context-a", "b", now.Add(time.Second))
	second, err := repository.ReserveAdmission(ctx, secondInput, limits)
	if err != nil {
		t.Fatalf("reserve second preview: %v", err)
	}

	// Commits may arrive in either order. Creation order, not upload timing, defines
	// the latest snapshot and the context unique index is the final guard.
	commitInputs := []CommitAdmissionInput{
		testPreviewCommitInput(first, firstInput, now.Add(2*time.Second)),
		testPreviewCommitInput(second, secondInput, now.Add(2*time.Second)),
	}
	var wait sync.WaitGroup
	commitErrors := make([]error, 2)
	for index := range commitInputs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, commitErrors[index] = repository.CommitAdmission(ctx, commitInputs[index])
		}(index)
	}
	wait.Wait()
	for _, commitErr := range commitErrors {
		if commitErr != nil {
			t.Fatalf("concurrent preview commit: %v", commitErr)
		}
	}
	var queuedID string
	var queuedCount, canceledCount int
	if err := repository.db.QueryRowContext(ctx, `
		SELECT min(id::text) FILTER (WHERE state = 'queued'),
			count(*) FILTER (WHERE state = 'queued'), count(*) FILTER (WHERE state = 'canceled')
		FROM rin_renderer.render_jobs
		WHERE principal_id = 'preview-principal' AND owner_scope = 'preview-context-a'`).Scan(
		&queuedID, &queuedCount, &canceledCount); err != nil {
		t.Fatalf("read concurrent preview state: %v", err)
	}
	if queuedID != second.Job.ID || queuedCount != 1 || canceledCount != 1 {
		t.Fatalf("concurrent preview state = queued %s/%d canceled %d", queuedID, queuedCount, canceledCount)
	}
	var supersededBy string
	if err := repository.db.QueryRowContext(ctx, `SELECT superseded_by::text FROM rin_renderer.render_jobs WHERE id = $1::uuid`, first.Job.ID).Scan(&supersededBy); err != nil || supersededBy != second.Job.ID {
		t.Fatalf("first preview superseded_by = %q, %v", supersededBy, err)
	}

	leader, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	pdfLeader, err := repository.AcquireResourceLeadership(ctx, "latex-pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer pdfLeader.Close()
	policy := testSchedulingPolicy()
	policy.Resources.Heavy = 1
	claimAt := now.Add(3 * time.Second)
	if _, err := repository.ClaimNext(ctx, leader, ClaimInput{
		AttemptID: "81000000-0000-4000-8000-000000000001", WorkerID: "ordinary-worker",
		LeaseTokenHash: strings.Repeat("1", 64), Now: claimAt,
		LeaseExpiresAt: claimAt.Add(time.Minute), Policy: policy,
	}); !errors.Is(err, ErrNoEligibleJob) {
		t.Fatalf("ordinary worker PDF claim = %v", err)
	}
	heavyInput := testAdmissionInput("81000000-0000-4000-8000-000000000003", "publish-principal", "publish-context", 3, now.Add(3*time.Second))
	heavyReservation, err := repository.ReserveAdmission(ctx, heavyInput, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(heavyReservation, heavyInput, now.Add(3*time.Second))); err != nil {
		t.Fatal(err)
	}
	heavyLease, err := repository.ClaimNext(ctx, leader, ClaimInput{
		AttemptID: "81000000-0000-4000-8000-000000000004", WorkerID: "ordinary-worker",
		LeaseTokenHash: strings.Repeat("4", 64), Now: claimAt,
		LeaseExpiresAt: claimAt.Add(time.Minute), Policy: policy,
	})
	if err != nil || heavyLease.Job.ID != heavyReservation.Job.ID || heavyLease.Job.PriorityClass != "publish" {
		t.Fatalf("ordinary publish claim = %#v, %v", heavyLease, err)
	}
	if _, err := repository.ClaimNext(ctx, pdfLeader, ClaimInput{
		AttemptID: "81000000-0000-4000-8000-000000000005", WorkerID: "pdf-worker",
		LeaseTokenHash: strings.Repeat("5", 64), Now: claimAt,
		LeaseExpiresAt: claimAt.Add(time.Minute), Policy: policy, ResourceClass: "latex-pdf",
	}); !errors.Is(err, ErrNoEligibleJob) {
		t.Fatalf("shared heavy token PDF claim = %v", err)
	}
	if _, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: heavyLease.AttemptID, LeaseTokenHash: strings.Repeat("4", 64),
		Now: claimAt.Add(time.Second), Succeeded: true,
	}); err != nil {
		t.Fatalf("finish ordinary heavy publish: %v", err)
	}
	secondLease, err := repository.ClaimNext(ctx, pdfLeader, ClaimInput{
		AttemptID: "81000000-0000-4000-8000-000000000002", WorkerID: "pdf-worker",
		LeaseTokenHash: strings.Repeat("2", 64), Now: claimAt,
		LeaseExpiresAt: claimAt.Add(time.Minute), Policy: policy, ResourceClass: "latex-pdf",
	})
	if err != nil || secondLease.Job.ID != second.Job.ID {
		t.Fatalf("dedicated PDF claim = %#v, %v", secondLease, err)
	}

	thirdInput := testPreviewAdmissionInput("80000000-0000-4000-8000-000000000003", "preview-third", "preview-context-a", "c", now.Add(4*time.Second))
	third, err := repository.ReserveAdmission(ctx, thirdInput, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(third, thirdInput, now.Add(5*time.Second))); err != nil {
		t.Fatal(err)
	}
	fourthInput := testPreviewAdmissionInput("80000000-0000-4000-8000-000000000004", "preview-fourth", "preview-context-a", "d", now.Add(6*time.Second))
	fourth, err := repository.ReserveAdmission(ctx, fourthInput, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(fourth, fourthInput, now.Add(7*time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE state = 'running'), count(*) FILTER (WHERE state = 'queued')
		FROM rin_renderer.render_jobs WHERE owner_scope = 'preview-context-a'`).Scan(&canceledCount, &queuedCount); err != nil || canceledCount != 1 || queuedCount != 1 {
		t.Fatalf("running/latest preview slots = %d/%d, %v", canceledCount, queuedCount, err)
	}

	resultExpires := now.Add(24 * time.Hour)
	resultArtifact := Artifact{
		ID:     "render-jobs/v1/result/" + second.Job.ID + "/" + strings.Repeat("e", 64) + ".pdf",
		SHA256: strings.Repeat("e", 64), Kind: "result", Visibility: "private",
		StorageKey: "render-jobs/v1/result/" + second.Job.ID + "/" + strings.Repeat("e", 64) + ".pdf",
		ByteSize:   128, MediaType: "application/pdf", SchemaVersion: "rin-latex-pdf-preview/v1", ExpiresAt: &resultExpires,
	}
	finished, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: secondLease.AttemptID, LeaseTokenHash: strings.Repeat("2", 64),
		Now: now.Add(8 * time.Second), Succeeded: true, ResultArtifact: &resultArtifact,
	})
	if err != nil || finished.State != "succeeded" {
		t.Fatalf("finish PDF preview = %#v, %v", finished, err)
	}
	cacheInput := testPreviewAdmissionInput("80000000-0000-4000-8000-000000000005", "preview-cache", "preview-context-a", "b", now.Add(9*time.Second))
	cached, err := repository.ReserveAdmission(ctx, cacheInput, limits)
	if err != nil || !cached.Reused || cached.Uploading || cached.Job.ID != second.Job.ID {
		t.Fatalf("preview content cache = %#v, %v", cached, err)
	}
	if canceled, err := repository.RequestCancellation(ctx, fourth.Job.ID, now.Add(10*time.Second)); err != nil || canceled.Job.State != "canceled" {
		t.Fatalf("cancel latest queued preview = %#v, %v", canceled, err)
	}

	// An expired running preview with a newer queued snapshot must not requeue and
	// displace that snapshot during recovery.
	recoveryOldInput := testPreviewAdmissionInput("82000000-0000-4000-8000-000000000001", "recovery-old", "preview-context-recovery", "1", now.Add(11*time.Second))
	recoveryOld, err := repository.ReserveAdmission(ctx, recoveryOldInput, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(recoveryOld, recoveryOldInput, now.Add(12*time.Second))); err != nil {
		t.Fatal(err)
	}
	recoveryClaimAt := now.Add(13 * time.Second)
	recoveryLease, err := repository.ClaimNext(ctx, pdfLeader, ClaimInput{
		AttemptID: "82000000-0000-4000-8000-000000000002", WorkerID: "pdf-worker",
		LeaseTokenHash: strings.Repeat("3", 64), Now: recoveryClaimAt,
		LeaseExpiresAt: recoveryClaimAt.Add(time.Second), Policy: policy, ResourceClass: "latex-pdf",
	})
	if err != nil || recoveryLease.Job.ID != recoveryOld.Job.ID {
		t.Fatalf("claim recovery preview = %#v, %v", recoveryLease, err)
	}
	recoveryNewInput := testPreviewAdmissionInput("82000000-0000-4000-8000-000000000003", "recovery-new", "preview-context-recovery", "2", now.Add(14*time.Second))
	recoveryNew, err := repository.ReserveAdmission(ctx, recoveryNewInput, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(recoveryNew, recoveryNewInput, now.Add(15*time.Second))); err != nil {
		t.Fatal(err)
	}
	if recovered, err := repository.RecoverExpiredLeases(ctx, pdfLeader, recoveryClaimAt.Add(2*time.Second), recoveryClaimAt.Add(20*time.Second), 4); err != nil || recovered != 1 {
		t.Fatalf("recover superseded preview = %d, %v", recovered, err)
	}
	var oldState, oldSupersededBy, newState string
	if err := repository.db.QueryRowContext(ctx, `SELECT state, superseded_by::text FROM rin_renderer.render_jobs WHERE id = $1::uuid`, recoveryOld.Job.ID).Scan(&oldState, &oldSupersededBy); err != nil {
		t.Fatal(err)
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT state FROM rin_renderer.render_jobs WHERE id = $1::uuid`, recoveryNew.Job.ID).Scan(&newState); err != nil {
		t.Fatal(err)
	}
	if oldState != "canceled" || oldSupersededBy != recoveryNew.Job.ID || newState != "queued" {
		t.Fatalf("recovery transition = old %s -> %s, new %s", oldState, oldSupersededBy, newState)
	}
	if _, err := repository.RequestCancellation(ctx, recoveryNew.Job.ID, now.Add(17*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	leader = nil
	if err := pdfLeader.Close(); err != nil {
		t.Fatal(err)
	}
	pdfLeader = nil

	// Fill 19 contexts, reserve two more concurrently eligible uploads, and prove
	// the commit-side admission lock admits exactly one twentieth queued preview.
	for index := 0; index < 19; index++ {
		id := fmt.Sprintf("83000000-0000-4000-8000-%012d", index+1)
		input := testPreviewAdmissionInput(id, fmt.Sprintf("capacity-%d", index), fmt.Sprintf("preview-capacity-%d", index), "3", now.Add(time.Duration(20+index)*time.Second))
		reservation, err := repository.ReserveAdmission(ctx, input, limits)
		if err != nil {
			t.Fatalf("reserve capacity preview %d: %v", index, err)
		}
		if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(reservation, input, now.Add(time.Duration(40+index)*time.Second))); err != nil {
			t.Fatalf("commit capacity preview %d: %v", index, err)
		}
	}
	capacityInputs := []AdmissionReservationInput{
		testPreviewAdmissionInput("84000000-0000-4000-8000-000000000001", "capacity-race-a", "preview-capacity-race-a", "4", now.Add(70*time.Second)),
		testPreviewAdmissionInput("84000000-0000-4000-8000-000000000002", "capacity-race-b", "preview-capacity-race-b", "5", now.Add(71*time.Second)),
	}
	capacityReservations := make([]AdmissionReservation, 2)
	for index := range capacityInputs {
		capacityReservations[index], err = repository.ReserveAdmission(ctx, capacityInputs[index], limits)
		if err != nil {
			t.Fatalf("reserve capacity race %d: %v", index, err)
		}
	}
	capacityErrors := make([]error, 2)
	for index := range capacityInputs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, capacityErrors[index] = repository.CommitAdmission(ctx, testPreviewCommitInput(capacityReservations[index], capacityInputs[index], now.Add(72*time.Second)))
		}(index)
	}
	wait.Wait()
	capacitySuccesses, capacityRejections := 0, 0
	for index, capacityErr := range capacityErrors {
		if capacityErr == nil {
			capacitySuccesses++
		} else if errors.Is(capacityErr, ErrPreviewQueueCapacity) {
			capacityRejections++
			if err := repository.AbortAdmission(ctx, capacityReservations[index].Job.ID, capacityInputs[index].AdmissionTokenHash); err != nil {
				t.Fatalf("abort capacity loser: %v", err)
			}
		} else {
			t.Fatalf("capacity race error: %v", capacityErr)
		}
	}
	if capacitySuccesses != 1 || capacityRejections != 1 {
		t.Fatalf("capacity race = %d success, %d rejection", capacitySuccesses, capacityRejections)
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM rin_renderer.render_jobs WHERE resource_class = 'latex-pdf' AND state = 'queued' AND cancel_requested = false`).Scan(&queuedCount); err != nil || queuedCount != 20 {
		t.Fatalf("bounded preview queue = %d, %v", queuedCount, err)
	}
	replacementInput := testPreviewAdmissionInput("85000000-0000-4000-8000-000000000001", "capacity-replace", "preview-capacity-0", "6", now.Add(73*time.Second))
	replacement, err := repository.ReserveAdmission(ctx, replacementInput, limits)
	if err != nil {
		t.Fatalf("reserve full-queue replacement: %v", err)
	}
	if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(replacement, replacementInput, now.Add(74*time.Second))); err != nil {
		t.Fatalf("commit full-queue replacement: %v", err)
	}
	if err := repository.db.QueryRowContext(ctx, `SELECT count(*) FROM rin_renderer.render_jobs WHERE resource_class = 'latex-pdf' AND state = 'queued' AND cancel_requested = false`).Scan(&queuedCount); err != nil || queuedCount != 20 {
		t.Fatalf("replacement preview queue = %d, %v", queuedCount, err)
	}
}

func testPostgresAtomicTerminalCompletions(t *testing.T, ctx context.Context, repository *Repository, source Artifact, now time.Time) {
	t.Helper()
	metadata := func(requestCharacter string, projectID string) json.RawMessage {
		value, err := json.Marshal(map[string]string{
			"requestId": "render-" + strings.Repeat(requestCharacter, 32), "controlProjectId": projectID,
			"sourceCommit": strings.Repeat(requestCharacter, 40), "controlProjectHash": strings.Repeat(requestCharacter, 64),
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	createQueued := func(id, requestCharacter, projectID string, maxAttempts int16, queuedAt time.Time) Job {
		t.Helper()
		job, err := repository.CreateJob(ctx, Job{
			ID: id, PrincipalID: "atomic-" + id, OwnerScope: projectID,
			ContentKind: "markdown", DocumentEngine: "unified", ResourceClass: "document-light",
			PriorityClass: "publish", State: "queued", SourceArtifactID: source.ID,
			ProjectHash: strings.Repeat(requestCharacter, 64), OptionsHash: strings.Repeat("f", 64),
			RendererVersion: "atomic-test", RequestMetadata: metadata(requestCharacter, projectID),
			MaxAttempts: maxAttempts, QueuedAt: &queuedAt, AvailableAt: &queuedAt,
			ExpiresAt: queuedAt.Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("create atomic completion job %s: %v", id, err)
		}
		return job
	}
	countOutbox := func(jobID string) int {
		t.Helper()
		var count int
		if err := repository.db.QueryRowContext(ctx, `
			SELECT count(*) FROM rin_renderer.renderer_completion_outbox WHERE job_id=$1::uuid`, jobID).Scan(&count); err != nil {
			t.Fatalf("count completion outbox for %s: %v", jobID, err)
		}
		return count
	}
	claim := func(leadership *Leadership, jobID, attemptID, token string, claimAt time.Time) Lease {
		t.Helper()
		lease, err := repository.ClaimNext(ctx, leadership, ClaimInput{
			AttemptID: attemptID, WorkerID: "atomic-worker", LeaseTokenHash: token,
			Now: claimAt, LeaseExpiresAt: claimAt.Add(time.Minute), Policy: testSchedulingPolicy(),
		})
		if err != nil || lease.Job.ID != jobID {
			t.Fatalf("claim atomic completion job %s = %#v, %v", jobID, lease, err)
		}
		return lease
	}

	leadership, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatalf("acquire atomic completion leadership: %v", err)
	}
	defer leadership.Close()

	successJob := createQueued("41414141-4141-4141-8141-414141414141", "1", "article:41", 1, now)
	successLease := claim(leadership, successJob.ID, "51515151-5151-4151-8151-515151515151", strings.Repeat("1", 64), now.Add(time.Second))
	resultExpiresAt := now.Add(12 * time.Hour)
	successResult := Artifact{
		ID:     "render-jobs/v1/result/41414141-4141-4141-8141-414141414141/" + strings.Repeat("2", 64) + ".json",
		SHA256: strings.Repeat("2", 64), Kind: "result", Visibility: "private",
		StorageKey: "render-jobs/v1/result/41414141-4141-4141-8141-414141414141/" + strings.Repeat("2", 64) + ".json",
		ByteSize:   64, MediaType: "application/json", SchemaVersion: "rin-stored-render-output/v1", ExpiresAt: &resultExpiresAt,
	}
	badEvidence := CompletionResultEvidence{RendererProjectHash: strings.Repeat("0", 64), ResultHash: strings.Repeat("3", 64)}
	if _, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: successLease.AttemptID, LeaseTokenHash: strings.Repeat("1", 64), Now: now.Add(2 * time.Second),
		Succeeded: true, ResultArtifact: &successResult, CompletionResult: &badEvidence,
	}); err == nil {
		t.Fatal("terminal transaction accepted invalid completion evidence")
	}
	rolledBackJob, err := repository.Job(ctx, successJob.ID)
	if err != nil || rolledBackJob.State != "running" || countOutbox(successJob.ID) != 0 {
		t.Fatalf("terminal transaction rollback = %#v, outbox=%d, %v", rolledBackJob, countOutbox(successJob.ID), err)
	}
	if _, err := repository.Artifact(ctx, successResult.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back terminal artifact record = %v", err)
	}
	goodEvidence := CompletionResultEvidence{RendererProjectHash: strings.Repeat("2", 64), ResultHash: strings.Repeat("3", 64)}
	finished, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: successLease.AttemptID, LeaseTokenHash: strings.Repeat("1", 64), Now: now.Add(3 * time.Second),
		Succeeded: true, ResultArtifact: &successResult, CompletionResult: &goodEvidence,
	})
	if err != nil || finished.State != "succeeded" || countOutbox(successJob.ID) != 1 {
		t.Fatalf("atomic success completion = %#v, outbox=%d, %v", finished, countOutbox(successJob.ID), err)
	}
	var terminalState, resultReference, rendererProjectHash, resultHash string
	if err := repository.db.QueryRowContext(ctx, `
		SELECT terminal_state,result_reference,renderer_project_hash,result_hash
		FROM rin_renderer.renderer_completion_outbox WHERE job_id=$1::uuid`, successJob.ID).Scan(
		&terminalState, &resultReference, &rendererProjectHash, &resultHash,
	); err != nil || terminalState != "succeeded" || resultReference != successResult.ID ||
		rendererProjectHash != goodEvidence.RendererProjectHash || resultHash != goodEvidence.ResultHash {
		t.Fatalf("success completion evidence = %q %q %q %q, %v", terminalState, resultReference, rendererProjectHash, resultHash, err)
	}
	if _, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: successLease.AttemptID, LeaseTokenHash: strings.Repeat("1", 64), Now: now.Add(4 * time.Second),
		Succeeded: true, ResultArtifact: &successResult, CompletionResult: &goodEvidence,
	}); !errors.Is(err, ErrLeaseLost) || countOutbox(successJob.ID) != 1 {
		t.Fatalf("duplicate success completion = outbox %d, %v", countOutbox(successJob.ID), err)
	}

	retryJob := createQueued("42424242-4242-4242-8242-424242424242", "2", "article:42", 2, now.Add(5*time.Second))
	firstRetryLease := claim(leadership, retryJob.ID, "52525252-5252-4252-8252-525252525252", strings.Repeat("4", 64), now.Add(6*time.Second))
	if retried, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: firstRetryLease.AttemptID, LeaseTokenHash: strings.Repeat("4", 64), Now: now.Add(7 * time.Second),
		Retryable: true, ErrorCode: "worker_transient", RetryAvailableAt: now.Add(8 * time.Second),
	}); err != nil || retried.State != "queued" || countOutbox(retryJob.ID) != 0 {
		t.Fatalf("retryable failure completion = %#v, outbox=%d, %v", retried, countOutbox(retryJob.ID), err)
	}
	secondRetryLease := claim(leadership, retryJob.ID, "53535353-5353-4353-8353-535353535353", strings.Repeat("5", 64), now.Add(9*time.Second))
	if failed, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: secondRetryLease.AttemptID, LeaseTokenHash: strings.Repeat("5", 64), Now: now.Add(10 * time.Second),
		Retryable: true, ErrorCode: "worker_transient", RetryAvailableAt: now.Add(11 * time.Second),
	}); err != nil || failed.State != "failed" || countOutbox(retryJob.ID) != 1 {
		t.Fatalf("final failure completion = %#v, outbox=%d, %v", failed, countOutbox(retryJob.ID), err)
	}
	var failureCode string
	if err := repository.db.QueryRowContext(ctx, `
		SELECT failure_code FROM rin_renderer.renderer_completion_outbox WHERE job_id=$1::uuid`, retryJob.ID).Scan(&failureCode); err != nil || failureCode != "document_render_failed" {
		t.Fatalf("safe final failure code = %q, %v", failureCode, err)
	}

	queuedCancelJob := createQueued("43434343-4343-4343-8343-434343434343", "3", "article:43", 1, now.Add(12*time.Second))
	legacyMetadata, _ := json.Marshal(map[string]string{
		"controlProjectId": queuedCancelJob.OwnerScope, "sourceCommit": strings.Repeat("3", 40),
		"controlProjectHash": strings.Repeat("3", 64),
	})
	if _, err := repository.db.ExecContext(ctx, `
		UPDATE rin_renderer.render_jobs SET request_metadata=$2::jsonb WHERE id=$1::uuid`,
		queuedCancelJob.ID, legacyMetadata); err != nil {
		t.Fatalf("prepare legacy queued completion metadata: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.idempotency_keys (
			principal_id,owner_scope,idempotency_key,request_hash,job_id,expires_at
		) VALUES ($1,$2,$3,$4,$5::uuid,$6)`, queuedCancelJob.PrincipalID, queuedCancelJob.OwnerScope,
		"render-"+strings.Repeat("3", 32), strings.Repeat("9", 64), queuedCancelJob.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("prepare legacy queued completion idempotency: %v", err)
	}
	if canceled, err := repository.RequestCancellation(ctx, queuedCancelJob.ID, now.Add(13*time.Second)); err != nil ||
		canceled.Job.State != "canceled" || countOutbox(queuedCancelJob.ID) != 1 {
		t.Fatalf("queued cancellation completion = %#v, outbox=%d, %v", canceled, countOutbox(queuedCancelJob.ID), err)
	}
	if _, err := repository.RequestCancellation(ctx, queuedCancelJob.ID, now.Add(14*time.Second)); err != nil || countOutbox(queuedCancelJob.ID) != 1 {
		t.Fatalf("repeated queued cancellation = outbox %d, %v", countOutbox(queuedCancelJob.ID), err)
	}
	var legacyRequestID string
	if err := repository.db.QueryRowContext(ctx, `
		SELECT request_id FROM rin_renderer.renderer_completion_outbox WHERE job_id=$1::uuid`, queuedCancelJob.ID).Scan(&legacyRequestID); err != nil ||
		legacyRequestID != "render-"+strings.Repeat("3", 32) {
		t.Fatalf("legacy completion request identity = %q, %v", legacyRequestID, err)
	}

	runningCancelJob := createQueued("44444444-4444-4444-8444-444444444444", "4", "article:44", 1, now.Add(15*time.Second))
	runningCancelLease := claim(leadership, runningCancelJob.ID, "54545454-5454-4454-8454-545454545454", strings.Repeat("6", 64), now.Add(16*time.Second))
	if requested, err := repository.RequestCancellation(ctx, runningCancelJob.ID, now.Add(17*time.Second)); err != nil ||
		!requested.WorkerMustStop || countOutbox(runningCancelJob.ID) != 0 {
		t.Fatalf("running cancellation request = %#v, outbox=%d, %v", requested, countOutbox(runningCancelJob.ID), err)
	}
	if canceled, err := repository.ConfirmCancellation(ctx, runningCancelLease.AttemptID, strings.Repeat("6", 64), now.Add(18*time.Second)); err != nil ||
		canceled.State != "canceled" || countOutbox(runningCancelJob.ID) != 1 {
		t.Fatalf("running cancellation confirmation = %#v, outbox=%d, %v", canceled, countOutbox(runningCancelJob.ID), err)
	}

	raceJob := createQueued("45454545-4545-4545-8545-454545454545", "5", "article:45", 1, now.Add(19*time.Second))
	raceLease := claim(leadership, raceJob.ID, "55555555-5555-4555-8555-555555555555", strings.Repeat("7", 64), now.Add(20*time.Second))
	if _, err := repository.RequestCancellation(ctx, raceJob.ID, now.Add(21*time.Second)); err != nil {
		t.Fatalf("request cancellation race: %v", err)
	}
	raceEvidence := CompletionResultEvidence{RendererProjectHash: strings.Repeat("5", 64), ResultHash: strings.Repeat("6", 64)}
	raceErrors := make(chan error, 2)
	go func() {
		_, finishErr := repository.FinishAttempt(ctx, FinishInput{
			AttemptID: raceLease.AttemptID, LeaseTokenHash: strings.Repeat("7", 64), Now: now.Add(22 * time.Second),
			Succeeded: true, ResultArtifact: &successResult, CompletionResult: &raceEvidence,
		})
		raceErrors <- finishErr
	}()
	go func() {
		_, confirmErr := repository.ConfirmCancellation(ctx, raceLease.AttemptID, strings.Repeat("7", 64), now.Add(22*time.Second))
		raceErrors <- confirmErr
	}()
	firstRaceError, secondRaceError := <-raceErrors, <-raceErrors
	validRace := (firstRaceError == nil && errors.Is(secondRaceError, ErrLeaseLost)) ||
		(secondRaceError == nil && errors.Is(firstRaceError, ErrLeaseLost))
	raceTerminal, err := repository.Job(ctx, raceJob.ID)
	if !validRace || err != nil || raceTerminal.State != "canceled" || countOutbox(raceJob.ID) != 1 {
		t.Fatalf("cancellation terminal race = state %#v, errors %v/%v, outbox=%d, read=%v", raceTerminal, firstRaceError, secondRaceError, countOutbox(raceJob.ID), err)
	}

	cancelRecoveryJob := createQueued("46464646-4646-4646-8646-464646464646", "6", "article:46", 1, now.Add(23*time.Second))
	cancelRecoveryLease := claim(leadership, cancelRecoveryJob.ID, "56565656-5656-4656-8656-565656565656", strings.Repeat("8", 64), now.Add(24*time.Second))
	if _, err := repository.RequestCancellation(ctx, cancelRecoveryJob.ID, now.Add(25*time.Second)); err != nil {
		t.Fatalf("request cancellation for lease recovery: %v", err)
	}
	cancelRecoveryAt := now.Add(36 * time.Second)
	if recovered, err := repository.RecoverExpiredLeases(ctx, leadership, cancelRecoveryAt, cancelRecoveryAt.Add(time.Second), 10); err != nil || recovered != 1 {
		t.Fatalf("recover bounded cancellation lease = %d, %v", recovered, err)
	}
	cancelRecoveryTerminal, err := repository.Job(ctx, cancelRecoveryJob.ID)
	if err != nil || cancelRecoveryTerminal.State != "canceled" || countOutbox(cancelRecoveryJob.ID) != 1 {
		t.Fatalf("bounded cancellation recovery = %#v, outbox=%d, %v", cancelRecoveryTerminal, countOutbox(cancelRecoveryJob.ID), err)
	}
	if _, err := repository.ConfirmCancellation(ctx, cancelRecoveryLease.AttemptID, strings.Repeat("8", 64), cancelRecoveryAt); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("recovered cancellation accepted stale worker confirmation: %v", err)
	}

	recoveryJob := createQueued("47474747-4747-4747-8747-474747474747", "7", "article:47", 1, now.Add(37*time.Second))
	recoveryLease := claim(leadership, recoveryJob.ID, "57575757-5757-4757-8757-575757575757", strings.Repeat("9", 64), now.Add(38*time.Second))
	recoveryAt := recoveryLease.LeaseExpiresAt.Add(time.Second)
	if recovered, err := repository.RecoverExpiredLeases(ctx, leadership, recoveryAt, recoveryAt.Add(time.Second), 10); err != nil ||
		recovered != 1 || countOutbox(recoveryJob.ID) != 1 {
		t.Fatalf("lease-exhausted completion = %d, outbox=%d, %v", recovered, countOutbox(recoveryJob.ID), err)
	}

	compatQueuedAt := now.Add(2 * time.Minute)
	compatMetadata, _ := json.Marshal(map[string]string{
		"requestId": "render-" + strings.Repeat("8", 32), "sourceName": "long-book.zip", "entrypoint": "main.tex",
	})
	compatJob, err := repository.CreateJob(ctx, Job{
		ID: "48484848-4848-4848-8848-484848484848", PrincipalID: "atomic-compat", OwnerScope: "mixed-long",
		ContentKind: "latex", DocumentEngine: "latexml", ResourceClass: "document-latexml",
		PriorityClass: "migration", State: "queued", SourceArtifactID: source.ID,
		ProjectHash: strings.Repeat("8", 64), OptionsHash: strings.Repeat("f", 64),
		RendererVersion: "atomic-test", RequestMetadata: compatMetadata,
		MaxAttempts: 1, QueuedAt: &compatQueuedAt, AvailableAt: &compatQueuedAt,
		ExpiresAt: compatQueuedAt.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create compatibility cancellation job: %v", err)
	}
	compatLease := claim(leadership, compatJob.ID, "58585858-5858-4858-8858-585858585858", strings.Repeat("a", 64), compatQueuedAt.Add(time.Second))
	if _, err := repository.RequestCancellation(ctx, compatJob.ID, compatQueuedAt.Add(2*time.Second)); err != nil {
		t.Fatalf("request compatibility cancellation: %v", err)
	}
	if canceled, err := repository.ConfirmCancellation(ctx, compatLease.AttemptID, strings.Repeat("a", 64), compatQueuedAt.Add(13*time.Second)); err != nil ||
		canceled.State != "canceled" || countOutbox(compatJob.ID) != 0 {
		t.Fatalf("compatibility cancellation = %#v, outbox=%d, %v", canceled, countOutbox(compatJob.ID), err)
	}

	if err := leadership.Close(); err != nil {
		t.Fatalf("close atomic completion leadership: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		DELETE FROM rin_renderer.renderer_completion_outbox
		WHERE job_id IN (SELECT id FROM rin_renderer.render_jobs WHERE principal_id LIKE 'atomic-%')`); err != nil {
		t.Fatalf("clean atomic completion outbox: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_jobs WHERE principal_id LIKE 'atomic-%'`); err != nil {
		t.Fatalf("clean atomic completion jobs: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_artifacts WHERE id=$1`, successResult.ID); err != nil {
		t.Fatalf("clean atomic completion artifact: %v", err)
	}
}

func testPostgresCacheIndex(t *testing.T, ctx context.Context, repository *Repository, now time.Time) {
	t.Helper()
	versions := map[string]string{}
	for _, name := range orchestration.RequiredCacheVersions(orchestration.CacheStageMath) {
		versions[name] = name + "/v1"
	}
	key, err := orchestration.BuildCacheKey(orchestration.CacheKeyInput{
		Stage:            orchestration.CacheStageMath,
		NormalizedInputs: map[string]string{"source": `\not\exists`, "display": "false"},
		ProjectContext:   map[string]string{"macro-hash": strings.Repeat("1", 64)},
		Versions:         versions,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"schemaVersion":"rin-math-cache/v1","html":"<mjx-container></mjx-container>"}`)
	digest := sha256.Sum256(body)
	hexDigest := hex.EncodeToString(digest[:])
	artifactID, err := orchestration.CacheArtifactID(orchestration.CacheStageMath, body)
	if err != nil {
		t.Fatal(err)
	}
	artifactExpiry := now.Add(2 * time.Hour).UTC().Truncate(time.Second)
	artifact, err := repository.CreateArtifact(ctx, Artifact{
		ID:     artifactID,
		SHA256: hexDigest, Kind: "cache", Visibility: "private",
		StorageKey: artifactID,
		ByteSize:   int64(len(body)), MediaType: "application/json", SchemaVersion: "rin-math-cache/v1",
		ExpiresAt: &artifactExpiry,
	})
	if err != nil {
		t.Fatalf("create cache artifact: %v", err)
	}
	entry := orchestration.CacheRecord{
		Key: key,
		Artifact: contracts.ArtifactReference{
			ArtifactID: artifact.ID, SHA256: artifact.SHA256, Bytes: artifact.ByteSize,
			MediaType: artifact.MediaType, Visibility: artifact.Visibility, ExpiresAt: artifactExpiry.Format(time.RFC3339),
		},
		SchemaVersion: artifact.SchemaVersion, ExpiresAt: now.Add(time.Hour),
	}
	if err := repository.PutCacheEntry(ctx, entry); err != nil {
		t.Fatalf("PutCacheEntry() error = %v", err)
	}
	if err := repository.PutCacheEntry(ctx, entry); err != nil {
		t.Fatalf("idempotent PutCacheEntry() error = %v", err)
	}
	read, found, err := repository.CacheEntry(ctx, key, now)
	if err != nil || !found || read.Key != key || read.Artifact != entry.Artifact ||
		read.SchemaVersion != entry.SchemaVersion || read.ArtifactSchemaVersion != entry.SchemaVersion {
		t.Fatalf("CacheEntry() = %#v, %t, %v", read, found, err)
	}
	if err := repository.TouchCacheEntry(ctx, key, now.Add(time.Minute)); err != nil {
		t.Fatalf("TouchCacheEntry() error = %v", err)
	}
	var lastHit time.Time
	if err := repository.db.QueryRowContext(ctx, `SELECT last_hit_at FROM rin_renderer.render_cache_entries WHERE cache_key = $1`, key.String()).Scan(&lastHit); err != nil || lastHit.Before(now.Add(time.Minute)) {
		t.Fatalf("cache last hit = %s, %v", lastHit, err)
	}
	conflict := entry
	conflict.ExpiresAt = entry.ExpiresAt.Add(time.Minute)
	if err := repository.PutCacheEntry(ctx, conflict); !errors.Is(err, ErrCacheConflict) {
		t.Fatalf("immutable cache conflict error = %v", err)
	}
	if _, found, err := repository.CacheEntry(ctx, key, entry.ExpiresAt); err != nil || found {
		t.Fatalf("expired CacheEntry() found=%t error=%v", found, err)
	}
	testPostgresPublicDiagramCache(t, ctx, repository, now)
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_cache_entries
			(cache_key, stage, artifact_id, schema_version, renderer_version, expires_at)
		VALUES ('unsafe-key', 'math', $1, 'rin-math-cache/v1', 'rin-cache-key/v1', $2)`,
		artifact.ID, now.Add(time.Hour)); err == nil {
		t.Fatal("cache key constraint accepted an unsafe key")
	}
	for name, values := range map[string][]string{
		"stage":   {"unknown", "rin-cache-key/v1", strings.Repeat("2", 64)},
		"version": {"math", "rin-cache-key/v0", strings.Repeat("3", 64)},
	} {
		if _, err := repository.db.ExecContext(ctx, `
			INSERT INTO rin_renderer.render_cache_entries
				(cache_key, stage, artifact_id, schema_version, renderer_version, expires_at)
			VALUES ($1, $2, $3, 'rin-math-cache/v1', $4, $5)`,
			"rin-cache/v1/math/"+values[2], values[0], artifact.ID, values[1], now.Add(time.Hour)); err == nil {
			t.Fatalf("cache %s constraint accepted invalid metadata", name)
		}
	}
	if err := repository.DeleteCacheEntry(ctx, key); err != nil {
		t.Fatalf("DeleteCacheEntry() error = %v", err)
	}
	if _, found, err := repository.CacheEntry(ctx, key, now); err != nil || found {
		t.Fatalf("deleted CacheEntry() found=%t error=%v", found, err)
	}
}

func testPostgresPublicDiagramCache(t *testing.T, ctx context.Context, repository *Repository, now time.Time) {
	t.Helper()
	versions := map[string]string{}
	for _, name := range orchestration.RequiredCacheVersions(orchestration.CacheStageDiagram) {
		versions[name] = name + "/v1"
	}
	key, err := orchestration.BuildCacheKey(orchestration.CacheKeyInput{
		Stage:            orchestration.CacheStageDiagram,
		NormalizedInputs: map[string]string{"source": `\begin{tikzpicture}\end{tikzpicture}`},
		ProjectContext:   map[string]string{"preamble-hash": strings.Repeat("4", 64)},
		Versions:         versions,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	digest := sha256.Sum256(body)
	hexDigest := hex.EncodeToString(digest[:])
	artifactID := "diagrams/v1/svg-sha256/" + hexDigest[:2] + "/" + hexDigest + ".svg"
	artifact, err := repository.CreateArtifact(ctx, Artifact{
		ID: artifactID, SHA256: hexDigest, Kind: "public-diagram", Visibility: "public",
		StorageKey: artifactID, ByteSize: int64(len(body)), MediaType: "image/svg+xml",
		SchemaVersion: "rin-diagram-svg/v1",
	})
	if err != nil {
		t.Fatalf("create public diagram cache artifact: %v", err)
	}
	entry := orchestration.CacheRecord{
		Key: key,
		Artifact: contracts.ArtifactReference{
			ArtifactID: artifact.ID, SHA256: artifact.SHA256, Bytes: artifact.ByteSize,
			MediaType: artifact.MediaType, Visibility: artifact.Visibility,
		},
		SchemaVersion: artifact.SchemaVersion, ExpiresAt: now.Add(time.Hour),
	}
	if err := repository.PutCacheEntry(ctx, entry); err != nil {
		t.Fatalf("PutCacheEntry(public diagram) error = %v", err)
	}
	read, found, err := repository.CacheEntry(ctx, key, now)
	if err != nil || !found || read.Artifact != entry.Artifact || read.ArtifactSchemaVersion != entry.SchemaVersion {
		t.Fatalf("CacheEntry(public diagram) = %#v, %t, %v", read, found, err)
	}
}

func testPostgresSupportOperations(t *testing.T, ctx context.Context, repository *Repository, now time.Time) {
	t.Helper()
	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_jobs`); err != nil {
		t.Fatalf("reset jobs for support operations: %v", err)
	}
	source := Artifact{
		ID: "support-source", SHA256: strings.Repeat("a", 64), Kind: "source", Visibility: "private",
		StorageKey: "render-jobs/v1/source/51515151-5151-4151-8151-515151515151/" + strings.Repeat("a", 64),
		ByteSize:   10, MediaType: "application/zip", SchemaVersion: "rin-project-archive/v1",
		ExpiresAt: timePointer(now.Add(time.Hour)),
	}
	resultArtifact := Artifact{
		ID: "support-result", SHA256: strings.Repeat("b", 64), Kind: "result", Visibility: "private",
		StorageKey: "render-jobs/v1/result/51515151-5151-4151-8151-515151515151/" + strings.Repeat("b", 64) + ".json",
		ByteSize:   20, MediaType: "application/json", SchemaVersion: "rin-render-stored-output/v1",
		ExpiresAt: timePointer(now.Add(time.Hour)),
	}
	for _, artifact := range []Artifact{source, resultArtifact} {
		if _, err := repository.CreateArtifact(ctx, artifact); err != nil {
			t.Fatalf("create support artifact: %v", err)
		}
	}
	queuedAt, startedAt, finishedAt := now.Add(-time.Minute), now.Add(-30*time.Second), now.Add(-time.Second)
	job, err := repository.CreateJob(ctx, Job{
		ID: "51515151-5151-4151-8151-515151515151", PrincipalID: "support-principal", OwnerScope: "support-owner",
		ContentKind: "latex", DocumentEngine: "latexml", ResourceClass: "document-latexml",
		PriorityClass: "publish", State: "failed", SourceArtifactID: source.ID, ResultArtifactID: resultArtifact.ID,
		ProjectHash: strings.Repeat("c", 64), OptionsHash: strings.Repeat("d", 64), RendererVersion: "support-version",
		MaxAttempts: 2, QueuedAt: &queuedAt, StartedAt: &startedAt, FinishedAt: &finishedAt,
		ExpiresAt: now.Add(time.Hour), DeclaredSourceBytes: 10,
	})
	if err != nil {
		t.Fatalf("create support job: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_attempts (
			id, job_id, attempt_no, state, error_code, duration_ms, started_at, finished_at
		) VALUES ('52525252-5252-4252-8252-525252525252', $1::uuid, 1, 'failed',
			'document_invalid', 29000, $2, $3)`, job.ID, startedAt, finishedAt); err != nil {
		t.Fatalf("create support attempt: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_wait_estimates (
			job_id, estimator_version, estimated_start_at, earliest_start_at, latest_start_at,
			confidence, sample_count, scope, calculated_at, actual_started_at,
			central_error_ms, interval_covered
		) VALUES ($1::uuid, $2, $3, $3, $4, 'low', 20, 'instance', $5, $6, 500, true)`,
		job.ID, WaitEstimatorVersion, startedAt.Add(-500*time.Millisecond), startedAt.Add(time.Second), queuedAt, startedAt); err != nil {
		t.Fatalf("create support estimate: %v", err)
	}
	snapshot, err := repository.AuthorizedSupportSnapshot(ctx, job.ID, job.PrincipalID, job.OwnerScope, now)
	if err != nil || snapshot.Job.AttemptCount != 1 || len(snapshot.Attempts) != 1 ||
		snapshot.Estimator == nil || snapshot.Estimator.EstimatorVersion != WaitEstimatorVersion ||
		len(snapshot.Artifacts) != 2 || snapshot.Artifacts[0].Visibility != "private" {
		t.Fatalf("AuthorizedSupportSnapshot() = %#v, %v", snapshot, err)
	}
	if _, err := repository.AuthorizedSupportSnapshot(ctx, job.ID, job.PrincipalID, "another-owner", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner support snapshot error = %v", err)
	}
	if _, err := repository.AuthorizedManualRetry(ctx, job.ID, job.PrincipalID, "another-owner", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner manual retry error = %v", err)
	}
	if _, err := repository.AuthorizedManualExpire(ctx, job.ID, job.PrincipalID, "another-owner", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner manual expiry error = %v", err)
	}
	retried, err := repository.AuthorizedManualRetry(ctx, job.ID, job.PrincipalID, job.OwnerScope, now)
	if err != nil || retried.State != "queued" || retried.ResultArtifactID != "" || retried.StartedAt != nil || retried.FinishedAt != nil {
		t.Fatalf("AuthorizedManualRetry() = %#v, %v", retried, err)
	}
	if _, err := repository.AuthorizedManualRetry(ctx, job.ID, job.PrincipalID, job.OwnerScope, now); !errors.Is(err, ErrSupportActionNotAllowed) {
		t.Fatalf("repeat manual retry error = %v", err)
	}
	secondStarted, secondFinished := now.Add(time.Second), now.Add(2*time.Second)
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_attempts (
			id, job_id, attempt_no, state, error_code, duration_ms, started_at, finished_at
		) VALUES ('53535353-5353-4353-8353-535353535353', $1::uuid, 2, 'failed',
			'document_invalid', 1000, $2, $3)`,
		job.ID, secondStarted, secondFinished); err != nil {
		t.Fatalf("exhaust support retry budget: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `
		UPDATE rin_renderer.render_jobs SET state = 'failed', finished_at = $2 WHERE id = $1::uuid`,
		job.ID, secondFinished); err != nil {
		t.Fatalf("finish exhausted support job: %v", err)
	}
	if _, err := repository.AuthorizedManualRetry(ctx, job.ID, job.PrincipalID, job.OwnerScope, secondFinished); !errors.Is(err, ErrSupportActionNotAllowed) {
		t.Fatalf("exhausted manual retry error = %v", err)
	}
	expired, err := repository.AuthorizedManualExpire(ctx, job.ID, job.PrincipalID, job.OwnerScope, secondFinished)
	if err != nil || expired.State != "expired" || !expired.ExpiresAt.Equal(secondFinished) {
		t.Fatalf("AuthorizedManualExpire() = %#v, %v", expired, err)
	}
	var sourceExpiry time.Time
	if err := repository.db.QueryRowContext(ctx, `SELECT expires_at FROM rin_renderer.render_artifacts WHERE id = $1`, source.ID).Scan(&sourceExpiry); err != nil || !sourceExpiry.Equal(secondFinished) {
		t.Fatalf("manual expiry artifact retention = %s, %v", sourceExpiry, err)
	}
	var auditEvents int
	if err := repository.db.QueryRowContext(ctx, `
		SELECT count(*) FROM rin_renderer.render_job_events
		WHERE job_id = $1::uuid AND event_type IN ('manual_retry_queued', 'manual_expired')`, job.ID).Scan(&auditEvents); err != nil || auditEvents != 2 {
		t.Fatalf("support audit events = %d, %v", auditEvents, err)
	}
}

func testPostgresWaitEstimation(t *testing.T, ctx context.Context, repository *Repository, artifact Artifact, now time.Time) {
	t.Helper()
	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.render_jobs`); err != nil {
		t.Fatalf("reset jobs for wait estimation: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `DELETE FROM rin_renderer.workload_profiles`); err != nil {
		t.Fatalf("reset profiles for wait estimation: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, `UPDATE rin_renderer.scheduler_priority_state SET dispatch_count = 0`); err != nil {
		t.Fatalf("reset fairness for wait estimation: %v", err)
	}
	createEstimateJob := func(id, principal string, queued time.Time) (Job, WorkloadRecord) {
		job, err := repository.CreateJob(ctx, Job{
			ID: id, PrincipalID: principal, OwnerScope: "wait-owner", ContentKind: "markdown",
			DocumentEngine: "unified", ResourceClass: "document-light", PriorityClass: "publish",
			State: "queued", SourceArtifactID: artifact.ID, ProjectHash: strings.Repeat("c", 64),
			OptionsHash: strings.Repeat("d", 64), RendererVersion: "wait-version", MaxAttempts: 1,
			QueuedAt: &queued, AvailableAt: &queued, ExpiresAt: now.Add(8 * 24 * time.Hour),
			DeclaredSourceBytes: 1024,
		})
		if err != nil {
			t.Fatalf("create wait estimate job: %v", err)
		}
		workload, err := AdmissionWorkload(WorkloadAdmission{
			ContentKind: "markdown", DocumentEngine: "unified", ResourceClass: "document-light",
			PriorityClass: "publish", ProjectBytes: 1024, DocumentClass: "article",
		})
		if err != nil {
			t.Fatal(err)
		}
		workload.JobID = job.ID
		tx, err := repository.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := upsertJobWorkload(ctx, tx, workload); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return job, workload
	}
	ahead, workload := createEstimateJob("41414141-4141-4141-8141-414141414141", "wait-ahead", now.Add(-time.Second))
	target, _ := createEstimateJob("42424242-4242-4242-8242-424242424242", "wait-target", now)
	if _, err := repository.db.ExecContext(ctx, `
		INSERT INTO rin_renderer.workload_profiles (
			profile_key, renderer_version, sample_count, p50_ms, p90_ms, mean_ms, samples_ms
		) VALUES ($1, 'wait-version', 20, 1000, 2000, 1500,
			array_fill(1000::bigint, ARRAY[10]) || array_fill(2000::bigint, ARRAY[10]))`, workload.ProfileKey); err != nil {
		t.Fatalf("seed wait profile: %v", err)
	}
	policy := EstimationPolicy{MinimumSamples: 20, Scheduling: testSchedulingPolicy()}
	policy.Scheduling.Resources.DocumentLight = 1
	queue, err := repository.AuthorizedJobQueue(ctx, target.ID, target.PrincipalID, target.OwnerScope, now, policy)
	if err != nil || queue.Estimate == nil || queue.JobsAheadEstimate != 1 ||
		!queue.Estimate.EstimatedStartRange.Earliest.Equal(now.Add(time.Second)) ||
		!queue.Estimate.EstimatedStartRange.Latest.Equal(now.Add(2*time.Second)) ||
		queue.Estimate.SampleCount != 20 || queue.Estimate.Confidence != "low" {
		t.Fatalf("sampled wait estimate = %#v, %v", queue, err)
	}
	cancelAt := now.Add(100 * time.Millisecond)
	if _, err := repository.RequestCancellation(ctx, ahead.ID, cancelAt); err != nil {
		t.Fatalf("cancel estimated job ahead: %v", err)
	}
	queue, err = repository.AuthorizedJobQueue(ctx, target.ID, target.PrincipalID, target.OwnerScope, cancelAt, policy)
	if err != nil || queue.Estimate == nil || queue.JobsAheadEstimate != 0 ||
		!queue.Estimate.EstimatedStartAt.Equal(cancelAt) {
		t.Fatalf("recalculated wait estimate = %#v, %v", queue, err)
	}
	leader, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatalf("acquire wait estimation leadership: %v", err)
	}
	actualStart := now.Add(500 * time.Millisecond)
	lease, err := repository.ClaimNext(ctx, leader, ClaimInput{
		AttemptID: "43434343-4343-4343-8343-434343434343", WorkerID: "wait-worker",
		LeaseTokenHash: strings.Repeat("9", 64), Now: actualStart,
		LeaseExpiresAt: actualStart.Add(time.Minute), Policy: policy.Scheduling,
	})
	if err != nil || lease.Job.ID != target.ID {
		t.Fatalf("claim estimated target = %#v, %v", lease, err)
	}
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	var recordedStart time.Time
	var centralError int64
	var covered bool
	if err := repository.db.QueryRowContext(ctx, `
		SELECT actual_started_at, central_error_ms, interval_covered
		FROM rin_renderer.render_job_wait_estimates WHERE job_id = $1::uuid`, target.ID).Scan(
		&recordedStart, &centralError, &covered); err != nil {
		t.Fatalf("read wait calibration: %v", err)
	}
	if !recordedStart.Equal(actualStart) || centralError != 400 || covered {
		t.Fatalf("wait calibration = %s, %d, %v", recordedStart, centralError, covered)
	}
}

func assertDatabaseRejects(t *testing.T, db *sql.DB, ctx context.Context, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err == nil {
		t.Fatalf("database accepted invalid row for query %q", query)
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func testAdmissionInput(id string, principal string, ownerScope string, declaredBytes int64, now time.Time) AdmissionReservationInput {
	workload, err := AdmissionWorkload(WorkloadAdmission{
		ContentKind: "latex", DocumentEngine: "latexml", ResourceClass: "document-latexml",
		PriorityClass: "publish", ProjectBytes: declaredBytes,
	})
	if err != nil {
		panic(err)
	}
	return AdmissionReservationInput{
		Job: Job{
			ID:              id,
			PrincipalID:     principal,
			OwnerScope:      ownerScope,
			ContentKind:     "latex",
			DocumentEngine:  "latexml",
			ResourceClass:   "document-latexml",
			PriorityClass:   "publish",
			ProjectHash:     strings.Repeat("1", 64),
			OptionsHash:     strings.Repeat("2", 64),
			RendererVersion: "test-version",
			MaxAttempts:     2,
			ExpiresAt:       now.Add(8 * 24 * time.Hour),
		},
		Workload:             workload,
		DeclaredSourceBytes:  declaredBytes,
		AdmissionTokenHash:   strings.Repeat("3", 64),
		IdempotencyKey:       "idempotency-" + id,
		RequestHash:          strings.Repeat("4", 64),
		IdempotencyExpiresAt: now.Add(24 * time.Hour),
		ReservationExpiresAt: now.Add(5 * time.Minute),
		Now:                  now,
	}
}

func TestPostgresTypstPDFPreviewQueueSemantics(t *testing.T) {
	dataSourceName := os.Getenv("RIN_RENDERER_TEST_DATABASE_URL")
	if dataSourceName == "" {
		t.Skip("RIN_RENDERER_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	repository, err := Open(ctx, dataSourceName)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS rin_renderer CASCADE`); err != nil {
		t.Fatalf("reset isolated typst schema: %v", err)
	}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("migrate typst schema: %v", err)
	}

	now := time.Date(2026, 9, 16, 3, 0, 0, 0, time.UTC)
	limits := AdmissionLimits{GlobalNonTerminal: 100, PrincipalQueued: 8, PreviewQueued: 20, PrivateSourceBytes: 1 << 20}

	// A Typst preview admission must persist content_kind = 'typst' and a queued
	// typst-pdf job, proving the widened DB constraint and workload path accept it.
	firstInput := testTypstPreviewAdmissionInput("90000000-0000-4000-8000-000000000001", "typst-first", "typst-context-a", "a", now)
	first, err := repository.ReserveAdmission(ctx, firstInput, limits)
	if err != nil {
		t.Fatalf("reserve first typst preview: %v", err)
	}
	if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(first, firstInput, now.Add(time.Second))); err != nil {
		t.Fatalf("commit first typst preview: %v", err)
	}
	var persistedKind, persistedClass string
	if err := repository.db.QueryRowContext(ctx, `
		SELECT content_kind, resource_class FROM rin_renderer.render_jobs WHERE id = $1::uuid`, first.Job.ID).Scan(
		&persistedKind, &persistedClass); err != nil || persistedKind != "typst" || persistedClass != "typst-pdf" {
		t.Fatalf("persisted typst job = %q/%q, %v", persistedKind, persistedClass, err)
	}

	// Snapshot revisions supersede only within the Typst preview pool.
	secondInput := testTypstPreviewAdmissionInput("90000000-0000-4000-8000-000000000002", "typst-second", "typst-context-a", "b", now.Add(2*time.Second))
	second, err := repository.ReserveAdmission(ctx, secondInput, limits)
	if err != nil {
		t.Fatalf("reserve second typst preview: %v", err)
	}
	if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(second, secondInput, now.Add(3*time.Second))); err != nil {
		t.Fatalf("commit second typst preview: %v", err)
	}
	var firstState, firstSuperseded string
	if err := repository.db.QueryRowContext(ctx, `
		SELECT state, COALESCE(superseded_by::text, '') FROM rin_renderer.render_jobs WHERE id = $1::uuid`, first.Job.ID).Scan(
		&firstState, &firstSuperseded); err != nil || firstState != "canceled" || firstSuperseded != second.Job.ID {
		t.Fatalf("superseded typst preview = %q -> %q, %v", firstState, firstSuperseded, err)
	}

	// A LaTeX preview for the same principal and context shares neither pool nor
	// supersede scope, so the queued Typst preview survives it.
	latexInput := testPreviewAdmissionInput("90000000-0000-4000-8000-000000000003", "latex-parallel", "typst-context-a", "c", now.Add(4*time.Second))
	latex, err := repository.ReserveAdmission(ctx, latexInput, limits)
	if err != nil {
		t.Fatalf("reserve parallel latex preview: %v", err)
	}
	if _, err := repository.CommitAdmission(ctx, testPreviewCommitInput(latex, latexInput, now.Add(5*time.Second))); err != nil {
		t.Fatalf("commit parallel latex preview: %v", err)
	}
	var typstQueued, latexQueued int
	if err := repository.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE resource_class = 'typst-pdf' AND state = 'queued'),
			count(*) FILTER (WHERE resource_class = 'latex-pdf' AND state = 'queued')
		FROM rin_renderer.render_jobs
		WHERE principal_id = 'preview-principal' AND owner_scope = 'typst-context-a'`).Scan(
		&typstQueued, &latexQueued); err != nil || typstQueued != 1 || latexQueued != 1 {
		t.Fatalf("cross-pool queued typst/latex = %d/%d, %v", typstQueued, latexQueued, err)
	}

	// A general document worker must never claim a Typst job; only the dedicated
	// typst-pdf resource leadership may.
	leader, err := repository.AcquireLeadership(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	typstLeader, err := repository.AcquireResourceLeadership(ctx, "typst-pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer typstLeader.Close()
	policy := testSchedulingPolicy()
	policy.Resources.Heavy = 1
	claimAt := now.Add(6 * time.Second)
	if _, err := repository.ClaimNext(ctx, leader, ClaimInput{
		AttemptID: "91000000-0000-4000-8000-000000000001", WorkerID: "ordinary-worker",
		LeaseTokenHash: strings.Repeat("1", 64), Now: claimAt,
		LeaseExpiresAt: claimAt.Add(time.Minute), Policy: policy,
	}); !errors.Is(err, ErrNoEligibleJob) {
		t.Fatalf("ordinary worker typst claim = %v", err)
	}
	lease, err := repository.ClaimNext(ctx, typstLeader, ClaimInput{
		AttemptID: "91000000-0000-4000-8000-000000000002", WorkerID: "typst-worker",
		LeaseTokenHash: strings.Repeat("2", 64), Now: claimAt,
		LeaseExpiresAt: claimAt.Add(time.Minute), Policy: policy, ResourceClass: "typst-pdf",
	})
	if err != nil || lease.Job.ID != second.Job.ID || lease.Job.ContentKind != "typst" {
		t.Fatalf("dedicated typst claim = %#v, %v", lease, err)
	}

	// Finishing stores a private Typst result that the next identical preview
	// reuses as a context-scoped content cache hit.
	resultExpires := now.Add(24 * time.Hour)
	resultArtifact := Artifact{
		ID:     "render-jobs/v1/result/" + second.Job.ID + "/" + strings.Repeat("e", 64) + ".pdf",
		SHA256: strings.Repeat("e", 64), Kind: "result", Visibility: "private",
		StorageKey: "render-jobs/v1/result/" + second.Job.ID + "/" + strings.Repeat("e", 64) + ".pdf",
		ByteSize:   128, MediaType: "application/pdf", SchemaVersion: "rin-typst-pdf-preview/v1", ExpiresAt: &resultExpires,
	}
	finished, err := repository.FinishAttempt(ctx, FinishInput{
		AttemptID: lease.AttemptID, LeaseTokenHash: strings.Repeat("2", 64),
		Now: now.Add(7 * time.Second), Succeeded: true, ResultArtifact: &resultArtifact,
	})
	if err != nil || finished.State != "succeeded" {
		t.Fatalf("finish typst preview = %#v, %v", finished, err)
	}
	cacheInput := testTypstPreviewAdmissionInput("90000000-0000-4000-8000-000000000004", "typst-cache", "typst-context-a", "b", now.Add(8*time.Second))
	cached, err := repository.ReserveAdmission(ctx, cacheInput, limits)
	if err != nil || !cached.Reused || cached.Uploading || cached.Job.ID != second.Job.ID {
		t.Fatalf("typst content cache = %#v, %v", cached, err)
	}

	// The content cache is context-bound: the same snapshot in another tenant
	// context must not reuse this private Typst result.
	foreignInput := testTypstPreviewAdmissionInput("90000000-0000-4000-8000-000000000005", "typst-foreign", "typst-context-b", "b", now.Add(9*time.Second))
	foreign, err := repository.ReserveAdmission(ctx, foreignInput, limits)
	if err != nil || foreign.Reused || foreign.Job.ID == second.Job.ID {
		t.Fatalf("cross-tenant typst cache isolation = %#v, %v", foreign, err)
	}
}

func testPreviewAdmissionInput(id, idempotencyKey, ownerScope, snapshotNibble string, now time.Time) AdmissionReservationInput {
	workload, err := AdmissionWorkload(WorkloadAdmission{
		ContentKind: "latex", DocumentEngine: "latexmk", ResourceClass: "latex-pdf",
		PriorityClass: "preview", ProjectBytes: 3,
	})
	if err != nil {
		panic(err)
	}
	requestHash := sha256.Sum256([]byte("request:" + id))
	return AdmissionReservationInput{
		Job: Job{
			ID: id, PrincipalID: "preview-principal", OwnerScope: ownerScope,
			ContentKind: "latex", DocumentEngine: "latexmk", ResourceClass: "latex-pdf",
			PriorityClass: "preview", ProjectHash: strings.Repeat(snapshotNibble, 64),
			OptionsHash: strings.Repeat("2", 64), RendererVersion: "preview-test-version",
			RequestMetadata: json.RawMessage(`{"outputKind":"latex-pdf-preview","entrypoint":"main.tex"}`),
			MaxAttempts:     2, ExpiresAt: now.Add(8 * 24 * time.Hour),
		},
		Workload: workload, DeclaredSourceBytes: 3, AdmissionTokenHash: testDigest([]byte("token:" + id)),
		IdempotencyKey: idempotencyKey, RequestHash: hex.EncodeToString(requestHash[:]),
		IdempotencyExpiresAt: now.Add(24 * time.Hour), ReservationExpiresAt: now.Add(5 * time.Minute), Now: now,
	}
}

func testTypstPreviewAdmissionInput(id, idempotencyKey, ownerScope, snapshotNibble string, now time.Time) AdmissionReservationInput {
	workload, err := AdmissionWorkload(WorkloadAdmission{
		ContentKind: "typst", DocumentEngine: "typst", ResourceClass: "typst-pdf",
		PriorityClass: "preview", ProjectBytes: 3,
	})
	if err != nil {
		panic(err)
	}
	requestHash := sha256.Sum256([]byte("typst-request:" + id))
	return AdmissionReservationInput{
		Job: Job{
			ID: id, PrincipalID: "preview-principal", OwnerScope: ownerScope,
			ContentKind: "typst", DocumentEngine: "typst", ResourceClass: "typst-pdf",
			PriorityClass: "preview", ProjectHash: strings.Repeat(snapshotNibble, 64),
			OptionsHash: strings.Repeat("2", 64), RendererVersion: "typst-preview-test-version",
			RequestMetadata: json.RawMessage(`{"outputKind":"typst-pdf-preview","entrypoint":"main.typ"}`),
			MaxAttempts:     2, ExpiresAt: now.Add(8 * 24 * time.Hour),
		},
		Workload: workload, DeclaredSourceBytes: 3, AdmissionTokenHash: testDigest([]byte("typst-token:" + id)),
		IdempotencyKey: idempotencyKey, RequestHash: hex.EncodeToString(requestHash[:]),
		IdempotencyExpiresAt: now.Add(24 * time.Hour), ReservationExpiresAt: now.Add(5 * time.Minute), Now: now,
	}
}

func testPreviewCommitInput(reservation AdmissionReservation, input AdmissionReservationInput, queuedAt time.Time) CommitAdmissionInput {
	digest := testDigest([]byte("source:" + reservation.Job.ID))
	artifactID := "render-jobs/v1/source/" + reservation.Job.ID + "/" + digest
	expiresAt := queuedAt.Add(24 * time.Hour)
	return CommitAdmissionInput{
		JobID: reservation.Job.ID, AdmissionTokenHash: input.AdmissionTokenHash,
		Artifact: Artifact{
			ID: artifactID, SHA256: digest, Kind: "source", Visibility: "private",
			StorageKey: artifactID, ByteSize: 3, MediaType: "application/x-tar",
			SchemaVersion: "rinspace-snapshot/v1", ExpiresAt: &expiresAt,
		},
		Workload: input.Workload, QueuedAt: queuedAt, PreviewQueueLimit: 20,
	}
}

func testDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func testSchedulingPolicy() SchedulingPolicy {
	return SchedulingPolicy{
		Resources: ResourceCapacities{
			DocumentLight: 2, DocumentLaTeXML: 1, MathNode: 2, TeXSVG: 2, BatchMigration: 1, LatexPDF: 1,
			DocumentTypst: 1, TypstPDF: 1, Heavy: 5,
		},
		Weights:          PriorityWeights{Publish: 8, Preview: 5, Rebuild: 3, Migration: 1},
		AgingInterval:    5 * time.Minute,
		MaxAgingSteps:    12,
		PrincipalRunning: 2,
	}
}

// lib/pq-style array text is accepted by PostgreSQL's text[] input parser and keeps the repository
// on database/sql without coupling production types to a driver-specific array value.
func stringsToPostgresArray(values []string) string {
	return "{" + strings.Join(values, ",") + "}"
}
