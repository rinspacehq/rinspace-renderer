package projectdiagrams

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

func TestExtractTikzCDEnvironment(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `\documentclass{article}
\begin{document}
Before.
\begin{tikzcd}[column sep=huge]
A \arrow[r] & B
\end{tikzcd}
After.
\end{document}`,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected 1 diagram, got %#v", result.Diagrams)
	}
	diagram := result.Diagrams[0]
	if diagram.ID != "diagram-000001" || diagram.Type != "tikzcd" || diagram.Options != "column sep=huge" {
		t.Fatalf("unexpected diagram metadata: %#v", diagram)
	}
	if diagram.Body != `A \arrow[r] & B` {
		t.Fatalf("unexpected diagram body: %q", diagram.Body)
	}
	if !strings.Contains(diagram.Source, `\begin{tikzcd}[column sep=huge]`) || diagram.SourceFile != "main.tex" {
		t.Fatalf("unexpected source fields: %#v", diagram)
	}
	if diagram.SourceLine != 4 || diagram.SourceColumn != 1 {
		t.Fatalf("unexpected diagram source position: %#v", diagram)
	}
	if !strings.Contains(result.Files[0].Body, diagram.Placeholder) {
		t.Fatalf("expected placeholder in rewritten source: %s", result.Files[0].Body)
	}
	if strings.Contains(result.Files[0].Body, `\begin{tikzcd}`) || strings.Contains(result.Files[0].Body, `A \arrow[r] & B`) {
		t.Fatalf("expected diagram source to be replaced, got %s", result.Files[0].Body)
	}
}

func TestExtractCarriesSafeColorPreambleIntoStandaloneDiagram(t *testing.T) {
	result := Extract([]projectcore.File{
		{
			Path: "main.tex",
			Kind: "tex",
			Body: strings.Join([]string{
				`\documentclass{book}`,
				`\definecolor{diagramblue}{HTML}{245B78}`,
				`\providecolor{quietgray}{rgb}{0.2,0.3,0.4}`,
				`\colorlet{diagramaccent}{diagramblue!70!black}`,
				`\tikzcdset{row sep=2.4em,column sep=3.2em,every arrow/.append style={line width=.55pt},every label/.append style={font=\small}}`,
				`% \definecolor{commented}{HTML}{FFFFFF}`,
				`\definecolor{unsafe}{rgb}{1,0,0\input{secret}}`,
				`\tikzset{execute at begin picture={\write18{bad}}}`,
				`\begin{document}\input{chapters/chapter-01}\end{document}`,
			}, "\n"),
		},
		{
			Path: "chapters/chapter-01.tex",
			Kind: "tex",
			Body: `\begin{tikzcd}A \arrow[r,color=diagramblue] & B\end{tikzcd}`,
		},
	})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected one extracted diagram, got %#v", result.Diagrams)
	}
	diagram := result.Diagrams[0]
	for _, expected := range []string{
		`\definecolor{diagramblue}{HTML}{245B78}`,
		`\providecolor{quietgray}{rgb}{0.2,0.3,0.4}`,
		`\colorlet{diagramaccent}{diagramblue!70!black}`,
		`\tikzcdset{row sep=2.4em,column sep=3.2em,every arrow/.append style={line width=.55pt},every label/.append style={font=\small}}`,
		`\begin{tikzcd}A \arrow[r,color=diagramblue] & B\end{tikzcd}`,
	} {
		if !strings.Contains(diagram.RenderSource, expected) {
			t.Fatalf("expected render source to contain %q, got:\n%s", expected, diagram.RenderSource)
		}
	}
	for _, forbidden := range []string{"commented", "unsafe", `\input{secret}`, `\write18`, "execute at"} {
		if strings.Contains(diagram.RenderSource, forbidden) {
			t.Fatalf("unsafe or commented declaration %q reached render source:\n%s", forbidden, diagram.RenderSource)
		}
	}
	if strings.Contains(diagram.Source, `\definecolor`) || !strings.HasPrefix(diagram.RenderSource, `\definecolor`) {
		t.Fatalf("expected original source to stay pristine and render source to carry the preamble: %#v", diagram)
	}
}

