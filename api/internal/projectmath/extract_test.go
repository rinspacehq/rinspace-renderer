package projectmath

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

func TestExtractDisplayMathDelimitersAndEnvironments(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: strings.Join([]string{
			`Before inline $x+y$ stays.`,
			`\[R S = \begin{pmatrix} \cos \frac{\pi}{6}&-\sin \frac{\pi}{6}\\[2pt] \sin \frac{\pi}{6}&\cos \frac{\pi}{6} \end{pmatrix}\]`,
			`\begin{align}`,
			`\mathbf{x}_{1}\pm\mathbf{x}_{2} &= \ket{x_{1}}\pm\ket{x_{2}}\\`,
			`\label{eq:vector}`,
			`\end{align}`,
		}, "\n"),
	}})

	if len(result.Math) != 2 {
		t.Fatalf("expected 2 display math units, got %#v", result.Math)
	}
	if result.Math[0].Environment != "display" || !strings.Contains(result.Math[0].RenderSource, `\begin{pmatrix}`) {
		t.Fatalf("unexpected first unit: %#v", result.Math[0])
	}
	if result.Math[1].Environment != "align" || !strings.HasPrefix(result.Math[1].RenderSource, `\begin{aligned}`) || strings.Contains(result.Math[1].RenderSource, `\label`) {
		t.Fatalf("unexpected align render source: %#v", result.Math[1])
	}
	if result.Math[0].SourceFile != "main.tex" || result.Math[0].SourceLine != 2 || result.Math[0].SourceColumn != 1 ||
		result.Math[1].SourceLine != 3 || result.Math[1].SourceColumn != 1 {
		t.Fatalf("expected adapter-owned source locations, got %#v", result.Math)
	}
	body := result.Files[0].Body
	if !strings.Contains(body, `$x+y$`) {
		t.Fatalf("expected inline math to remain, got %s", body)
	}
	for _, unit := range result.Math {
		if !strings.Contains(body, unit.Placeholder) {
			t.Fatalf("expected placeholder %q in rewritten source: %s", unit.Placeholder, body)
		}
		if strings.Contains(body, unit.Source) {
			t.Fatalf("expected source-level display math to be replaced, got %s", body)
		}
	}
}

func TestExtractAlignatConsumesColumnCount(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `\begin{alignat}{2}a&=b & c&=d\end{alignat}`,
	}})

	if len(result.Math) != 1 {
		t.Fatalf("expected 1 math unit, got %#v", result.Math)
	}
	if result.Math[0].RenderSource != `\begin{aligned}a&=b & c&=d\end{aligned}` {
		t.Fatalf("unexpected alignat render source: %#v", result.Math[0])
	}
}

func TestExtractLeavesDoubleDollarMathForLateXML(t *testing.T) {
	source := `句中 display 有$$\underline{X_0}\cong\operatorname{sk}_0(X),$$后面继续正文。`
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: source,
	}})

	if len(result.Math) != 0 {
		t.Fatalf("expected no source-level extraction for double-dollar math, got %#v", result.Math)
	}
	if result.Files[0].Body != source {
		t.Fatalf("expected double-dollar math to remain unchanged, got %s", result.Files[0].Body)
	}
}

func TestExtractSkipsCommentsAndLineSpacingCommands(t *testing.T) {
	source := strings.Join([]string{
		`% \[commented\]`,
		`a\\[2pt]b`,
		`\[real\]`,
	}, "\n")
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: source,
	}})

	if len(result.Math) != 1 {
		t.Fatalf("expected only real display math to be extracted, got %#v", result.Math)
	}
	body := result.Files[0].Body
	if !strings.Contains(body, `% \[commented\]`) || !strings.Contains(body, `a\\[2pt]b`) {
		t.Fatalf("expected comments and line spacing commands to remain, got %s", body)
	}
	if strings.Contains(body, `\[real\]`) {
		t.Fatalf("expected real display math to be replaced, got %s", body)
	}
}

func TestExtractDoesNotTouchNonTexFiles(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "README.md",
		Kind: "text",
		Body: `\[x+y\]`,
	}})

	if len(result.Math) != 0 || result.Files[0].Body != `\[x+y\]` {
		t.Fatalf("expected non-tex file to remain unchanged, got %#v", result)
	}
}

func TestExtractCarriesSafePreambleMacrosForDisplayMath(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: strings.Join([]string{
			`\providecommand{\calE}{\mathcal{E}}`,
			`\providecommand{\bbZ}{\mathbb{Z}}`,
			`\[\Phi_\calE\colon \bbZ \to \calE\]`,
		}, "\n"),
	}})

	if result.Macros[`\calE`] != `\mathcal{E}` || result.Macros[`\bbZ`] != `\mathbb{Z}` {
		t.Fatalf("expected extracted display math to retain project macros, got %#v", result.Macros)
	}
	if len(result.Math) != 1 || !strings.Contains(result.Math[0].RenderSource, `\calE`) {
		t.Fatalf("unexpected extracted display math: %#v", result.Math)
	}
}

func TestExtractCarriesDeclaredMathOperators(t *testing.T) {
	result := Extract([]projectcore.File{
		{
			Path: "mymath.sty",
			Kind: "style",
			Body: strings.Join([]string{
				`\DeclareMathOperator{\Hom}{Hom}`,
				`\DeclareMathOperator{\PGL}{PGL}`,
				`\DeclareMathOperator*{\argmax}{arg\,max}`,
			}, "\n"),
		},
		{
			Path: "main.tex",
			Kind: "tex",
			Body: strings.Join([]string{
				`\DeclareMathOperator{\Spec}{Spec}`,
				`\[\Hom(T,\bbG_m) = \Spec k\]`,
			}, "\n"),
		},
	})

	for macro, want := range map[string]string{
		`\Hom`:    `\operatorname{Hom}`,
		`\PGL`:    `\operatorname{PGL}`,
		`\argmax`: `\operatorname{arg\,max}`,
		`\Spec`:   `\operatorname{Spec}`,
	} {
		if got := result.Macros[macro]; got != want {
			t.Fatalf("expected %s to resolve to %q, got %q (all: %#v)", macro, want, got, result.Macros)
		}
	}
}

func TestExtractIgnoresOperatorDeclarationsWithArguments(t *testing.T) {
	result := Extract([]projectcore.File{{
		Path: "main.tex",
		Kind: "tex",
		Body: `\DeclareMathOperator{\bad}{#1}`,
	}})
	if _, ok := result.Macros[`\bad`]; ok {
		t.Fatalf("expected argument-taking operator declaration to be ignored, got %#v", result.Macros)
	}
}
