package projectdiagrams

import (
	"regexp"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

const (
	maxDiagramPreambleBytes        = 16 << 10
	maxDiagramPreambleDeclarations = 256
)

var (
	safeColorNamePattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9@:_-]{0,63}$`)
	safeColorModelPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9, ./_-]{0,63}$`)
	safeColorSpecPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.,:+!#/ _-]{0,255}$`)
	safeTikzControl       = regexp.MustCompile(`\\(?:tiny|scriptsize|footnotesize|small|normalsize|large|Large|LARGE|huge|Huge|bfseries|mdseries|itshape|upshape|sffamily|rmfamily|ttfamily)\b`)
)

// collectSafeDiagramPreamble preserves declarative color configuration that
// standalone diagram compilation would otherwise lose. It deliberately does
// not forward arbitrary project preamble TeX into the isolated worker.
func collectSafeDiagramPreamble(files []projectcore.File, diagramType string) string {
	declarations := make([]string, 0)
	seen := make(map[string]struct{})
	totalBytes := 0
	for _, file := range files {
		if file.Encoding == "base64" || (file.Kind != "tex" && file.Kind != "style") {
			continue
		}
		for cursor := 0; cursor < len(file.Body) && len(declarations) < maxDiagramPreambleDeclarations; {
			start, command, ok := nextSafeDiagramPreambleCommand(file.Body, cursor)
			if !ok {
				break
			}
			declaration, next, ok := parseSafeDiagramPreambleDeclaration(file.Body, start, command)
			if !ok {
				cursor = start + len(command)
				continue
			}
			cursor = next
			// tikzcd settings belong to the commutative-diagram package, which
			// ordinary standalone TikZ wrappers do not load.
			if command == `\tikzcdset` && diagramType != "tikzcd" {
				continue
			}
			declaration = strings.TrimSpace(declaration)
			if _, duplicate := seen[declaration]; duplicate {
				continue
			}
			additionalBytes := len(declaration)
			if len(declarations) > 0 {
				additionalBytes++
			}
			if totalBytes+additionalBytes > maxDiagramPreambleBytes {
				return strings.Join(declarations, "\n")
			}
			seen[declaration] = struct{}{}
			declarations = append(declarations, declaration)
			totalBytes += additionalBytes
		}
	}
	return strings.Join(declarations, "\n")
}

func nextSafeDiagramPreambleCommand(source string, cursor int) (int, string, bool) {
	bestStart := -1
	bestCommand := ""
	for _, command := range []string{`\definecolor`, `\providecolor`, `\colorlet`, `\tikzset`, `\tikzcdset`} {
		search := cursor
		for search < len(source) {
			start := indexUncommentedToken(source, search, command)
			if start < 0 {
				break
			}
			if hasCommandBoundary(source, start, command) {
				if bestStart < 0 || start < bestStart {
					bestStart = start
					bestCommand = command
				}
				break
			}
			search = start + len(command)
		}
	}
	return bestStart, bestCommand, bestStart >= 0
}

func parseSafeDiagramPreambleDeclaration(source string, start int, command string) (string, int, bool) {
	if command == `\tikzset` || command == `\tikzcdset` {
		settings, ok := readBraceGroup(source, start+len(command))
		if !ok || !safeTikzSettings(settings.Value) {
			return "", start + len(command), false
		}
		return source[start:settings.Next], settings.Next, true
	}
	return parseSafeColorDeclaration(source, start, command)
}

func parseSafeColorDeclaration(source string, start int, command string) (string, int, bool) {
	cursor := start + len(command)
	if optional, ok := readBracketGroup(source, cursor); ok {
		if !safeColorModelPattern.MatchString(strings.TrimSpace(optional.Value)) {
			return "", optional.Next, false
		}
		cursor = optional.Next
	}
	name, ok := readBraceGroup(source, cursor)
	if !ok || !safeColorNamePattern.MatchString(strings.TrimSpace(name.Value)) {
		return "", cursor, false
	}
	cursor = name.Next

	if command == `\colorlet` {
		if optional, ok := readBracketGroup(source, cursor); ok {
			if !safeColorModelPattern.MatchString(strings.TrimSpace(optional.Value)) {
				return "", optional.Next, false
			}
			cursor = optional.Next
		}
		value, ok := readBraceGroup(source, cursor)
		if !ok || !safeColorSpecPattern.MatchString(strings.TrimSpace(value.Value)) {
			return "", cursor, false
		}
		return source[start:value.Next], value.Next, true
	}

	model, ok := readBraceGroup(source, cursor)
	if !ok || !safeColorModelPattern.MatchString(strings.TrimSpace(model.Value)) {
		return "", cursor, false
	}
	specification, ok := readBraceGroup(source, model.Next)
	if !ok || !safeColorSpecPattern.MatchString(strings.TrimSpace(specification.Value)) {
		return "", model.Next, false
	}
	return source[start:specification.Next], specification.Next, true
}

func safeTikzSettings(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 4096 || strings.ContainsRune(value, 0) {
		return false
	}
	lower := strings.ToLower(value)
	for _, blocked := range []string{
		`/.code`, `/.unknown`, `execute at`, `external/system call`, `/utils/exec`,
		`prefix after command`, `append after command`,
		`\input`, `\include`, `\openin`, `\openout`, `\read`, `\write`, `\immediate`,
		`\catcode`, `\csname`, `\endcsname`, `\usepackage`, `\documentclass`, `\special`,
		`\directlua`, `\lua`, `\pdf`, `\newread`, `\newwrite`, `\loop`, `\repeat`,
		`\def`, `\gdef`, `\edef`, `\xdef`,
	} {
		if strings.Contains(lower, blocked) {
			return false
		}
	}
	withoutControls := safeTikzControl.ReplaceAllString(value, "")
	if strings.ContainsRune(withoutControls, '\\') || strings.ContainsRune(withoutControls, '%') {
		return false
	}
	depth := 0
	for _, character := range withoutControls {
		switch character {
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func diagramRenderSource(preamble, source string) string {
	preamble = strings.TrimSpace(preamble)
	source = strings.TrimSpace(source)
	if preamble == "" {
		return source
	}
	return preamble + "\n" + source
}