func TestExtractIgnoresDiagramsInLatexComments(t *testing.T) {
	source := `Before.
% $$\begin{tikzcd}
% 0 \ar[r] & A' \ar[r] & A
% \end{tikzcd}$$
Literal percent \% before an active diagram: \begin{tikzcd}
A \arrow[r] & B
% \end{tikzcd} must not close the active environment
\end{tikzcd}
After.`
	result := Extract([]projectcore.File{{
		Path: "chapter.tex",
		Kind: "tex",
		Body: source,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected only the active diagram, got %#v", result.Diagrams)
	}
	diagram := result.Diagrams[0]
	if diagram.SourceLine != 5 || !strings.Contains(diagram.Body, `A \arrow[r] & B`) {
		t.Fatalf("unexpected active diagram: %#v", diagram)
	}
	for _, expected := range []string{
		`% $$\begin{tikzcd}`,
		`% \end{tikzcd}$$`,
		`Literal percent \% before an active diagram:`,
	} {
		if !strings.Contains(result.Files[0].Body, expected) {
			t.Fatalf("expected commented source to preserve %q, got %s", expected, result.Files[0].Body)
		}
	}
}

func TestExtractScopesCommutativeSettingsToCommutativeDiagrams(t *testing.T) {
	source := `\definecolor{diagramblue}{HTML}{245B78}
\tikzset{every node/.style={color=diagramblue}}
\tikzcdset{coherence/.style={row sep=2em,column sep=3em}}
\begin{tikzpicture}\draw (0,0)--(1,1);\end{tikzpicture}
\begin{tikzcd}[coherence] A \arrow[r] & B \end{tikzcd}`
	result := Extract([]projectcore.File{{Path: "main.tex", Kind: "tex", Body: source}})
	if len(result.Diagrams) != 2 {
		t.Fatalf("expected mixed diagrams: %+v", result.Diagrams)
	}
	for _, d := range result.Diagrams {
		if !strings.Contains(d.RenderSource, `\definecolor`) || !strings.Contains(d.RenderSource, `\tikzset`) {
			t.Fatalf("lost shared style: %+v", d)
		}
		if strings.Contains(d.RenderSource, `\tikzcdset`) != (d.Type == "tikzcd") {
			t.Fatalf("wrong package scope: %+v", d)
		}
		if strings.Contains(d.Source, `\tikzcdset`) {
			t.Fatal("changed original diagram")
		}
	}
}

func TestExtractIgnoresCommentedCommandDiagrams(t *testing.T) {
	source := strings.Join([]string{
		`% \xymatrix{A \ar[r] & B}`,
		`text % \chemfig{H-O-H}`,
		`% \schemestart \chemfig{A}\arrow{->}\chemfig{B} \schemestop`,
	}, "\n")
	result := Extract([]projectcore.File{{Path: "main.tex", Kind: "tex", Body: source}})

	if len(result.Diagrams) != 0 {
		t.Fatalf("expected commented commands to be ignored, got %#v", result.Diagrams)
	}
	if result.Files[0].Body != source {
		t.Fatalf("expected commented source to remain unchanged, got %q", result.Files[0].Body)
	}
}

func TestExtractMultipleFilesUsesStablePlaceholders(t *testing.T) {
	result := Extract([]projectcore.File{
		{
			Path: "chapter-a.tex",
			Kind: "tex",
			Body: `\begin{tikzpicture}
\draw (0,0) -- (1,1);
\end{tikzpicture}`,
		},
		{
			Path: "chapter-b.tex",
			Kind: "tex",
			Body: `\begin{forest}
[Root [Leaf]]
\end{forest}`,
		},
	})

	if len(result.Diagrams) != 2 {
		t.Fatalf("expected 2 diagrams, got %#v", result.Diagrams)
	}
	if result.Diagrams[0].Placeholder != "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000001" ||
		result.Diagrams[1].Placeholder != "RINRENDERERDIAGRAMPLACEHOLDERDIAGRAM000002" {
		t.Fatalf("unexpected placeholders: %#v", result.Diagrams)
	}
	if result.Diagrams[0].SourceFile != "chapter-a.tex" || result.Diagrams[1].SourceFile != "chapter-b.tex" {
		t.Fatalf("unexpected source files: %#v", result.Diagrams)
	}
}

func TestExtractSupportedEnvironmentTypes(t *testing.T) {
	source := strings.Join([]string{
		`\begin{tikzpicture}\draw (0,0) -- (1,1);\end{tikzpicture}`,
		`\begin{axis}\addplot coordinates {(0,0) (1,1)};\end{axis}`,
		`\begin{pspicture}(0,0)(1,1)\psline(0,0)(1,1)\end{pspicture}`,
		`\begin{CD}A @>>> B\end{CD}`,
		`\begin{picture}(10,10)\put(0,0){A}\end{picture}`,
		`\begin{circuitikz}\draw (0,0) to[R] (1,0);\end{circuitikz}`,
	}, "\n")
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: source,
	}})

	got := make([]string, 0, len(result.Diagrams))
	for _, diagram := range result.Diagrams {
		got = append(got, diagram.Type)
	}
	expected := []string{"tikzpicture", "axis", "pspicture", "amscd", "picture", "circuitikz"}
	if strings.Join(got, ",") != strings.Join(expected, ",") {
		t.Fatalf("expected diagram types %v, got %v", expected, got)
	}
}

