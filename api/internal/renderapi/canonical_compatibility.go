package renderapi

import (
	"errors"
	"sort"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

type LegacyFieldClass string

const (
	LegacyFieldCanonical     LegacyFieldClass = "canonical-projection"
	LegacyFieldCompatibility LegacyFieldClass = "compatibility-only"
	LegacyFieldPrivateDebug  LegacyFieldClass = "private-debug"
)

var ProjectRenderResponseFieldClasses = map[string]LegacyFieldClass{
	"requestId": LegacyFieldCanonical, "engine": LegacyFieldCanonical, "assets": LegacyFieldCanonical,
	"generatedArtifacts": LegacyFieldCanonical,
	"knowledgeIndex":     LegacyFieldCanonical,
	"versions":           LegacyFieldCanonical, "diagnostics": LegacyFieldCanonical,
	"title": LegacyFieldCompatibility, "html": LegacyFieldCompatibility, "fallback": LegacyFieldCompatibility,
	"primaryEngine": LegacyFieldCompatibility, "fallbackEngine": LegacyFieldCompatibility, "mainFile": LegacyFieldCompatibility,
	"diagrams": LegacyFieldCompatibility, "assetFiles": LegacyFieldCompatibility, "assetManifest": LegacyFieldCompatibility,
	"reader": LegacyFieldCompatibility, "math": LegacyFieldCompatibility, "documentMode": LegacyFieldCompatibility,
	"texSource": LegacyFieldPrivateDebug, "source": LegacyFieldPrivateDebug, "analysisSource": LegacyFieldPrivateDebug,
	"resolvedSource": LegacyFieldPrivateDebug, "assetInventory": LegacyFieldPrivateDebug, "project": LegacyFieldPrivateDebug,
}

type LegacyCompatibilityPayload struct {
	Title          string
	HTML           string
	Fallback       bool
	PrimaryEngine  string
	FallbackEngine string
	MainFile       string
	Diagrams       []DiagramRef
	AssetFiles     []AssetFile
	AssetManifest  any
	Reader         any
	Math           MathSummary
}

type PrivateRenderDebug struct {
	TexSource      string
	Source         string
	AnalysisSource string
	ResolvedSource string
	AssetInventory any
	Project        any
}

// ProjectRenderResponseFromCanonical is the single compatibility projection for future durable
// jobs. Private source/debug fields are opt-in and cannot leak merely because debug data exists.
func ProjectRenderResponseFromCanonical(result contracts.RenderResult, compatibility LegacyCompatibilityPayload, debug *PrivateRenderDebug, includePrivateDebug bool) (ProjectRenderResponse, error) {
	if err := result.Validate(); err != nil {
		return ProjectRenderResponse{}, err
	}
	if result.Versions["rinRenderer"] == "" {
		return ProjectRenderResponse{}, errors.New("canonical result is missing rinRenderer version")
	}
	if includePrivateDebug && debug == nil {
		return ProjectRenderResponse{}, errors.New("private debug was requested without authorized debug data")
	}

	artifacts := append([]contracts.ArtifactReference(nil), result.Assets...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].ArtifactID < artifacts[j].ArtifactID })
	assets := make([]AssetRef, 0, len(artifacts))
	for _, artifact := range artifacts {
		assets = append(assets, AssetRef{Path: artifact.ArtifactID, MIME: artifact.MediaType})
	}
	diagnostics := make([]Diagnostic, 0, len(result.Diagnostics))
	for _, diagnostic := range result.Diagnostics {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: diagnostic.Severity,
			Code:     diagnostic.Code,
			Message:  diagnostic.Message,
		})
	}
	response := ProjectRenderResponse{
		RequestID: result.RequestID, Title: compatibility.Title, HTML: compatibility.HTML,
		Engine: result.Engine, Fallback: compatibility.Fallback, PrimaryEngine: compatibility.PrimaryEngine,
		FallbackEngine: compatibility.FallbackEngine, MainFile: compatibility.MainFile,
		Diagrams: append([]DiagramRef(nil), compatibility.Diagrams...), Assets: assets,
		GeneratedArtifacts: artifacts,
		AssetFiles:         append([]AssetFile(nil), compatibility.AssetFiles...), AssetManifest: compatibility.AssetManifest,
		Reader: compatibility.Reader, Math: compatibility.Math, Versions: versionsFromCanonical(result.Versions),
		Diagnostics:    diagnostics,
		KnowledgeIndex: result.Inline.KnowledgeIndex,
	}
	if includePrivateDebug {
		response.TexSource = debug.TexSource
		response.Source = debug.Source
		response.AnalysisSource = debug.AnalysisSource
		response.ResolvedSource = debug.ResolvedSource
		response.AssetInventory = debug.AssetInventory
		response.Project = debug.Project
	}
	return response, nil
}

func versionsFromCanonical(versions map[string]string) Versions {
	return Versions{
		RinRenderer: versions["rinRenderer"], FinalOutput: versions["finalOutput"], LateXML: versions["latexml"], LaTeXMLAdapter: versions["latexmlAdapter"],
		Perl: versions["perl"], TeXLive: versions["texlive"], Dvisvgm: versions["dvisvgm"],
		DiagramEngine: versions["diagramEngine"], MathJax: versions["mathjax"], MathJaxOutput: versions["mathjaxOutput"],
		MathJaxFont: versions["mathjaxFont"], KaTeX: versions["katex"],
	}
}
