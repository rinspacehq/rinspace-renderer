package orchestration

import (
	"context"
	"errors"
	"fmt"
	stdhtml "html"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

type WorkResolver struct {
	Math     MathService
	Diagrams DiagramService
	Code     CodeService
}

type WorkResolutionOptions struct {
	MacroContexts map[string]map[string]string
	MathStrategy  MathOutputStrategy
	CodeTheme     string
}

// Resolve validates and partitions a draft Bundle, then routes each typed unit through the one
// shared platform service for that kind. It never inserts output into the draft; adapter Finalize
// remains the only publish boundary.
func (resolver WorkResolver) Resolve(ctx context.Context, draft contracts.DocumentBundle, options WorkResolutionOptions) (ResolvedWork, error) {
	if err := draft.Validate(); err != nil {
		return ResolvedWork{}, fmt.Errorf("draft document bundle: %w", err)
	}
	if draft.State != contracts.DocumentBundleStateDraft {
		return ResolvedWork{}, errors.New("work resolver requires a draft document bundle")
	}
	if options.MathStrategy == "" {
		options.MathStrategy = MathOutputCHTML
	}
	if strings.TrimSpace(options.CodeTheme) == "" {
		return ResolvedWork{}, errors.New("work resolver requires a code theme")
	}

	mathItems := make([]MathBatchItem, 0)
	diagramItems := make([]DiagramBatchItem, 0)
	codeItems := make([]contracts.WorkUnit, 0)
	for _, unit := range draft.WorkUnits {
		switch unit.Kind {
		case contracts.WorkUnitMath:
			mathItems = append(mathItems, MathBatchItem{Unit: unit})
		case contracts.WorkUnitDiagram:
			diagramItems = append(diagramItems, DiagramBatchItem{
				Unit: unit, SourceMode: DiagramSourceBody, Body: unit.Source,
			})
		case contracts.WorkUnitCode:
			codeItems = append(codeItems, unit)
		default:
			return ResolvedWork{}, fmt.Errorf("unsupported work kind %q", unit.Kind)
		}
	}

	resolved := ResolvedWork{Units: make(map[string]ResolvedWorkUnit, len(draft.WorkUnits))}
	if len(mathItems) > 0 {
		if resolver.Math == nil {
			return ResolvedWork{}, errors.New("draft contains math but the shared math service is unavailable")
		}
		batch := MathBatch{
			ContractVersion: MathBatchContractVersion, Items: mathItems,
			MacroContexts: options.MacroContexts, OutputStrategy: options.MathStrategy,
		}
		result, err := resolver.Math.ResolveMath(ctx, batch)
		if err != nil {
			return ResolvedWork{}, fmt.Errorf("resolve shared math work: %w", err)
		}
		if err := result.Validate(batch); err != nil {
			return ResolvedWork{}, fmt.Errorf("shared math result: %w", err)
		}
		for index, unit := range result.Units {
			// Math CSS describes the batch runtime, not an individual formula. Carry it once so
			// large articles do not duplicate the same pinned font stylesheet in every unit of
			// the Markdown finalizer request.
			var css []string
			if index == 0 {
				css = append([]string(nil), result.CSS...)
			}
			if err := addResolvedUnit(resolved.Units, ResolvedWorkUnit{
				ID: unit.ID, Kind: contracts.WorkUnitMath, HTML: unit.HTML,
				CSS: css, Artifact: unit.Artifact,
				Diagnostics: append([]contracts.Diagnostic(nil), unit.Diagnostics...),
			}); err != nil {
				return ResolvedWork{}, err
			}
		}
	}
	if len(diagramItems) > 0 {
		if resolver.Diagrams == nil {
			return ResolvedWork{}, errors.New("draft contains diagrams but the shared diagram service is unavailable")
		}
		batch := DiagramBatch{
			ContractVersion: DiagramBatchContractVersion, Items: diagramItems, OutputStrategy: "svg",
		}
		result, err := resolver.Diagrams.ResolveDiagrams(ctx, batch)
		if err != nil {
			return ResolvedWork{}, fmt.Errorf("resolve shared diagram work: %w", err)
		}
		if err := result.Validate(batch); err != nil {
			return ResolvedWork{}, fmt.Errorf("shared diagram result: %w", err)
		}
		for _, unit := range result.Units {
			resolvedUnit := ResolvedWorkUnit{
				ID: unit.ID, Kind: contracts.WorkUnitDiagram, Artifact: unit.Artifact,
				Diagnostics: append([]contracts.Diagnostic(nil), unit.Diagnostics...),
			}
			if unit.State == "succeeded" {
				resolvedUnit.HTML = resolvedDiagramHTML(unit)
			} else {
				resolvedUnit.HTML = `<pre class="rin-diagram-fallback"><code>` + stdhtml.EscapeString(unit.Source.Source) + `</code></pre>`
			}
			if err := addResolvedUnit(resolved.Units, resolvedUnit); err != nil {
				return ResolvedWork{}, err
			}
		}
	}
	if len(codeItems) > 0 {
		if resolver.Code == nil {
			return ResolvedWork{}, errors.New("draft contains code but the shared code service is unavailable")
		}
		batch := CodeBatch{
			ContractVersion: CodeBatchContractVersion, Items: codeItems,
			Theme: options.CodeTheme, OutputStrategy: "html",
		}
		result, err := resolver.Code.ResolveCode(ctx, batch)
		if err != nil {
			return ResolvedWork{}, fmt.Errorf("resolve shared code work: %w", err)
		}
		if err := result.Validate(batch); err != nil {
			return ResolvedWork{}, fmt.Errorf("shared code result: %w", err)
		}
		for _, unit := range result.Units {
			if err := addResolvedUnit(resolved.Units, ResolvedWorkUnit{
				ID: unit.ID, Kind: contracts.WorkUnitCode, HTML: unit.HTML,
				CSS:         append([]string(nil), result.CSS...),
				Diagnostics: append([]contracts.Diagnostic(nil), unit.Diagnostics...),
			}); err != nil {
				return ResolvedWork{}, err
			}
		}
	}
	if len(resolved.Units) != len(draft.WorkUnits) {
		return ResolvedWork{}, errors.New("resolved work count does not match draft")
	}
	return resolved, nil
}

func addResolvedUnit(units map[string]ResolvedWorkUnit, unit ResolvedWorkUnit) error {
	if strings.TrimSpace(unit.ID) == "" || strings.TrimSpace(unit.HTML) == "" {
		return errors.New("resolved work unit is incomplete")
	}
	if _, duplicate := units[unit.ID]; duplicate {
		return fmt.Errorf("duplicate resolved work unit %q", unit.ID)
	}
	units[unit.ID] = unit
	return nil
}

func resolvedDiagramHTML(unit DiagramResolvedUnit) string {
	kind := safeClassSuffix(unit.Source.DiagramType)
	classes := "rin-reader-diagram rin-reader-diagram-" + kind
	inner := fmt.Sprintf(
		`<figure class="%s" data-diagram-id="%s"><img src="%s" alt="%s diagram" loading="lazy" decoding="async" data-rin-diagram-object-id="%s"></figure>`,
		classes, stdhtml.EscapeString(unit.DiagramID), stdhtml.EscapeString(unit.URL),
		stdhtml.EscapeString(unit.Source.DiagramType), stdhtml.EscapeString(unit.ObjectID),
	)
	if unit.Source.Layout == nil || strings.TrimSpace(unit.Source.Layout.Alignment) == "" {
		return inner
	}
	alignment := safeClassSuffix(unit.Source.Layout.Alignment)
	return fmt.Sprintf(`<div class="rin-align-block rin-align-%s" data-rin-align="%s">%s</div>`, alignment, alignment, inner)
}

func safeClassSuffix(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '-', character == '_':
			output.WriteRune(character)
		default:
			output.WriteByte('-')
		}
	}
	return strings.Trim(output.String(), "-")
}
