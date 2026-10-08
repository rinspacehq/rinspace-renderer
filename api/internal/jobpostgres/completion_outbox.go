package jobpostgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

var (
	ErrNoCompletionDelivery  = errors.New("no Renderer completion delivery is ready")
	ErrCompletionLeaseLost   = errors.New("Renderer completion delivery lease is no longer active")
	completionHashPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	completionErrorPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,119}$`)
	completionControlPattern = regexp.MustCompile(`[[:cntrl:]]`)
)

type CompletionOutboxInput struct {
	JobID               string
	RequestID           string
	ControlProjectID    string
	SourceCommit        string
	ControlProjectHash  string
	RendererProjectHash string
	TerminalState       string
	ResultReference     string
	ResultHash          string
	FailureCode         string
	OccurredAt          time.Time
	// TerminalVersion identifies this terminal outcome within the job. The
	// attempt that produced the outcome supplies it, so replaying the same
	// terminal transition keeps the same identity and stays idempotent, while a
	// job retried after a terminal outcome publishes a new identity instead of
	// colliding with its own history.
	TerminalVersion int16
}

type CompletionDelivery struct {
	EventID             string
	JobID               string
	SchemaVersion       string
	ProtocolVersion     string
	TerminalVersion     int16
	RequestID           string
	ControlProjectID    string
	SourceCommit        string
	ControlProjectHash  string
	RendererProjectHash string
	TerminalState       string
	ResultReference     string
	ResultHash          string
	FailureCode         string
	State               string
	AttemptCount        int
	LeaseExpiresAt      time.Time
	OccurredAt          time.Time
	CreatedAt           time.Time
}

type ClaimCompletionInput struct {
	LeaseTokenHash string
	Now            time.Time
	LeaseExpiresAt time.Time
}

const CompletionDeliveryMaxAge = 60 * time.Second

type CompletionHealth struct {
	State            string     `json:"state"`
	Pending          int64      `json:"pending"`
	Failed           int64      `json:"failed"`
	Delivering       int64      `json:"delivering"`
	Dead             int64      `json:"dead"`
	OldestAgeSeconds int64      `json:"oldestAgeSeconds"`
	LastAcceptedAt   *time.Time `json:"lastAcceptedAt,omitempty"`
	CheckedAt        time.Time  `json:"checkedAt"`
}

func (health CompletionHealth) Ready() error {
	if health.State != "healthy" {
		return errors.New("completion_delivery_degraded")
	}
	return nil
}

type controlPlaneJobMetadata struct {
	RequestID          string `json:"requestId"`
	ControlProjectID   string `json:"controlProjectId"`
	SourceCommit       string `json:"sourceCommit"`
	ControlProjectHash string `json:"controlProjectHash"`
}

type CompletionResultEvidence struct {
	RendererProjectHash string
	ResultHash          string
}

func insertTerminalCompletionTx(
	ctx context.Context,
	tx *sql.Tx,
	job Job,
	terminalState string,
	resultReference string,
	evidence *CompletionResultEvidence,
	failureCode string,
	terminalVersion int16,
	occurredAt time.Time,
) error {
	var metadata controlPlaneJobMetadata
	if len(job.RequestMetadata) > 0 {
		if err := json.Unmarshal(job.RequestMetadata, &metadata); err != nil {
			return fmt.Errorf("decode Renderer Control Plane job metadata: %w", err)
		}
	}
	hasControlPlaneIdentity := metadata.ControlProjectID != "" || metadata.SourceCommit != "" || metadata.ControlProjectHash != ""
	if !hasControlPlaneIdentity {
		return nil
	}
	if metadata.RequestID == "" {
		requestID, err := legacyControlPlaneRequestID(ctx, tx, job.ID)
		if err != nil {
			return err
		}
		metadata.RequestID = requestID
	}
	if metadata.ControlProjectID != job.OwnerScope {
		return errors.New("Renderer Control Plane project identity does not match the job owner scope")
	}
	input := CompletionOutboxInput{
		JobID: job.ID, RequestID: metadata.RequestID, ControlProjectID: metadata.ControlProjectID,
		SourceCommit: metadata.SourceCommit, ControlProjectHash: metadata.ControlProjectHash,
		RendererProjectHash: job.ProjectHash, TerminalState: terminalState,
		ResultReference: resultReference, FailureCode: failureCode, OccurredAt: occurredAt,
		TerminalVersion: terminalVersion,
	}
	if terminalState == "succeeded" {
		if evidence == nil {
			return errors.New("successful Renderer Control Plane completion evidence is required")
		}
		input.RendererProjectHash = evidence.RendererProjectHash
		input.ResultHash = evidence.ResultHash
	} else if !contracts.IsControlPlaneCompletionFailureCode(input.FailureCode) {
		input.FailureCode = "document_render_failed"
	}
	if err := insertCompletionOutboxTx(ctx, tx, input); err != nil {
		return fmt.Errorf("record terminal Renderer completion: %w", err)
	}
	return nil
}

func legacyControlPlaneRequestID(ctx context.Context, tx *sql.Tx, jobID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT idempotency_key
FROM rin_renderer.idempotency_keys
WHERE job_id=$1::uuid AND idempotency_key ~ '^render-[0-9a-f]{32}$'
ORDER BY idempotency_key
LIMIT 2`, jobID)
	if err != nil {
		return "", fmt.Errorf("read legacy Renderer Control Plane request identity: %w", err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return "", fmt.Errorf("scan legacy Renderer Control Plane request identity: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate legacy Renderer Control Plane request identity: %w", err)
	}
	if len(values) != 1 {
		return "", errors.New("legacy Renderer Control Plane request identity is missing or ambiguous")
	}
	return values[0], nil
}

