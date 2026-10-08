package projectdiagrams

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

const placeholderPrefix = "RINRENDERERDIAGRAMPLACEHOLDER"

type Diagram struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Options      string `json:"options,omitempty"`
	Body         string `json:"body"`
	Source       string `json:"source"`
	RenderSource string `json:"-"`
	SourceFile   string `json:"sourceFile"`
	SourceLine   int    `json:"sourceLine"`
	SourceColumn int    `json:"sourceColumn"`
	Placeholder  string `json:"placeholder"`
	Layout       Layout `json:"layout,omitempty"`
}

type Layout struct {
	Alignment string `json:"alignment,omitempty"`
}

type Result struct {
	Files    []projectcore.File
	Diagrams []Diagram
}

func Extract(files []projectcore.File) Result {
	result := Result{
		Files: make([]projectcore.File, 0, len(files)),
	}
	for _, file := range files {
		next := file
		if file.Kind == "tex" && strings.TrimSpace(file.Body) != "" {
			body, diagrams := extractFromSource(file.Path, file.Body, len(result.Diagrams))
			next.Body = body
			result.Diagrams = append(result.Diagrams, diagrams...)
		}
		result.Files = append(result.Files, next)
	}
	preambles := make(map[string]string)
	for index := range result.Diagrams {
		kind := result.Diagrams[index].Type
		preamble, ok := preambles[kind]
		if !ok {
			preamble = collectSafeDiagramPreamble(files, kind)
			preambles[kind] = preamble
		}
		result.Diagrams[index].RenderSource = diagramRenderSource(preamble, result.Diagrams[index].Source)
	}
	return result
}

func extractFromSource(sourceFile string, source string, offset int) (string, []Diagram) {
	var out strings.Builder
	diagrams := make([]Diagram, 0)
	cursor := 0
	for cursor < len(source) {
		start, token, ok := nextDiagramStart(source, cursor)
		if !ok {
			out.WriteString(source[cursor:])
			break
		}
		parsed, ok := parseDiagramAt(source, cursor, start, token)
		if !ok {
			out.WriteString(source[cursor:parsed.Next])
			cursor = parsed.Next
			continue
		}
		parsed = expandDiagramLayout(source, cursor, parsed)

		id := fmt.Sprintf("diagram-%06d", offset+len(diagrams)+1)
		placeholder := placeholderPrefix + strings.ToUpper(strings.ReplaceAll(id, "-", ""))
		line, column := sourcePosition(source, parsed.SourceStart)
		diagrams = append(diagrams, Diagram{
			ID:           id,
			Type:         parsed.Type,
			Options:      strings.TrimSpace(parsed.Options),
			Body:         strings.TrimSpace(parsed.Body),
			Source:       parsed.Source,
			SourceFile:   sourceFile,
			SourceLine:   line,
			SourceColumn: column,
			Placeholder:  placeholder,
			Layout:       parsed.Layout,
		})

		out.WriteString(source[cursor:parsed.ReplaceStart])
		out.WriteString("\n\n")
		out.WriteString(placeholder)
		out.WriteString("\n\n")
		cursor = parsed.ReplaceEnd
	}
	return out.String(), diagrams
}

type parsedDiagram struct {
	Type         string
	Options      string
	Body         string
	Source       string
	SourceStart  int
	ReplaceStart int
	ReplaceEnd   int
	Next         int
	Layout       Layout
}

func nextDiagramStart(source string, cursor int) (int, string, bool) {
	bestStart := -1
	bestToken := ""
	for _, token := range []string{`\begin{`, `\schemestart`, `\xymatrix`, `\chemfig`} {
		search := cursor
		for search < len(source) {
			index := indexUncommentedToken(source, search, token)
			if index < 0 {
				break
			}
			if token == `\begin{` {
				if _, ok := diagramEnvironmentNameAt(source, index); !ok {
					search = index + len(token)
					continue
				}
			}
			if token == `\begin{` || hasCommandBoundary(source, index, token) {
				if bestStart < 0 || index < bestStart {
					bestStart = index
					bestToken = token
				}
				break
			}
			search = index + len(token)
		}
	}
	if bestStart < 0 {
		return 0, "", false
	}
	return bestStart, bestToken, true
}

