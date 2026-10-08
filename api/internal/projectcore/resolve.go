package projectcore

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	importLikePattern                = regexp.MustCompile(`\\(import|subimport|inputfrom|subinputfrom|includefrom|subincludefrom)\s*\{([^}]*)\}\s*\{([^}]*)\}`)
	bracedInputPattern               = regexp.MustCompile(`\\(input|include|subfile|subfileinclude)\s*\{([^}]+)\}`)
	spaceInputPattern                = regexp.MustCompile(`\\(input|include|subfile|subfileinclude)\s+([^\s%{}]+)`)
	includeGraphicsPattern           = regexp.MustCompile(`\\((?:adj)?includegraphics)\*?(?:\[([^\]]*)\])?\s*\{([^}]+)\}`)
	legacyGraphicsPattern            = regexp.MustCompile(`\\(epsfig|psfig)\s*\{([^}]*)\}`)
	epsfboxPattern                   = regexp.MustCompile(`\\epsfbox\s*\{([^}]+)\}`)
	bibliographyPattern              = regexp.MustCompile(`\\bibliography\s*\{([^}]+)\}`)
	addBibResourcePattern            = regexp.MustCompile(`\\addbibresource(?:\s*\[[^\]]*\])?\s*\{([^}]+)\}`)
	bibliographyStylePattern         = regexp.MustCompile(`\\bibliographystyle\s*\{([^}]+)\}`)
	graphicsExtensionPattern         = regexp.MustCompile(`\.[A-Za-z0-9]{1,12}$`)
	generatedListSetupPattern        = regexp.MustCompile(`(?m)^[ \t]*\\(?:makeindex|makeglossaries|makenomenclature)\b[^\n]*(?:\n|$)`)
	generatedListSingleEntryPattern  = regexp.MustCompile(`\\(?:index|glsadd)\b(?:\[[^\]]*\])?\{[^{}\n]*(?:\{[^{}\n]*\}[^{}\n]*)*\}`)
	generatedListAllGlossaryPattern  = regexp.MustCompile(`\\glsaddall\b(?:\[[^\]]*\])?`)
	generatedListNomenclaturePattern = regexp.MustCompile(`\\nomenclature\b(?:\[[^\]]*\])?\{[^{}\n]*(?:\{[^{}\n]*\}[^{}\n]*)*\}\{[^{}\n]*(?:\{[^{}\n]*\}[^{}\n]*)*\}`)
)

type generatedListSpec struct {
	listType           string
	command            string
	label              string
	pattern            *regexp.Regexp
	linePattern        *regexp.Regexp
	extensions         []string
	fallbackExtensions []string
	emptyEnvironment   string
}

type resolveContext struct {
	inputPaths []string
	graphics   graphicsOptions
	jobName    string
	macros     map[string]string
}

type graphicsOptions struct {
	paths      []string
	extensions []string
}

