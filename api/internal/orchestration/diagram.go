package orchestration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

const (
	DiagramBatchContractVersion  = "rin-diagram-batch/v1"
	DiagramResultContractVersion = "rin-diagram-result/v1"
	DiagramCacheSchemaVersion    = "rin-diagram-cache/v1"
	DiagramArtifactContract      = "rin-diagram-artifact/v1"
)

type DiagramSourceMode string

const (
	DiagramSourceComplete DiagramSourceMode = "complete"
	DiagramSourceBody     DiagramSourceMode = "body"
)

type DiagramBatchItem struct {
	Unit       contracts.WorkUnit `json:"unit"`
	SourceMode DiagramSourceMode  `json:"sourceMode"`
	Body       string             `json:"body,omitempty"`
	WrapperID  string             `json:"wrapperId,omitempty"`
}

type DiagramBatch struct {
	ContractVersion string             `json:"contractVersion"`
	Items           []DiagramBatchItem `json:"items"`
	OutputStrategy  string             `json:"outputStrategy"`
}

type DiagramSourceMetadata struct {
	DiagramType    string                    `json:"diagramType"`
	Source         string                    `json:"source"`
	Options        string                    `json:"options,omitempty"`
	Layout         *contracts.DiagramLayout  `json:"layout,omitempty"`
	SourceMode     DiagramSourceMode         `json:"sourceMode"`
	SourceLocation *contracts.SourceLocation `json:"sourceLocation,omitempty"`
}