// indexUncommentedToken returns the next occurrence of token that is visible
// to TeX. An unescaped percent sign comments out the remainder of its line;
// commands in that remainder must not become independent diagram work units.
func indexUncommentedToken(source string, start int, token string) int {
	for start < len(source) {
		relative := strings.Index(source[start:], token)
		if relative < 0 {
			return -1
		}
		index := start + relative
		if !isInLineComment(source, index) {
			return index
		}
		start = index + len(token)
	}
	return -1
}

func isInLineComment(source string, index int) bool {
	if index <= 0 || index > len(source) {
		return false
	}
	lineStart := strings.LastIndexByte(source[:index], '\n') + 1
	for cursor := lineStart; cursor < index; cursor++ {
		if source[cursor] != '%' {
			continue
		}
		backslashes := 0
		for previous := cursor - 1; previous >= lineStart && source[previous] == '\\'; previous-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return true
		}
	}
	return false
}

func diagramEnvironmentNameAt(source string, begin int) (string, bool) {
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
	_, ok := normalizeEnvironment(name)
	return name, ok
}

func parseDiagramAt(source string, cursor int, start int, token string) (parsedDiagram, bool) {
	switch token {
	case `\begin{`:
		return parseEnvironmentDiagram(source, cursor, start)
	case `\xymatrix`:
		return parseXyMatrixCommand(source, cursor, start)
	case `\chemfig`:
		return parseChemfigCommand(source, cursor, start)
	case `\schemestart`:
		return parseChemfigScheme(source, cursor, start)
	default:
		return parsedDiagram{Next: start + len(token)}, false
	}
}

func parseEnvironmentDiagram(source string, cursor int, begin int) (parsedDiagram, bool) {
	nameStart := begin + len(`\begin{`)
	nameEnd := strings.IndexByte(source[nameStart:], '}')
	if nameEnd < 0 {
		return parsedDiagram{Next: len(source)}, false
	}
	nameEnd += nameStart
	name := source[nameStart:nameEnd]
	kind, ok := normalizeEnvironment(name)
	if !ok {
		return parsedDiagram{Next: nameEnd + 1}, false
	}

	options, bodyStart := readEnvironmentArguments(source, name, nameEnd+1)
	endToken := `\end{` + name + `}`
	bodyEnd := findMatchingEnvironmentEnd(source, bodyStart, name)
	if bodyEnd < 0 {
		return parsedDiagram{Next: nameEnd + 1}, false
	}
	sourceEnd := bodyEnd + len(endToken)
	replaceStart, replaceEnd := replaceBoundsWithDisplayMath(source, cursor, begin, sourceEnd)
	return parsedDiagram{
		Type:         kind,
		Options:      options,
		Body:         source[bodyStart:bodyEnd],
		Source:       source[begin:sourceEnd],
		SourceStart:  begin,
		ReplaceStart: replaceStart,
		ReplaceEnd:   replaceEnd,
		Next:         sourceEnd,
	}, true
}

func parseXyMatrixCommand(source string, cursor int, start int) (parsedDiagram, bool) {
	index := start + len(`\xymatrix`)
	optionsStart := index
	for strings.HasPrefix(source[index:], "@") {
		index++
		for index < len(source) && !isSpace(source[index]) && source[index] != '{' {
			index++
		}
		index = skipSpaces(source, index)
	}
	options := strings.TrimSpace(source[optionsStart:index])
	group, ok := readBraceGroup(source, skipSpaces(source, index))
	if !ok {
		return parsedDiagram{Next: start + len(`\xymatrix`)}, false
	}
	sourceEnd := group.Next
	replaceStart, replaceEnd := replaceBoundsWithDisplayMath(source, cursor, start, sourceEnd)
	return parsedDiagram{
		Type:         "xymatrix",
		Options:      options,
		Body:         group.Value,
		Source:       source[start:sourceEnd],
		SourceStart:  start,
		ReplaceStart: replaceStart,
		ReplaceEnd:   replaceEnd,
		Next:         sourceEnd,
	}, true
}