func ResolveProject(files []File, mainFile string) Resolution {
	byPath := make(map[string]File, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	jobName := strings.TrimSuffix(path.Base(mainFile), path.Ext(mainFile))
	if jobName == "" {
		jobName = path.Base(mainFile)
	}
	macros := collectReferenceMacros(files, jobName)
	ctx := resolveContext{
		inputPaths: collectInputSearchPaths(files, macros, jobName),
		graphics:   collectGraphicsOptions(files, macros, jobName),
		jobName:    jobName,
		macros:     macros,
	}

	var diagnostics []Diagnostic
	var includes []Reference
	resolved := expandFile(mainFile, byPath, &diagnostics, &includes, nil, 0, ctx)
	generatedLists := createGeneratedListInventory(resolved, mainFile, byPath)
	for _, ref := range generatedLists.Missing {
		diagnostics = append(diagnostics, generatedListDiagnostic("warning", "project.generated_list.missing", fmt.Sprintf("missing generated %s file: %s", ref.Label, strings.Join(ref.ExpectedExtensions, " or ")), ref))
	}
	resolved = applyGeneratedListFiles(resolved, mainFile, byPath)
	analysis := stripDocumentWrapper(resolved)
	bibliography := createBibliographyInventory(resolved, mainFile, byPath, ctx)
	assetInventory := createAssetInventory(files, mainFile, byPath, ctx)
	for _, ref := range bibliography.Missing {
		diagnostics = append(diagnostics, referenceDiagnostic("error", "project.bibliography.missing", fmt.Sprintf("missing bibliography file: %s", ref.RawRef), ref))
	}
	for _, ref := range assetInventory.Missing {
		diagnostics = append(diagnostics, referenceDiagnostic("error", "project.asset.missing", fmt.Sprintf("missing graphics asset: %s", ref.RawRef), ref))
	}

	return Resolution{
		Source:         analysis,
		AnalysisSource: analysis,
		ResolvedSource: resolved,
		Includes:       includes,
		AssetInventory: assetInventory,
		Bibliography:   bibliography,
		GeneratedLists: generatedLists,
		Diagnostics:    diagnostics,
	}
}

func expandFile(filePath string, byPath map[string]File, diagnostics *[]Diagnostic, includes *[]Reference, stack []string, depth int, ctx resolveContext) string {
	if depth > 32 {
		*diagnostics = append(*diagnostics, errorDiagnostic("project.include.depth", fmt.Sprintf("include/input nesting is too deep: %s", filePath), filePath, 0))
		return ""
	}
	if containsString(stack, filePath) {
		chain := append(append([]string{}, stack...), filePath)
		*diagnostics = append(*diagnostics, errorDiagnostic("project.include.cycle", fmt.Sprintf("cyclic include/input: %s", strings.Join(chain, " -> ")), filePath, 0))
		return ""
	}
	file, ok := byPath[filePath]
	if !ok {
		*diagnostics = append(*diagnostics, errorDiagnostic("project.include.missing", fmt.Sprintf("missing file: %s", filePath), filePath, 0))
		return ""
	}
	if file.Encoding == "base64" {
		*diagnostics = append(*diagnostics, errorDiagnostic("project.include.binary", fmt.Sprintf("cannot expand binary file as TeX: %s", filePath), filePath, 0))
		return ""
	}

	nextStack := append(append([]string{}, stack...), filePath)
	lines := strings.Split(strings.ReplaceAll(file.Body, "\r\n", "\n"), "\n")
	expanded := make([]string, 0, len(lines))
	for index, line := range lines {
		expanded = append(expanded, expandLine(line, filePath, byPath, diagnostics, includes, nextStack, depth, ctx, index+1))
	}
	return strings.Join(expanded, "\n")
}

func expandLine(line string, currentPath string, byPath map[string]File, diagnostics *[]Diagnostic, includes *[]Reference, stack []string, depth int, ctx resolveContext, lineNumber int) string {
	code, comment := splitLatexComment(line)
	code = expandImportLike(code, currentPath, byPath, diagnostics, includes, stack, depth, ctx, lineNumber)
	code = expandInputLike(code, bracedInputPattern, currentPath, byPath, diagnostics, includes, stack, depth, ctx, lineNumber)
	code = expandInputLike(code, spaceInputPattern, currentPath, byPath, diagnostics, includes, stack, depth, ctx, lineNumber)
	return code + comment
}

func expandImportLike(code string, currentPath string, byPath map[string]File, diagnostics *[]Diagnostic, includes *[]Reference, stack []string, depth int, ctx resolveContext, lineNumber int) string {
	return replaceRegexp(code, importLikePattern, func(groups []string) string {
		command, rawDir, rawFile := groups[1], groups[2], groups[3]
		rawRef := rawDir + rawFile
		resolved := resolveReference(currentPath, rawRef, byPath, inputExtensionsForCommand(command), ctx)
		ref := Reference{Kind: "include", Command: command, RawRef: rawRef, Path: resolved, SourceFile: currentPath, Line: lineNumber, Resolved: resolved != ""}
		*includes = append(*includes, ref)
		if resolved == "" {
			*diagnostics = append(*diagnostics, referenceDiagnostic("error", "project.include.missing", fmt.Sprintf("cannot resolve \\%s{%s}{%s}", command, rawDir, rawFile), ref))
			return ""
		}
		return "\n" + unwrapIncludedDocument(expandFile(resolved, byPath, diagnostics, includes, stack, depth+1, ctx), command) + "\n"
	})
}

func expandInputLike(code string, pattern *regexp.Regexp, currentPath string, byPath map[string]File, diagnostics *[]Diagnostic, includes *[]Reference, stack []string, depth int, ctx resolveContext, lineNumber int) string {
	return replaceRegexp(code, pattern, func(groups []string) string {
		command, rawRef := groups[1], groups[2]
		resolved := resolveReference(currentPath, rawRef, byPath, inputExtensionsForCommand(command), ctx)
		ref := Reference{Kind: "include", Command: command, RawRef: rawRef, Path: resolved, SourceFile: currentPath, Line: lineNumber, Resolved: resolved != ""}
		*includes = append(*includes, ref)
		if resolved == "" {
			*diagnostics = append(*diagnostics, referenceDiagnostic("error", "project.include.missing", fmt.Sprintf("cannot resolve \\%s{%s}", command, rawRef), ref))
			return ""
		}
		return "\n" + unwrapIncludedDocument(expandFile(resolved, byPath, diagnostics, includes, stack, depth+1, ctx), command) + "\n"
	})
}

func createAssetInventory(files []File, mainFile string, byPath map[string]File, ctx resolveContext) AssetInventory {
	references := make([]Reference, 0)
	visited := make(map[string]bool)
	collectAssetReferencesFromFile(mainFile, byPath, &references, visited, 0, ctx)

	referencedPaths := make(map[string]bool, len(references))
	for _, ref := range references {
		if ref.Resolved && ref.Path != "" {
			referencedPaths[ref.Path] = true
		}
	}

	assets := make([]Asset, 0)
	for _, file := range files {
		if fileKind(file.Path) != "asset" {
			continue
		}
		asset := Asset{
			Path:       file.Path,
			MIME:       firstNonEmpty(file.MIME, mimeForPath(file.Path)),
			Encoding:   file.Encoding,
			Referenced: referencedPaths[file.Path],
		}
		for _, ref := range references {
			if ref.Path == file.Path {
				asset.References = append(asset.References, ref)
			}
		}
		assets = append(assets, asset)
	}

	missing := make([]Reference, 0)
	for _, ref := range references {
		if !ref.Resolved {
			missing = append(missing, ref)
		}
	}
	unused := make([]Asset, 0)
	for _, asset := range assets {
		if !asset.Referenced {
			unused = append(unused, asset)
		}
	}

	return AssetInventory{
		References:         references,
		Assets:             assets,
		Missing:            missing,
		Unused:             unused,
		GraphicsPaths:      append([]string{}, ctx.graphics.paths...),
		GraphicsExtensions: append([]string{}, ctx.graphics.extensions...),
	}
}

func collectAssetReferencesFromFile(filePath string, byPath map[string]File, references *[]Reference, visited map[string]bool, depth int, ctx resolveContext) {
	if depth > 32 || visited[filePath] {
		return
	}
	visited[filePath] = true
	file, ok := byPath[filePath]
	if !ok || file.Encoding == "base64" {
		return
	}
	lines := strings.Split(strings.ReplaceAll(file.Body, "\r\n", "\n"), "\n")
	for index, line := range lines {
		lineNumber := index + 1
		code, _ := splitLatexComment(line)
		for _, match := range includeGraphicsPattern.FindAllStringSubmatch(code, -1) {
			rawRef := match[2]
			if len(match) > 3 {
				rawRef = match[3]
			}
			resolved := resolveAssetReference(filePath, rawRef, byPath, ctx)
			command := "includegraphics"
			if len(match) > 1 && strings.TrimSpace(match[1]) != "" {
				command = match[1]
			}
			*references = append(*references, Reference{Kind: "asset", Command: command, RawRef: rawRef, Path: resolved, SourceFile: filePath, Line: lineNumber, Resolved: resolved != ""})
		}
		for _, match := range legacyGraphicsPattern.FindAllStringSubmatch(code, -1) {
			command, rawOptions := match[1], match[2]
			rawRef := legacyGraphicsOptionValue(rawOptions)
			resolved := ""
			if rawRef != "" {
				resolved = resolveAssetReference(filePath, rawRef, byPath, ctx)
			}
			*references = append(*references, Reference{Kind: "asset", Command: command, RawRef: firstNonEmpty(rawRef, rawOptions), Path: resolved, SourceFile: filePath, Line: lineNumber, Resolved: resolved != ""})
		}
		for _, match := range epsfboxPattern.FindAllStringSubmatch(code, -1) {
			rawRef := match[1]
			resolved := resolveAssetReference(filePath, rawRef, byPath, ctx)
			*references = append(*references, Reference{Kind: "asset", Command: "epsfbox", RawRef: rawRef, Path: resolved, SourceFile: filePath, Line: lineNumber, Resolved: resolved != ""})
		}
		collectIncludedReferences(code, bracedInputPattern, filePath, byPath, references, visited, depth, ctx)
		collectIncludedReferences(code, spaceInputPattern, filePath, byPath, references, visited, depth, ctx)
	}
}

func collectIncludedReferences(code string, pattern *regexp.Regexp, currentPath string, byPath map[string]File, references *[]Reference, visited map[string]bool, depth int, ctx resolveContext) {
	for _, match := range pattern.FindAllStringSubmatch(code, -1) {
		if len(match) < 3 {
			continue
		}
		resolved := resolveReference(currentPath, match[2], byPath, inputExtensionsForCommand(match[1]), ctx)
		if resolved != "" {
			collectAssetReferencesFromFile(resolved, byPath, references, visited, depth+1, ctx)
		}
	}
}

func createGeneratedListInventory(source string, mainFile string, byPath map[string]File) GeneratedListInventory {
	references := make([]GeneratedListReference, 0)
	seen := make(map[string]bool)
	text := stripLatexComments(source)
	for _, spec := range generatedListSpecs() {
		for _, match := range spec.pattern.FindAllStringIndex(text, -1) {
			file, ok := resolveGeneratedListFile(mainFile, byPath, spec.extensions, spec.fallbackExtensions)
			ref := GeneratedListReference{
				Kind:               "generated-list",
				ListType:           spec.listType,
				Command:            spec.command,
				Label:              spec.label,
				Resolved:           ok,
				ExpectedExtensions: append([]string{}, spec.extensions...),
				FallbackExtensions: append([]string{}, spec.fallbackExtensions...),
				SourceFile:         mainFile,
				Line:               lineNumberAt(text, match[0]),
			}
			if ok {
				ref.Path = file.Path
			}
			key := strings.Join([]string{ref.ListType, ref.Command, ref.Path}, "\x00")
			if seen[key] {
				continue
			}
			seen[key] = true
			references = append(references, ref)
		}
	}
	missing := make([]GeneratedListReference, 0)
	for _, ref := range references {
		if !ref.Resolved {
			missing = append(missing, ref)
		}
	}
	return GeneratedListInventory{References: references, Missing: missing}
}

func applyGeneratedListFiles(source string, mainFile string, byPath map[string]File) string {
	text := generatedListSetupPattern.ReplaceAllString(source, "")
	text = generatedListNomenclaturePattern.ReplaceAllString(text, "")
	text = generatedListSingleEntryPattern.ReplaceAllString(text, "")
	text = generatedListAllGlossaryPattern.ReplaceAllString(text, "")
	for _, spec := range generatedListSpecs() {
		text = spec.linePattern.ReplaceAllStringFunc(text, func(match string) string {
			file, ok := resolveGeneratedListFile(mainFile, byPath, spec.extensions, spec.fallbackExtensions)
			if !ok || file.Encoding == "base64" {
				return fmt.Sprintf(`\begin{%s}`+"\n"+`\end{%s}`, spec.emptyEnvironment, spec.emptyEnvironment)
			}
			return trimEnvironmentBody(file.Body)
		})
	}
	return text
}

func generatedListSpecs() []generatedListSpec {
	return []generatedListSpec{
		{
			listType:           "index",
			command:            "printindex",
			label:              "index",
			pattern:            regexp.MustCompile(`\\printindex\b(?:\s*\[[^\]]*\])?`),
			linePattern:        regexp.MustCompile(`(?m)^[ \t]*\\printindex(?:\s*\[[^\]]*\])?[ \t]*$`),
			extensions:         []string{".ind"},
			fallbackExtensions: []string{".idx"},
			emptyEnvironment:   "theindex",
		},
		{
			listType:           "glossary",
			command:            "printglossary",
			label:              "glossary",
			pattern:            regexp.MustCompile(`\\printglossar(?:y|ies)\b(?:\s*\[[^\]]*\])?`),
			linePattern:        regexp.MustCompile(`(?m)^[ \t]*\\printglossar(?:y|ies)(?:\s*\[[^\]]*\])?[ \t]*$`),
			extensions:         []string{".gls"},
			fallbackExtensions: []string{".glo"},
			emptyEnvironment:   "theglossary",
		},
		{
			listType:           "nomenclature",
			command:            "printnomenclature",
			label:              "nomenclature",
			pattern:            regexp.MustCompile(`\\printnomenclature\b(?:\s*\[[^\]]*\])?`),
			linePattern:        regexp.MustCompile(`(?m)^[ \t]*\\printnomenclature(?:\s*\[[^\]]*\])?[ \t]*$`),
			extensions:         []string{".nls"},
			fallbackExtensions: []string{".nlo"},
			emptyEnvironment:   "thenomenclature",
		},
	}
}

func resolveGeneratedListFile(mainFile string, byPath map[string]File, extensions []string, fallbackExtensions []string) (File, bool) {
	mainPath, ok := CleanProjectPath(mainFile)
	if !ok || mainPath == "" {
		return File{}, false
	}
	ownerDir := path.Dir(mainPath)
	if ownerDir == "." {
		ownerDir = ""
	}
	mainBase := strings.TrimSuffix(mainPath, path.Ext(mainPath))
	baseName := path.Base(mainBase)
	candidates := make([]string, 0)
	for _, ext := range extensions {
		candidates = append(candidates, mainBase+ext)
		if ownerDir != "" {
			candidates = append(candidates, ownerDir+"/"+baseName+ext)
		}
		candidates = append(candidates, baseName+ext)
	}
	for _, candidate := range candidates {
		cleaned, ok := CleanProjectPath(candidate)
		if !ok {
			continue
		}
		if file, exists := byPath[cleaned]; exists && file.Encoding != "base64" {
			return file, true
		}
	}
	wanted := make(map[string]bool)
	for _, ext := range append(append([]string{}, extensions...), fallbackExtensions...) {
		wanted[strings.ToLower(ext)] = true
	}
	paths := make([]string, 0, len(byPath))
	for filePath := range byPath {
		paths = append(paths, filePath)
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		if !wanted[strings.ToLower(path.Ext(filePath))] {
			continue
		}
		file := byPath[filePath]
		if file.Encoding != "base64" {
			return file, true
		}
	}
	return File{}, false
}

func trimEnvironmentBody(value string) string {
	return strings.TrimRight(strings.TrimLeft(value, "\r\n\t "), "\r\n\t ")
}

func collectReferenceMacros(files []File, jobName string) map[string]string {
	macros := make(map[string]string)
	for _, file := range files {
		if file.Encoding == "base64" || (fileKind(file.Path) != "tex" && fileKind(file.Path) != "style") {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(file.Body, "\r\n", "\n"), "\n")
		for _, line := range lines {
			code, _ := splitLatexComment(line)
			collectReferenceMacrosFromCode(code, macros, jobName)
		}
	}
	return macros
}

// CollectMathMacros returns safe, zero-argument TeX macros declared by a
// project. Unlike reference macros, math definitions may contain nested brace
// groups such as \mathcal{E}.
func CollectMathMacros(files []File) map[string]string {
	macros := make(map[string]string)
	for _, file := range files {
		if file.Encoding == "base64" || (fileKind(file.Path) != "tex" && fileKind(file.Path) != "style") {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(file.Body, "\r\n", "\n"), "\n")
		for _, line := range lines {
			code, _ := splitLatexComment(line)
			collectMathMacrosFromCode(code, macros)
		}
	}
	return macros
}

func collectMathMacrosFromCode(code string, macros map[string]string) {
	for index := 0; index < len(code); {
		nextSlash := strings.IndexByte(code[index:], '\\')
		if nextSlash < 0 {
			return
		}
		index += nextSlash
		token, next := readControlSequence(code, index)
		if token == `\DeclareMathOperator` {
			index = next
			if index < len(code) && code[index] == '*' {
				index++
			}
			name, afterName, ok := readMacroArgument(code, index)
			if !ok {
				continue
			}
			value, afterValue, ok := readBraceGroup(code, skipSpaces(code, afterName))
			if !ok || !safeMathMacroValue(value) || strings.TrimSpace(value) == "" {
				continue
			}
			macros[name] = `\operatorname{` + strings.TrimSpace(value) + `}`
			index = afterValue
			continue
		}
		if token != `\newcommand` && token != `\renewcommand` && token != `\providecommand` && token != `\DeclareRobustCommand` {
			index = next
			continue
		}
		cursorStart := next
		if cursorStart < len(code) && code[cursorStart] == '*' {
			cursorStart++
		}
		macro, cursor, ok := readMacroArgument(code, cursorStart)
		if !ok {
			index = next
			continue
		}
		cursor = skipSpaces(code, cursor)
		if cursor < len(code) && code[cursor] == '[' {
			argumentCount, afterCount, countOK := readBracketGroup(code, cursor)
			if !countOK || (strings.TrimSpace(argumentCount) != "" && strings.TrimSpace(argumentCount) != "0") {
				index = next
				continue
			}
			cursor = skipSpaces(code, afterCount)
		}
		value, afterValue, valueOK := readBraceGroup(code, cursor)
		if valueOK && safeMathMacroValue(value) {
			macros[macro] = value
			index = afterValue
			continue
		}
		index = next
	}
}

func safeMathMacroValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "#\r\n%") {
		return false
	}
	lower := strings.ToLower(value)
	for _, blocked := range []string{`\input`, `\include`, `\write`, `\openout`, `\read`, `\csname`, `\catcode`} {
		if strings.Contains(lower, blocked) {
			return false
		}
	}
	return true
}

