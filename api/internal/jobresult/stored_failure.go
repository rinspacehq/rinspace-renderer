package jobresult

import (
	"errors"
	"fmt"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

const StoredFailureSchemaVersion = "rin-stored-render-failure/v1"

type StoredFailure struct {
	SchemaVersion string                        `json:"schemaVersion"`
	Status        int                           `json:"status"`
	Code          string                        `json:"code"`
	Error         string                        `json:"error"`
	Retryable     bool                          `json:"retryable"`
	ExitCode      *int                          `json:"exitCode,omitempty"`
	Diagnostics   []renderapi.Diagnostic        `json:"diagnostics"`
	Artifacts     []contracts.ArtifactReference `json:"artifacts,omitempty"`
}

func (failure StoredFailure) Validate() error {
	if failure.SchemaVersion != StoredFailureSchemaVersion {
		return fmt.Errorf("unsupported stored failure schema %q", failure.SchemaVersion)
	}
	if failure.Status < 400 || failure.Status > 599 || failure.Code == "" || failure.Error == "" {
		return errors.New("stored failure status or error is invalid")
	}
	if failure.ExitCode != nil && (*failure.ExitCode < 0 || *failure.ExitCode > 255) {
		return errors.New("stored failure exit code is invalid")
	}
	for index, artifact := range failure.Artifacts {
		if err := artifact.Validate(); err != nil || artifact.Visibility != "private" || artifact.ExpiresAt == "" {
			return fmt.Errorf("stored failure artifact %d is invalid", index)
		}
	}
	return nil
}