func TestExtractCommandDiagramTypes(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: strings.Join([]string{
			`\xymatrix@C=2em@R=1em{A \ar[r] & B}`,
			`\chemfig[atom sep=2em]{H-O-H}`,
			`\schemestart`,
			`\chemfig{A}\arrow{->}\chemfig{B}`,
			`\schemestop`,
		}, "\n"),
	}})

	if len(result.Diagrams) != 3 {
		t.Fatalf("expected 3 diagrams, got %#v", result.Diagrams)
	}
	assertDiagram := func(index int, kind string, options string, bodyPart string) {
		t.Helper()
		diagram := result.Diagrams[index]
		if diagram.Type != kind || diagram.Options != options || !strings.Contains(diagram.Body, bodyPart) {
			t.Fatalf("unexpected diagram %d: %#v", index, diagram)
		}
	}
	assertDiagram(0, "xymatrix", "@C=2em@R=1em", `A \ar[r] & B`)
	assertDiagram(1, "chemfig", "atom sep=2em", "H-O-H")
	assertDiagram(2, "chemfig-scheme", "", `\arrow{->}`)
}

func TestExtractRemovesDisplayMathWrapperAroundDiagram(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `Before \[
\chemfig{H-O-H}
\] After`,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected 1 diagram, got %#v", result.Diagrams)
	}
	body := result.Files[0].Body
	if strings.Contains(body, `\[`) || strings.Contains(body, `\]`) || strings.Contains(body, `\chemfig`) {
		t.Fatalf("expected display wrapper and diagram command to be replaced, got %s", body)
	}
	if !strings.Contains(body, result.Diagrams[0].Placeholder) {
		t.Fatalf("expected placeholder in source, got %s", body)
	}
}

func TestExtractRemovesDisplayMathWrapperAroundDiagramWithTrailingPunctuation(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `Before.
\[
\begin{tikzcd}
A \arrow[r] & B
\end{tikzcd}.
\]
Middle.
\[
\begin{tikzcd}
C \arrow[r] & D
\end{tikzcd},
\]
After.`,
	}})

	if len(result.Diagrams) != 2 {
		t.Fatalf("expected 2 diagrams, got %#v", result.Diagrams)
	}
	body := result.Files[0].Body
	for _, unexpected := range []string{`\[`, `\]`, `\begin{tikzcd}`, `\end{tikzcd}`} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("expected display wrappers and diagram source to be replaced without %q, got %s", unexpected, body)
		}
	}
	for _, expected := range []string{"Before.", "Middle.", "After.", result.Diagrams[0].Placeholder, result.Diagrams[1].Placeholder} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected rewritten source to contain %q, got %s", expected, body)
		}
	}
}

func TestExtractConsumesAlignmentEnvironmentWrapper(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `Before.
\begin{center}
\begin{tikzcd}
A \arrow[r] & B
\end{tikzcd}
\end{center}
After.`,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected 1 diagram, got %#v", result.Diagrams)
	}
	diagram := result.Diagrams[0]
	if diagram.Layout.Alignment != "center" {
		t.Fatalf("expected center layout, got %#v", diagram.Layout)
	}
	body := result.Files[0].Body
	for _, unexpected := range []string{`\begin{center}`, `\end{center}`, `\begin{tikzcd}`} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("expected wrapper and diagram source to be replaced without %q, got %s", unexpected, body)
		}
	}
	if !strings.Contains(body, "Before.") || !strings.Contains(body, "After.") || !strings.Contains(body, diagram.Placeholder) {
		t.Fatalf("expected surrounding text and placeholder to remain, got %s", body)
	}
}

func TestExtractConsumesLeadingAlignmentCommand(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `Before.
\raggedleft
\begin{tikzcd}
A
\end{tikzcd}
After.`,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected 1 diagram, got %#v", result.Diagrams)
	}
	if result.Diagrams[0].Layout.Alignment != "flushright" {
		t.Fatalf("expected flushright layout, got %#v", result.Diagrams[0].Layout)
	}
	body := result.Files[0].Body
	if strings.Contains(body, `\raggedleft`) || strings.Contains(body, `\begin{tikzcd}`) {
		t.Fatalf("expected leading alignment command and diagram source to be replaced, got %s", body)
	}
	if !strings.Contains(body, "Before.") || !strings.Contains(body, "After.") {
		t.Fatalf("expected surrounding text to remain, got %s", body)
	}
}