func insertCompletionOutboxTx(ctx context.Context, tx *sql.Tx, input CompletionOutboxInput) error {
	event, err := completionEvent(input)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO rin_renderer.renderer_completion_outbox (
 event_id,job_id,schema_version,protocol_version,terminal_version,
 request_id,control_project_id,source_commit,control_project_hash,renderer_project_hash,
 terminal_state,result_reference,result_hash,failure_code,occurred_at
) VALUES ($1,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, input.JobID, event.Data.SchemaVersion, event.Data.ProtocolVersion,
		event.Data.TerminalVersion, input.RequestID, input.ControlProjectID, input.SourceCommit,
		input.ControlProjectHash, input.RendererProjectHash, input.TerminalState,
		input.ResultReference, input.ResultHash, input.FailureCode, input.OccurredAt)
	if err != nil {
		return fmt.Errorf("insert Renderer completion outbox: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count Renderer completion outbox insert: %w", err)
	}
	if inserted == 1 {
		return nil
	}
	var matches bool
	err = tx.QueryRowContext(ctx, `
SELECT job_id=$2::uuid AND schema_version=$3 AND protocol_version=$4 AND terminal_version=$5
   AND request_id=$6 AND control_project_id=$7 AND source_commit=$8
   AND control_project_hash=$9 AND renderer_project_hash=$10 AND terminal_state=$11
   AND result_reference=$12 AND result_hash=$13 AND failure_code=$14 AND occurred_at=$15
FROM rin_renderer.renderer_completion_outbox
WHERE event_id=$1`, event.EventID, input.JobID, event.Data.SchemaVersion, event.Data.ProtocolVersion,
		event.Data.TerminalVersion, input.RequestID, input.ControlProjectID, input.SourceCommit,
		input.ControlProjectHash, input.RendererProjectHash, input.TerminalState,
		input.ResultReference, input.ResultHash, input.FailureCode, input.OccurredAt).Scan(&matches)
	if err != nil {
		return fmt.Errorf("read Renderer completion outbox conflict: %w", err)
	}
	if !matches {
		return errors.New("Renderer completion outbox evidence conflicts with an existing event")
	}
	return nil
}