func collectReferenceMacrosFromCode(code string, macros map[string]string, jobName string) {
	for index := 0; index < len(code); {
		nextSlash := strings.IndexByte(code[index:], '\\')
		if nextSlash < 0 {
			return
		}
		index += nextSlash
		token, next := readControlSequence(code, index)
		switch token {
		case `\newcommand`, `\renewcommand`, `\providecommand`, `\DeclareRobustCommand`:
			cursorStart := next
			if cursorStart < len(code) && code[cursorStart] == '*' {
				cursorStart++
			}
			macro, cursor, ok := readMacroArgument(code, cursorStart)
			if !ok {
				index = next
				continue
			}
			cursor = skipSpaces(code, cursor)
			if cursor < len(code) && code[cursor] == '[' {
				argumentCount, afterCount, ok := readBracketGroup(code, cursor)
				if !ok {
					index = next
					continue
				}
				if strings.TrimSpace(argumentCount) != "" && strings.TrimSpace(argumentCount) != "0" {
					index = afterCount
					continue
				}
				cursor = skipSpaces(code, afterCount)
			}
			value, afterValue, ok := readBraceGroup(code, cursor)
			if ok && safeReferenceMacroValue(value) {
				macros[macro] = expandReferenceMacros(value, resolveContext{jobName: jobName, macros: macros})
				index = afterValue
				continue
			}
		case `\def`, `\gdef`, `\xdef`, `\edef`:
			macro, cursor, ok := readBareMacroName(code, next)
			if !ok {
				index = next
				continue
			}
			cursor = skipSpaces(code, cursor)
			value, afterValue, ok := readBraceGroup(code, cursor)
			if ok && safeReferenceMacroValue(value) {
				macros[macro] = expandReferenceMacros(value, resolveContext{jobName: jobName, macros: macros})
				index = afterValue
				continue
			}
		}
		index = next
	}
}

