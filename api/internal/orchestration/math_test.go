package orchestration

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestMathBatchAndResultContractsPreserveSourceMetadata(t *testing.T) {
	batch := validMathBatch()
	if err := batch.Validate(); err != nil {
		t.Fatal(err)
	}
	unit := batch.Items[0].Unit
	result := MathBatchResult{
		ContractVersion: MathResultContractVersion,
		Units: []MathResolvedUnit{{
			ID: unit.ID, State: "succeeded", HTML: `<span class="rin-math"><mjx-container></mjx-container></span>`,
			Engine: "mathjax-chtml", EngineVersion: "4.1.3",
			Source: MathSourceMetadata{
				TeX: unit.Source, Display: *unit.Display, MacroContextHash: unit.MacroContextHash,
				AccessibilityContext: unit.AccessibilityContext, SourceLocation: unit.SourceLocation,
			},
			Cache:       MathCacheResult{Status: "miss", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("a", 64)},
			Diagnostics: []contracts.Diagnostic{},
		}},
		CSS: []string{"mjx-container{display:inline-block}"},
		Versions: map[string]string{
			"primary-engine": "mathjax-chtml", "primary-engine-version": "4.1.3",
			"font": "mathjax-newcm-4.1.3", "plugin": "tex-input/v1", "sanitizer": "math-output/v1",
			"compatibility": "reader/v1", "batch-contract": MathBatchContractVersion,
			"result-contract": MathResultContractVersion, "wrapper-contract": MathWrapperContractVersion,
			"cache-schema": MathCacheSchemaVersion,
		},
	}
	if err := result.Validate(batch); err != nil {
		t.Fatal(err)
	}
	if result.Units[0].Source.AccessibilityContext.Label != "不存在实数 k" ||
		result.Units[0].Source.SourceLocation.Path != "main.tex" {
		t.Fatalf("source/accessibility metadata was not preserved: %#v", result.Units[0].Source)
	}
}

func TestMathBatchRejectsMissingContextsDuplicatesAndUnsafeWrapperIDs(t *testing.T) {
	for name, mutate := range map[string]func(*MathBatch){
		"contract": func(batch *MathBatch) { batch.ContractVersion = "rin-math-batch/v0" },
		"strategy": func(batch *MathBatch) { batch.OutputStrategy = "unknown" },
		"context":  func(batch *MathBatch) { delete(batch.MacroContexts, batch.Items[0].Unit.MacroContextHash) },
		"context-hash": func(batch *MathBatch) {
			batch.MacroContexts[batch.Items[0].Unit.MacroContextHash] = map[string]string{"\\RR": `\mathbb{R}`}
		},
		"duplicate": func(batch *MathBatch) {
			batch.Items = append(batch.Items, batch.Items[0])
		},
		"wrapper": func(batch *MathBatch) { batch.Items[0].WrapperID = `bad" id` },
	} {
		t.Run(name, func(t *testing.T) {
			batch := validMathBatch()
			mutate(&batch)
			if err := batch.Validate(); err == nil {
				t.Fatalf("invalid math batch was accepted: %#v", batch)
			}
		})
	}
}

func TestMathCacheResultRequiresBoundedVersionedStatus(t *testing.T) {
	for _, cache := range []MathCacheResult{
		{Status: "hit", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("a", 64)},
		{Status: "miss", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("b", 64)},
		{Status: "bypass"},
	} {
		if err := cache.Validate(); err != nil {
			t.Fatalf("valid cache result %#v: %v", cache, err)
		}
	}
	for _, cache := range []MathCacheResult{
		{Status: "hit"},
		{Status: "miss", KeyVersion: "v0", Digest: strings.Repeat("a", 64)},
		{Status: "bypass", Digest: strings.Repeat("a", 64)},
		{Status: "unknown"},
	} {
		if err := cache.Validate(); err == nil {
			t.Fatalf("invalid cache result was accepted: %#v", cache)
		}
	}
}

func validMathBatch() MathBatch {
	display := false
	macroHash := ComputeMathMacroContextHash(map[string]string{})
	return MathBatch{
		ContractVersion: MathBatchContractVersion,
		Items: []MathBatchItem{{
			WrapperID: "math-000001",
			Unit: contracts.WorkUnit{
				Kind: contracts.WorkUnitMath, ID: "rw_11111111111111111111111111111111",
				Source: `\not\exists k\in\mathbb{R}`, Display: &display, MacroContextHash: macroHash,
				AccessibilityContext: &contracts.MathAccessibilityContext{Language: "zh-CN", Label: "不存在实数 k"},
				SourceLocation:       &contracts.SourceLocation{Path: "main.tex", Start: contracts.SourcePosition{Line: 8, Column: 4}},
			},
		}},
		MacroContexts:  map[string]map[string]string{macroHash: {}},
		OutputStrategy: MathOutputCHTML,
	}
}
