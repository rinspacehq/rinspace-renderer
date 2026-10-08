package projectcore

import (
	"strings"
	"testing"
)

func TestResolveProjectExpandsNestedIncludes(t *testing.T) {
	resolved := ResolveProject([]File{
		{Path: "main.tex", Kind: "tex", Body: `\documentclass{article}
\begin{document}
\input{sections/intro}
\end{document}`},
		{Path: "sections/intro.tex", Kind: "tex", Body: `Intro.
\input{details}`},
		{Path: "sections/details.tex", Kind: "tex", Body: "Details."},
	}, "main.tex")

	if !containsAll(resolved.Source, "Intro.", "Details.") {
		t.Fatalf("expected expanded source to include nested files, got %q", resolved.Source)
	}
	if len(resolved.Includes) != 2 {
		t.Fatalf("expected two include references, got %#v", resolved.Includes)
	}
	for _, diagnostic := range resolved.Diagnostics {
		if diagnostic.Severity == "error" {
			t.Fatalf("expected no error diagnostics, got %#v", resolved.Diagnostics)
		}
	}
}

func TestResolveProjectReportsMissingInclude(t *testing.T) {
	resolved := ResolveProject([]File{
		{Path: "main.tex", Kind: "tex", Body: `\begin{document}
\input{missing}
\end{document}`},
	}, "main.tex")

	if !hasDiagnostic(resolved.Diagnostics, "project.include.missing") {
		t.Fatalf("expected missing include diagnostic, got %#v", resolved.Diagnostics)
	}
}

func TestResolveProjectBuildsBibliographyInventory(t *testing.T) {
	resolved := ResolveProject([]File{
		{Path: "main.tex", Kind: "tex", Body: `\begin{document}
\bibliography{refs,missing}
\end{document}`},
		{Path: "refs.bib", Kind: "bib", Body: "@book{knuth}"},
	}, "main.tex")

	if len(resolved.Bibliography.BibPaths) != 1 || resolved.Bibliography.BibPaths[0] != "refs.bib" {
		t.Fatalf("expected refs.bib bibliography path, got %#v", resolved.Bibliography)
	}
	if len(resolved.Bibliography.Missing) != 1 || resolved.Bibliography.Missing[0].RawRef != "missing" {
		t.Fatalf("expected missing bibliography reference, got %#v", resolved.Bibliography)
	}
	if !hasDiagnostic(resolved.Diagnostics, "project.bibliography.missing") {
		t.Fatalf("expected bibliography diagnostic, got %#v", resolved.Diagnostics)
	}
}

func TestResolveProjectBuildsAssetInventoryFromGraphicspath(t *testing.T) {
	resolved := ResolveProject([]File{
		{Path: "main.tex", Kind: "tex", Body: `\graphicspath{{figures/}}
\begin{document}
\includegraphics{chart}
\end{document}`},
		{Path: "figures/chart.png", Kind: "asset", Encoding: "base64", MIME: "image/png", Body: "iVBORw0KGgo=", Bytes: 8},
		{Path: "figures/unused.png", Kind: "asset", Encoding: "base64", MIME: "image/png", Body: "iVBORw0KGgo=", Bytes: 8},
	}, "main.tex")

	if len(resolved.AssetInventory.References) != 1 || resolved.AssetInventory.References[0].Path != "figures/chart.png" {
		t.Fatalf("expected chart asset reference, got %#v", resolved.AssetInventory)
	}
	if len(resolved.AssetInventory.Missing) != 0 {
		t.Fatalf("expected no missing assets, got %#v", resolved.AssetInventory.Missing)
	}
	if len(resolved.AssetInventory.Unused) != 1 || resolved.AssetInventory.Unused[0].Path != "figures/unused.png" {
		t.Fatalf("expected unused asset, got %#v", resolved.AssetInventory.Unused)
	}
}