func readMacroArgument(source string, start int) (string, int, bool) {
	cursor := skipSpaces(source, start)
	if cursor < len(source) && source[cursor] == '{' {
		value, next, ok := readBraceGroup(source, cursor)
		if !ok {
			return "", start, false
		}
		cleaned := strings.TrimSpace(value)
		macro, macroNext, ok := readBareMacroName(cleaned, 0)
		if !ok || macroNext != len(cleaned) {
			return "", next, false
		}
		return macro, next, true
	}
	return readBareMacroName(source, cursor)
}

func readBareMacroName(source string, start int) (string, int, bool) {
	cursor := skipSpaces(source, start)
	if cursor >= len(source) || source[cursor] != '\\' {
		return "", start, false
	}
	token, next := readControlSequence(source, cursor)
	if len(token) <= 1 || token == `\jobname` {
		return "", next, false
	}
	return token, next, true
}

func safeReferenceMacroValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "{}#\r\n%") {
		return false
	}
	lower := strings.ToLower(value)
	for _, blocked := range []string{`\input`, `\include`, `\write`, `\openout`, `\read`} {
		if strings.Contains(lower, blocked) {
			return false
		}
	}
	return true
}

func createBibliographyInventory(source string, mainFile string, byPath map[string]File, ctx resolveContext) BibliographyInventory {
	references := make([]Reference, 0)
	styles := make([]Reference, 0)
	for _, match := range bibliographyPattern.FindAllStringSubmatchIndex(source, -1) {
		rawFiles := source[match[2]:match[3]]
		line := lineNumberAt(source, match[0])
		for _, rawRef := range commaListValues(rawFiles) {
			resolved := resolveBibliographyReference(mainFile, rawRef, byPath, ctx)
			references = append(references, Reference{Kind: "bibliography-file", Command: "bibliography", RawRef: rawRef, Path: resolved, SourceFile: mainFile, Line: line, Resolved: resolved != ""})
		}
	}
	for _, match := range addBibResourcePattern.FindAllStringSubmatchIndex(source, -1) {
		rawRef := source[match[2]:match[3]]
		line := lineNumberAt(source, match[0])
		resolved := resolveBibliographyReference(mainFile, rawRef, byPath, ctx)
		references = append(references, Reference{Kind: "bibliography-file", Command: "addbibresource", RawRef: rawRef, Path: resolved, SourceFile: mainFile, Line: line, Resolved: resolved != ""})
	}
	for _, match := range bibliographyStylePattern.FindAllStringSubmatchIndex(source, -1) {
		rawRef := source[match[2]:match[3]]
		line := lineNumberAt(source, match[0])
		resolved := resolveStyleReference(mainFile, rawRef, byPath, ctx)
		styles = append(styles, Reference{Kind: "bibliography-style", Command: "bibliographystyle", RawRef: rawRef, Path: resolved, SourceFile: mainFile, Line: line, Resolved: resolved != ""})
	}

	missing := make([]Reference, 0)
	bibPaths := make([]string, 0)
	seenBibPaths := make(map[string]bool)
	for _, ref := range references {
		if ref.Resolved {
			if !seenBibPaths[ref.Path] {
				bibPaths = append(bibPaths, ref.Path)
				seenBibPaths[ref.Path] = true
			}
			continue
		}
		missing = append(missing, ref)
	}
	missingStyles := make([]Reference, 0)
	for _, ref := range styles {
		if !ref.Resolved && strings.Contains(ref.RawRef, "/") {
			missingStyles = append(missingStyles, ref)
		}
	}
	sort.Strings(bibPaths)

	return BibliographyInventory{
		References:    references,
		Missing:       missing,
		BibPaths:      bibPaths,
		Styles:        styles,
		MissingStyles: missingStyles,
	}
}