type DiagramCacheResult struct {
	Status     string `json:"status"`
	KeyVersion string `json:"keyVersion,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

type DiagramResolvedUnit struct {
	ID            string                       `json:"id"`
	State         string                       `json:"state"`
	DiagramID     string                       `json:"diagramId,omitempty"`
	Engine        string                       `json:"engine"`
	EngineVersion string                       `json:"engineVersion"`
	URL           string                       `json:"url,omitempty"`
	SVGHash       string                       `json:"svgHash,omitempty"`
	ObjectID      string                       `json:"objectId,omitempty"`
	SVGBytes      int64                        `json:"svgBytes,omitempty"`
	Uploaded      bool                         `json:"uploaded,omitempty"`
	EngineCached  bool                         `json:"engineCached,omitempty"`
	RenderURL     string                       `json:"renderUrl,omitempty"`
	Artifact      *contracts.ArtifactReference `json:"artifact,omitempty"`
	Source        DiagramSourceMetadata        `json:"source"`
	Cache         DiagramCacheResult           `json:"cache"`
	Diagnostics   []contracts.Diagnostic       `json:"diagnostics"`
}

type DiagramBatchResult struct {
	ContractVersion string                `json:"contractVersion"`
	Units           []DiagramResolvedUnit `json:"units"`
	Versions        map[string]string     `json:"versions"`
}

type DiagramService interface {
	ResolveDiagrams(context.Context, DiagramBatch) (DiagramBatchResult, error)
}

func (mode DiagramSourceMode) Valid() bool {
	return mode == DiagramSourceComplete || mode == DiagramSourceBody
}

func (batch DiagramBatch) Validate() error {
	if batch.ContractVersion != DiagramBatchContractVersion {
		return fmt.Errorf("unsupported diagram batch contract %q", batch.ContractVersion)
	}
	if len(batch.Items) == 0 {
		return errors.New("diagram batch requires at least one item")
	}
	if strings.TrimSpace(batch.OutputStrategy) != "svg" {
		return fmt.Errorf("unsupported diagram output strategy %q", batch.OutputStrategy)
	}
	seen := make(map[string]struct{}, len(batch.Items))
	for index, item := range batch.Items {
		if err := item.Unit.Validate(); err != nil {
			return fmt.Errorf("diagram batch item %d: %w", index, err)
		}
		if item.Unit.Kind != contracts.WorkUnitDiagram {
			return fmt.Errorf("diagram batch item %d is %q", index, item.Unit.Kind)
		}
		if !item.SourceMode.Valid() {
			return fmt.Errorf("diagram unit %q has invalid source mode", item.Unit.ID)
		}
		if item.SourceMode == DiagramSourceBody && strings.TrimSpace(item.Body) == "" {
			return fmt.Errorf("diagram unit %q body mode requires body", item.Unit.ID)
		}
		if item.SourceMode == DiagramSourceBody && item.Unit.Source != item.Body {
			return fmt.Errorf("diagram unit %q body mode source must match body", item.Unit.ID)
		}
		if _, duplicate := seen[item.Unit.ID]; duplicate {
			return fmt.Errorf("duplicate diagram unit %q", item.Unit.ID)
		}
		seen[item.Unit.ID] = struct{}{}
		if !validWrapperID(item.WrapperID) {
			return fmt.Errorf("diagram unit %q has invalid wrapper id", item.Unit.ID)
		}
	}
	return nil
}

func (result DiagramBatchResult) Validate(batch DiagramBatch) error {
	if err := batch.Validate(); err != nil {
		return err
	}
	if result.ContractVersion != DiagramResultContractVersion {
		return fmt.Errorf("unsupported diagram result contract %q", result.ContractVersion)
	}
	if len(result.Units) != len(batch.Items) {
		return errors.New("diagram result count does not match batch")
	}
	for _, required := range []string{
		"engine", "engine-version", "plugin", "font", "sanitizer", "compatibility",
		"batch-contract", "result-contract", "artifact-contract", "cache-schema",
	} {
		if strings.TrimSpace(result.Versions[required]) == "" {
			return fmt.Errorf("diagram result requires version %q", required)
		}
	}
	if result.Versions["batch-contract"] != DiagramBatchContractVersion ||
		result.Versions["result-contract"] != DiagramResultContractVersion ||
		result.Versions["artifact-contract"] != DiagramArtifactContract ||
		result.Versions["cache-schema"] != DiagramCacheSchemaVersion {
		return errors.New("diagram result contract version mismatch")
	}
	inputs := make(map[string]DiagramBatchItem, len(batch.Items))
	for _, item := range batch.Items {
		inputs[item.Unit.ID] = item
	}
	seen := make(map[string]struct{}, len(result.Units))
	for index, unit := range result.Units {
		input, ok := inputs[unit.ID]
		if !ok {
			return fmt.Errorf("diagram result unit %d has unknown id %q", index, unit.ID)
		}
		if _, duplicate := seen[unit.ID]; duplicate {
			return fmt.Errorf("duplicate resolved diagram unit %q", unit.ID)
		}
		seen[unit.ID] = struct{}{}
		if unit.Source.DiagramType != input.Unit.DiagramType || unit.Source.Source != input.Unit.Source ||
			unit.Source.Options != input.Unit.Options || unit.Source.SourceMode != input.SourceMode ||
			!reflect.DeepEqual(unit.Source.Layout, input.Unit.Layout) ||
			!reflect.DeepEqual(unit.Source.SourceLocation, input.Unit.SourceLocation) {
			return fmt.Errorf("diagram result source metadata %q does not match input", unit.ID)
		}
		switch unit.State {
		case "succeeded":
			if strings.TrimSpace(unit.Engine) == "" || strings.TrimSpace(unit.EngineVersion) == "" ||
				strings.TrimSpace(unit.URL) == "" || !sha256Pattern.MatchString(unit.SVGHash) ||
				strings.TrimSpace(unit.ObjectID) == "" || unit.SVGBytes <= 0 || unit.Artifact == nil {
				return fmt.Errorf("successful diagram result %q is incomplete", unit.ID)
			}
			if err := unit.Artifact.Validate(); err != nil {
				return fmt.Errorf("diagram result artifact %q: %w", unit.ID, err)
			}
			expectedObjectID := fmt.Sprintf("diagrams/v1/svg-sha256/%s/%s.svg", unit.SVGHash[:2], unit.SVGHash)
			if unit.ObjectID != expectedObjectID || unit.Artifact.ArtifactID != unit.ObjectID ||
				unit.Artifact.SHA256 != unit.SVGHash || unit.Artifact.Bytes != unit.SVGBytes ||
				unit.Artifact.MediaType != "image/svg+xml; charset=utf-8" || unit.Artifact.Visibility != "public" ||
				unit.Engine != result.Versions["engine"] || unit.EngineVersion != result.Versions["engine-version"] {
				return fmt.Errorf("diagram result artifact %q does not match the immutable SVG contract", unit.ID)
			}
		case "failed":
			if len(unit.Diagnostics) == 0 {
				return fmt.Errorf("failed diagram result %q requires diagnostics", unit.ID)
			}
		default:
			return fmt.Errorf("diagram result %q has invalid state %q", unit.ID, unit.State)
		}
		if err := unit.Cache.Validate(); err != nil {
			return fmt.Errorf("diagram result cache %q: %w", unit.ID, err)
		}
		for diagnosticIndex, diagnostic := range unit.Diagnostics {
			if err := diagnostic.Validate(); err != nil {
				return fmt.Errorf("diagram result diagnostic %q[%d]: %w", unit.ID, diagnosticIndex, err)
			}
		}
	}
	return nil
}

func (cache DiagramCacheResult) Validate() error {
	switch cache.Status {
	case "hit", "miss":
		if cache.KeyVersion != CacheKeyVersion || !sha256Pattern.MatchString(cache.Digest) {
			return errors.New("diagram cache key is incomplete")
		}
	case "bypass":
		if cache.KeyVersion != "" || cache.Digest != "" {
			return errors.New("bypassed diagram cache cannot contain a key")
		}
	default:
		return fmt.Errorf("unsupported diagram cache status %q", cache.Status)
	}
	return nil
}