func TestResolveProjectExpandsPathMacrosInReferences(t *testing.T) {
	resolved := ResolveProject([]File{
		{Path: "main.tex", Kind: "tex", Body: `\newcommand{\sectiondir}{sections}
\newcommand*{\figdir}{figures}
\DeclareRobustCommand{\bibbase}{refs}
\def\stylebase{plain}
\graphicspath{{\figdir/}}
\begin{document}
\input{\sectiondir/intro}
\adjincludegraphics{\figdir/chart}
\bibliographystyle{\stylebase}
\bibliography{\bibbase,\jobname-extra}
\end{document}`},
		{Path: "sections/intro.tex", Kind: "tex", Body: "Intro."},
		{Path: "figures/chart.png", Kind: "asset", Encoding: "base64", MIME: "image/png", Body: "iVBORw0KGgo=", Bytes: 8},
		{Path: "refs.bib", Kind: "bib", Body: "@book{knuth}"},
		{Path: "main-extra.bib", Kind: "bib", Body: "@book{extra}"},
		{Path: "plain.bst", Kind: "style", Body: "ENTRY{}{}{}"},
	}, "main.tex")

	if !strings.Contains(resolved.Source, "Intro.") {
		t.Fatalf("expected macro-expanded include source, got %q", resolved.Source)
	}
	if len(resolved.Includes) != 1 || resolved.Includes[0].Path != "sections/intro.tex" {
		t.Fatalf("expected macro-expanded include reference, got %#v", resolved.Includes)
	}
	if len(resolved.AssetInventory.References) != 1 || resolved.AssetInventory.References[0].Path != "figures/chart.png" || resolved.AssetInventory.References[0].Command != "adjincludegraphics" {
		t.Fatalf("expected macro-expanded adjincludegraphics asset reference, got %#v", resolved.AssetInventory)
	}
	if !containsAll(strings.Join(resolved.Bibliography.BibPaths, ","), "refs.bib", "main-extra.bib") {
		t.Fatalf("expected macro-expanded bibliography paths, got %#v", resolved.Bibliography)
	}
	if len(resolved.Bibliography.Styles) != 1 || resolved.Bibliography.Styles[0].Path != "plain.bst" {
		t.Fatalf("expected macro-expanded bibliography style, got %#v", resolved.Bibliography.Styles)
	}
	for _, diagnostic := range resolved.Diagnostics {
		if diagnostic.Severity == "error" {
			t.Fatalf("expected no error diagnostics, got %#v", resolved.Diagnostics)
		}
	}
}

func TestResolveProjectReportsMissingAsset(t *testing.T) {
	resolved := ResolveProject([]File{
		{Path: "main.tex", Kind: "tex", Body: `\begin{document}
\includegraphics{missing}
\end{document}`},
	}, "main.tex")

	if len(resolved.AssetInventory.Missing) != 1 || resolved.AssetInventory.Missing[0].RawRef != "missing" {
		t.Fatalf("expected missing asset reference, got %#v", resolved.AssetInventory.Missing)
	}
	if !hasDiagnostic(resolved.Diagnostics, "project.asset.missing") {
		t.Fatalf("expected asset diagnostic, got %#v", resolved.Diagnostics)
	}
}

func TestResolveProjectAppliesGeneratedIndexFile(t *testing.T) {
	resolved := ResolveProject([]File{
		{Path: "main.tex", Kind: "tex", Body: `\documentclass{article}
\makeindex
\begin{document}
Alpha\index{Alpha}
\printindex
\end{document}`},
		{Path: "main.ind", Kind: "text", Body: `\begin{theindex}
\item Alpha, 1
\end{theindex}`},
	}, "main.tex")

	if len(resolved.GeneratedLists.References) != 1 {
		t.Fatalf("expected generated list reference, got %#v", resolved.GeneratedLists)
	}
	ref := resolved.GeneratedLists.References[0]
	if !ref.Resolved || ref.Path != "main.ind" || ref.ListType != "index" {
		t.Fatalf("expected resolved generated index, got %#v", ref)
	}
	if len(resolved.GeneratedLists.Missing) != 0 {
		t.Fatalf("expected no missing generated lists, got %#v", resolved.GeneratedLists.Missing)
	}
	if !strings.Contains(resolved.AnalysisSource, `\item Alpha, 1`) {
		t.Fatalf("expected generated index body in analysis source, got %q", resolved.AnalysisSource)
	}
	for _, removed := range []string{`\makeindex`, `\index{Alpha}`, `\printindex`} {
		if strings.Contains(resolved.AnalysisSource, removed) {
			t.Fatalf("expected %s to be removed from analysis source, got %q", removed, resolved.AnalysisSource)
		}
	}
}