func collectInputSearchPaths(files []File, macros map[string]string, jobName string) []string {
	seen := make(map[string]bool)
	paths := make([]string, 0)
	add := func(ownerPath, rawPath string) {
		rawPath = expandReferenceMacros(strings.TrimSpace(rawPath), resolveContext{jobName: jobName, macros: macros})
		if rawPath == "" {
			return
		}
		ownerDir := path.Dir(ownerPath)
		if ownerDir == "." {
			ownerDir = ""
		}
		for _, candidate := range []string{rawPath, joinProjectPath(ownerDir, rawPath)} {
			prefix, ok := normalizeSearchPrefix(candidate)
			if !ok || seen[prefix] {
				continue
			}
			seen[prefix] = true
			paths = append(paths, prefix)
		}
	}

	for _, file := range files {
		if file.Encoding == "base64" || (fileKind(file.Path) != "tex" && fileKind(file.Path) != "style") {
			continue
		}
		for _, inputPath := range parseInputPathDefinitions(file.Body) {
			add(file.Path, inputPath)
		}
	}
	return paths
}

func collectGraphicsOptions(files []File, macros map[string]string, jobName string) graphicsOptions {
	seenPaths := make(map[string]bool)
	paths := make([]string, 0)
	extensions := make([]string, 0)
	addPath := func(rawPath string) {
		rawPath = expandReferenceMacros(rawPath, resolveContext{jobName: jobName, macros: macros})
		prefix, ok := normalizeSearchPrefix(rawPath)
		if !ok || seenPaths[prefix] {
			return
		}
		seenPaths[prefix] = true
		paths = append(paths, prefix)
	}
	addExtensions := func(values []string) {
		next := make([]string, 0)
		seen := make(map[string]bool)
		for _, value := range values {
			extension := normalizeGraphicsExtension(value)
			if extension == "" || seen[extension] {
				continue
			}
			seen[extension] = true
			next = append(next, extension)
		}
		if len(next) > 0 {
			extensions = next
		}
	}

	for _, file := range files {
		if file.Encoding == "base64" || (fileKind(file.Path) != "tex" && fileKind(file.Path) != "style") {
			continue
		}
		for _, value := range parseGraphicspath(file.Body) {
			addPath(value)
		}
		declared := parseGraphicsExtensions(file.Body)
		if len(declared) > 0 {
			addExtensions(declared)
		}
	}
	if len(extensions) == 0 {
		extensions = defaultGraphicsExtensions()
	}
	return graphicsOptions{paths: paths, extensions: extensions}
}