// nextCompletionTerminalVersionTx returns the next free terminal-outcome
// ordinal for the job. A job can reach a terminal state without ever claiming an
// attempt, so a queued cancellation has no attempt number to publish; it takes
// the next ordinal after every recorded terminal outcome instead. The caller
// holds the render_jobs row lock for the whole terminal transition.
func nextCompletionTerminalVersionTx(ctx context.Context, tx *sql.Tx, jobID string) (int16, error) {
	var terminalVersion int16
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(max(terminal_version), 0) + 1
FROM rin_renderer.renderer_completion_outbox
WHERE job_id = $1::uuid`, jobID).Scan(&terminalVersion); err != nil {
		return 0, fmt.Errorf("allocate Renderer completion terminal version: %w", err)
	}
	return terminalVersion, nil
}

func completionEvent(input CompletionOutboxInput) (contracts.ControlPlaneCompletionEvent, error) {
	if input.OccurredAt.IsZero() || strings.TrimSpace(input.ResultReference) != input.ResultReference ||
		len(input.ResultReference) > 2048 || completionControlPattern.MatchString(input.ResultReference) {
		return contracts.ControlPlaneCompletionEvent{}, errors.New("Renderer completion outbox input is invalid")
	}
	terminalVersion := int(input.TerminalVersion)
	if terminalVersion < 1 {
		return contracts.ControlPlaneCompletionEvent{}, errors.New("Renderer completion terminal version is invalid")
	}
	eventID, err := contracts.ControlPlaneCompletionEventID(input.JobID, terminalVersion)
	if err != nil {
		return contracts.ControlPlaneCompletionEvent{}, err
	}
	event := contracts.ControlPlaneCompletionEvent{
		Schema: contracts.ControlPlaneEventSchemaV1, EventID: eventID,
		Type: contracts.ControlPlaneCompletionEventType, Producer: "rin-renderer",
		OccurredAt: input.OccurredAt.UTC().Format(time.RFC3339Nano), CorrelationID: input.RequestID,
		Subject: contracts.ControlPlaneCompletionSubject{RendererJobID: input.JobID},
		Data: contracts.ControlPlaneCompletionData{
			SchemaVersion: contracts.ControlPlaneCompletionSchemaV2, ProtocolVersion: "v2", TerminalVersion: terminalVersion,
			RendererJobID: input.JobID, RequestID: input.RequestID, ControlProjectID: input.ControlProjectID,
			SourceCommit: input.SourceCommit, ControlProjectHash: input.ControlProjectHash,
			RendererProjectHash: input.RendererProjectHash, TerminalState: input.TerminalState,
			ResultHash: input.ResultHash, FailureCode: input.FailureCode,
		},
	}
	if err := contracts.ValidateControlPlaneCompletionEvent(event); err != nil {
		return contracts.ControlPlaneCompletionEvent{}, err
	}
	if input.TerminalState == "succeeded" && input.ResultReference == "" ||
		input.TerminalState != "succeeded" && input.ResultReference != "" {
		return contracts.ControlPlaneCompletionEvent{}, errors.New("Renderer completion result reference is invalid")
	}
	return event, nil
}

func (repository *Repository) ClaimCompletion(ctx context.Context, input ClaimCompletionInput) (CompletionDelivery, error) {
	if !validCompletionTokenHash(input.LeaseTokenHash) || input.Now.IsZero() ||
		!input.LeaseExpiresAt.After(input.Now) || input.LeaseExpiresAt.After(input.Now.Add(5*time.Minute)) {
		return CompletionDelivery{}, errors.New("Renderer completion claim input is invalid")
	}
	row := repository.db.QueryRowContext(ctx, `
WITH candidate AS (
 SELECT event_id
 FROM rin_renderer.renderer_completion_outbox
 WHERE (state IN ('pending','failed') AND (next_attempt_at IS NULL OR next_attempt_at <= $1))
    OR (state='delivering' AND lease_until <= $1)
 ORDER BY occurred_at,event_id
 FOR UPDATE SKIP LOCKED
 LIMIT 1
)
UPDATE rin_renderer.renderer_completion_outbox AS delivery
SET state='delivering',attempt_count=attempt_count+1,next_attempt_at=NULL,
    lease_token_hash=$2,lease_until=$3,last_error_code='',updated_at=clock_timestamp()
FROM candidate
WHERE delivery.event_id=candidate.event_id
RETURNING delivery.event_id,delivery.job_id::text,delivery.schema_version,delivery.protocol_version,
 delivery.terminal_version,delivery.request_id,delivery.control_project_id,delivery.source_commit,
 delivery.control_project_hash,delivery.renderer_project_hash,delivery.terminal_state,
 delivery.result_reference,delivery.result_hash,delivery.failure_code,delivery.state,
 delivery.attempt_count,delivery.lease_until,delivery.occurred_at,delivery.created_at`,
		input.Now, input.LeaseTokenHash, input.LeaseExpiresAt)
	delivery, err := scanCompletionDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CompletionDelivery{}, ErrNoCompletionDelivery
	}
	if err != nil {
		return CompletionDelivery{}, fmt.Errorf("claim Renderer completion delivery: %w", err)
	}
	return delivery, nil
}

func (repository *Repository) AckCompletion(ctx context.Context, eventID, leaseTokenHash string, now time.Time) error {
	if strings.TrimSpace(eventID) == "" || !validCompletionTokenHash(leaseTokenHash) || now.IsZero() {
		return errors.New("Renderer completion acknowledgement input is invalid")
	}
	result, err := repository.db.ExecContext(ctx, `
UPDATE rin_renderer.renderer_completion_outbox
SET state='delivered',delivered_at=$3,lease_token_hash=NULL,lease_until=NULL,
    next_attempt_at=NULL,dead_lettered_at=NULL,last_error_code='',updated_at=clock_timestamp()
WHERE event_id=$1 AND state='delivering' AND lease_token_hash=$2 AND lease_until>$3`,
		eventID, leaseTokenHash, now)
	if err != nil {
		return fmt.Errorf("ack Renderer completion delivery: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return ErrCompletionLeaseLost
	}
	return nil
}

func (repository *Repository) FailCompletion(ctx context.Context, eventID, leaseTokenHash string, now time.Time, errorCode string, maxAttempts int) (string, error) {
	if strings.TrimSpace(eventID) == "" || !validCompletionTokenHash(leaseTokenHash) || now.IsZero() ||
		!completionErrorPattern.MatchString(errorCode) || maxAttempts < 1 || maxAttempts > 100 {
		return "", errors.New("Renderer completion failure input is invalid")
	}
	var state string
	err := repository.db.QueryRowContext(ctx, `
UPDATE rin_renderer.renderer_completion_outbox
SET state=CASE WHEN attempt_count >= $5 THEN 'dead_letter' ELSE 'failed' END,
    next_attempt_at=CASE WHEN attempt_count >= $5 THEN NULL ELSE $3 +
      (LEAST(300, power(2,LEAST(attempt_count,8))) * INTERVAL '1 second') END,
    lease_token_hash=NULL,lease_until=NULL,
    dead_lettered_at=CASE WHEN attempt_count >= $5 THEN $3 ELSE NULL END,
    last_error_code=$4,updated_at=clock_timestamp()
WHERE event_id=$1 AND state='delivering' AND lease_token_hash=$2 AND lease_until>$3
RETURNING state`, eventID, leaseTokenHash, now, errorCode, maxAttempts).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrCompletionLeaseLost
	}
	if err != nil {
		return "", fmt.Errorf("fail Renderer completion delivery: %w", err)
	}
	return state, nil
}

func (repository *Repository) ReplayCompletion(ctx context.Context, eventID string, now time.Time) error {
	if strings.TrimSpace(eventID) == "" || now.IsZero() {
		return errors.New("Renderer completion replay input is invalid")
	}
	result, err := repository.db.ExecContext(ctx, `
UPDATE rin_renderer.renderer_completion_outbox
SET state='pending',attempt_count=0,next_attempt_at=$2,lease_token_hash=NULL,lease_until=NULL,
    dead_lettered_at=NULL,last_error_code='',updated_at=clock_timestamp()
WHERE event_id=$1 AND state='dead_letter'`, eventID, now)
	if err != nil {
		return fmt.Errorf("replay Renderer completion delivery: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return ErrNotFound
	}
	return nil
}

func (repository *Repository) CleanupDeliveredCompletions(ctx context.Context, deliveredBefore time.Time, limit int) (int, error) {
	if deliveredBefore.IsZero() || limit < 1 || limit > 10000 {
		return 0, errors.New("Renderer completion retention input is invalid")
	}
	result, err := repository.db.ExecContext(ctx, `
DELETE FROM rin_renderer.renderer_completion_outbox AS delivery
WHERE event_id IN (
 SELECT candidate.event_id
 FROM rin_renderer.renderer_completion_outbox AS candidate
 JOIN rin_renderer.render_jobs AS job ON job.id=candidate.job_id
 WHERE candidate.state='delivered' AND candidate.delivered_at<$1 AND job.state='expired'
 ORDER BY candidate.delivered_at,candidate.event_id
 LIMIT $2
)`, deliveredBefore, limit)
	if err != nil {
		return 0, fmt.Errorf("cleanup delivered Renderer completions: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleaned Renderer completions: %w", err)
	}
	return int(count), nil
}

func (repository *Repository) CompletionHealth(ctx context.Context, now time.Time) (CompletionHealth, error) {
	if now.IsZero() {
		return CompletionHealth{}, errors.New("Renderer completion health time is required")
	}
	health := CompletionHealth{State: "healthy", CheckedAt: now.UTC()}
	var oldestOccurredAt, lastAcceptedAt sql.NullTime
	err := repository.db.QueryRowContext(ctx, `
SELECT
 count(*) FILTER (WHERE state='pending'),
 count(*) FILTER (WHERE state='failed'),
 count(*) FILTER (WHERE state='delivering'),
 count(*) FILTER (WHERE state='dead_letter'),
 min(occurred_at) FILTER (WHERE state<>'delivered'),
 max(delivered_at) FILTER (WHERE state='delivered')
FROM rin_renderer.renderer_completion_outbox`).Scan(
		&health.Pending, &health.Failed, &health.Delivering, &health.Dead,
		&oldestOccurredAt, &lastAcceptedAt,
	)
	if err != nil {
		return CompletionHealth{}, fmt.Errorf("read Renderer completion health: %w", err)
	}
	if oldestOccurredAt.Valid && now.After(oldestOccurredAt.Time) {
		health.OldestAgeSeconds = int64(now.Sub(oldestOccurredAt.Time) / time.Second)
	}
	if lastAcceptedAt.Valid {
		accepted := lastAcceptedAt.Time.UTC()
		health.LastAcceptedAt = &accepted
	}
	if health.Failed > 0 || health.Dead > 0 || time.Duration(health.OldestAgeSeconds)*time.Second > CompletionDeliveryMaxAge {
		health.State = "degraded"
	}
	return health, nil
}

func scanCompletionDelivery(row interface{ Scan(...any) error }) (CompletionDelivery, error) {
	delivery := CompletionDelivery{}
	var resultHash string
	err := row.Scan(&delivery.EventID, &delivery.JobID, &delivery.SchemaVersion, &delivery.ProtocolVersion,
		&delivery.TerminalVersion, &delivery.RequestID, &delivery.ControlProjectID, &delivery.SourceCommit,
		&delivery.ControlProjectHash, &delivery.RendererProjectHash, &delivery.TerminalState,
		&delivery.ResultReference, &resultHash, &delivery.FailureCode, &delivery.State,
		&delivery.AttemptCount, &delivery.LeaseExpiresAt, &delivery.OccurredAt, &delivery.CreatedAt)
	delivery.ResultHash = strings.TrimSpace(resultHash)
	return delivery, err
}

func validCompletionTokenHash(value string) bool {
	return completionHashPattern.MatchString(value) && strings.Trim(value, "0") != ""
}
