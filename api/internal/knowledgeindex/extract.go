package knowledgeindex

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

const (
	maxSourceBytes = 8 << 20
	maxAnchors     = 10000
	maxReferences  = 50000
	maxUnresolved  = 10000
	maxLabelRunes  = 240
)

var (
	anchorIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{2,79}$`)
	anchorKinds     = map[string]bool{"section": true, "definition": true, "theorem": true, "lemma": true, "example": true, "equation": true, "figure": true, "note": true, "other": true}
	knownCommands   = map[string]int{"rinanchor": 3, "rintagref": 2, "rinanchorref": 3, "rinmissing": 1}
)

type Identity struct {
	ProjectID    string
	SourceCommit string
	ProjectHash  string
}

type Error struct {
	Code string
	Path string
	Line int
	Err  error
}

func (err *Error) Error() string {
	return fmt.Sprintf("%s:%d: %s", err.Path, err.Line, err.Err)
}

func (err *Error) Unwrap() error { return err.Err }

func Extract(files []projectcore.File, identity Identity) (contracts.KnowledgeIndex, error) {
	result := contracts.KnowledgeIndex{
		SchemaVersion: contracts.KnowledgeIndexSchemaVersion,
		ProjectID:     identity.ProjectID, SourceCommit: identity.SourceCommit, ProjectHash: identity.ProjectHash,
		Anchors: []contracts.KnowledgeAnchor{}, References: []contracts.KnowledgeReference{}, Unresolved: []contracts.KnowledgeUnresolved{},
	}
	ordered := append([]projectcore.File(nil), files...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	seen := map[string]contracts.KnowledgeLocator{}
	total := 0
	for _, file := range ordered {
		ext := strings.ToLower(filepath.Ext(file.Path))
		if ext != ".tex" && ext != ".ltx" {
			continue
		}
		total += len(file.Body)
		if total > maxSourceBytes {
			return contracts.KnowledgeIndex{}, extractionError("knowledge_index.source_too_large", file.Path, 1, "LaTeX knowledge source exceeds 8 MiB")
		}
		if !utf8.ValidString(file.Body) {
			return contracts.KnowledgeIndex{}, extractionError("knowledge_index.invalid_utf8", file.Path, 1, "LaTeX knowledge source is not valid UTF-8")
		}
		currentAnchor := ""
		for position, line := 0, 1; position < len(file.Body); {
			character := file.Body[position]
			if character == '\n' {
				line++
				position++
				continue
			}
			if character == '%' && !escaped(file.Body, position) {
				for position < len(file.Body) && file.Body[position] != '\n' {
					position++
				}
				continue
			}
			if character != '\\' {
				position++
				continue
			}
			commandStart, commandLine := position, line
			position++
			nameStart := position
			for position < len(file.Body) && ((file.Body[position] >= 'a' && file.Body[position] <= 'z') || (file.Body[position] >= 'A' && file.Body[position] <= 'Z')) {
				position++
			}
			name := file.Body[nameStart:position]
			argumentCount, known := knownCommands[name]
			if !known {
				if position == nameStart && position < len(file.Body) {
					position++
				}
				continue
			}
			arguments := make([]string, 0, argumentCount)
			for index := 0; index < argumentCount; index++ {
				for position < len(file.Body) {
					if file.Body[position] == '\n' {
						line++
						position++
						continue
					}
					if file.Body[position] == ' ' || file.Body[position] == '\t' || file.Body[position] == '\r' {
						position++
						continue
					}
					break
				}
				value, next, nextLine, parseErr := bracedArgument(file.Body, position, line)
				if parseErr != nil {
					return contracts.KnowledgeIndex{}, extractionError("knowledge_index.malformed_command", file.Path, commandLine, fmt.Sprintf("malformed \\%s argument %d", name, index+1))
				}
				arguments, position, line = append(arguments, value), next, nextLine
			}
			locator := contracts.KnowledgeLocator{Path: file.Path, Line: commandLine}
			switch name {
			case "rinanchor":
				id, kind, label := strings.TrimSpace(arguments[0]), strings.TrimSpace(arguments[1]), strings.TrimSpace(arguments[2])
				if !anchorIDPattern.MatchString(id) || !anchorKinds[kind] || !validLabel(label) {
					return contracts.KnowledgeIndex{}, extractionError("knowledge_index.invalid_anchor", file.Path, commandLine, "anchor ID, kind, or label is invalid")
				}
				if first, exists := seen[id]; exists {
					return contracts.KnowledgeIndex{}, extractionError("knowledge_index.duplicate_anchor", file.Path, commandLine, fmt.Sprintf("anchor %q duplicates %s:%d", id, first.Path, first.Line))
				}
				if len(result.Anchors) == maxAnchors {
					return contracts.KnowledgeIndex{}, extractionError("knowledge_index.anchor_limit", file.Path, commandLine, "anchor limit exceeded")
				}
				digest := sha256.Sum256([]byte(kind + "\x00" + label))
				result.Anchors = append(result.Anchors, contracts.KnowledgeAnchor{ID: id, Kind: kind, Label: label, SourceLocator: locator, ContentHash: hex.EncodeToString(digest[:])})
				seen[id], currentAnchor = locator, id
			case "rintagref", "rinanchorref":
				id, parseErr := strconv.ParseInt(strings.TrimSpace(arguments[0]), 10, 64)
				labelIndex := 1
				targetAnchor := ""
				if name == "rinanchorref" {
					targetAnchor, labelIndex = strings.TrimSpace(arguments[1]), 2
				}
				if parseErr != nil || id <= 0 || (targetAnchor != "" && !anchorIDPattern.MatchString(targetAnchor)) || !validLabel(strings.TrimSpace(arguments[labelIndex])) {
					return contracts.KnowledgeIndex{}, extractionError("knowledge_index.invalid_reference", file.Path, commandLine, "numeric Tag target, anchor ID, or display label is invalid")
				}
				if len(result.References) == maxReferences {
					return contracts.KnowledgeIndex{}, extractionError("knowledge_index.reference_limit", file.Path, commandLine, "reference limit exceeded")
				}
				result.References = append(result.References, contracts.KnowledgeReference{SourceAnchorID: currentAnchor, Target: contracts.KnowledgeTarget{Kind: "tag", ID: id, AnchorID: targetAnchor}, AuthoredRelation: "citation", SourceLocator: locator})
			case "rinmissing":
				label := strings.TrimSpace(arguments[0])
				if !validLabel(label) {
					return contracts.KnowledgeIndex{}, extractionError("knowledge_index.invalid_unresolved", file.Path, commandLine, "unresolved label is invalid")
				}
				if len(result.Unresolved) == maxUnresolved {
					return contracts.KnowledgeIndex{}, extractionError("knowledge_index.unresolved_limit", file.Path, commandLine, "unresolved-reference limit exceeded")
				}
				result.Unresolved = append(result.Unresolved, contracts.KnowledgeUnresolved{Label: label, SourceAnchorID: currentAnchor, SourceLocator: locator})
			}
			if position <= commandStart {
				return contracts.KnowledgeIndex{}, errors.New("knowledge extractor made no progress")
			}
		}
	}
	return result, nil
}

func bracedArgument(source string, position, line int) (string, int, int, error) {
	if position >= len(source) || source[position] != '{' {
		return "", position, line, errors.New("missing opening brace")
	}
	start, depth := position+1, 1
	for position++; position < len(source); position++ {
		if source[position] == '\n' {
			line++
		}
		if escaped(source, position) {
			continue
		}
		switch source[position] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start:position], position + 1, line, nil
			}
		}
	}
	return "", position, line, errors.New("unterminated argument")
}

func escaped(source string, position int) bool {
	count := 0
	for index := position - 1; index >= 0 && source[index] == '\\'; index-- {
		count++
	}
	return count%2 == 1
}

func validLabel(value string) bool {
	return value != "" && utf8.RuneCountInString(value) <= maxLabelRunes
}

func extractionError(code, path string, line int, message string) error {
	return &Error{Code: code, Path: path, Line: line, Err: errors.New(message)}
}
