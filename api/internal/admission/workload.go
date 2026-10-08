package admission

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

const admissionSourceInspectLimit = 1 << 20

var admissionLatexBookPattern = regexp.MustCompile(`(?i)\\documentclass\s*(?:\[[^\]]*\]\s*)?\{(?:book|report|memoir)\}`)

type admissionSourceObservation struct {
	FileCount     *int64
	DocumentClass string
}

func observeAdmissionSource(body []byte, contentKind string, metadata json.RawMessage) admissionSourceObservation {
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		if observation, ok := observeAdmissionTar(body, contentKind, metadata); ok {
			return observation
		}
		count := int64(1)
		return admissionSourceObservation{FileCount: &count, DocumentClass: classifyAdmissionDocument(body, contentKind)}
	}
	var count int64
	var contentFiles []*zip.File
	entrypoint := admissionEntrypoint(metadata)
	var selected *zip.File
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		count++
		extension := strings.ToLower(filepath.Ext(file.Name))
		if admissionContentFile(contentKind, extension) {
			contentFiles = append(contentFiles, file)
			if file.Name == entrypoint {
				selected = file
			}
		}
	}
	observation := admissionSourceObservation{FileCount: &count}
	if contentKind == "markdown" && len(contentFiles) > 1 {
		observation.DocumentClass = "book"
	}
	if selected == nil && len(contentFiles) > 0 {
		selected = contentFiles[0]
	}
	if selected != nil && selected.UncompressedSize64 <= admissionSourceInspectLimit {
		stream, err := selected.Open()
		if err == nil {
			source, readErr := io.ReadAll(io.LimitReader(stream, admissionSourceInspectLimit+1))
			_ = stream.Close()
			if readErr == nil && len(source) <= admissionSourceInspectLimit {
				if classified := classifyAdmissionDocument(source, contentKind); classified != "" && observation.DocumentClass == "" {
					observation.DocumentClass = classified
				}
			}
		}
	}
	return observation
}

func observeAdmissionTar(body []byte, contentKind string, metadata json.RawMessage) (admissionSourceObservation, bool) {
	reader := tar.NewReader(bytes.NewReader(body))
	entrypoint := admissionEntrypoint(metadata)
	var count int64
	var firstContent, selected []byte
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return admissionSourceObservation{}, false
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		count++
		extension := strings.ToLower(filepath.Ext(header.Name))
		contentFile := admissionContentFile(contentKind, extension)
		if !contentFile || header.Size > admissionSourceInspectLimit {
			continue
		}
		source, err := io.ReadAll(io.LimitReader(reader, admissionSourceInspectLimit+1))
		if err != nil || len(source) > admissionSourceInspectLimit {
			continue
		}
		if firstContent == nil {
			firstContent = source
		}
		if header.Name == entrypoint {
			selected = source
		}
	}
	if count == 0 {
		return admissionSourceObservation{}, false
	}
	if selected == nil {
		selected = firstContent
	}
	observation := admissionSourceObservation{FileCount: &count}
	if selected != nil {
		observation.DocumentClass = classifyAdmissionDocument(selected, contentKind)
	}
	return observation, true
}

func admissionEntrypoint(metadata json.RawMessage) string {
	var values map[string]any
	if len(metadata) == 0 || json.Unmarshal(metadata, &values) != nil {
		return ""
	}
	for _, key := range []string{"entrypoint", "mainFile", "metadataMainFile"} {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// admissionContentFile reports whether an archive entry is a primary source
// file for the content kind. Typst documents are authored in .typ files, which
// are inventoried for the bounded file-count and document-class observation
// without invoking the TeX project parser.
func admissionContentFile(contentKind, extension string) bool {
	switch contentKind {
	case "latex":
		return extension == ".tex"
	case "markdown":
		return extension == ".md" || extension == ".markdown"
	case "typst":
		return extension == ".typ"
	default:
		return false
	}
}

func classifyAdmissionDocument(source []byte, contentKind string) string {
	if len(source) > admissionSourceInspectLimit {
		source = source[:admissionSourceInspectLimit]
	}
	if contentKind == "latex" {
		if admissionLatexBookPattern.Match(source) {
			return "book"
		}
		if bytes.Contains(bytes.ToLower(source), []byte(`\documentclass`)) {
			return "article"
		}
		return ""
	}
	if contentKind == "markdown" || contentKind == "typst" {
		return "article"
	}
	return ""
}