func parseChemfigCommand(source string, cursor int, start int) (parsedDiagram, bool) {
	index := start + len(`\chemfig`)
	options := ""
	if group, ok := readBracketGroup(source, skipSpaces(source, index)); ok {
		options = group.Value
		index = group.Next
	}
	group, ok := readBraceGroup(source, skipSpaces(source, index))
	if !ok {
		return parsedDiagram{Next: start + len(`\chemfig`)}, false
	}
	sourceEnd := group.Next
	replaceStart, replaceEnd := replaceBoundsWithDisplayMath(source, cursor, start, sourceEnd)
	return parsedDiagram{
		Type:         "chemfig",
		Options:      options,
		Body:         group.Value,
		Source:       source[start:sourceEnd],
		SourceStart:  start,
		ReplaceStart: replaceStart,
		ReplaceEnd:   replaceEnd,
		Next:         sourceEnd,
	}, true
}

func parseChemfigScheme(source string, cursor int, start int) (parsedDiagram, bool) {
	bodyStart := start + len(`\schemestart`)
	stop := findMatchingSchemeStop(source, bodyStart)
	if stop < 0 {
		return parsedDiagram{Next: bodyStart}, false
	}
	sourceEnd := stop + len(`\schemestop`)
	replaceStart, replaceEnd := replaceBoundsWithDisplayMath(source, cursor, start, sourceEnd)
	return parsedDiagram{
		Type:         "chemfig-scheme",
		Body:         source[bodyStart:stop],
		Source:       source[start:sourceEnd],
		SourceStart:  start,
		ReplaceStart: replaceStart,
		ReplaceEnd:   replaceEnd,
		Next:         sourceEnd,
	}, true
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
	return line, utf8.RuneCountInString(source[lineStart:index]) + 1
}

func expandDiagramLayout(source string, cursor int, diagram parsedDiagram) parsedDiagram {
	changed := true
	for changed {
		changed = false
		if expanded, ok := expandEnvironmentWrapper(source, cursor, diagram); ok {
			diagram = expanded
			changed = true
			continue
		}
		if expanded, ok := expandCommandWrapper(source, cursor, diagram); ok {
			diagram = expanded
			changed = true
			continue
		}
		if expanded, ok := expandLeadingAlignment(source, cursor, diagram); ok {
			diagram = expanded
			changed = true
			continue
		}
	}
	return diagram
}

func expandEnvironmentWrapper(source string, cursor int, diagram parsedDiagram) (parsedDiagram, bool) {
	beginStart, name, bodyStart, alignment, ok := layoutEnvironmentBefore(source, cursor, diagram.ReplaceStart)
	if !ok || !isOnlyLayoutGap(source[bodyStart:diagram.ReplaceStart]) {
		return diagram, false
	}
	endToken := `\end{` + name + `}`
	endStart := skipSpaces(source, diagram.ReplaceEnd)
	if !strings.HasPrefix(source[endStart:], endToken) {
		return diagram, false
	}
	diagram.ReplaceStart = beginStart
	diagram.ReplaceEnd = endStart + len(endToken)
	diagram = withAlignment(diagram, alignment)
	return diagram, true
}

