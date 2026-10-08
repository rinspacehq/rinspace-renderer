package projectcore

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

type archiveEntry struct {
	name string
	body []byte
}

type archiveEntries []archiveEntry

func ImportArchive(filename string, body []byte, limits Limits) ([]File, []Diagnostic, error) {
	return importArchive(filename, body, limits, false, false)
}

// ImportMarkdownArchive keeps the repository manifest in Markdown projects so
// asset promotion can read the same publication settings as the repository.
func ImportMarkdownArchive(filename string, body []byte, limits Limits) ([]File, []Diagnostic, error) {
	return importArchive(filename, body, limits, false, true)
}

// ImportTypstArchive uses the same archive safety limits while including the
// Typst source and its two managed declarations. Existing TeX/Markdown graph
// hashes remain unchanged because their importer keeps the old allowlist.
func ImportTypstArchive(filename string, body []byte, limits Limits) ([]File, []Diagnostic, error) {
	return importArchive(filename, body, limits, true, false)
}

func importArchive(filename string, body []byte, limits Limits, typst, markdown bool) ([]File, []Diagnostic, error) {
	if limits.ArchiveMaxBytes > 0 && int64(len(body)) > limits.ArchiveMaxBytes {
		return nil, nil, errors.New("project archive too large")
	}

	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return unpackZip(body, limits, typst, markdown)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"), strings.HasSuffix(lower, ".gz"):
		return unpackGzipArchive(filename, body, limits, typst, markdown)
	case strings.HasSuffix(lower, ".tar"):
		return unpackTar(bytes.NewReader(body), limits, typst, markdown)
	default:
		if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
			return unpackGzipArchive(filename, body, limits, typst, markdown)
		}
		if typst && strings.HasSuffix(lower, ".typ") {
			return (archiveEntries{{name: filepath.Base(filename), body: body}}).toProjectFiles(limits, typst, markdown)
		}
		if name := singleProjectEntryName(filename, body); name != "" {
			return (archiveEntries{{name: name, body: body}}).toProjectFiles(limits, typst, markdown)
		}
		return nil, nil, errors.New("unsupported source type; use .zip, .tar, .tar.gz, .tgz, gzipped TeX source, or a single TeX project file")
	}
}

func unpackGzipArchive(filename string, body []byte, limits Limits, typst, markdown bool) ([]File, []Diagnostic, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	defer gz.Close()

	payload, err := readLimited(gz, limits.ArchiveMaxBytes)
	if err != nil {
		return nil, nil, err
	}

	if files, diagnostics, err := unpackTar(bytes.NewReader(payload), limits, typst, markdown); err == nil {
		return files, diagnostics, nil
	}

	name := singleGzipEntryName(filename, payload)
	return (archiveEntries{{name: name, body: payload}}).toProjectFiles(limits, typst, markdown)
}

func unpackZip(body []byte, limits Limits, typst, markdown bool) ([]File, []Diagnostic, error) {
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, nil, err
	}

	entries := make(archiveEntries, 0, len(reader.File))
	for _, file := range reader.File {
		if file.FileInfo().Mode()&os.ModeSymlink != 0 && typst {
			return nil, nil, fmt.Errorf("Typst archive contains a symbolic link: %s", file.Name)
		}
		if file.FileInfo().IsDir() || file.FileInfo().Mode()&os.ModeSymlink != 0 {
			continue
		}
		if isUnsafeArchiveEntryName(file.Name) {
			return nil, nil, fmt.Errorf("unsafe archive path: %s", file.Name)
		}
		rc, err := file.Open()
		if err != nil {
			return nil, nil, err
		}
		content, readErr := readWithOverflow(rc, limits.FileMaxBytes)
		_ = rc.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		entries = append(entries, archiveEntry{name: file.Name, body: content})
	}
	return entries.toProjectFiles(limits, typst, markdown)
}

