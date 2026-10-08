package mathservice

import (
	"context"
	"errors"
	"fmt"
	stdhtml "html"
	"regexp"
	"sort"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/mathrender"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

type FallbackResult struct {
	URL           string
	SVGHash       string
	EngineVersion string
	Artifact      *contracts.ArtifactReference
	Diagnostics   []contracts.Diagnostic
}

type FallbackFunc func(context.Context, orchestration.MathBatchItem) (FallbackResult, error)

type Config struct {
	Primary              mathrender.Renderer
	PrimaryEngine        string
	PrimaryVersion       string
	FontVersion          string
	PluginVersion        string
	SanitizerVersion     string
	CompatibilityVersion string
	FallbackVersion      string
	Fallback             FallbackFunc
	MaxBatchSize         int
}

type Service struct{ config Config }

func New(config Config) (*Service, error) {
	for name, value := range map[string]string{
		"primary engine": config.PrimaryEngine, "primary version": config.PrimaryVersion,
		"font version": config.FontVersion, "plugin version": config.PluginVersion,
		"sanitizer version": config.SanitizerVersion, "compatibility version": config.CompatibilityVersion,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("math service requires %s", name)
		}
	}
	if config.Primary == nil {
		return nil, errors.New("math service requires primary renderer")
	}
	if config.Fallback != nil && strings.TrimSpace(config.FallbackVersion) == "" {
		return nil, errors.New("math service fallback requires a version")
	}
	if config.MaxBatchSize < 0 {
		return nil, errors.New("math service max batch size cannot be negative")
	}
	if config.MaxBatchSize == 0 {
		config.MaxBatchSize = 512
	}
	return &Service{config: config}, nil
}

func (service *Service) ResolveMath(ctx context.Context, batch orchestration.MathBatch) (orchestration.MathBatchResult, error) {
	if service == nil {
		return orchestration.MathBatchResult{}, errors.New("math service is not configured")
	}
	if err := batch.Validate(); err != nil {
		return orchestration.MathBatchResult{}, err
	}
	result := orchestration.MathBatchResult{
		ContractVersion: orchestration.MathResultContractVersion,
		Units:           make([]orchestration.MathResolvedUnit, len(batch.Items)),
		CSS:             []string{},
		Versions: map[string]string{
			"primary-engine": service.config.PrimaryEngine, "primary-engine-version": service.config.PrimaryVersion,
			"font": service.config.FontVersion, "plugin": service.config.PluginVersion,
			"sanitizer": service.config.SanitizerVersion, "compatibility": service.config.CompatibilityVersion,
			"batch-contract": orchestration.MathBatchContractVersion, "result-contract": orchestration.MathResultContractVersion,
			"wrapper-contract": orchestration.MathWrapperContractVersion, "cache-schema": orchestration.MathCacheSchemaVersion,
		},
	}
	if service.config.Fallback != nil {
		result.Versions["fallback-engine-version"] = service.config.FallbackVersion
	}
	type primaryTarget struct {
		key     orchestration.CacheKey
		request mathrender.Request
		indexes []int
	}
	targets := []primaryTarget{}
	targetByDigest := map[string]int{}
	preferredFallback := []int{}
	for index, item := range batch.Items {
		key, err := service.cacheKey(item.Unit, batch.OutputStrategy)
		if err != nil {
			return orchestration.MathBatchResult{}, err
		}
		result.Units[index] = baseResolvedUnit(item.Unit, key, "miss")
		if service.prefersSVG(item.Unit.Source, *item.Unit.Display, batch.OutputStrategy) && service.config.Fallback != nil {
			preferredFallback = append(preferredFallback, index)
			continue
		}
		if target, ok := targetByDigest[key.Digest]; ok {
			targets[target].indexes = append(targets[target].indexes, index)
			result.Units[index].Cache.Status = "hit"
			continue
		}
		targetByDigest[key.Digest] = len(targets)
		targets = append(targets, primaryTarget{
			key: key,
			request: mathrender.Request{
				Source: item.Unit.Source, DisplayMode: *item.Unit.Display,
				Macros: cloneMacros(batch.MacroContexts[item.Unit.MacroContextHash]),
			},
			indexes: []int{index},
		})
	}
	type fallbackAttempt struct {
		result FallbackResult
		err    error
	}
	preferredByDigest := map[string]fallbackAttempt{}
	for _, index := range preferredFallback {
		digest := result.Units[index].Cache.Digest
		attempt, reused := preferredByDigest[digest]
		if !reused {
			attempt.result, attempt.err = service.renderFallback(ctx, batch.Items[index])
			preferredByDigest[digest] = attempt
		} else {
			result.Units[index].Cache.Status = "hit"
		}
		service.applyFallback(batch.Items[index], &result.Units[index], nil, attempt.result, attempt.err)
	}
	requests := make([]mathrender.Request, len(targets))
	for index, target := range targets {
		requests[index] = target.request
	}
	for start := 0; start < len(requests); start += service.config.MaxBatchSize {
		end := start + service.config.MaxBatchSize
		if end > len(requests) {
			end = len(requests)
		}
		primary, batchErr := service.config.Primary.RenderBatch(ctx, requests[start:end])
		if batchErr == nil && len(primary) != end-start {
			batchErr = errors.New("primary math renderer returned wrong batch size")
		}
		for targetIndex := start; targetIndex < end; targetIndex++ {
			target := targets[targetIndex]
			var rendered mathrender.BatchResult
			if batchErr != nil {
				rendered.Err = batchErr
			} else {
				rendered = primary[targetIndex-start]
			}
			var fallback fallbackAttempt
			if rendered.Err != nil {
				fallback.result, fallback.err = service.renderFallback(ctx, batch.Items[target.indexes[0]])
			}
			for _, index := range target.indexes {
				if rendered.Err != nil {
					service.applyFallback(batch.Items[index], &result.Units[index], rendered.Err, fallback.result, fallback.err)
					continue
				}
				service.resolvePrimary(batch.Items[index], rendered.Result, &result.Units[index])
			}
			if rendered.Err == nil && strings.TrimSpace(rendered.Result.CSS) != "" {
				result.CSS = append(result.CSS, rendered.Result.CSS)
			}
		}
	}
	result.CSS = MergeCSS(result.CSS...)
	if err := result.Validate(batch); err != nil {
		return orchestration.MathBatchResult{}, fmt.Errorf("validate math service result: %w", err)
	}
	return result, nil
}

