package projectidentity

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

type Project struct {
	Graph contracts.ProjectGraph
	Files []projectcore.File
}

type MarkdownBookPage struct {
	Path  string `json:"path"`
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

func Import(filename string, archive []byte, contentKind contracts.ContentKind, entrypoint string, limits projectcore.Limits) (Project, error) {
	return importProject(filename, archive, contentKind,
		[]contracts.ProjectEntrypoint{{Path: entrypoint, Role: contracts.EntrypointRoleDocument}},
		[]contracts.ProjectReference{}, map[string]any{"documentMode": "article"}, limits)
}

// ImportMarkdownBook turns the ordered files[] representation used by existing Rinspace Markdown
// books into the canonical Project Graph without changing its stable page IDs or titles.
func ImportMarkdownBook(filename string, archive []byte, title string, pages []MarkdownBookPage, limits projectcore.Limits) (Project, error) {
	if len(pages) < 1 {
		return Project{}, errors.New("Markdown Book requires at least one page")
	}
	entrypoints := make([]contracts.ProjectEntrypoint, 0, len(pages))
	references := make([]contracts.ProjectReference, 0, (len(pages)-1)*2)
	order := make([]string, 0, len(pages))
	pageIDs := make(map[string]any, len(pages))
	pageTitles := make(map[string]any, len(pages))
	seenIDs := map[string]struct{}{}
	for _, page := range pages {
		cleaned, ok := contracts.CleanProjectPath(page.Path)
		if !ok || !validEntrypoint(contracts.ContentKindMarkdown, cleaned) || !contracts.ValidPageID(page.ID) {
			return Project{}, fmt.Errorf("Markdown Book page is invalid: %s", page.Path)
		}
		if _, exists := seenIDs[page.ID]; exists {
			return Project{}, fmt.Errorf("duplicate Markdown Book page id %q", page.ID)
		}
		seenIDs[page.ID] = struct{}{}
		entrypoints = append(entrypoints, contracts.ProjectEntrypoint{Path: cleaned, Role: contracts.EntrypointRoleBookPage})
		order = append(order, cleaned)
		pageIDs[cleaned] = page.ID
		if value := strings.TrimSpace(page.Title); value != "" {
			pageTitles[cleaned] = value
		}
	}
	options := map[string]any{
		"documentMode": "book", "pageOrder": order, "pageIds": pageIDs, "pageTitles": pageTitles,
	}
	if value := strings.TrimSpace(title); value != "" {
		options["title"] = value
	}
	for index := 0; index+1 < len(order); index++ {
		references = append(references,
			contracts.ProjectReference{From: order[index], To: order[index+1], Kind: "book-navigation-next", Resolved: true},
			contracts.ProjectReference{From: order[index+1], To: order[index], Kind: "book-navigation-previous", Resolved: true},
		)
	}
	return importProject(filename, archive, contracts.ContentKindMarkdown, entrypoints,
		references, options, limits)
}

func importProject(filename string, archive []byte, contentKind contracts.ContentKind, entrypoints []contracts.ProjectEntrypoint, references []contracts.ProjectReference, options map[string]any, limits projectcore.Limits) (Project, error) {
	var files []projectcore.File
	var err error
	if contentKind == contracts.ContentKindTypst {
		files, _, err = projectcore.ImportTypstArchive(filename, archive, limits)
	} else if contentKind == contracts.ContentKindMarkdown {
		files, _, err = projectcore.ImportMarkdownArchive(filename, archive, limits)
	} else {
		files, _, err = projectcore.ImportArchive(filename, archive, limits)
	}
	if err != nil {
		return Project{}, err
	}
	if len(entrypoints) == 0 {
		return Project{}, errors.New("project requires an entrypoint")
	}
	cleanEntrypoints := make([]contracts.ProjectEntrypoint, 0, len(entrypoints))
	wanted := make(map[string]contracts.EntrypointRole, len(entrypoints))
	for _, entrypoint := range entrypoints {
		cleaned, ok := contracts.CleanProjectPath(entrypoint.Path)
		if !ok || !entrypoint.Role.Valid() || !validEntrypoint(contentKind, cleaned) {
			return Project{}, errors.New("project entrypoint is invalid")
		}
		if _, exists := wanted[cleaned]; exists {
			return Project{}, fmt.Errorf("duplicate project entrypoint %q", cleaned)
		}
		wanted[cleaned] = entrypoint.Role
		cleanEntrypoints = append(cleanEntrypoints, contracts.ProjectEntrypoint{Path: cleaned, Role: entrypoint.Role})
	}
	graphFiles := make([]contracts.ProjectGraphFile, 0, len(files))
	found := make(map[string]bool, len(wanted))
	for _, file := range files {
		body, err := FileBytes(file)
		if err != nil {
			return Project{}, err
		}
		role := contracts.ProjectFileRoleSource
		if file.Kind == "asset" {
			role = contracts.ProjectFileRoleAsset
		} else if file.Kind == "bib" {
			role = contracts.ProjectFileRoleBibliography
		}
		mediaType := strings.TrimSpace(file.MIME)
		if mediaType == "" {
			mediaType = sourceMediaType(file.Path)
		}
		if contentKind == contracts.ContentKindMarkdown && role == contracts.ProjectFileRoleAsset && strings.EqualFold(filepath.Ext(file.Path), ".svg") {
			mediaType = "image/svg+xml"
		}
		graphFiles = append(graphFiles, contracts.ProjectGraphFile{
			Path: file.Path, SHA256: contracts.SHA256Hex(body), Bytes: int64(len(body)),
			MediaType: mediaType, Role: role,
		})
		if _, ok := wanted[file.Path]; ok {
			found[file.Path] = true
		}
	}
	for path := range wanted {
		if !found[path] {
			return Project{}, fmt.Errorf("project entrypoint is missing or incompatible: %s", path)
		}
	}
	projectHash, err := contracts.ComputeProjectHash(contentKind, graphFiles)
	if err != nil {
		return Project{}, err
	}
	graph := contracts.ProjectGraph{
		SchemaVersion: contracts.ProjectGraphSchemaVersion, ProjectHash: projectHash,
		ContentKind: contentKind,
		Entrypoints: cleanEntrypoints,
		Files:       graphFiles, References: references,
		Options: options,
	}
	if err := graph.Validate(); err != nil {
		return Project{}, err
	}
	return Project{Graph: graph, Files: files}, nil
}

func FileBytes(file projectcore.File) ([]byte, error) {
	if file.Encoding == "base64" {
		body, err := base64.StdEncoding.DecodeString(file.Body)
		if err != nil {
			return nil, fmt.Errorf("decode project file %q: %w", file.Path, err)
		}
		return body, nil
	}
	return []byte(file.Body), nil
}

func sourceMediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".tex", ".ltx":
		return "application/x-tex; charset=utf-8"
	case ".typ":
		return "application/x-typst; charset=utf-8"
	case ".bib", ".bbl":
		return "application/x-bibtex; charset=utf-8"
	default:
		return "text/plain; charset=utf-8"
	}
}

func validEntrypoint(kind contracts.ContentKind, path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	switch kind {
	case contracts.ContentKindMarkdown:
		return extension == ".md" || extension == ".markdown"
	case contracts.ContentKindLaTeX:
		return extension == ".tex" || extension == ".ltx"
	case contracts.ContentKindTypst:
		return extension == ".typ"
	case contracts.ContentKindPDF:
		return extension == ".pdf"
	default:
		return false
	}
}
