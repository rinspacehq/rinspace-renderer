package contracts

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const KnowledgeIndexSchemaVersion = "knowledge-index/v1"

var (
	knowledgeProjectID = regexp.MustCompile(`^(article|book|tag-wiki|pdf):[1-9][0-9]*$`)
	knowledgeCommit    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	knowledgeAnchorID  = regexp.MustCompile(`^[a-z][a-z0-9-]{2,79}$`)
)

type KnowledgeIndex struct {
	SchemaVersion string                `json:"schemaVersion"`
	ProjectID     string                `json:"projectId"`
	SourceCommit  string                `json:"sourceCommit"`
	ProjectHash   string                `json:"projectHash"`
	Anchors       []KnowledgeAnchor     `json:"anchors"`
	References    []KnowledgeReference  `json:"references"`
	Unresolved    []KnowledgeUnresolved `json:"unresolved"`
}

type KnowledgeLocator struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

type KnowledgeAnchor struct {
	ID            string           `json:"id"`
	Kind          string           `json:"kind"`
	Label         string           `json:"label"`
	SourceLocator KnowledgeLocator `json:"sourceLocator"`
	ContentHash   string           `json:"contentHash"`
}

type KnowledgeTarget struct {
	Kind     string `json:"kind"`
	ID       int64  `json:"id"`
	AnchorID string `json:"anchorId,omitempty"`
}

type KnowledgeReference struct {
	SourceAnchorID   string           `json:"sourceAnchorId,omitempty"`
	Target           KnowledgeTarget  `json:"target"`
	AuthoredRelation string           `json:"authoredRelation"`
	SourceLocator    KnowledgeLocator `json:"sourceLocator"`
}

type KnowledgeUnresolved struct {
	Label          string           `json:"label"`
	SourceAnchorID string           `json:"sourceAnchorId,omitempty"`
	ParentHintIDs  []int64          `json:"parentHintIds,omitempty"`
	SourceLocator  KnowledgeLocator `json:"sourceLocator"`
}

func (index KnowledgeIndex) Validate(projectHash string) error {
	if index.SchemaVersion != KnowledgeIndexSchemaVersion || !knowledgeProjectID.MatchString(index.ProjectID) || !knowledgeCommit.MatchString(index.SourceCommit) || strings.Trim(index.SourceCommit, "0") == "" || !isSHA256(index.ProjectHash) || index.ProjectHash != projectHash {
		return errors.New("knowledge index publication identity is invalid")
	}
	if index.Anchors == nil || index.References == nil || index.Unresolved == nil || len(index.Anchors) > 10000 || len(index.References) > 50000 || len(index.Unresolved) > 10000 {
		return errors.New("knowledge index collections are missing or exceed limits")
	}
	seen := map[string]struct{}{}
	for _, anchor := range index.Anchors {
		if !knowledgeAnchorID.MatchString(anchor.ID) || !knowledgeKind(anchor.Kind) || !knowledgeLabel(anchor.Label) || !knowledgeLocator(anchor.SourceLocator) || !isSHA256(anchor.ContentHash) {
			return fmt.Errorf("knowledge anchor %q is invalid", anchor.ID)
		}
		if _, exists := seen[anchor.ID]; exists {
			return fmt.Errorf("knowledge anchor %q is duplicated", anchor.ID)
		}
		seen[anchor.ID] = struct{}{}
	}
	for _, reference := range index.References {
		if reference.SourceAnchorID != "" && !knowledgeAnchorID.MatchString(reference.SourceAnchorID) || reference.Target.Kind != "tag" || reference.Target.ID <= 0 || reference.Target.AnchorID != "" && !knowledgeAnchorID.MatchString(reference.Target.AnchorID) || reference.AuthoredRelation != "citation" || !knowledgeLocator(reference.SourceLocator) {
			return errors.New("knowledge reference is invalid")
		}
	}
	for _, unresolved := range index.Unresolved {
		if !knowledgeLabel(unresolved.Label) || unresolved.SourceAnchorID != "" && !knowledgeAnchorID.MatchString(unresolved.SourceAnchorID) || len(unresolved.ParentHintIDs) > 16 || !knowledgeLocator(unresolved.SourceLocator) {
			return errors.New("unresolved knowledge reference is invalid")
		}
		hints := map[int64]struct{}{}
		for _, id := range unresolved.ParentHintIDs {
			if id <= 0 {
				return errors.New("unresolved parent hint is invalid")
			}
			if _, exists := hints[id]; exists {
				return errors.New("unresolved parent hint is duplicated")
			}
			hints[id] = struct{}{}
		}
	}
	return nil
}

func knowledgeKind(value string) bool {
	switch value {
	case "section", "definition", "theorem", "lemma", "example", "equation", "figure", "note", "other":
		return true
	}
	return false
}
func knowledgeLabel(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= 240
}
func knowledgeLocator(value KnowledgeLocator) bool {
	if value.Line < 1 || value.Line > 10000000 || value.Path == "" || len(value.Path) > 1024 || strings.ContainsRune(value.Path, 0) || strings.HasPrefix(value.Path, "/") || strings.Contains(value.Path, `\`) {
		return false
	}
	cleaned := path.Clean(value.Path)
	return cleaned == value.Path && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}
