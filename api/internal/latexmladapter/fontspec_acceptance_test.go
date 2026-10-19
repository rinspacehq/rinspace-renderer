package latexmladapter

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

const fontspecAcceptanceSource = `\documentclass{article}
\usepackage{fontspec}
\setmainfont{Libertinus Serif}
\setsansfont[Scale=MatchLowercase]{TeX Gyre Heros}
\setmonofont{Latin Modern Mono}
\newfontfamily\testfamily[Scale=MatchLowercase]{FandolSong}
\newfontface\testface{Libertinus Serif}
\begin{document}
FontFamilyBody regular. {\sffamily SansBody.} {\ttfamily MonoBody.}
{\testfamily 中文字体正文。} {\testface FaceBody.}
\end{document}`

func TestFontspecDeclarationsRetainContentAndDefineNamedFontCommands(t *testing.T) {
	files, err := normalizeXeCJKSources([]projectcore.File{{Path: "main.tex", Kind: "tex", Body: fontspecAcceptanceSource}}, "main.tex")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{`\let\setsansfont\setmainfont`, `\let\setmonofont\setmainfont`, `\def\newfontfamily`, `\let\newfontface\newfontfamily`, "FontFamilyBody", "SansBody", "MonoBody", "中文字体正文", "FaceBody"} {
		if !strings.Contains(files[0].Body, command) {
			t.Fatalf("font compatibility lost %q", command)
		}
	}
}

func TestFontspecDeclarationsWithRealLateXMLRuntime(t *testing.T) {
	if os.Getenv("RIN_RENDERER_FONT_RUNTIME_TEST") != "1" {
		t.Skip("requires the pinned LaTeXML runtime; executed separately in the offline runtime sandbox")
	}
	files := []projectcore.File{{Path: "main.tex", Kind: "tex", Body: fontspecAcceptanceSource}}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "Synthetic fonts"})
	result, err := (Adapter{Config: Config{Binary: "/usr/local/bin/latexmlc", Timeout: 90 * time.Second, MaxHTMLBytes: 2 << 20}}).Render(context.Background(), Request{Files: files, Manifest: manifest, Title: "Synthetic fonts"})
	if err != nil {
		t.Fatalf("real LaTeXML failed: %v; %#v", err, result.Diagnostics)
	}
	if err := finaloutput.ValidateRenderingErrors(result.HTML); err != nil {
		t.Fatalf("font declarations produced error markup: %v; %#v", err, result.Diagnostics)
	}
	for _, text := range []string{"FontFamilyBody", "SansBody", "MonoBody", "中文字体正文", "FaceBody"} {
		if !strings.Contains(result.HTML, text) {
			t.Fatalf("real LaTeXML lost authored content %q", text)
		}
	}
}
