package projectidentity

import (
	"fmt"
	"path"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

const MarkdownEntrypoint = "main.md"

// ParseMarkdownCatalog accepts only the repository's nested relative-path list.
// It does not guess an old JSON/YAML format, glob files, or infer hierarchy from headings.
func ParseMarkdownCatalog(source string, files map[string][]byte) ([]MarkdownBookPage, error) {
	pages := make([]MarkdownBookPage, 0)
	seen := map[string]bool{}
	previousLevel := 0
	for lineNumber, raw := range strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		line := strings.TrimLeft(raw, " ")
		indent := len(raw) - len(line)
		level := indent/2 + 1
		if indent%2 != 0 || level > 6 || level > previousLevel+1 || !strings.HasPrefix(line, "- ") || strings.ContainsAny(raw, "\t\r\x00") {
			return nil, fmt.Errorf("main.md line %d must be a nested relative Markdown path (two spaces per level)", lineNumber+1)
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		for _, segment := range strings.Split(name, "/") {
			if strings.HasPrefix(segment, ".") || strings.ContainsAny(segment, "\\:") {
				return nil, fmt.Errorf("main.md contains a hidden or invalid path")
			}
		}
		cleaned, ok := contracts.CleanProjectPath(name)
		if !ok || cleaned != name || name == MarkdownEntrypoint || path.Ext(name) != ".md" || seen[name] {
			return nil, fmt.Errorf("main.md line %d has an invalid, repeated or self-referencing path", lineNumber+1)
		}
		body, exists := files[name]
		if !exists {
			return nil, fmt.Errorf("main.md chapter does not exist: %s", name)
		}
		title := strings.TrimSuffix(path.Base(name), ".md")
		for _, bodyLine := range strings.Split(string(body), "\n") {
			candidate := strings.TrimSpace(bodyLine)
			if strings.HasPrefix(candidate, "# ") {
				title = strings.TrimSpace(strings.TrimPrefix(candidate, "# "))
				break
			}
		}
		pages = append(pages, MarkdownBookPage{Path: name, ID: "rin-md-" + contracts.SHA256Hex([]byte(name))[:24], Title: title, Level: level})
		if len(pages) > 512 {
			return nil, fmt.Errorf("main.md exceeds 512 chapters")
		}
		seen[name] = true
		previousLevel = level
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("main.md requires at least one chapter")
	}
	return pages, nil
}

func ImportMarkdownCatalogBook(filename string, archive []byte, title string, limits projectcore.Limits) (Project, error) {
	files, _, err := projectcore.ImportMarkdownArchive(filename, archive, limits)
	if err != nil {
		return Project{}, err
	}
	byPath := make(map[string][]byte, len(files))
	for _, file := range files {
		body, err := FileBytes(file)
		if err != nil {
			return Project{}, err
		}
		byPath[file.Path] = body
	}
	catalog, exists := byPath[MarkdownEntrypoint]
	if !exists {
		return Project{}, fmt.Errorf("Markdown book entrypoint main.md is missing")
	}
	pages, err := ParseMarkdownCatalog(string(catalog), byPath)
	if err != nil {
		return Project{}, err
	}
	return markdownBookGraph(files, title, pages)
}
