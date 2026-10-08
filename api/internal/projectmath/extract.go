package projectmath

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

const placeholderPrefix = "RINRENDERERMATHPLACEHOLDER"

type Unit struct {
	ID           string `json:"id"`
	Environment  string `json:"environment"`
	Source       string `json:"source"`
	RenderSource string `json:"renderSource"`
	SourceFile   string `json:"sourceFile"`
	SourceLine   int    `json:"sourceLine"`
	SourceColumn int    `json:"sourceColumn"`
	Placeholder  string `json:"placeholder"`
	DisplayMode  bool   `json:"displayMode"`
}

type Result struct {
	Files  []projectcore.File
	Math   []Unit
	Macros map[string]string
}

func Extract(files []projectcore.File) Result {
	result := Result{
		Files:  make([]projectcore.File, 0, len(files)),
		Macros: projectcore.CollectMathMacros(files),
	}
	for _, file := range files {
		next := file
		if file.Kind == "tex" && strings.TrimSpace(file.Body) != "" {
			body, units := extractFromSource(file.Path, file.Body, len(result.Math))
			next.Body = body
			result.Math = append(result.Math, units...)
		}
		result.Files = append(result.Files, next)
	}
	return result
}

func extractFromSource(sourceFile string, source string, offset int) (string, []Unit) {
	var out strings.Builder
	units := make([]Unit, 0)
	cursor := 0
	for cursor < len(source) {
		start, token, ok := nextMathStart(source, cursor)
		if !ok {
			out.WriteString(source[cursor:])
			break
		}
		parsed, ok := parseMathAt(source, start, token)
		if !ok {
			out.WriteString(source[cursor : start+len(token)])
			cursor = start + len(token)
			continue
		}
		id := fmt.Sprintf("math-%06d", offset+len(units)+1)
		placeholder := placeholderPrefix + strings.ToUpper(strings.ReplaceAll(id, "-", ""))
		line, column := sourcePosition(source, parsed.Start)
		units = append(units, Unit{
			ID:           id,
			Environment:  parsed.Environment,
			Source:       parsed.Source,
			RenderSource: parsed.RenderSource,
			SourceFile:   sourceFile,
			SourceLine:   line,
			SourceColumn: column,
			Placeholder:  placeholder,
			DisplayMode:  true,
		})
		out.WriteString(source[cursor:parsed.Start])
		out.WriteString("\n\n")
		out.WriteString(placeholder)
		out.WriteString("\n\n")
		cursor = parsed.End
	}
	return out.String(), units
}

func sourcePosition(source string, index int) (int, int) {
	if index < 0 {
		index = 0
	}
	if index > len(source) {
		index = len(source)
	}
	prefix := source[:index]
	line := strings.Count(prefix, "\n") + 1
	lineStart := strings.LastIndex(prefix, "\n") + 1
	column := utf8.RuneCountInString(source[lineStart:index]) + 1
	return line, column
}

type parsedMath struct {
	Start        int
	End          int
	Environment  string
	Source       string
	RenderSource string
}

func nextMathStart(source string, cursor int) (int, string, bool) {
	bestStart := -1
	bestToken := ""
	for _, token := range []string{`\[`, `\begin{`} {
		search := cursor
		for search < len(source) {
			index := strings.Index(source[search:], token)
			if index < 0 {
				break
			}
			index += search
			if token == `\begin{` {
				if _, ok := mathEnvironmentNameAt(source, index); !ok {
					search = index + len(token)
					continue
				}
			}
			if isCommented(source, index) || (token == `\[` && index > 0 && source[index-1] == '\\') {
				search = index + len(token)
				continue
			}
			if bestStart < 0 || index < bestStart {
				bestStart = index
				bestToken = token
			}
			break
		}
	}
	if bestStart < 0 {
		return 0, "", false
	}
	return bestStart, bestToken, true
}

func isCommented(source string, index int) bool {
	lineStart := strings.LastIndex(source[:index], "\n") + 1
	for cursor := lineStart; cursor < index; cursor++ {
		if source[cursor] != '%' {
			continue
		}
		backslashes := 0
		for slash := cursor - 1; slash >= lineStart && source[slash] == '\\'; slash-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return true
		}
	}
	return false
}

func mathEnvironmentNameAt(source string, begin int) (string, bool) {
	if !strings.HasPrefix(source[begin:], `\begin{`) {
		return "", false
	}
	nameStart := begin + len(`\begin{`)
	nameEnd := strings.IndexByte(source[nameStart:], '}')
	if nameEnd < 0 {
		return "", false
	}
	nameEnd += nameStart
	name := source[nameStart:nameEnd]
	_, ok := normalizeMathEnvironment(name)
	return name, ok
}

