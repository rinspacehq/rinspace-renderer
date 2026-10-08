package diagramservice

import "strings"

var diagramAliases = map[string]string{
	"tikz": "tikzpicture", "tikzpicture": "tikzpicture",
	"tikzcd": "tikzcd", "tikz-cd": "tikzcd",
	"axis": "axis", "pgfplots": "axis",
	"pspicture": "pspicture", "xymatrix": "xymatrix", "xy": "xymatrix",
	"amscd": "amscd", "cd": "amscd", "picture": "picture", "forest": "forest",
	"circuitikz": "circuitikz", "chemfig": "chemfig",
	"chemfig-scheme": "chemfig-scheme", "scheme": "chemfig-scheme",
}

func NormalizeType(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	kind, ok := diagramAliases[value]
	return kind, ok
}

func SupportedTypes() []string {
	return []string{
		"tikzpicture", "tikzcd", "axis", "pgfplots", "pspicture", "xymatrix", "amscd", "picture",
		"forest", "circuitikz", "chemfig", "chemfig-scheme",
	}
}