func TestResolveProjectReportsMissingGeneratedLists(t *testing.T) {
	resolved := ResolveProject([]File{
		{Path: "main.tex", Kind: "tex", Body: `\documentclass{article}
\makeglossaries
\makenomenclature
\begin{document}
\glsaddall
\nomenclature{$a$}{A coefficient}
\printglossary
\printnomenclature
\end{document}`},
	}, "main.tex")

	if len(resolved.GeneratedLists.Missing) != 2 {
		t.Fatalf("expected missing glossary and nomenclature, got %#v", resolved.GeneratedLists.Missing)
	}
	if !hasGeneratedList(resolved.GeneratedLists.Missing, "glossary") || !hasGeneratedList(resolved.GeneratedLists.Missing, "nomenclature") {
		t.Fatalf("expected glossary and nomenclature missing refs, got %#v", resolved.GeneratedLists.Missing)
	}
	if countDiagnostics(resolved.Diagnostics, "project.generated_list.missing") != 2 {
		t.Fatalf("expected generated list diagnostics, got %#v", resolved.Diagnostics)
	}
	if !containsAll(resolved.AnalysisSource, `\begin{theglossary}`, `\end{theglossary}`, `\begin{thenomenclature}`, `\end{thenomenclature}`) {
		t.Fatalf("expected empty generated list environments, got %q", resolved.AnalysisSource)
	}
	for _, removed := range []string{`\makeglossaries`, `\makenomenclature`, `\glsaddall`, `\nomenclature{$a$}{A coefficient}`, `\printglossary`, `\printnomenclature`} {
		if strings.Contains(resolved.AnalysisSource, removed) {
			t.Fatalf("expected %s to be removed from analysis source, got %q", removed, resolved.AnalysisSource)
		}
	}
}

func TestBuildManifestIncludesGeneratedLists(t *testing.T) {
	manifest := BuildManifest([]File{
		{Path: "main.tex", Kind: "tex", Body: `\begin{document}
\printindex
\end{document}`},
		{Path: "main.ind", Kind: "text", Body: `\begin{theindex}
\item Alpha, 1
\end{theindex}`},
	}, BuildOptions{Title: "Paper"})

	if len(manifest.GeneratedLists.References) != 1 {
		t.Fatalf("expected manifest generated list reference, got %#v", manifest.GeneratedLists)
	}
	if manifest.GeneratedLists.References[0].Path != "main.ind" {
		t.Fatalf("expected manifest generated list path, got %#v", manifest.GeneratedLists.References[0])
	}
	if !strings.Contains(manifest.AnalysisSource, `\item Alpha, 1`) {
		t.Fatalf("expected manifest analysis source to include generated list body, got %q", manifest.AnalysisSource)
	}
}

func TestBuildManifestIncludesResolutionContract(t *testing.T) {
	manifest := BuildManifest([]File{
		{Path: "main.tex", Kind: "tex", Body: `\begin{document}
\input{body}
\end{document}`},
		{Path: "body.tex", Kind: "tex", Body: "Hello."},
	}, BuildOptions{Title: "Paper"})

	if manifest.Source == "" || manifest.AnalysisSource == "" || manifest.ResolvedSource == "" {
		t.Fatalf("expected manifest source fields, got %#v", manifest)
	}
	if len(manifest.Includes) != 1 || manifest.Includes[0].Path != "body.tex" {
		t.Fatalf("expected manifest include references, got %#v", manifest.Includes)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

func hasDiagnostic(diagnostics []Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func hasGeneratedList(references []GeneratedListReference, listType string) bool {
	for _, ref := range references {
		if ref.ListType == listType {
			return true
		}
	}
	return false
}

func countDiagnostics(diagnostics []Diagnostic, code string) int {
	count := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			count++
		}
	}
	return count
}
