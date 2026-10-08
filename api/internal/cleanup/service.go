package cleanup

import (
	"context"
	"errors"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
)

type Repository interface {
	ExpireTerminalJobs(context.Context, time.Time, int) (int, error)
	CleanupExpiredMetadata(context.Context, time.Time, time.Time, int) (jobpostgres.MetadataCleanup, error)
	StageExpiredPrivateArtifacts(context.Context, time.Time, int) (int, error)
	PendingArtifactDeletions(context.Context, int) ([]jobpostgres.Artifact, error)
	AckArtifactDeletion(context.Context, string) error
}

type PrivateStore interface {
	DeletePrivate(context.Context, contracts.ArtifactReference) error
}

type Config struct {
	BatchLimit int
	EventTTL   time.Duration
	Now        func() time.Time
}

type Report struct {
	ExpiredJobs      int
	Metadata         jobpostgres.MetadataCleanup
	StagedArtifacts  int
	DeletedArtifacts int
}

type Service struct {
	repository Repository
	store      PrivateStore
	config     Config
}

func New(repository Repository, store PrivateStore, config Config) (*Service, error) {
	if repository == nil || store == nil || config.BatchLimit <= 0 || config.EventTTL <= 0 {
		return nil, errors.New("renderer cleanup configuration is invalid")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Service{repository: repository, store: store, config: config}, nil
}

func (service *Service) RunOnce(ctx context.Context) (Report, error) {
	now := service.config.Now().UTC().Truncate(time.Second)
	report := Report{}
	var err error
	if report.ExpiredJobs, err = service.repository.ExpireTerminalJobs(ctx, now, service.config.BatchLimit); err != nil {
		return report, err
	}
	if report.Metadata, err = service.repository.CleanupExpiredMetadata(ctx, now, now.Add(-service.config.EventTTL), service.config.BatchLimit); err != nil {
		return report, err
	}
	if report.StagedArtifacts, err = service.repository.StageExpiredPrivateArtifacts(ctx, now, service.config.BatchLimit); err != nil {
		return report, err
	}
	artifacts, err := service.repository.PendingArtifactDeletions(ctx, service.config.BatchLimit)
	if err != nil {
		return report, err
	}
	for _, artifact := range artifacts {
		reference := contracts.ArtifactReference{
			ArtifactID: artifact.StorageKey, SHA256: artifact.SHA256, Bytes: artifact.ByteSize,
			MediaType: artifact.MediaType, Visibility: "private",
		}
		if artifact.ExpiresAt != nil {
			reference.ExpiresAt = artifact.ExpiresAt.UTC().Format(time.RFC3339)
		}
		if err := service.store.DeletePrivate(ctx, reference); err != nil {
			return report, err
		}
		if err := service.repository.AckArtifactDeletion(ctx, artifact.ID); err != nil {
			return report, err
		}
		report.DeletedArtifacts++
	}
	return report, nil
}