func parseInputPathDefinitions(source string) []string {
	var paths []string
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`\\(?:def|gdef|xdef|edef)\s*\\input@path\b`),
		regexp.MustCompile(`\\g@addto@macro\s*\\input@path\b`),
	} {
		for _, match := range pattern.FindAllStringIndex(source, -1) {
			if value, _, ok := readBraceGroup(source, skipSpaces(source, match[1])); ok {
				paths = append(paths, parseBraceList(value)...)
			}
		}
	}
	return paths
}

func parseGraphicspath(source string) []string {
	var paths []string
	pattern := regexp.MustCompile(`\\graphicspath\b`)
	for _, match := range pattern.FindAllStringIndex(source, -1) {
		if value, _, ok := readBraceGroup(source, skipSpaces(source, match[1])); ok {
			paths = append(paths, parseBraceList(value)...)
		}
	}
	return paths
}

func parseGraphicsExtensions(source string) []string {
	var extensions []string
	pattern := regexp.MustCompile(`\\DeclareGraphicsExtensions\b`)
	for _, match := range pattern.FindAllStringIndex(source, -1) {
		if value, _, ok := readBraceGroup(source, skipSpaces(source, match[1])); ok {
			extensions = append(extensions, commaListValues(value)...)
		}
	}
	return extensions
}

func resolveReference(currentPath string, rawRef string, byPath map[string]File, extensions []string, ctx resolveContext) string {
	ref, ok := CleanProjectPath(expandReferenceMacros(rawRef, ctx))
	if !ok {
		return ""
	}
	currentDir := path.Dir(currentPath)
	if currentDir == "." {
		currentDir = ""
	}
	withDir := joinProjectPath(currentDir, ref)
	candidates := []string{ref, withDir}
	for _, prefix := range normalizeSearchPrefixes(ctx.inputPaths, "") {
		candidates = append(candidates, prefix+ref)
	}
	if !hasProjectExtension(ref) {
		for _, ext := range extensions {
			candidates = append(candidates, ref+ext, withDir+ext)
			for _, prefix := range normalizeSearchPrefixes(ctx.inputPaths, "") {
				candidates = append(candidates, prefix+ref+ext)
			}
		}
	}
	for _, candidate := range candidates {
		if cleaned, ok := CleanProjectPath(candidate); ok {
			if _, exists := byPath[cleaned]; exists {
				return cleaned
			}
		}
	}
	return ""
}

func resolveAssetReference(currentPath string, rawRef string, byPath map[string]File, ctx resolveContext) string {
	ref, ok := CleanProjectPath(expandReferenceMacros(rawRef, ctx))
	if !ok {
		return ""
	}
	currentDir := path.Dir(currentPath)
	if currentDir == "." {
		currentDir = ""
	}
	if preview := resolvePostScriptPreviewReference(ref, currentDir, byPath, ctx.graphics); preview != "" {
		return preview
	}

	bases := []string{ref, joinProjectPath(currentDir, ref)}
	for _, prefix := range normalizeSearchPrefixes(ctx.graphics.paths, currentDir) {
		bases = append(bases, prefix+ref)
	}
	extensions := []string{""}
	if !hasProjectExtension(ref) {
		extensions = normalizeGraphicsExtensions(ctx.graphics.extensions)
	}
	for _, base := range bases {
		for _, ext := range extensions {
			if cleaned, ok := CleanProjectPath(base + ext); ok {
				if file, exists := byPath[cleaned]; exists && fileKind(file.Path) == "asset" {
					return cleaned
				}
			}
		}
	}
	return ""
}

func resolvePostScriptPreviewReference(ref string, currentDir string, byPath map[string]File, graphics graphicsOptions) string {
	ext := strings.ToLower(path.Ext(ref))
	if ext != ".eps" && ext != ".epsi" && ext != ".ps" {
		return ""
	}
	withoutExt := strings.TrimSuffix(ref, path.Ext(ref))
	bases := []string{withoutExt, joinProjectPath(currentDir, withoutExt)}
	for _, prefix := range normalizeSearchPrefixes(graphics.paths, currentDir) {
		bases = append(bases, prefix+withoutExt)
	}
	for _, base := range bases {
		for _, ext := range []string{".png", ".jpg", ".jpeg", ".svg", ".webp", ".gif"} {
			if cleaned, ok := CleanProjectPath(base + ext); ok {
				if file, exists := byPath[cleaned]; exists && fileKind(file.Path) == "asset" {
					return cleaned
				}
			}
		}
	}
	return ""
}

func resolveBibliographyReference(ownerPath string, rawRef string, byPath map[string]File, ctx resolveContext) string {
	if ctx.jobName == "" {
		ctx.jobName = strings.TrimSuffix(path.Base(ownerPath), path.Ext(ownerPath))
	}
	ref, ok := CleanProjectPath(expandReferenceMacros(rawRef, ctx))
	if !ok {
		return ""
	}
	candidates := resolveProjectFileCandidates(ref, ownerPath, []string{".bib", ".bbl"})
	for _, candidate := range candidates {
		if file, exists := byPath[candidate]; exists && fileKind(file.Path) == "bib" {
			return candidate
		}
	}
	return ""
}

