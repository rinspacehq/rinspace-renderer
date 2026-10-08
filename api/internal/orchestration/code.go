package orchestration

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

const (
	CodeBatchContractVersion  = "rin-code-batch/v1"
	CodeResultContractVersion = "rin-code-result/v1"
	CodeCacheSchemaVersion    = "rin-code-cache/v1"
	CodeWrapperContract       = "rin-code-wrapper/v1"
)

type CodeBatch struct {
	ContractVersion string               `json:"contractVersion"`
	Items           []contracts.WorkUnit `json:"items"`
	Theme           string               `json:"theme"`
	OutputStrategy  string               `json:"outputStrategy"`
}

type CodeSourceMetadata struct {
	Source         string                    `json:"source"`
	Language       string                    `json:"language,omitempty"`
	Meta           string                    `json:"meta,omitempty"`
	SourceLocation *contracts.SourceLocation `json:"sourceLocation,omitempty"`
}

type CodeCacheResult struct {
	Status     string `json:"status"`
	KeyVersion string `json:"keyVersion,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

type CodeResolvedUnit struct {
	ID                string                 `json:"id"`
	State             string                 `json:"state"`
	HTML              string                 `json:"html"`
	Engine            string                 `json:"engine"`
	EngineVersion     string                 `json:"engineVersion"`
	Theme             string                 `json:"theme"`
	CanonicalLanguage string                 `json:"canonicalLanguage,omitempty"`
	Highlighted       bool                   `json:"highlighted"`
	Source            CodeSourceMetadata     `json:"source"`
	Cache             CodeCacheResult        `json:"cache"`
	Diagnostics       []contracts.Diagnostic `json:"diagnostics"`
}

type CodeBatchResult struct {
	ContractVersion string             `json:"contractVersion"`
	Units           []CodeResolvedUnit `json:"units"`
	CSS             []string           `json:"css"`
	Versions        map[string]string  `json:"versions"`
}

func (batch CodeBatch) Validate() error {
	if batch.ContractVersion != CodeBatchContractVersion {
		return fmt.Errorf("unsupported code batch contract %q", batch.ContractVersion)
	}
	if len(batch.Items) == 0 {
		return errors.New("code batch requires at least one item")
	}
	if strings.TrimSpace(batch.Theme) == "" || strings.TrimSpace(batch.OutputStrategy) != "html" {
		return errors.New("code batch requires a theme and html output strategy")
	}
	seen := make(map[string]struct{}, len(batch.Items))
	for index, unit := range batch.Items {
		if err := unit.Validate(); err != nil {
			return fmt.Errorf("code batch item %d: %w", index, err)
		}
		if unit.Kind != contracts.WorkUnitCode {
			return fmt.Errorf("code batch item %d is %q", index, unit.Kind)
		}
		if _, duplicate := seen[unit.ID]; duplicate {
			return fmt.Errorf("duplicate code unit %q", unit.ID)
		}
		seen[unit.ID] = struct{}{}
	}
	return nil
}

func (result CodeBatchResult) Validate(batch CodeBatch) error {
	if err := batch.Validate(); err != nil {
		return err
	}
	if result.ContractVersion != CodeResultContractVersion {
		return fmt.Errorf("unsupported code result contract %q", result.ContractVersion)
	}
	if len(result.Units) != len(batch.Items) {
		return errors.New("code result count does not match batch")
	}
	for _, required := range []string{
		"engine", "engine-version", "plugin", "theme", "sanitizer", "compatibility",
		"batch-contract", "result-contract", "wrapper-contract", "cache-schema",
	} {
		if strings.TrimSpace(result.Versions[required]) == "" {
			return fmt.Errorf("code result requires version %q", required)
		}
	}
	if result.Versions["theme"] != batch.Theme ||
		result.Versions["batch-contract"] != CodeBatchContractVersion ||
		result.Versions["result-contract"] != CodeResultContractVersion ||
		result.Versions["wrapper-contract"] != CodeWrapperContract ||
		result.Versions["cache-schema"] != CodeCacheSchemaVersion {
		return errors.New("code result contract version mismatch")
	}
	inputs := make(map[string]contracts.WorkUnit, len(batch.Items))
	for _, unit := range batch.Items {
		inputs[unit.ID] = unit
	}
	seen := make(map[string]struct{}, len(result.Units))
	for index, unit := range result.Units {
		input, ok := inputs[unit.ID]
		if !ok {
			return fmt.Errorf("code result unit %d has unknown id %q", index, unit.ID)
		}
		if _, duplicate := seen[unit.ID]; duplicate {
			return fmt.Errorf("duplicate resolved code unit %q", unit.ID)
		}
		seen[unit.ID] = struct{}{}
		if unit.Source.Source != input.Source || unit.Source.Language != input.Language ||
			unit.Source.Meta != input.Meta || !reflect.DeepEqual(unit.Source.SourceLocation, input.SourceLocation) {
			return fmt.Errorf("code result source metadata %q does not match input", unit.ID)
		}
		if unit.State != "succeeded" || strings.TrimSpace(unit.HTML) == "" ||
			unit.Engine != result.Versions["engine"] || unit.EngineVersion != result.Versions["engine-version"] ||
			unit.Theme != batch.Theme {
			return fmt.Errorf("code result unit %q is incomplete", unit.ID)
		}
		if unit.Highlighted && strings.TrimSpace(unit.CanonicalLanguage) == "" {
			return fmt.Errorf("highlighted code result %q requires a canonical language", unit.ID)
		}
		if !unit.Highlighted && strings.TrimSpace(input.Language) != "" && len(unit.Diagnostics) == 0 {
			return fmt.Errorf("plain code result %q requires an unknown-language diagnostic", unit.ID)
		}
		if err := unit.Cache.Validate(); err != nil {
			return fmt.Errorf("code result cache %q: %w", unit.ID, err)
		}
		for diagnosticIndex, diagnostic := range unit.Diagnostics {
			if err := diagnostic.Validate(); err != nil {
				return fmt.Errorf("code result diagnostic %q[%d]: %w", unit.ID, diagnosticIndex, err)
			}
		}
	}
	for index, css := range result.CSS {
		if strings.TrimSpace(css) == "" {
			return fmt.Errorf("code result css %d is empty", index)
		}
	}
	return nil
}

func (cache CodeCacheResult) Validate() error {
	switch cache.Status {
	case "hit", "miss":
		if cache.KeyVersion != CacheKeyVersion || !sha256Pattern.MatchString(cache.Digest) {
			return errors.New("code cache hit/miss requires a versioned SHA-256 key")
		}
	case "bypass":
		if cache.KeyVersion != "" || cache.Digest != "" {
			return errors.New("bypassed code cache cannot contain a key")
		}
	default:
		return fmt.Errorf("unsupported code cache status %q", cache.Status)
	}
	return nil
}
