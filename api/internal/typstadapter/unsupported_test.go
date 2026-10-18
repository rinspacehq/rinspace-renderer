package typstadapter

import "testing"

func TestUnsupportedHTMLOnlyRecognizesCompilerCapabilityLoss(t *testing.T) {
	for _, log := range []string{
		"main.typ:1:1: warning: cancel was ignored during MathML export",
		"main.typ:1:1: warning: grid was ignored during HTML export",
		"main.typ:1:1: error: custom content is not supported in HTML export",
	} {
		if UnsupportedHTML(log) == "" {
			t.Fatalf("missed capability failure: %s", log)
		}
	}
	for _, log := range []string{"", "warning: unknown font family", "error: unknown variable: x", "error: failed to download package",
		"error: unclosed delimiter\nwarning: grid was ignored during HTML export",
		"warning: grid was ignored during HTML export\nerror: unknown variable: x",
		"error: HTTP request timed out", "HTML export not supported"} {
		if UnsupportedHTML(log) != "" {
			t.Fatalf("real source/dependency failure masked: %s", log)
		}
	}
}