func resolveStyleReference(ownerPath string, rawRef string, byPath map[string]File, ctx resolveContext) string {
	if ctx.jobName == "" {
		ctx.jobName = strings.TrimSuffix(path.Base(ownerPath), path.Ext(ownerPath))
	}
	ref, ok := CleanProjectPath(expandReferenceMacros(rawRef, ctx))
	if !ok {
		return ""
	}
	for _, candidate := range resolveProjectFileCandidates(ref, ownerPath, []string{".bst"}) {
		if file, exists := byPath[candidate]; exists && fileKind(file.Path) == "style" {
			return candidate
		}
	}
	return ""
}

func resolveProjectFileCandidates(rawRef string, ownerPath string, extensions []string) []string {
	ref, ok := CleanProjectPath(rawRef)
	if !ok {
		return nil
	}
	ownerDir := path.Dir(ownerPath)
	if ownerDir == "." {
		ownerDir = ""
	}
	bases := []string{ref, joinProjectPath(ownerDir, ref)}
	candidates := make([]string, 0)
	seen := make(map[string]bool)
	for _, base := range bases {
		if base == "" {
			continue
		}
		addCandidate := func(value string) {
			cleaned, ok := CleanProjectPath(value)
			if !ok || seen[cleaned] {
				return
			}
			seen[cleaned] = true
			candidates = append(candidates, cleaned)
		}
		addCandidate(base)
		if !hasProjectExtension(base) {
			for _, ext := range extensions {
				addCandidate(base + ext)
			}
		}
	}
	return candidates
}

func inputExtensionsForCommand(command string) []string {
	if command == "include" || command == "subfileinclude" {
		return []string{".tex", ".ltx"}
	}
	return []string{".tex", ".ltx", ".sty", ".cls", ".def", ".clo", ".cfg", ".ldf", ".fd", ".bbl", ".ind", ".gls", ".nls", ".txt"}
}

func stripDocumentWrapper(source string) string {
	begin := regexp.MustCompile(`\\begin\{document\}`).FindStringIndex(source)
	if begin == nil {
		return strings.TrimSpace(source)
	}
	bodyStart := begin[1]
	rest := source[bodyStart:]
	end := regexp.MustCompile(`\\end\{document\}`).FindStringIndex(rest)
	if end != nil {
		rest = rest[:end[0]]
	}
	return strings.TrimSpace(rest)
}

func unwrapIncludedDocument(source string, command string) string {
	if command != "subfile" && command != "subfileinclude" && !strings.Contains(source, `\begin{document}`) {
		return source
	}
	begin := regexp.MustCompile(`\\begin\{document\}`).FindStringIndex(source)
	if begin == nil {
		return source
	}
	preamble := source[:begin[0]]
	rest := source[begin[1]:]
	end := regexp.MustCompile(`\\end\{document\}`).FindStringIndex(rest)
	if end != nil {
		rest = rest[:end[0]]
	}
	return strings.TrimSpace(strings.TrimSpace(preamble) + "\n" + strings.TrimSpace(rest))
}

func replaceRegexp(source string, pattern *regexp.Regexp, replace func(groups []string) string) string {
	matches := pattern.FindAllStringSubmatchIndex(source, -1)
	if len(matches) == 0 {
		return source
	}
	var out strings.Builder
	cursor := 0
	for _, match := range matches {
		out.WriteString(source[cursor:match[0]])
		groups := make([]string, len(match)/2)
		for i := 0; i < len(match); i += 2 {
			if match[i] >= 0 && match[i+1] >= 0 {
				groups[i/2] = source[match[i]:match[i+1]]
			}
		}
		out.WriteString(replace(groups))
		cursor = match[1]
	}
	out.WriteString(source[cursor:])
	return out.String()
}

func splitLatexComment(line string) (string, string) {
	for i := 0; i < len(line); i++ {
		if line[i] == '%' && !isEscapedAt(line, i) {
			return line[:i], line[i:]
		}
	}
	return line, ""
}

func stripLatexComments(source string) string {
	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	for index, line := range lines {
		code, _ := splitLatexComment(line)
		lines[index] = code
	}
	return strings.Join(lines, "\n")
}

func isEscapedAt(value string, index int) bool {
	count := 0
	for i := index - 1; i >= 0 && value[i] == '\\'; i-- {
		count++
	}
	return count%2 == 1
}

func readBraceGroup(source string, start int) (string, int, bool) {
	if start < 0 || start >= len(source) || source[start] != '{' {
		return "", start, false
	}
	depth := 0
	for i := start; i < len(source); i++ {
		if isEscapedAt(source, i) {
			continue
		}
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start+1 : i], i + 1, true
			}
		}
	}
	return "", start, false
}

func readBracketGroup(source string, start int) (string, int, bool) {
	if start < 0 || start >= len(source) || source[start] != '[' {
		return "", start, false
	}
	depth := 0
	for i := start; i < len(source); i++ {
		if isEscapedAt(source, i) {
			continue
		}
		switch source[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return source[start+1 : i], i + 1, true
			}
		}
	}
	return "", start, false
}

func parseBraceList(source string) []string {
	values := make([]string, 0)
	cursor := 0
	for cursor < len(source) {
		cursor = skipSpaces(source, cursor)
		value, next, ok := readBraceGroup(source, cursor)
		if !ok {
			break
		}
		if strings.TrimSpace(value) != "" {
			values = append(values, strings.TrimSpace(value))
		}
		cursor = next
	}
	return values
}

func skipSpaces(source string, index int) int {
	for index < len(source) && (source[index] == ' ' || source[index] == '\t' || source[index] == '\n' || source[index] == '\r') {
		index++
	}
	return index
}

func commaListValues(source string) []string {
	values := make([]string, 0)
	for _, item := range strings.Split(source, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			values = append(values, item)
		}
	}
	return values
}