func parseMathAt(source string, start int, token string) (parsedMath, bool) {
	switch token {
	case `\[`:
		end := strings.Index(source[start+len(token):], `\]`)
		if end < 0 {
			return parsedMath{}, false
		}
		bodyStart := start + len(token)
		bodyEnd := bodyStart + end
		sourceEnd := bodyEnd + len(`\]`)
		body := strings.TrimSpace(source[bodyStart:bodyEnd])
		return parsedMath{Start: start, End: sourceEnd, Environment: "display", Source: source[start:sourceEnd], RenderSource: normalizeRenderSource("display", body)}, true
	case `$$`:
		bodyStart := start + len(token)
		end := strings.Index(source[bodyStart:], `$$`)
		if end < 0 {
			return parsedMath{}, false
		}
		bodyEnd := bodyStart + end
		sourceEnd := bodyEnd + len(`$$`)
		body := strings.TrimSpace(source[bodyStart:bodyEnd])
		return parsedMath{Start: start, End: sourceEnd, Environment: "display", Source: source[start:sourceEnd], RenderSource: normalizeRenderSource("display", body)}, true
	case `\begin{`:
		return parseEnvironmentMath(source, start)
	default:
		return parsedMath{}, false
	}
}

func parseEnvironmentMath(source string, begin int) (parsedMath, bool) {
	nameStart := begin + len(`\begin{`)
	nameEnd := strings.IndexByte(source[nameStart:], '}')
	if nameEnd < 0 {
		return parsedMath{}, false
	}
	nameEnd += nameStart
	name := source[nameStart:nameEnd]
	environment, ok := normalizeMathEnvironment(name)
	if !ok {
		return parsedMath{}, false
	}
	bodyStart := skipMathEnvironmentArguments(source, environment, nameEnd+1)
	bodyEnd := findMatchingEnvironmentEnd(source, bodyStart, name)
	if bodyEnd < 0 {
		return parsedMath{}, false
	}
	sourceEnd := bodyEnd + len(`\end{`+name+`}`)
	body := strings.TrimSpace(source[bodyStart:bodyEnd])
	return parsedMath{
		Start:        begin,
		End:          sourceEnd,
		Environment:  environment,
		Source:       source[begin:sourceEnd],
		RenderSource: normalizeRenderSource(environment, body),
	}, true
}

func normalizeMathEnvironment(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "*"))
	switch name {
	case "equation", "align", "gather", "multline", "flalign", "alignat":
		return name, true
	default:
		return "", false
	}
}

func skipMathEnvironmentArguments(source string, environment string, cursor int) int {
	cursor = skipSpaces(source, cursor)
	if group, ok := readBracketGroup(source, cursor); ok {
		cursor = skipSpaces(source, group.Next)
	}
	if environment == "alignat" {
		if group, ok := readBraceGroup(source, cursor); ok {
			cursor = skipSpaces(source, group.Next)
		}
	}
	return cursor
}

func normalizeRenderSource(environment string, body string) string {
	body = stripMathMetadata(strings.TrimSpace(body))
	switch environment {
	case "align", "flalign", "alignat":
		return `\begin{aligned}` + body + `\end{aligned}`
	case "gather":
		return `\begin{gathered}` + body + `\end{gathered}`
	default:
		return body
	}
}

var labelCommandPattern = regexp.MustCompile(`(?s)\\label\s*\{[^{}]*\}`)

func stripMathMetadata(source string) string {
	return strings.TrimSpace(labelCommandPattern.ReplaceAllString(source, ""))
}

func findMatchingEnvironmentEnd(source string, bodyStart int, name string) int {
	beginToken := `\begin{` + name + `}`
	endToken := `\end{` + name + `}`
	depth := 1
	cursor := bodyStart
	for cursor < len(source) {
		nextBegin := strings.Index(source[cursor:], beginToken)
		nextEnd := strings.Index(source[cursor:], endToken)
		if nextEnd < 0 {
			return -1
		}
		if nextBegin >= 0 && nextBegin < nextEnd {
			depth++
			cursor += nextBegin + len(beginToken)
			continue
		}
		if depth == 1 {
			return cursor + nextEnd
		}
		depth--
		cursor += nextEnd + len(endToken)
	}
	return -1
}

type parsedGroup struct {
	Value string
	Start int
	Next  int
}

func readBracketGroup(source string, start int) (parsedGroup, bool) {
	return readDelimitedGroup(source, start, '[', ']')
}

func readBraceGroup(source string, start int) (parsedGroup, bool) {
	return readDelimitedGroup(source, start, '{', '}')
}

func readDelimitedGroup(source string, start int, open byte, close byte) (parsedGroup, bool) {
	start = skipSpaces(source, start)
	if start >= len(source) || source[start] != open {
		return parsedGroup{}, false
	}
	depth := 0
	for cursor := start; cursor < len(source); cursor++ {
		ch := source[cursor]
		if ch == '\\' {
			cursor++
			continue
		}
		if ch == open {
			depth++
			continue
		}
		if ch != close {
			continue
		}
		depth--
		if depth == 0 {
			return parsedGroup{
				Value: source[start+1 : cursor],
				Start: start,
				Next:  cursor + 1,
			}, true
		}
	}
	return parsedGroup{}, false
}

func skipSpaces(source string, index int) int {
	for index < len(source) {
		switch source[index] {
		case ' ', '\t', '\r', '\n':
			index++
		default:
			return index
		}
	}
	return index
}