func (service *Service) cacheKey(unit contracts.WorkUnit, strategy orchestration.MathOutputStrategy) (orchestration.CacheKey, error) {
	engineVersion := service.config.PrimaryEngine + "@" + service.config.PrimaryVersion
	if service.config.Fallback != nil {
		engineVersion += "+rin-texsvg@" + service.config.FallbackVersion
	}
	return orchestration.BuildCacheKey(orchestration.CacheKeyInput{
		Stage: orchestration.CacheStageMath,
		NormalizedInputs: map[string]string{
			"source": unit.Source, "display": fmt.Sprintf("%t", *unit.Display),
		},
		ProjectContext: map[string]string{"macro-context-hash": unit.MacroContextHash},
		Versions: map[string]string{
			"math-engine": engineVersion, "plugin": service.config.PluginVersion,
			"font": service.config.FontVersion, "sanitizer": service.config.SanitizerVersion,
			"math-contract":   orchestration.MathResultContractVersion + "+" + orchestration.MathWrapperContractVersion,
			"output-strategy": string(strategy), "compatibility": service.config.CompatibilityVersion,
		},
	})
}

func (service *Service) resolvePrimary(item orchestration.MathBatchItem, rendered mathrender.Result, target *orchestration.MathResolvedUnit) {
	engine := firstNonEmpty(rendered.Engine, service.config.PrimaryEngine)
	version := firstNonEmpty(rendered.Version, service.config.PrimaryVersion)
	target.State = "succeeded"
	target.Engine = engine
	target.EngineVersion = version
	target.HTML = mathHTML(item, rendered.HTML, engine, version)
}

func (service *Service) renderFallback(ctx context.Context, item orchestration.MathBatchItem) (FallbackResult, error) {
	if service.config.Fallback == nil {
		return FallbackResult{}, errors.New("TeX SVG fallback is unavailable")
	}
	return service.config.Fallback(ctx, item)
}