func unpackTar(reader io.Reader, limits Limits, typst, markdown bool) ([]File, []Diagnostic, error) {
	tr := tar.NewReader(reader)
	var entries archiveEntries
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if typst && (header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink) {
			return nil, nil, fmt.Errorf("Typst archive contains a link: %s", header.Name)
		}
		if header.FileInfo().IsDir() || header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			continue
		}
		if isUnsafeArchiveEntryName(header.Name) {
			return nil, nil, fmt.Errorf("unsafe archive path: %s", header.Name)
		}
		content, err := readWithOverflow(tr, limits.FileMaxBytes)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, archiveEntry{name: header.Name, body: content})
	}
	return entries.toProjectFiles(limits, typst, markdown)
}

func readLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(reader)
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("project file is too large")
	}
	return body, nil
}

func readWithOverflow(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(reader)
	}
	return io.ReadAll(io.LimitReader(reader, maxBytes+1))
}

func (entries archiveEntries) toProjectFiles(limits Limits, typst, markdown bool) ([]File, []Diagnostic, error) {
	if limits.FileMaxCount > 0 && len(entries) > limits.FileMaxCount {
		return nil, nil, fmt.Errorf("archive has too many files: %d", len(entries))
	}

	stripPrefix := commonTopDirectory(entries)
	files := make([]File, 0, len(entries))
	diagnostics := make([]Diagnostic, 0)
	seen := make(map[string]bool, len(entries))

	for _, entry := range entries {
		if limits.FileMaxBytes > 0 && int64(len(entry.body)) > limits.FileMaxBytes {
			diagnostics = append(diagnostics, warning("project.file.oversized", fmt.Sprintf("skip oversized file: %s", entry.name), entry.name))
			continue
		}

		name := strings.TrimPrefix(strings.ReplaceAll(entry.name, "\\", "/"), stripPrefix)
		cleaned, ok := CleanProjectPath(name)
		if !ok {
			return nil, nil, fmt.Errorf("unsafe archive path: %s", entry.name)
		}
		if seen[cleaned] || shouldSkipArchivePath(cleaned) {
			continue
		}
		seen[cleaned] = true

		kind := fileKind(cleaned)
		if markdown && cleaned == "rinspace.yaml" {
			kind = "text"
		}
		if typst {
			switch {
			case strings.HasSuffix(strings.ToLower(cleaned), ".typ"):
				kind = "typst"
			case cleaned == "rinspace.yaml", cleaned == "rinspace.typst.lock.json":
				kind = "text"
			}
		}
		if kind == "" {
			diagnostics = append(diagnostics, warning("project.file.unsupported", fmt.Sprintf("skip unsupported file: %s", cleaned), cleaned))
			continue
		}
		if typst && kind == "typst" && !utf8.Valid(entry.body) {
			return nil, nil, fmt.Errorf("Typst source is not valid UTF-8: %s", cleaned)
		}

		projectFile := File{
			Path:  cleaned,
			Kind:  kind,
			Bytes: int64(len(entry.body)),
		}
		if isTextFile(cleaned, entry.body) || typst && (kind == "typst" || cleaned == "rinspace.yaml" || cleaned == "rinspace.typst.lock.json") && utf8.Valid(entry.body) {
			projectFile.Body = strings.TrimPrefix(string(entry.body), "\ufeff")
		} else {
			projectFile.Encoding = "base64"
			projectFile.MIME = mime.TypeByExtension(strings.ToLower(filepath.Ext(cleaned)))
			projectFile.Body = base64.StdEncoding.EncodeToString(entry.body)
		}
		files = append(files, projectFile)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	if len(files) == 0 {
		return nil, diagnostics, errors.New("archive contains no supported project files")
	}
	return files, diagnostics, nil
}

func singleGzipEntryName(filename string, payload []byte) string {
	name := trimArchiveExtension(filepath.Base(filename))
	if strings.Contains(name, ".") {
		return name
	}
	if looksLikeTexSource(payload) {
		return name + ".tex"
	}
	return name
}

func singleProjectEntryName(filename string, body []byte) string {
	name := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	cleaned, ok := CleanProjectPath(name)
	if ok && fileKind(cleaned) != "" {
		return cleaned
	}
	if looksLikeTexSource(body) {
		base := ""
		if ok {
			base = strings.TrimSuffix(cleaned, filepath.Ext(cleaned))
		}
		if base == "" {
			base = "main"
		}
		return base + ".tex"
	}
	return ""
}

func looksLikeTexSource(body []byte) bool {
	text := bytes.TrimSpace(body)
	if len(text) == 0 || !utf8.Valid(text) {
		return false
	}
	source := string(text)
	return strings.Contains(source, `\documentclass`) ||
		strings.Contains(source, `\begin{document}`) ||
		strings.Contains(source, `\usepackage`) ||
		strings.Contains(source, `\input`)
}

func trimArchiveExtension(filename string) string {
	lower := strings.ToLower(filename)
	for _, ext := range []string{".tar.gz", ".tgz", ".zip", ".tar"} {
		if strings.HasSuffix(lower, ext) {
			return strings.TrimSuffix(filename, filename[len(filename)-len(ext):])
		}
	}
	return strings.TrimSuffix(filename, filepath.Ext(filename))
}

func commonTopDirectory(entries archiveEntries) string {
	prefix := ""
	for _, entry := range entries {
		name := strings.ReplaceAll(entry.name, "\\", "/")
		if strings.HasPrefix(name, "__MACOSX/") || !strings.Contains(name, "/") {
			return ""
		}
		top := strings.SplitN(name, "/", 2)[0]
		if prefix == "" {
			prefix = top
			continue
		}
		if prefix != top {
			return ""
		}
	}
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

func shouldSkipArchivePath(value string) bool {
	base := path.Base(value)
	return strings.HasPrefix(value, "__MACOSX/") ||
		strings.HasPrefix(base, ".") ||
		strings.HasSuffix(base, "~")
}

func fileKind(value string) string {
	switch strings.ToLower(filepath.Ext(value)) {
	case ".tex", ".ltx":
		return "tex"
	case ".bib", ".bbl":
		return "bib"
	case ".sty", ".cls", ".bst", ".bbx", ".cbx", ".def", ".clo", ".cfg", ".ldf", ".fd":
		return "style"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".pdf", ".eps", ".epsi", ".ps":
		return "asset"
	case ".txt", ".md", ".ind", ".idx", ".gls", ".glo", ".nls", ".nlo":
		return "text"
	default:
		return ""
	}
}

func isTextFile(value string, body []byte) bool {
	switch strings.ToLower(filepath.Ext(value)) {
	case ".tex", ".ltx", ".bib", ".bbl", ".sty", ".cls", ".bst", ".bbx", ".cbx", ".def", ".clo", ".cfg", ".ldf", ".fd", ".txt", ".md", ".ind", ".idx", ".gls", ".glo", ".nls", ".nlo", ".svg", ".eps":
		return utf8.Valid(body)
	default:
		return false
	}
}

func isUnsafeArchiveEntryName(value string) bool {
	_, ok := CleanProjectPath(value)
	return !ok
}

func CleanProjectPath(value string) (string, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.HasPrefix(value, "/") || hasWindowsDrivePrefix(value) {
		return "", false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", false
		}
	}
	cleaned := path.Clean(value)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") || hasWindowsDrivePrefix(cleaned) {
		return "", false
	}
	return cleaned, true
}

func hasWindowsDrivePrefix(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	c := value[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func warning(code string, message string, sourcePath string) Diagnostic {
	diagnostic := Diagnostic{
		Severity: "warning",
		Code:     code,
		Message:  message,
	}
	if strings.TrimSpace(sourcePath) != "" {
		diagnostic.Source = map[string]string{"path": sourcePath}
	}
	return diagnostic
}