func layoutEnvironmentBefore(source string, cursor int, start int) (int, string, int, string, bool) {
	segment := source[cursor:start]
	beginRel := strings.LastIndex(segment, `\begin{`)
	if beginRel < 0 {
		return 0, "", 0, "", false
	}
	beginStart := cursor + beginRel
	nameStart := beginStart + len(`\begin{`)
	nameEnd := strings.IndexByte(source[nameStart:], '}')
	if nameEnd < 0 {
		return 0, "", 0, "", false
	}
	nameEnd += nameStart
	name := source[nameStart:nameEnd]
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "center":
		return beginStart, name, nameEnd + 1, "center", true
	case "flushleft":
		return beginStart, name, nameEnd + 1, "flushleft", true
	case "flushright":
		return beginStart, name, nameEnd + 1, "flushright", true
	case "adjustbox":
		index := skipSpaces(source, nameEnd+1)
		if group, ok := readBracketGroup(source, index); ok {
			index = skipSpaces(source, group.Next)
		}
		settings, ok := readBraceGroup(source, index)
		if !ok {
			return 0, "", 0, "", false
		}
		return beginStart, name, settings.Next, adjustboxAlignment(settings.Value), true
	default:
		return 0, "", 0, "", false
	}
}

func expandCommandWrapper(source string, cursor int, diagram parsedDiagram) (parsedDiagram, bool) {
	contentOpen := previousNonSpaceIndex(source, cursor, diagram.ReplaceStart)
	if contentOpen < cursor || source[contentOpen] != '{' {
		return diagram, false
	}
	content, ok := readBraceGroup(source, contentOpen)
	if !ok || content.Start != contentOpen || content.Next < diagram.ReplaceEnd {
		return diagram, false
	}
	if !isOnlyLayoutGap(source[content.Start+1:diagram.ReplaceStart]) ||
		!isOnlyLayoutGap(source[diagram.ReplaceEnd:content.Next-1]) {
		return diagram, false
	}
	wrapper, ok := commandWrapperBefore(source, cursor, content.Start)
	if !ok || wrapper.ContentStart != content.Start || wrapper.ContentEnd != content.Next {
		return diagram, false
	}
	diagram.ReplaceStart = wrapper.Start
	diagram.ReplaceEnd = wrapper.End
	diagram = withAlignment(diagram, wrapper.Alignment)
	return diagram, true
}

type parsedCommandWrapper struct {
	Start        int
	End          int
	ContentStart int
	ContentEnd   int
	Alignment    string
}

func commandWrapperBefore(source string, cursor int, contentStart int) (parsedCommandWrapper, bool) {
	candidates := []string{
		`\centerline`, `\center`, `\makebox`, `\resizebox`, `\scalebox`, `\rotatebox`, `\adjustbox`,
		`\parbox`, `\raisebox`, `\fbox`, `\framebox`, `\mbox`, `\hbox`,
	}
	bestStart := -1
	for _, command := range candidates {
		search := cursor
		for search < contentStart {
			index := strings.Index(source[search:contentStart], command)
			if index < 0 {
				break
			}
			index += search
			if hasCommandBoundary(source, index, command) && index > bestStart {
				bestStart = index
			}
			search = index + len(command)
		}
	}
	if bestStart < 0 {
		return parsedCommandWrapper{}, false
	}
	wrapper, ok := parseCommandWrapperAt(source, bestStart)
	if !ok || !isOnlyLayoutGap(source[wrapper.ContentEnd:wrapper.End]) {
		return parsedCommandWrapper{}, false
	}
	return wrapper, true
}