func (service *Service) applyFallback(item orchestration.MathBatchItem, target *orchestration.MathResolvedUnit, primaryErr error, fallback FallbackResult, fallbackErr error) {
	if primaryErr != nil {
		target.Diagnostics = append(target.Diagnostics, diagnostic("math.primary_fallback", "Primary math render failed; attempting TeX SVG fallback: "+primaryErr.Error()))
	}
	if service.config.Fallback == nil {
		target.State = "failed"
		message := "TeX SVG fallback is unavailable"
		if fallbackErr != nil {
			message = fallbackErr.Error()
		}
		target.Diagnostics = append(target.Diagnostics, diagnostic("math.fallback.unavailable", message))
		target.HTML = mathFailureHTML(item)
		return
	}
	artifactValid := fallback.Artifact != nil && fallback.Artifact.Validate() == nil &&
		len(fallback.SVGHash) == 64 && fallback.Artifact.SHA256 == fallback.SVGHash && fallback.Artifact.Bytes > 0 &&
		fallback.Artifact.ArtifactID == "diagrams/v1/svg-sha256/"+fallback.SVGHash[:2]+"/"+fallback.SVGHash+".svg" &&
		fallback.Artifact.MediaType == "image/svg+xml; charset=utf-8" &&
		fallback.Artifact.Visibility == "public" && fallback.Artifact.ExpiresAt == ""
	if fallbackErr != nil || strings.TrimSpace(fallback.URL) == "" || !sha256Pattern.MatchString(fallback.SVGHash) || !artifactValid {
		target.State = "failed"
		message := "TeX SVG fallback returned invalid output"
		if fallbackErr != nil {
			message = fallbackErr.Error()
		}
		code := "math.texsvg.failed"
		if strings.Contains(strings.ToLower(message), "fallback limit reached") {
			code = "math.texsvg.limit_exceeded"
		}
		target.Diagnostics = append(target.Diagnostics, diagnostic(code, message))
		target.HTML = mathFailureHTML(item)
		return
	}
	target.State = "succeeded"
	target.Engine = "rin-texsvg"
	target.EngineVersion = firstNonEmpty(fallback.EngineVersion, service.config.FallbackVersion)
	target.Artifact = fallback.Artifact
	target.Diagnostics = append(target.Diagnostics, fallback.Diagnostics...)
	target.HTML = mathSVGHTML(item, fallback)
}

func mathFailureHTML(item orchestration.MathBatchItem) string {
	tagName, displayClass := mathContainer(*item.Unit.Display)
	classes := []string{"rin-math", displayClass, "rin-math-fallback"}
	if !*item.Unit.Display && IsComplexSource(item.Unit.Source) {
		classes = append(classes, "rin-math-tall-inline")
	}
	attrs := wrapperAttributes(item, classes, "unavailable", "")
	body := `<code class="rin-math-source-fallback">` + stdhtml.EscapeString(item.Unit.Source) + `</code>`
	return fmt.Sprintf("<%s%s>%s</%s>", tagName, attrs, body, tagName)
}

func baseResolvedUnit(unit contracts.WorkUnit, key orchestration.CacheKey, status string) orchestration.MathResolvedUnit {
	return orchestration.MathResolvedUnit{
		ID: unit.ID,
		Source: orchestration.MathSourceMetadata{
			TeX: unit.Source, Display: *unit.Display, MacroContextHash: unit.MacroContextHash,
			AccessibilityContext: unit.AccessibilityContext, SourceLocation: unit.SourceLocation,
		},
		Cache:       orchestration.MathCacheResult{Status: status, KeyVersion: key.Version, Digest: key.Digest},
		Diagnostics: []contracts.Diagnostic{},
	}
}

func mathHTML(item orchestration.MathBatchItem, body, engine, version string) string {
	tagName, displayClass := mathContainer(*item.Unit.Display)
	classes := []string{"rin-math", displayClass, engineClass(engine)}
	if !*item.Unit.Display && IsComplexSource(item.Unit.Source) {
		classes = append(classes, "rin-math-tall-inline")
	}
	attrs := wrapperAttributes(item, classes, engine, version)
	return fmt.Sprintf("<%s%s>%s</%s>", tagName, attrs, body, tagName)
}

func mathSVGHTML(item orchestration.MathBatchItem, rendered FallbackResult) string {
	tagName, displayClass := mathContainer(*item.Unit.Display)
	classes := []string{"rin-math", displayClass, "rin-math-svg"}
	if !*item.Unit.Display && IsComplexSource(item.Unit.Source) {
		classes = append(classes, "rin-math-tall-inline")
	}
	attrs := wrapperAttributes(item, classes, "rin-texsvg", "")
	attrs += fmt.Sprintf(` data-rin-math-svg-hash="%s"`, stdhtml.EscapeString(rendered.SVGHash))
	if rendered.Artifact != nil {
		attrs += fmt.Sprintf(` data-rin-math-object-id="%s"`, stdhtml.EscapeString(rendered.Artifact.ArtifactID))
	}
	alt := item.Unit.Source
	if item.Unit.AccessibilityContext != nil && strings.TrimSpace(item.Unit.AccessibilityContext.Label) != "" {
		alt = item.Unit.AccessibilityContext.Label
	}
	image := fmt.Sprintf(`<img class="rin-math-svg-image" src="%s" alt="%s" loading="lazy" decoding="async">`,
		stdhtml.EscapeString(rendered.URL), stdhtml.EscapeString(alt))
	return fmt.Sprintf("<%s%s>%s</%s>", tagName, attrs, image, tagName)
}