func TestExtractPreservesFloatWhileConsumingCenteringCommand(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `\begin{figure}
\centering
\begin{tikzpicture}
\draw (0,0) -- (1,1);
\end{tikzpicture}
\caption{A centered diagram}
\label{fig:centered}
\end{figure}`,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected 1 diagram, got %#v", result.Diagrams)
	}
	diagram := result.Diagrams[0]
	if diagram.Layout.Alignment != "center" {
		t.Fatalf("expected center layout from centering command, got %#v", diagram.Layout)
	}
	body := result.Files[0].Body
	for _, expected := range []string{`\begin{figure}`, `\caption{A centered diagram}`, `\label{fig:centered}`, `\end{figure}`, diagram.Placeholder} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected source to preserve %q around placeholder, got %s", expected, body)
		}
	}
	for _, unexpected := range []string{`\centering`, `\begin{tikzpicture}`, `\end{tikzpicture}`} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("expected source to consume %q, got %s", unexpected, body)
		}
	}
}

func TestExtractConsumesMakeboxAlignmentWrapper(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `Intro. \makebox[\textwidth][r]{\begin{tikzcd}A\end{tikzcd}} Tail.`,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected 1 diagram, got %#v", result.Diagrams)
	}
	if result.Diagrams[0].Layout.Alignment != "flushright" {
		t.Fatalf("expected flushright layout, got %#v", result.Diagrams[0].Layout)
	}
	body := result.Files[0].Body
	for _, unexpected := range []string{`\makebox`, `\begin{tikzcd}`} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("expected command wrapper and diagram source to be replaced without %q, got %s", unexpected, body)
		}
	}
	if !strings.Contains(body, "Intro.") || !strings.Contains(body, "Tail.") || !strings.Contains(body, result.Diagrams[0].Placeholder) {
		t.Fatalf("expected surrounding text and placeholder to remain, got %s", body)
	}
}

func TestExtractConsumesGraphicsAndBoxWrappers(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `\resizebox{\textwidth}{!}{\fbox{\begin{tikzcd}
A \arrow[r] & B
\end{tikzcd}}}`,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected 1 diagram, got %#v", result.Diagrams)
	}
	body := result.Files[0].Body
	for _, unexpected := range []string{`\resizebox`, `\fbox`, `\begin{tikzcd}`} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("expected nested wrappers and diagram source to be replaced without %q, got %s", unexpected, body)
		}
	}
	if !strings.Contains(body, result.Diagrams[0].Placeholder) {
		t.Fatalf("expected placeholder in source, got %s", body)
	}
}

func TestExtractConsumesAdjustboxAlignmentWrapper(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `\adjustbox{right}{\chemfig{H-O-H}}`,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected 1 diagram, got %#v", result.Diagrams)
	}
	diagram := result.Diagrams[0]
	if diagram.Type != "chemfig" || diagram.Body != "H-O-H" || diagram.Layout.Alignment != "flushright" {
		t.Fatalf("unexpected diagram extraction: %#v", diagram)
	}
	body := result.Files[0].Body
	for _, unexpected := range []string{`\adjustbox`, `\chemfig`} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("expected adjustbox wrapper and chemfig source to be replaced without %q, got %s", unexpected, body)
		}
	}
	if !strings.Contains(body, diagram.Placeholder) {
		t.Fatalf("expected placeholder in source, got %s", body)
	}
}

func TestExtractKeepsWrapperWithMixedTextForLateXML(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `\fbox{Text \begin{tikzcd}A\end{tikzcd}}`,
	}})

	if len(result.Diagrams) != 1 {
		t.Fatalf("expected 1 diagram, got %#v", result.Diagrams)
	}
	body := result.Files[0].Body
	if !strings.Contains(body, `\fbox{Text`) || !strings.Contains(body, result.Diagrams[0].Placeholder) {
		t.Fatalf("expected mixed wrapper text to remain around placeholder, got %s", body)
	}
	if strings.Contains(body, `\begin{tikzcd}`) {
		t.Fatalf("expected diagram source to be replaced, got %s", body)
	}
}

func TestExtractIgnoresNonTexFiles(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "notes.txt",
		Kind: "asset",
		Body: `\begin{tikzcd}A\end{tikzcd}`,
	}})

	if len(result.Diagrams) != 0 {
		t.Fatalf("expected no diagrams from asset file, got %#v", result.Diagrams)
	}
	if result.Files[0].Body != `\begin{tikzcd}A\end{tikzcd}` {
		t.Fatalf("expected asset body to remain unchanged, got %q", result.Files[0].Body)
	}
}