func parseCommandWrapperAt(source string, start int) (parsedCommandWrapper, bool) {
	command, index, ok := readCommandName(source, start)
	if !ok {
		return parsedCommandWrapper{}, false
	}
	switch command {
	case "centerline", "center":
		return parseContentCommandWrapper(source, start, index, 0, 0, "center")
	case "makebox":
		wrapper, options, ok := parseContentCommandWrapperWithOptions(source, start, index, 2, 0)
		if !ok {
			return parsedCommandWrapper{}, false
		}
		wrapper.Alignment = makeboxAlignment(options)
		return wrapper, true
	case "resizebox":
		return parseContentCommandWrapper(source, start, index, 0, 2, "")
	case "scalebox", "rotatebox":
		return parseContentCommandWrapper(source, start, index, 1, 1, "")
	case "adjustbox":
		settings, ok := readBraceGroup(source, skipSpaces(source, index))
		if !ok {
			return parsedCommandWrapper{}, false
		}
		content, ok := readBraceGroup(source, skipSpaces(source, settings.Next))
		if !ok {
			return parsedCommandWrapper{}, false
		}
		return parsedCommandWrapper{
			Start:        start,
			End:          content.Next,
			ContentStart: content.Start,
			ContentEnd:   content.Next,
			Alignment:    adjustboxAlignment(settings.Value),
		}, true
	case "parbox":
		return parseContentCommandWrapper(source, start, index, 3, 1, "")
	case "raisebox":
		return parseRaiseboxWrapper(source, start, index)
	case "fbox", "mbox", "hbox":
		return parseContentCommandWrapper(source, start, index, 0, 0, "")
	case "framebox":
		return parseContentCommandWrapper(source, start, index, 2, 0, "")
	default:
		return parsedCommandWrapper{}, false
	}
}

func parseRaiseboxWrapper(source string, start int, index int) (parsedCommandWrapper, bool) {
	lift, ok := readBraceGroup(source, skipSpaces(source, index))
	if !ok {
		return parsedCommandWrapper{}, false
	}
	cursor := lift.Next
	for count := 0; count < 2; count++ {
		group, ok := readBracketGroup(source, cursor)
		if !ok {
			break
		}
		cursor = group.Next
	}
	content, ok := readBraceGroup(source, cursor)
	if !ok {
		return parsedCommandWrapper{}, false
	}
	return parsedCommandWrapper{
		Start:        start,
		End:          content.Next,
		ContentStart: content.Start,
		ContentEnd:   content.Next,
	}, true
}

func parseContentCommandWrapper(source string, start int, index int, maxOptions int, requiredLeadingGroups int, alignment string) (parsedCommandWrapper, bool) {
	wrapper, _, ok := parseContentCommandWrapperWithOptions(source, start, index, maxOptions, requiredLeadingGroups)
	if !ok {
		return parsedCommandWrapper{}, false
	}
	wrapper.Alignment = alignment
	return wrapper, true
}

func parseContentCommandWrapperWithOptions(source string, start int, index int, maxOptions int, requiredLeadingGroups int) (parsedCommandWrapper, []string, bool) {
	cursor := index
	options := make([]string, 0, maxOptions)
	for count := 0; count < maxOptions; count++ {
		group, ok := readBracketGroup(source, skipSpaces(source, cursor))
		if !ok {
			break
		}
		options = append(options, group.Raw)
		cursor = group.Next
	}
	groups := make([]groupResult, 0, requiredLeadingGroups+1)
	for count := 0; count < requiredLeadingGroups+1; count++ {
		group, ok := readBraceGroup(source, skipSpaces(source, cursor))
		if !ok {
			return parsedCommandWrapper{}, nil, false
		}
		groups = append(groups, group)
		cursor = group.Next
	}
	content := groups[requiredLeadingGroups]
	return parsedCommandWrapper{
		Start:        start,
		End:          content.Next,
		ContentStart: content.Start,
		ContentEnd:   content.Next,
	}, options, true
}

