package projectcore

import (
	"fmt"
	"path/filepath"
	"strings"
)

func BuildManifest(files []File, opts BuildOptions) Manifest {
	diagnostics := make([]Diagnostic, 0)
	mainFile, mainDiagnostics := ChooseMainFile(files, MainFileCandidates{
		Explicit: opts.ExplicitMainFile,
		Metadata: opts.MetadataMainFile,
		Active:   opts.ActiveFile,
	})
	diagnostics = append(diagnostics, mainDiagnostics...)
	resolution := ResolveProject(files, mainFile)
	diagnostics = append(diagnostics, resolution.Diagnostics...)

	return Manifest{
		Version:             "0.1",
		Title:               strings.TrimSpace(opts.Title),
		MainFile:            mainFile,
		Files:               files,
		FileCount:           len(files),
		Source:              resolution.Source,
		AnalysisSource:      resolution.AnalysisSource,
		ResolvedSource:      resolution.ResolvedSource,
		Includes:            resolution.Includes,
		DiagramPlaceholders: []DiagramPlaceholder{},
		AssetInventory:      resolution.AssetInventory,
		Bibliography:        resolution.Bibliography,
		GeneratedLists:      resolution.GeneratedLists,
		Diagnostics:         diagnostics,
	}
}

func RebuildManifestFiles(base Manifest, files []File) Manifest {
	resolution := ResolveProject(files, base.MainFile)
	next := base
	next.Files = files
	next.FileCount = len(files)
	next.Source = resolution.Source
	next.AnalysisSource = resolution.AnalysisSource
	next.ResolvedSource = resolution.ResolvedSource
	next.Includes = resolution.Includes
	next.AssetInventory = resolution.AssetInventory
	next.Bibliography = resolution.Bibliography
	next.GeneratedLists = resolution.GeneratedLists
	if next.DiagramPlaceholders == nil {
		next.DiagramPlaceholders = []DiagramPlaceholder{}
	}
	return next
}

type MainFileCandidates struct {
	Explicit string
	Metadata string
	Active   string
}

func ChooseMainFile(files []File, candidates MainFileCandidates) (string, []Diagnostic) {
	diagnostics := make([]Diagnostic, 0)
	byPath := make(map[string]File, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}

	for _, candidate := range []struct {
		label   string
		value   string
		missing bool
	}{
		{label: "explicit", value: candidates.Explicit, missing: true},
		{label: "metadata", value: candidates.Metadata},
		{label: "active", value: candidates.Active},
	} {
		if strings.TrimSpace(candidate.value) == "" {
			continue
		}
		cleaned, ok := CleanProjectPath(candidate.value)
		if !ok {
			diagnostics = append(diagnostics, warning("project.mainfile.invalid", fmt.Sprintf("ignore unsafe %s main file: %s", candidate.label, candidate.value), candidate.value))
		} else if file, ok := byPath[cleaned]; ok && file.Kind == "tex" {
			return cleaned, diagnostics
		} else if candidate.missing {
			diagnostics = append(diagnostics, warning("project.mainfile.missing", fmt.Sprintf("requested main file is not available: %s", cleaned), cleaned))
		}
	}

	for _, name := range []string{"main.tex", "paper.tex", "article.tex", "ms.tex"} {
		if file, ok := byPath[name]; ok && file.Kind == "tex" {
			return file.Path, diagnostics
		}
	}

	documentClassFiles := make([]string, 0)
	for _, file := range files {
		if file.Kind == "tex" && strings.Contains(file.Body, `\documentclass`) {
			documentClassFiles = append(documentClassFiles, file.Path)
		}
	}
	if len(documentClassFiles) > 0 {
		if len(documentClassFiles) > 1 {
			diagnostics = append(diagnostics, warning("project.mainfile.ambiguous", fmt.Sprintf("multiple TeX files contain \\documentclass; using %s", documentClassFiles[0]), documentClassFiles[0]))
		}
		return documentClassFiles[0], diagnostics
	}

	for _, file := range files {
		if file.Kind == "tex" {
			diagnostics = append(diagnostics, warning("project.mainfile.inferred", fmt.Sprintf("no conventional main file found; using %s", file.Path), file.Path))
			return file.Path, diagnostics
		}
	}

	if len(files) > 0 {
		diagnostics = append(diagnostics, warning("project.mainfile.missing", "no TeX file found in project", ""))
		return files[0].Path, diagnostics
	}
	return "", diagnostics
}

func TitleFromArchiveName(filename string) string {
	name := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	name = trimArchiveExtension(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." {
		return "Rinspace Book"
	}
	return name
}
