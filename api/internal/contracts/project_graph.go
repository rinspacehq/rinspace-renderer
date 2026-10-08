package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

const ProjectGraphSchemaVersion = "rin-project-graph/v1"

type ContentKind string

const (
	ContentKindLaTeX    ContentKind = "latex"
	ContentKindMarkdown ContentKind = "markdown"
	ContentKindTypst    ContentKind = "typst"
	ContentKindPDF      ContentKind = "pdf"
)

type EntrypointRole string

const (
	EntrypointRoleDocument EntrypointRole = "document"
	EntrypointRoleBookPage EntrypointRole = "book-page"
)

type ProjectFileRole string

const (
	ProjectFileRoleSource       ProjectFileRole = "source"
	ProjectFileRoleAsset        ProjectFileRole = "asset"
	ProjectFileRoleBibliography ProjectFileRole = "bibliography"
	ProjectFileRoleGenerated    ProjectFileRole = "generated"
)

type ProjectGraph struct {
	SchemaVersion string              `json:"schemaVersion"`
	ProjectHash   string              `json:"projectHash"`
	ContentKind   ContentKind         `json:"contentKind"`
	Entrypoints   []ProjectEntrypoint `json:"entrypoints"`
	Files         []ProjectGraphFile  `json:"files"`
	References    []ProjectReference  `json:"references"`
	Options       map[string]any      `json:"options"`
}

type ProjectEntrypoint struct {
	Path string         `json:"path"`
	Role EntrypointRole `json:"role"`
}

type ProjectGraphFile struct {
	Path      string          `json:"path"`
	SHA256    string          `json:"sha256"`
	Bytes     int64           `json:"bytes"`
	MediaType string          `json:"mediaType,omitempty"`
	Role      ProjectFileRole `json:"role"`
}

type ProjectReference struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Kind     string `json:"kind"`
	Resolved bool   `json:"resolved"`
}

// ComputeProjectHash returns the identity of the uploaded project content. It deliberately covers
// content kind plus canonical file paths and file hashes, not archive ordering, timestamps,
// entrypoint selection, inferred roles, or render options. Those inputs receive independent hashes
// in later cache/result contracts.
func ComputeProjectHash(contentKind ContentKind, files []ProjectGraphFile) (string, error) {
	if !contentKind.Valid() {
		return "", fmt.Errorf("unsupported content kind %q", contentKind)
	}
	canonical := make([]ProjectGraphFile, len(files))
	copy(canonical, files)
	for index := range canonical {
		cleaned, ok := CleanProjectPath(canonical[index].Path)
		if !ok || cleaned != canonical[index].Path {
			return "", fmt.Errorf("project file path must be canonical: %q", canonical[index].Path)
		}
		if !isSHA256(canonical[index].SHA256) {
			return "", fmt.Errorf("project file %q has invalid sha256", canonical[index].Path)
		}
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Path < canonical[j].Path })
	for index := 1; index < len(canonical); index++ {
		if canonical[index-1].Path == canonical[index].Path {
			return "", fmt.Errorf("duplicate project file path %q", canonical[index].Path)
		}
	}

	hash := sha256.New()
	writeHashPart(hash, ProjectGraphSchemaVersion)
	writeHashPart(hash, string(contentKind))
	for _, file := range canonical {
		writeHashPart(hash, file.Path)
		writeHashPart(hash, file.SHA256)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func SHA256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func (graph ProjectGraph) Validate() error {
	if graph.SchemaVersion != ProjectGraphSchemaVersion {
		return fmt.Errorf("unsupported project graph schema version %q", graph.SchemaVersion)
	}
	if len(graph.Files) == 0 {
		return errors.New("project graph requires at least one file")
	}
	expectedHash, err := ComputeProjectHash(graph.ContentKind, graph.Files)
	if err != nil {
		return err
	}
	if graph.ProjectHash != expectedHash {
		return errors.New("project graph hash does not match canonical files")
	}

	filePaths := make(map[string]struct{}, len(graph.Files))
	for _, file := range graph.Files {
		if file.Bytes < 0 {
			return fmt.Errorf("project file %q has negative byte size", file.Path)
		}
		if !file.Role.Valid() {
			return fmt.Errorf("project file %q has unsupported role %q", file.Path, file.Role)
		}
		filePaths[file.Path] = struct{}{}
	}
	if len(graph.Entrypoints) == 0 {
		return errors.New("project graph requires at least one entrypoint")
	}
	seenEntrypoints := map[string]struct{}{}
	for _, entrypoint := range graph.Entrypoints {
		cleaned, ok := CleanProjectPath(entrypoint.Path)
		if !ok || cleaned != entrypoint.Path {
			return fmt.Errorf("entrypoint path must be canonical: %q", entrypoint.Path)
		}
		if !entrypoint.Role.Valid() {
			return fmt.Errorf("entrypoint %q has unsupported role %q", entrypoint.Path, entrypoint.Role)
		}
		if _, ok := filePaths[entrypoint.Path]; !ok {
			return fmt.Errorf("entrypoint %q does not reference a project file", entrypoint.Path)
		}
		key := string(entrypoint.Role) + "\x00" + entrypoint.Path
		if _, ok := seenEntrypoints[key]; ok {
			return fmt.Errorf("duplicate entrypoint %q", entrypoint.Path)
		}
		seenEntrypoints[key] = struct{}{}
	}
	for _, reference := range graph.References {
		if _, ok := filePaths[reference.From]; !ok {
			return fmt.Errorf("reference source %q does not exist", reference.From)
		}
		if strings.TrimSpace(reference.Kind) == "" {
			return fmt.Errorf("reference from %q has empty kind", reference.From)
		}
		if reference.Resolved {
			if _, ok := filePaths[reference.To]; !ok {
				return fmt.Errorf("resolved reference target %q does not exist", reference.To)
			}
		} else if reference.To != "" {
			cleaned, ok := CleanProjectPath(reference.To)
			if !ok || cleaned != reference.To {
				return fmt.Errorf("unresolved reference target must be canonical: %q", reference.To)
			}
		}
	}
	if graph.Options == nil {
		return errors.New("project graph options must be an object")
	}
	return nil
}

func (kind ContentKind) Valid() bool {
	return kind == ContentKindLaTeX || kind == ContentKindMarkdown || kind == ContentKindTypst || kind == ContentKindPDF
}

func (role EntrypointRole) Valid() bool {
	return role == EntrypointRoleDocument || role == EntrypointRoleBookPage
}

func (role ProjectFileRole) Valid() bool {
	switch role {
	case ProjectFileRoleSource, ProjectFileRoleAsset, ProjectFileRoleBibliography, ProjectFileRoleGenerated:
		return true
	default:
		return false
	}
}

func CleanProjectPath(value string) (string, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') || hasWindowsDrivePrefix(value) {
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

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type stringWriter interface {
	Write([]byte) (int, error)
}

func writeHashPart(writer stringWriter, value string) {
	_, _ = writer.Write([]byte(value))
	_, _ = writer.Write([]byte{0})
}