func expandLeadingAlignment(source string, cursor int, diagram parsedDiagram) (parsedDiagram, bool) {
	segment := source[cursor:diagram.ReplaceStart]
	commandStart, command := lastAlignmentCommand(segment)
	if commandStart < 0 {
		return diagram, false
	}
	commandEnd := commandStart + len(`\`) + len(command)
	if !isOnlyLayoutGap(segment[commandEnd:]) {
		return diagram, false
	}
	replaceStart := cursor + commandStart
	if lineStart := lastLineStart(segment, commandStart); isOnlyLayoutGap(segment[lineStart:commandStart]) {
		replaceStart = cursor + lineStart
	}
	diagram.ReplaceStart = replaceStart
	diagram = withAlignment(diagram, alignmentForCommand(command))
	return diagram, true
}

func lastAlignmentCommand(value string) (int, string) {
	bestStart := -1
	bestCommand := ""
	for _, command := range []string{"centering", "raggedright", "raggedleft"} {
		token := `\` + command
		search := 0
		for search < len(value) {
			index := strings.Index(value[search:], token)
			if index < 0 {
				break
			}
			index += search
			if hasCommandBoundary(value, index, token) && index > bestStart {
				bestStart = index
				bestCommand = command
			}
			search = index + len(token)
		}
	}
	return bestStart, bestCommand
}

func withAlignment(diagram parsedDiagram, alignment string) parsedDiagram {
	alignment = normalizeAlignment(alignment)
	if diagram.Layout.Alignment == "" && alignment != "" {
		diagram.Layout.Alignment = alignment
	}
	return diagram
}

func normalizeAlignment(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "center", "flushleft", "flushright":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func alignmentForCommand(command string) string {
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "centering":
		return "center"
	case "raggedright":
		return "flushleft"
	case "raggedleft":
		return "flushright"
	default:
		return ""
	}
}

func makeboxAlignment(rawOptions []string) string {
	options := strings.ToLower(strings.Join(rawOptions, ""))
	switch {
	case strings.Contains(options, "[r]"):
		return "flushright"
	case strings.Contains(options, "[l]"):
		return "flushleft"
	case strings.Contains(options, "[c]") || strings.Contains(options, `\linewidth`) || strings.Contains(options, `\textwidth`) || strings.Contains(options, `\columnwidth`):
		return "center"
	default:
		return ""
	}
}

func adjustboxAlignment(settings string) string {
	for _, value := range strings.Split(settings, ",") {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "center", "centered", "valign=c":
			return "center"
		case "left", "flushleft":
			return "flushleft"
		case "right", "flushright":
			return "flushright"
		}
	}
	return ""
}

func normalizeEnvironment(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tikzpicture":
		return "tikzpicture", true
	case "tikzcd":
		return "tikzcd", true
	case "axis":
		return "axis", true
	case "pspicture":
		return "pspicture", true
	case "cd":
		return "amscd", true
	case "picture":
		return "picture", true
	case "forest":
		return "forest", true
	case "circuitikz":
		return "circuitikz", true
	default:
		return "", false
	}
}

func readEnvironmentArguments(source string, name string, start int) (string, int) {
	if strings.EqualFold(strings.TrimSpace(name), "pspicture") {
		return readPSTricksArguments(source, start)
	}
	if strings.EqualFold(strings.TrimSpace(name), "picture") {
		return readPictureArguments(source, start)
	}
	group, ok := readBracketGroup(source, start)
	if !ok {
		return "", skipSpaces(source, start)
	}
	return group.Value, group.Next
}

func readPSTricksArguments(source string, start int) (string, int) {
	cursor := skipSpaces(source, start)
	optionsStart := cursor
	if group, ok := readBracketGroup(source, cursor); ok {
		cursor = group.Next
	}
	for count := 0; count < 2; count++ {
		group, ok := readDelimitedGroup(source, skipSpaces(source, cursor), '(', ')')
		if !ok {
			break
		}
		cursor = group.Next
	}
	return strings.TrimSpace(source[optionsStart:cursor]), cursor
}

func readPictureArguments(source string, start int) (string, int) {
	cursor := skipSpaces(source, start)
	optionsStart := cursor
	for count := 0; count < 2; count++ {
		group, ok := readDelimitedGroup(source, skipSpaces(source, cursor), '(', ')')
		if !ok {
			break
		}
		cursor = group.Next
	}
	return strings.TrimSpace(source[optionsStart:cursor]), cursor
}

type groupResult struct {
	Start int
	Raw   string
	Value string
	Next  int
}

func readBracketGroup(source string, start int) (groupResult, bool) {
	return readDelimitedGroup(source, skipSpaces(source, start), '[', ']')
}

func readBraceGroup(source string, start int) (groupResult, bool) {
	return readDelimitedGroup(source, skipSpaces(source, start), '{', '}')
}

func readDelimitedGroup(source string, start int, open byte, close byte) (groupResult, bool) {
	cursor := start
	if cursor >= len(source) || source[cursor] != open {
		return groupResult{}, false
	}
	depth := 0
	for index := cursor; index < len(source); index++ {
		switch source[index] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return groupResult{
					Start: cursor,
					Raw:   source[cursor : index+1],
					Value: source[cursor+1 : index],
					Next:  index + 1,
				}, true
			}
		case '\\':
			if index+1 < len(source) {
				index++
			}
		}
	}
	return groupResult{}, false
}

func findMatchingEnvironmentEnd(source string, start int, name string) int {
	beginToken := `\begin{` + name + `}`
	endToken := `\end{` + name + `}`
	depth := 1
	cursor := start
	for cursor < len(source) {
		nextBegin := indexUncommentedToken(source, cursor, beginToken)
		nextEnd := indexUncommentedToken(source, cursor, endToken)
		if nextEnd < 0 {
			return -1
		}
		if nextBegin >= 0 && nextBegin < nextEnd {
			depth++
			cursor = nextBegin + len(beginToken)
			continue
		}
		depth--
		end := nextEnd
		if depth == 0 {
			return end
		}
		cursor = end + len(endToken)
	}
	return -1
}

func findMatchingSchemeStop(source string, start int) int {
	depth := 1
	cursor := start
	for cursor < len(source) {
		nextStart := indexUncommentedToken(source, cursor, `\schemestart`)
		nextStop := indexUncommentedToken(source, cursor, `\schemestop`)
		if nextStop < 0 {
			return -1
		}
		if nextStart >= 0 && nextStart < nextStop {
			depth++
			cursor = nextStart + len(`\schemestart`)
			continue
		}
		depth--
		stop := nextStop
		if depth == 0 {
			return stop
		}
		cursor = stop + len(`\schemestop`)
	}
	return -1
}

func replaceBoundsWithDisplayMath(source string, cursor int, start int, end int) (int, int) {
	replaceStart, wrap := displayMathOpenBefore(source, cursor, start)
	replaceEnd := end
	if wrap != "" {
		if closeEnd, ok := displayMathCloseEndAfter(source, end, wrap); ok {
			replaceEnd = closeEnd
		}
	}
	return replaceStart, replaceEnd
}

func displayMathCloseEndAfter(source string, end int, wrap string) (int, bool) {
	if closeEnd, ok := displayMathCloseAt(source, skipSpaces(source, end), wrap); ok {
		return closeEnd, true
	}
	cursor := skipSpaces(source, end)
	for cursor < len(source) && isDisplayMathTrailingPunctuation(source[cursor]) {
		cursor = skipSpaces(source, cursor+1)
	}
	return displayMathCloseAt(source, cursor, wrap)
}

func displayMathCloseAt(source string, start int, wrap string) (int, bool) {
	switch wrap {
	case "$$":
		if strings.HasPrefix(source[start:], "$$") {
			return start + len("$$"), true
		}
	case `\[`:
		if strings.HasPrefix(source[start:], `\]`) {
			return start + len(`\]`), true
		}
	}
	return 0, false
}

func isDisplayMathTrailingPunctuation(ch byte) bool {
	switch ch {
	case '.', ',', ';', ':':
		return true
	default:
		return false
	}
}

func displayMathOpenBefore(source string, cursor int, start int) (int, string) {
	index := start
	for index > cursor && isSpace(source[index-1]) {
		index--
	}
	if index >= cursor+2 && source[index-2:index] == "$$" {
		return index - 2, "$$"
	}
	if index >= cursor+2 && source[index-2:index] == `\[` {
		return index - 2, `\[`
	}
	return start, ""
}

func previousNonSpaceIndex(source string, cursor int, index int) int {
	if index > len(source) {
		index = len(source)
	}
	for index > cursor && isSpace(source[index-1]) {
		index--
	}
	return index - 1
}

func lastLineStart(value string, before int) int {
	if before < 0 {
		return 0
	}
	if before > len(value) {
		before = len(value)
	}
	index := strings.LastIndexByte(value[:before], '\n')
	if index < 0 {
		return 0
	}
	return index + 1
}

func readCommandName(source string, start int) (string, int, bool) {
	if start < 0 || start+1 >= len(source) || source[start] != '\\' {
		return "", start, false
	}
	index := start + 1
	if !unicode.IsLetter(rune(source[index])) {
		return source[index : index+1], index + 1, true
	}
	nameStart := index
	for index < len(source) && unicode.IsLetter(rune(source[index])) {
		index++
	}
	if index < len(source) && source[index] == '*' {
		index++
	}
	return source[nameStart:index], index, true
}

func isOnlyLayoutGap(value string) bool {
	cursor := 0
	for cursor < len(value) {
		cursor = skipSpaces(value, cursor)
		if cursor >= len(value) {
			return true
		}
		switch value[cursor] {
		case '%':
			cursor++
			for cursor < len(value) && value[cursor] != '\n' {
				cursor++
			}
			continue
		case '~', '{', '}':
			cursor++
			continue
		case '\\':
			if strings.HasPrefix(value[cursor:], `\\`) {
				cursor += len(`\\`)
				if cursor < len(value) && value[cursor] == '*' {
					cursor++
				}
				if group, ok := readBracketGroup(value, cursor); ok {
					cursor = group.Next
				}
				continue
			}
			if cursor+1 < len(value) && isLayoutControlSymbol(value[cursor+1]) {
				cursor += 2
				continue
			}
			command, next, ok := readCommandName(value, cursor)
			if !ok {
				return false
			}
			switch {
			case isLayoutNoArgCommand(command):
				cursor = next
			case isLayoutOptionalCommand(command):
				cursor = skipOptionalLayoutArguments(value, next)
			case isLayoutBraceArgCommand(command):
				cursor = skipOptionalLayoutArguments(value, next)
				group, ok := readBraceGroup(value, cursor)
				if !ok {
					return false
				}
				cursor = group.Next
			default:
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isLayoutControlSymbol(ch byte) bool {
	switch ch {
	case ',', ';', ':', '!', ' ', '/':
		return true
	default:
		return false
	}
}

func isLayoutNoArgCommand(command string) bool {
	switch command {
	case "tiny", "scriptsize", "footnotesize", "small", "normalsize", "large", "Large", "LARGE", "huge", "Huge",
		"centering", "raggedright", "raggedleft",
		"hfill", "hfil", "hss", "vfill",
		"smallskip", "medskip", "bigskip",
		"par", "noindent", "newline",
		"quad", "qquad", "enspace", "enskip",
		"thinspace", "medspace", "thickspace",
		"negthinspace", "negmedspace", "negthickspace",
		"newpage", "clearpage", "cleardoublepage",
		"relax", "leavevmode":
		return true
	default:
		return false
	}
}

func isLayoutOptionalCommand(command string) bool {
	switch command {
	case "linebreak", "pagebreak", "nopagebreak":
		return true
	default:
		return false
	}
}

func isLayoutBraceArgCommand(command string) bool {
	switch command {
	case "hspace", "hspace*", "vspace", "vspace*", "addvspace":
		return true
	default:
		return false
	}
}

func skipOptionalLayoutArguments(source string, cursor int) int {
	for {
		group, ok := readBracketGroup(source, cursor)
		if !ok {
			return skipSpaces(source, cursor)
		}
		cursor = group.Next
	}
}

func skipSpaces(source string, start int) int {
	cursor := start
	for cursor < len(source) && isSpace(source[cursor]) {
		cursor++
	}
	return cursor
}

func isSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n'
}

func hasCommandBoundary(source string, start int, command string) bool {
	if !strings.HasPrefix(source[start:], command) {
		return false
	}
	next := start + len(command)
	if next >= len(source) {
		return true
	}
	return !unicode.IsLetter(rune(source[next]))
}
