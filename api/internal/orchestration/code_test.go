package orchestration

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func TestCodeBatchAndResultContractsPreserveExactSourceMetadata(t *testing.T) {
	batch := validCodeBatch()
	if err := batch.Validate(); err != nil {
		t.Fatal(err)
	}
	input := batch.Items[0]
	result := CodeBatchResult{
		ContractVersion: CodeResultContractVersion,
		Units: []CodeResolvedUnit{{
			ID: input.ID, State: "succeeded", HTML: `<pre class="shiki"><code>x</code></pre>`,
			Engine: "shiki", EngineVersion: "4.4.2", Theme: "github-light",
			CanonicalLanguage: "javascript", Highlighted: true,
			Source:      CodeSourceMetadata{Source: input.Source, Language: input.Language, Meta: input.Meta, SourceLocation: input.SourceLocation},
			Cache:       CodeCacheResult{Status: "miss", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("a", 64)},
			Diagnostics: []contracts.Diagnostic{},
		}},
		CSS:      []string{},
		Versions: validCodeVersions(),
	}
	if err := result.Validate(batch); err != nil {
		t.Fatal(err)
	}
	result.Units[0].Source.Source = "changed"
	if err := result.Validate(batch); err == nil {
		t.Fatal("changed source metadata was accepted")
	}
}

func TestCodeBatchRejectsWrongKindsDuplicatesAndUnpinnedOutput(t *testing.T) {
	for name, mutate := range map[string]func(*CodeBatch){
		"contract":  func(batch *CodeBatch) { batch.ContractVersion = "rin-code-batch/v0" },
		"kind":      func(batch *CodeBatch) { batch.Items[0].Kind = contracts.WorkUnitMath },
		"duplicate": func(batch *CodeBatch) { batch.Items = append(batch.Items, batch.Items[0]) },
		"theme":     func(batch *CodeBatch) { batch.Theme = "" },
		"strategy":  func(batch *CodeBatch) { batch.OutputStrategy = "tokens" },
	} {
		t.Run(name, func(t *testing.T) {
			batch := validCodeBatch()
			mutate(&batch)
			if err := batch.Validate(); err == nil {
				t.Fatal("invalid code batch was accepted")
			}
		})
	}
}

func TestCodeResultRequiresUnknownLanguageDiagnosticAndCacheIdentity(t *testing.T) {
	batch := validCodeBatch()
	batch.Items[0].Language = "unknown-language"
	result := CodeBatchResult{
		ContractVersion: CodeResultContractVersion,
		Units: []CodeResolvedUnit{{
			ID: batch.Items[0].ID, State: "succeeded", HTML: `<pre><code>x</code></pre>`,
			Engine: "shiki", EngineVersion: "4.4.2", Theme: batch.Theme,
			Source:      CodeSourceMetadata{Source: batch.Items[0].Source, Language: batch.Items[0].Language, Meta: batch.Items[0].Meta, SourceLocation: batch.Items[0].SourceLocation},
			Cache:       CodeCacheResult{Status: "miss", KeyVersion: CacheKeyVersion, Digest: strings.Repeat("b", 64)},
			Diagnostics: []contracts.Diagnostic{},
		}},
		CSS: []string{}, Versions: validCodeVersions(),
	}
	if err := result.Validate(batch); err == nil {
		t.Fatal("plain unknown-language output without a diagnostic was accepted")
	}
	result.Units[0].Diagnostics = []contracts.Diagnostic{{
		Code: "code.language.unknown", Severity: "warning", Message: "plain fallback", Stage: "code",
	}}
	if err := result.Validate(batch); err != nil {
		t.Fatal(err)
	}
	result.Units[0].Cache.Digest = "bad"
	if err := result.Validate(batch); err == nil {
		t.Fatal("invalid cache identity was accepted")
	}
}

func validCodeBatch() CodeBatch {
	return CodeBatch{
		ContractVersion: CodeBatchContractVersion, Theme: "github-light", OutputStrategy: "html",
		Items: []contracts.WorkUnit{{
			Kind: contracts.WorkUnitCode, ID: "rw_00000000000000000000000000000001",
			Source: "const x = '<tag>';", Language: "js", Meta: "title=sample.js",
			SourceLocation: &contracts.SourceLocation{Path: "article.md", Start: contracts.SourcePosition{Line: 3, Column: 1}},
		}},
	}
}

func validCodeVersions() map[string]string {
	return map[string]string{
		"engine": "shiki", "engine-version": "4.4.2", "plugin": "rin-markdown-code/v1",
		"theme": "github-light", "sanitizer": "rin-markdown-final-sanitizer/v2", "compatibility": "rin-reader-code/v1",
		"batch-contract": CodeBatchContractVersion, "result-contract": CodeResultContractVersion,
		"wrapper-contract": CodeWrapperContract, "cache-schema": CodeCacheSchemaVersion,
	}
}