func normalizeSearchPrefixes(paths []string, currentDir string) []string {
	seen := make(map[string]bool)
	prefixes := make([]string, 0)
	add := func(raw string) {
		prefix, ok := normalizeSearchPrefix(raw)
		if !ok || seen[prefix] {
			return
		}
		seen[prefix] = true
		prefixes = append(prefixes, prefix)
	}
	add("")
	if currentDir != "" {
		add(currentDir)
	}
	for _, raw := range paths {
		add(raw)
		if currentDir != "" {
			add(joinProjectPath(currentDir, raw))
		}
	}
	return prefixes
}

func normalizeSearchPrefix(value string) (string, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || value == "." {
		return "", true
	}
	cleaned, ok := CleanProjectPath(value)
	if !ok {
		return "", false
	}
	return strings.TrimRight(cleaned, "/") + "/", true
}

func joinProjectPath(left string, right string) string {
	if strings.TrimSpace(left) == "" {
		return strings.TrimSpace(right)
	}
	return strings.TrimRight(left, "/") + "/" + strings.TrimLeft(strings.TrimSpace(right), "/")
}

func normalizeGraphicsExtension(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, ".") {
		value = "." + value
	}
	if !graphicsExtensionPattern.MatchString(value) {
		return ""
	}
	return value
}

func normalizeGraphicsExtensions(values []string) []string {
	extensions := make([]string, 0)
	seen := make(map[string]bool)
	for _, value := range values {
		extension := normalizeGraphicsExtension(value)
		if extension == "" || seen[extension] {
			continue
		}
		seen[extension] = true
		extensions = append(extensions, extension)
	}
	if len(extensions) == 0 {
		return defaultGraphicsExtensions()
	}
	return extensions
}

func defaultGraphicsExtensions() []string {
	return []string{".png", ".jpg", ".jpeg", ".svg", ".webp", ".gif", ".pdf", ".eps", ".epsi", ".ps"}
}

func expandReferenceMacros(rawRef string, ctx resolveContext) string {
	value := strings.TrimSpace(rawRef)
	for range 8 {
		next := expandReferenceMacrosOnce(value, ctx)
		if next == value {
			return strings.TrimSpace(next)
		}
		value = next
	}
	return strings.TrimSpace(value)
}

func expandReferenceMacrosOnce(value string, ctx resolveContext) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var out strings.Builder
	for index := 0; index < len(value); {
		if value[index] != '\\' {
			out.WriteByte(value[index])
			index++
			continue
		}
		token, next := readControlSequence(value, index)
		switch {
		case token == `\jobname` && ctx.jobName != "":
			out.WriteString(ctx.jobName)
		case ctx.macros != nil && ctx.macros[token] != "":
			out.WriteString(ctx.macros[token])
		default:
			out.WriteString(token)
		}
		index = next
	}
	return out.String()
}

func readControlSequence(source string, start int) (string, int) {
	if start < 0 || start >= len(source) || source[start] != '\\' {
		return "", start
	}
	if start+1 >= len(source) {
		return `\`, start + 1
	}
	next := start + 1
	if !isControlSequenceNameByte(source[next]) {
		return source[start : next+1], next + 1
	}
	for next < len(source) && isControlSequenceNameByte(source[next]) {
		next++
	}
	return source[start:next], next
}

func isControlSequenceNameByte(value byte) bool {
	return (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z') || value == '@'
}

func legacyGraphicsOptionValue(rawOptions string) string {
	for _, item := range commaListValues(rawOptions) {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		if key == "file" || key == "figure" {
			return strings.TrimSpace(parts[1])
		}
	}
	return strings.TrimSpace(rawOptions)
}

func mimeForPath(value string) string {
	switch strings.ToLower(path.Ext(value)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".eps", ".epsi", ".ps":
		return "application/postscript"
	default:
		return "application/octet-stream"
	}
}

func hasProjectExtension(value string) bool {
	return graphicsExtensionPattern.MatchString(value)
}

func lineNumberAt(source string, index int) int {
	if index <= 0 {
		return 1
	}
	if index > len(source) {
		index = len(source)
	}
	return strings.Count(source[:index], "\n") + 1
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func errorDiagnostic(code string, message string, sourcePath string, line int) Diagnostic {
	diagnostic := Diagnostic{Severity: "error", Code: code, Message: message}
	if sourcePath != "" {
		diagnostic.Source = map[string]string{"path": sourcePath}
		if line > 0 {
			diagnostic.Source["line"] = fmt.Sprintf("%d", line)
		}
	}
	return diagnostic
}

func referenceDiagnostic(severity string, code string, message string, ref Reference) Diagnostic {
	diagnostic := Diagnostic{Severity: severity, Code: code, Message: message}
	if ref.SourceFile != "" || ref.Line > 0 {
		diagnostic.Source = map[string]string{}
		if ref.SourceFile != "" {
			diagnostic.Source["path"] = ref.SourceFile
		}
		if ref.Line > 0 {
			diagnostic.Source["line"] = fmt.Sprintf("%d", ref.Line)
		}
	}
	return diagnostic
}

func generatedListDiagnostic(severity string, code string, message string, ref GeneratedListReference) Diagnostic {
	diagnostic := Diagnostic{Severity: severity, Code: code, Message: message}
	if ref.SourceFile != "" || ref.Line > 0 || ref.ListType != "" {
		diagnostic.Source = map[string]string{}
		if ref.SourceFile != "" {
			diagnostic.Source["path"] = ref.SourceFile
		}
		if ref.Line > 0 {
			diagnostic.Source["line"] = fmt.Sprintf("%d", ref.Line)
		}
		if ref.ListType != "" {
			diagnostic.Source["listType"] = ref.ListType
		}
		if ref.Command != "" {
			diagnostic.Source["command"] = ref.Command
		}
		if len(ref.ExpectedExtensions) > 0 {
			diagnostic.Source["expectedExtensions"] = strings.Join(ref.ExpectedExtensions, ",")
		}
		if len(ref.FallbackExtensions) > 0 {
			diagnostic.Source["fallbackExtensions"] = strings.Join(ref.FallbackExtensions, ",")
		}
	}
	return diagnostic
}
