package jobpostgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var ErrNotFound = errors.New("renderer job record not found")

type Repository struct {
	db *sql.DB
}

func Open(ctx context.Context, dataSourceName string) (*Repository, error) {
	db, err := sql.Open("pgx", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("open renderer postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping renderer postgres: %w", err)
	}
	return New(db), nil
}

func New(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (repository *Repository) Close() error {
	return repository.db.Close()
}

func (repository *Repository) Ready(ctx context.Context) error {
	if err := repository.db.PingContext(ctx); err != nil {
		return fmt.Errorf("database_unavailable: %w", err)
	}
	return nil
}

func (repository *Repository) Migrate(ctx context.Context) error {
	return Migrate(ctx, repository.db)
}

func (repository *Repository) VerifyMigrations(ctx context.Context) error {
	return VerifyMigrations(ctx, repository.db)
}

type Artifact struct {
	ID            string
	SHA256        string
	Kind          string
	Visibility    string
	StorageKey    string
	ByteSize      int64
	MediaType     string
	SchemaVersion string
	ExpiresAt     *time.Time
	CreatedAt     time.Time
}

func (repository *Repository) CreateArtifact(ctx context.Context, artifact Artifact) (Artifact, error) {
	err := repository.db.QueryRowContext(ctx, `
		INSERT INTO rin_renderer.render_artifacts (
			id, sha256, kind, visibility, storage_key, byte_size, media_type, schema_version, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9)
		RETURNING created_at`,
		artifact.ID, artifact.SHA256, artifact.Kind, artifact.Visibility, artifact.StorageKey,
		artifact.ByteSize, artifact.MediaType, artifact.SchemaVersion, artifact.ExpiresAt,
	).Scan(&artifact.CreatedAt)
	if err != nil {
		return Artifact{}, fmt.Errorf("create renderer artifact: %w", err)
	}
	return artifact, nil
}

type Job struct {
	ID                   string
	PrincipalID          string
	OwnerScope           string
	ContentKind          string
	DocumentEngine       string
	ResourceClass        string
	PriorityClass        string
	State                string
	SourceArtifactID     string
	ResultArtifactID     string
	ProjectHash          string
	OptionsHash          string
	RendererVersion      string
	RequestMetadata      json.RawMessage
	MaxAttempts          int16
	QueuedAt             *time.Time
	AvailableAt          *time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
	ExpiresAt            time.Time
	CancelRequested      bool
	DeclaredSourceBytes  int64
	ReservationExpiresAt *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func (repository *Repository) CreateJob(ctx context.Context, job Job) (Job, error) {
	err := repository.db.QueryRowContext(ctx, `
		INSERT INTO rin_renderer.render_jobs (
			id, principal_id, owner_scope, content_kind, document_engine, resource_class,
			priority_class, state, source_artifact_id, result_artifact_id, project_hash,
			options_hash, renderer_version, request_metadata, max_attempts, queued_at, available_at, started_at,
			finished_at, expires_at, cancel_requested, declared_source_bytes,
			reservation_expires_at
		) VALUES (
			$1::uuid, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''),
			$11, $12, $13, COALESCE($14::jsonb, '{}'::jsonb), $15, $16, $17, $18, $19, $20, $21, $22, $23
		)
		RETURNING created_at, updated_at`,
		job.ID, job.PrincipalID, job.OwnerScope, job.ContentKind, job.DocumentEngine,
		job.ResourceClass, job.PriorityClass, job.State, job.SourceArtifactID,
		job.ResultArtifactID, job.ProjectHash, job.OptionsHash, job.RendererVersion, jsonObject(job.RequestMetadata),
		job.MaxAttempts, job.QueuedAt, job.AvailableAt, job.StartedAt, job.FinishedAt,
		job.ExpiresAt, job.CancelRequested, job.DeclaredSourceBytes, job.ReservationExpiresAt,
	).Scan(&job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return Job{}, fmt.Errorf("create renderer job: %w", err)
	}
	return job, nil
}

func jsonObject(value json.RawMessage) string {
	if len(value) == 0 {
		return "{}"
	}
	return string(value)
}

func (repository *Repository) Job(ctx context.Context, id string) (Job, error) {
	var job Job
	var sourceArtifactID, resultArtifactID sql.NullString
	err := repository.db.QueryRowContext(ctx, `
		SELECT id::text, principal_id, owner_scope, content_kind, document_engine, resource_class,
			priority_class, state, source_artifact_id, result_artifact_id, project_hash,
			options_hash, renderer_version, request_metadata, max_attempts, queued_at, available_at, started_at,
			finished_at, expires_at, cancel_requested, declared_source_bytes,
			reservation_expires_at, created_at, updated_at
		FROM rin_renderer.render_jobs
		WHERE id = $1::uuid`, id).Scan(
		&job.ID, &job.PrincipalID, &job.OwnerScope, &job.ContentKind, &job.DocumentEngine,
		&job.ResourceClass, &job.PriorityClass, &job.State, &sourceArtifactID,
		&resultArtifactID, &job.ProjectHash, &job.OptionsHash, &job.RendererVersion, &job.RequestMetadata,
		&job.MaxAttempts, &job.QueuedAt, &job.AvailableAt, &job.StartedAt, &job.FinishedAt,
		&job.ExpiresAt, &job.CancelRequested, &job.DeclaredSourceBytes,
		&job.ReservationExpiresAt, &job.CreatedAt, &job.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("read renderer job: %w", err)
	}
	job.SourceArtifactID = sourceArtifactID.String
	job.ResultArtifactID = resultArtifactID.String
	return job, nil
}
