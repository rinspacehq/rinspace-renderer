package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

const (
	MathBatchContractVersion   = "rin-math-batch/v1"
	MathResultContractVersion  = "rin-math-result/v1"
	MathWrapperContractVersion = "rin-math-wrapper/v1"
	MathCacheSchemaVersion     = "rin-math-cache/v1"
)

type MathOutputStrategy string

const (
	MathOutputCHTML      MathOutputStrategy = "chtml"
	MathOutputComplexSVG MathOutputStrategy = "complex-svg"
	MathOutputDisplaySVG MathOutputStrategy = "display-svg"
	MathOutputSVG        MathOutputStrategy = "svg"
)

type MathBatchItem struct {
	Unit      contracts.WorkUnit `json:"unit"`
	WrapperID string             `json:"wrapperId,omitempty"`
}

type MathBatch struct {
	ContractVersion string                       `json:"contractVersion"`
	Items           []MathBatchItem              `json:"items"`
	MacroContexts   map[string]map[string]string `json:"macroContexts"`
	OutputStrategy  MathOutputStrategy           `json:"outputStrategy"`
}

type MathSourceMetadata struct {
	TeX                  string                              `json:"tex"`
	Display              bool                                `json:"display"`
	MacroContextHash     string                              `json:"macroContextHash"`
	AccessibilityContext *contracts.MathAccessibilityContext `json:"accessibilityContext,omitempty"`
	SourceLocation       *contracts.SourceLocation           `json:"sourceLocation,omitempty"`
}

