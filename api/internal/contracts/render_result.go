package contracts

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const RenderResultSchemaVersion = "rin-render-result/v1"

type RenderResult struct {
	SchemaVersion  string              `json:"schemaVersion"`
	JobID          string              `json:"jobId"`
	RequestID      string              `json:"requestId"`
	ProjectHash    string              `json:"projectHash"`
	ResultHash     string              `json:"resultHash"`
	ContentKind    ContentKind         `json:"contentKind"`
	Engine         string              `json:"engine"`
	Inline         *DocumentBundle     `json:"inline,omitempty"`
	ResultArtifact *ArtifactReference  `json:"resultArtifact,omitempty"`
	Assets         []ArtifactReference `json:"assets"`
	Diagnostics    []Diagnostic        `json:"diagnostics"`
	Versions       map[string]string   `json:"versions"`
	Cache          RenderCacheSummary  `json:"cache"`
}

type ArtifactReference struct {
	ArtifactID string `json:"artifactId"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	MediaType  string `json:"mediaType"`
	Visibility string `json:"visibility"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
}

type RenderCacheSummary struct {
	Hit          bool     `json:"hit"`
	ReusedStages []string `json:"reusedStages"`
}

func (artifact ArtifactReference) Validate() error {
	return artifact.validate()
}

func (result RenderResult) Validate() error {
	if result.SchemaVersion != RenderResultSchemaVersion {
		return fmt.Errorf("unsupported render result schema version %q", result.SchemaVersion)
	}
	if strings.TrimSpace(result.JobID) == "" || strings.TrimSpace(result.RequestID) == "" || strings.TrimSpace(result.Engine) == "" {
		return errors.New("render result requires job, request, and engine identifiers")
	}
	if !isSHA256(result.ProjectHash) || !isSHA256(result.ResultHash) || !result.ContentKind.Valid() {
		return errors.New("render result has invalid hashes or content kind")
	}
	if (result.Inline == nil) == (result.ResultArtifact == nil) {
		return errors.New("render result requires exactly one inline bundle or result artifact")
	}
	if result.Assets == nil || result.Diagnostics == nil || result.Cache.ReusedStages == nil {
		return errors.New("render result requires array-valued assets, diagnostics, and reused cache stages")
	}
	if result.Inline != nil {
		if err := result.Inline.Validate(); err != nil {
			return fmt.Errorf("inline bundle: %w", err)
		}
		if result.Inline.State != DocumentBundleStateFinal {
			return errors.New("inline result bundle must be final")
		}
		if result.Inline.ProjectHash != result.ProjectHash || result.Inline.ContentKind != result.ContentKind {
			return errors.New("inline bundle identity does not match render result")
		}
		if result.Inline.BundleHash == "" || result.Inline.BundleHash != result.ResultHash {
			return errors.New("inline bundle hash does not match render result")
		}
	} else {
		if err := result.ResultArtifact.validate(); err != nil {
			return fmt.Errorf("result artifact: %w", err)
		}
		if result.ResultArtifact.Visibility != "private" {
			return errors.New("large result artifact must be private")
		}
		if result.ResultArtifact.SHA256 != result.ResultHash || result.ResultArtifact.ExpiresAt == "" {
			return errors.New("large result artifact must match result hash and have an expiry")
		}
	}
	for index := range result.Assets {
		if err := result.Assets[index].validate(); err != nil {
			return fmt.Errorf("result asset %d: %w", index, err)
		}
	}
	for index := range result.Diagnostics {
		if err := result.Diagnostics[index].validate(); err != nil {
			return fmt.Errorf("result diagnostic %d: %w", index, err)
		}
	}
	if len(result.Versions) == 0 {
		return errors.New("render result requires versions")
	}
	for component, version := range result.Versions {
		if strings.TrimSpace(component) == "" || strings.TrimSpace(version) == "" {
			return errors.New("render result versions require non-empty names and values")
		}
	}
	seenStages := map[string]struct{}{}
	for _, stage := range result.Cache.ReusedStages {
		if strings.TrimSpace(stage) == "" {
			return errors.New("cache stage must not be empty")
		}
		if _, exists := seenStages[stage]; exists {
			return fmt.Errorf("duplicate reused cache stage %q", stage)
		}
		seenStages[stage] = struct{}{}
	}
	return nil
}

func (artifact ArtifactReference) validate() error {
	if strings.TrimSpace(artifact.ArtifactID) == "" || !isSHA256(artifact.SHA256) || artifact.Bytes < 0 || strings.TrimSpace(artifact.MediaType) == "" {
		return errors.New("invalid artifact identity or metadata")
	}
	if err := validateCanonicalPath(artifact.ArtifactID); err != nil {
		return fmt.Errorf("artifact id: %w", err)
	}
	if artifact.Visibility != "private" && artifact.Visibility != "public" {
		return fmt.Errorf("unsupported artifact visibility %q", artifact.Visibility)
	}
	if artifact.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, artifact.ExpiresAt); err != nil {
			return errors.New("artifact expiresAt must be RFC3339")
		}
	}
	return nil
}