func wrapperAttributes(item orchestration.MathBatchItem, classes []string, engine, version string) string {
	attrs := ""
	if item.WrapperID != "" {
		attrs += fmt.Sprintf(` id="%s"`, stdhtml.EscapeString(item.WrapperID))
	}
	attrs += fmt.Sprintf(` class="%s" data-rin-math-engine="%s" data-rin-math-source="%s"`,
		strings.Join(classes, " "), stdhtml.EscapeString(engine), stdhtml.EscapeString(item.Unit.Source))
	if version != "" {
		attrs += fmt.Sprintf(` data-rin-math-version="%s"`, stdhtml.EscapeString(version))
	}
	if accessibility := item.Unit.AccessibilityContext; accessibility != nil {
		if accessibility.Language != "" {
			attrs += fmt.Sprintf(` lang="%s"`, stdhtml.EscapeString(accessibility.Language))
		}
		if accessibility.Label != "" {
			attrs += fmt.Sprintf(` aria-label="%s"`, stdhtml.EscapeString(accessibility.Label))
		}
		if accessibility.SpeechStyle != "" {
			attrs += fmt.Sprintf(` data-rin-math-speech-style="%s"`, stdhtml.EscapeString(accessibility.SpeechStyle))
		}
	}
	return attrs
}

func mathContainer(display bool) (string, string) {
	if display {
		return "div", "rin-math-display"
	}
	return "span", "rin-math-inline"
}

func engineClass(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "katex", "katex-legacy":
		return "rin-math-katex"
	case "rin-texsvg", "texsvg", "texsvg-fallback":
		return "rin-math-svg"
	default:
		return "rin-math-mathjax"
	}
}

func IsComplexSource(source string) bool {
	source = strings.ToLower(source)
	for _, marker := range []string{
		`\begin{matrix}`, `\begin{pmatrix}`, `\begin{bmatrix}`, `\begin{bmatrix*}`,
		`\begin{vmatrix}`, `\begin{vmatrix*}`, `\begin{array}`, `\begin{cases}`,
		`\begin{aligned}`, `\begin{gathered}`, `\begin{split}`, `\substack`,
	} {
		if strings.Contains(source, marker) {
			return true
		}
	}
	return false
}

func (service *Service) prefersSVG(source string, display bool, strategy orchestration.MathOutputStrategy) bool {
	switch strategy {
	case orchestration.MathOutputComplexSVG:
		return IsComplexSource(source)
	case orchestration.MathOutputDisplaySVG:
		return display || IsComplexSource(source)
	case orchestration.MathOutputSVG:
		return true
	default:
		return false
	}
}

func MergeCSS(blocks ...string) []string {
	set := make(map[string]struct{})
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for existing := range set {
			switch {
			case existing == block || strings.Contains(existing, block):
				block = ""
			case strings.Contains(block, existing):
				delete(set, existing)
			}
		}
		if block != "" {
			set[block] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for block := range set {
		result = append(result, block)
	}
	sort.Strings(result)
	seen := make(map[string]struct{})
	rules := []string{}
	for _, block := range result {
		for _, rule := range splitCSSRules(block) {
			if _, duplicate := seen[rule]; duplicate {
				continue
			}
			seen[rule] = struct{}{}
			rules = append(rules, rule)
		}
	}
	if len(rules) == 0 {
		return []string{}
	}
	return []string{strings.Join(rules, "\n")}
}

// MergeCSSInOrder merges already-ordered CSS aggregates without changing cascade order. It is
// used when a later math batch augments a style block already present in a reader fragment.
func MergeCSSInOrder(blocks ...string) string {
	seen := make(map[string]struct{})
	rules := []string{}
	for _, block := range blocks {
		for _, rule := range splitCSSRules(block) {
			if _, duplicate := seen[rule]; duplicate {
				continue
			}
			seen[rule] = struct{}{}
			rules = append(rules, rule)
		}
	}
	return strings.Join(rules, "\n")
}

func splitCSSRules(css string) []string {
	css = strings.TrimSpace(css)
	if css == "" {
		return nil
	}
	rules := []string{}
	start := 0
	depth := 0
	for index, character := range css {
		switch character {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
			if depth == 0 {
				rule := strings.TrimSpace(css[start : index+len(string(character))])
				if rule != "" {
					rules = append(rules, rule)
				}
				start = index + len(string(character))
			}
		}
	}
	if tail := strings.TrimSpace(css[start:]); tail != "" {
		rules = append(rules, tail)
	}
	return rules
}

func diagnostic(code, message string) contracts.Diagnostic {
	return contracts.Diagnostic{Code: code, Severity: "warning", Message: message, Stage: "math"}
}

func cloneMacros(macros map[string]string) map[string]string {
	if len(macros) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(macros))
	for name, value := range macros {
		cloned[name] = value
	}
	return cloned
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

var _ orchestration.MathService = (*Service)(nil)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