type MathCacheResult struct {
	Status     string `json:"status"`
	KeyVersion string `json:"keyVersion,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

type MathResolvedUnit struct {
	ID            string                       `json:"id"`
	State         string                       `json:"state"`
	HTML          string                       `json:"html"`
	Artifact      *contracts.ArtifactReference `json:"artifact,omitempty"`
	Engine        string                       `json:"engine"`
	EngineVersion string                       `json:"engineVersion"`
	Source        MathSourceMetadata           `json:"source"`
	Cache         MathCacheResult              `json:"cache"`
	Diagnostics   []contracts.Diagnostic       `json:"diagnostics"`
}

type MathBatchResult struct {
	ContractVersion string             `json:"contractVersion"`
	Units           []MathResolvedUnit `json:"units"`
	CSS             []string           `json:"css"`
	Versions        map[string]string  `json:"versions"`
}

type MathService interface {
	ResolveMath(context.Context, MathBatch) (MathBatchResult, error)
}

func (strategy MathOutputStrategy) Valid() bool {
	switch strategy {
	case MathOutputCHTML, MathOutputComplexSVG, MathOutputDisplaySVG, MathOutputSVG:
		return true
	default:
		return false
	}
}

func (batch MathBatch) Validate() error {
	if batch.ContractVersion != MathBatchContractVersion {
		return fmt.Errorf("unsupported math batch contract %q", batch.ContractVersion)
	}
	if len(batch.Items) == 0 {
		return errors.New("math batch requires at least one item")
	}
	if !batch.OutputStrategy.Valid() {
		return fmt.Errorf("unsupported math output strategy %q", batch.OutputStrategy)
	}
	seen := make(map[string]struct{}, len(batch.Items))
	referencedContexts := make(map[string]struct{})
	for index, item := range batch.Items {
		if err := item.Unit.Validate(); err != nil {
			return fmt.Errorf("math batch item %d: %w", index, err)
		}
		if item.Unit.Kind != contracts.WorkUnitMath {
			return fmt.Errorf("math batch item %d is %q", index, item.Unit.Kind)
		}
		if _, duplicate := seen[item.Unit.ID]; duplicate {
			return fmt.Errorf("duplicate math unit %q", item.Unit.ID)
		}
		seen[item.Unit.ID] = struct{}{}
		if !validWrapperID(item.WrapperID) {
			return fmt.Errorf("math unit %q has invalid wrapper id", item.Unit.ID)
		}
		referencedContexts[item.Unit.MacroContextHash] = struct{}{}
	}
	for hash := range referencedContexts {
		if _, ok := batch.MacroContexts[hash]; !ok {
			return fmt.Errorf("math batch is missing macro context %q", hash)
		}
	}
	for hash, macros := range batch.MacroContexts {
		if !sha256Pattern.MatchString(hash) {
			return fmt.Errorf("math batch macro context has invalid hash %q", hash)
		}
		for name, value := range macros {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
				return fmt.Errorf("math macro context %q contains an empty macro", hash)
			}
		}
		if computed := ComputeMathMacroContextHash(macros); computed != hash {
			return fmt.Errorf("math macro context %q does not match its content hash", hash)
		}
	}
	return nil
}

func ComputeMathMacroContextHash(macros map[string]string) string {
	names := make([]string, 0, len(macros))
	for name := range macros {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		for _, value := range []string{name, macros[name]} {
			var length [8]byte
			binary.BigEndian.PutUint64(length[:], uint64(len(value)))
			_, _ = digest.Write(length[:])
			_, _ = digest.Write([]byte(value))
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func (result MathBatchResult) Validate(batch MathBatch) error {
	if err := batch.Validate(); err != nil {
		return err
	}
	if result.ContractVersion != MathResultContractVersion {
		return fmt.Errorf("unsupported math result contract %q", result.ContractVersion)
	}
	if len(result.Units) != len(batch.Items) {
		return errors.New("math result count does not match batch")
	}
	for _, required := range []string{
		"primary-engine", "primary-engine-version", "font", "plugin", "sanitizer", "compatibility",
		"batch-contract", "result-contract", "wrapper-contract", "cache-schema",
	} {
		if strings.TrimSpace(result.Versions[required]) == "" {
			return fmt.Errorf("math result requires version %q", required)
		}
	}
	if result.Versions["wrapper-contract"] != MathWrapperContractVersion {
		return errors.New("math result wrapper contract mismatch")
	}
	if result.Versions["batch-contract"] != MathBatchContractVersion ||
		result.Versions["result-contract"] != MathResultContractVersion ||
		result.Versions["cache-schema"] != MathCacheSchemaVersion {
		return errors.New("math result contract version mismatch")
	}
	seen := make(map[string]struct{}, len(result.Units))
	inputs := make(map[string]contracts.WorkUnit, len(batch.Items))
	for _, item := range batch.Items {
		inputs[item.Unit.ID] = item.Unit
	}
	for index, unit := range result.Units {
		if strings.TrimSpace(unit.ID) == "" {
			return fmt.Errorf("math result unit %d is incomplete", index)
		}
		switch unit.State {
		case "succeeded":
			if strings.TrimSpace(unit.HTML) == "" || strings.TrimSpace(unit.Engine) == "" || strings.TrimSpace(unit.EngineVersion) == "" {
				return fmt.Errorf("successful math result unit %q is incomplete", unit.ID)
			}
		case "failed":
			if strings.TrimSpace(unit.HTML) == "" || len(unit.Diagnostics) == 0 {
				return fmt.Errorf("failed math result unit %q requires diagnostics and escaped fallback html", unit.ID)
			}
		default:
			return fmt.Errorf("math result unit %q has invalid state %q", unit.ID, unit.State)
		}
		if _, duplicate := seen[unit.ID]; duplicate {
			return fmt.Errorf("duplicate resolved math unit %q", unit.ID)
		}
		seen[unit.ID] = struct{}{}
		input, ok := inputs[unit.ID]
		if !ok || input.Display == nil || unit.Source.TeX != input.Source || unit.Source.Display != *input.Display ||
			unit.Source.MacroContextHash != input.MacroContextHash ||
			!reflect.DeepEqual(unit.Source.AccessibilityContext, input.AccessibilityContext) ||
			!reflect.DeepEqual(unit.Source.SourceLocation, input.SourceLocation) {
			return fmt.Errorf("math result source metadata %q does not match its input", unit.ID)
		}
		if unit.Artifact != nil {
			if err := unit.Artifact.Validate(); err != nil {
				return fmt.Errorf("math result artifact %q: %w", unit.ID, err)
			}
		}
		if err := unit.Cache.Validate(); err != nil {
			return fmt.Errorf("math result cache %q: %w", unit.ID, err)
		}
		for diagnosticIndex, diagnostic := range unit.Diagnostics {
			if err := diagnostic.Validate(); err != nil {
				return fmt.Errorf("math result diagnostic %q[%d]: %w", unit.ID, diagnosticIndex, err)
			}
		}
	}
	for _, item := range batch.Items {
		if _, ok := seen[item.Unit.ID]; !ok {
			return fmt.Errorf("math result is missing unit %q", item.Unit.ID)
		}
	}
	for index, css := range result.CSS {
		if strings.TrimSpace(css) == "" {
			return fmt.Errorf("math result css %d is empty", index)
		}
	}
	return nil
}

func (cache MathCacheResult) Validate() error {
	switch cache.Status {
	case "hit", "miss":
		if cache.KeyVersion != CacheKeyVersion || !sha256Pattern.MatchString(cache.Digest) {
			return errors.New("math cache hit/miss requires a versioned SHA-256 key")
		}
	case "bypass":
		if cache.KeyVersion != "" || cache.Digest != "" {
			return errors.New("bypassed math cache must not claim a key")
		}
	default:
		return fmt.Errorf("unsupported math cache status %q", cache.Status)
	}
	return nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validWrapperID(value string) bool {
	if value == "" {
		return true
	}
	if value != strings.TrimSpace(value) || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character <= 0x20 || character == '"' || character == '\'' || character == '<' || character == '>' {
			return false
		}
	}
	return true
}
