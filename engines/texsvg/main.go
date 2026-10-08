package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultAddr            = ":8091"
	defaultMaxDiagramBytes = 128 * 1024
	defaultRenderTimeout   = 25 * time.Second
	defaultCommandTimeout  = 20 * time.Second
	defaultEngineVersion   = "rin-diagram-go-v1"
)

type config struct {
	addr           string
	token          string
	maxBodyBytes   int64
	renderTimeout  time.Duration
	commandTimeout time.Duration
	pdfLaTeX       string
	xeLaTeX        string
	dvisvgm        string
	dvisvgmArgs    []string
	engineVersion  string
}

type diagramRequest struct {
	Type    string `json:"type"`
	Options string `json:"options"`
	Body    string `json:"body"`
	Source  string `json:"source"`
}

type diagramResponse struct {
	ID          string              `json:"id"`
	URL         string              `json:"url"`
	SVG         string              `json:"svg"`
	Cached      bool                `json:"cached"`
	Type        string              `json:"type"`
	Diagnostics []diagramDiagnostic `json:"diagnostics,omitempty"`
}

type diagramDiagnostic struct {
	Severity string            `json:"severity"`
	Code     string            `json:"code"`
	Message  string            `json:"message"`
	Source   map[string]string `json:"source,omitempty"`
}

var (
	errPayloadTooLarge = errors.New("request payload too large")
	errEmptyDiagram    = errors.New("empty diagram body")
	errUnauthorized    = errors.New("unauthorized")
)

func main() {
	cfg := loadConfig()
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"service": "rin-renderer-texsvg-worker",
			"version": cfg.engineVersion,
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		kind, ok := normalizedDiagramTypeFromPath(r.URL.Path)
		if !ok {
			writeError(w, http.StatusBadRequest, "unsupported diagram type")
			return
		}
		if err := requireToken(cfg.token, r); err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}

		payload, err := readPayload(r.Body, cfg.maxBodyBytes)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		var req diagramRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		req.Type = strings.TrimSpace(req.Type)
		if req.Type != "" {
			normalized, ok := normalizeDiagramType(req.Type)
			if !ok {
				writeError(w, http.StatusBadRequest, "unsupported diagram type")
				return
			}
			kind = normalized
		}

		req.Options = strings.TrimSpace(req.Options)
		req.Body = strings.TrimSpace(req.Body)
		req.Source = strings.TrimSpace(req.Source)
		if req.Body == "" && req.Source == "" {
			writeError(w, http.StatusBadRequest, errEmptyDiagram.Error())
			return
		}

		renderCtx, cancel := context.WithTimeout(r.Context(), cfg.renderTimeout)
		defer cancel()
		resp, err := renderDiagram(renderCtx, cfg, kind, req.Options, req.Body, req.Source)
		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				writeError(w, http.StatusGatewayTimeout, "diagram render timeout")
			case errors.Is(err, errPayloadTooLarge), errors.Is(err, errEmptyDiagram), errors.Is(err, errUnauthorized):
				writeError(w, http.StatusBadRequest, err.Error())
			default:
				log.Printf("diagram render failed: %v", err)
				writeError(w, http.StatusBadGateway, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("rin-renderer-texsvg-worker listening on %s", cfg.addr)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

func loadConfig() config {
	addr := strings.TrimSpace(os.Getenv("RIN_RENDERER_ADDR"))
	if addr == "" {
		addr = defaultAddr
	}

	maxBodyBytes := envInt64("RIN_RENDERER_TEXSVG_MAX_BODY_BYTES", defaultMaxDiagramBytes)
	renderTimeout := envDurationSeconds("RIN_RENDERER_TEXSVG_RENDER_TIMEOUT_SECONDS", defaultRenderTimeout)
	commandTimeout := envDurationSeconds("RIN_RENDERER_TEXSVG_COMMAND_TIMEOUT_SECONDS", defaultCommandTimeout)
	if commandTimeout <= 0 {
		commandTimeout = defaultCommandTimeout
	}
	if commandTimeout > renderTimeout {
		commandTimeout = renderTimeout
	}

	dvisvgmArgs := []string{"--pdf", "--no-fonts", "--exact", "--bbox=min", "-n"}
	if raw := strings.TrimSpace(os.Getenv("RIN_RENDERER_DVISVGM_ARGS")); raw != "" {
		dvisvgmArgs = splitCommandArgs(raw)
	}

	engineVersion := strings.TrimSpace(os.Getenv("RIN_RENDERER_TEXSVG_ENGINE_ID"))
	if engineVersion == "" {
		engineVersion = defaultEngineVersion
	}

	return config{
		addr:           addr,
		token:          strings.TrimSpace(os.Getenv("RIN_RENDERER_SERVICE_TOKEN")),
		maxBodyBytes:   maxBodyBytes,
		renderTimeout:  renderTimeout,
		commandTimeout: commandTimeout,
		pdfLaTeX:       firstNonEmpty(os.Getenv("RIN_RENDERER_PDFLATEX_BIN"), "pdflatex"),
		xeLaTeX:        firstNonEmpty(os.Getenv("RIN_RENDERER_XELATEX_BIN"), "xelatex"),
		dvisvgm:        firstNonEmpty(os.Getenv("RIN_RENDERER_DVISVGM_BIN"), "dvisvgm"),
		dvisvgmArgs:    dvisvgmArgs,
		engineVersion:  engineVersion,
	}
}

func splitCommandArgs(raw string) []string {
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		parts = strings.Split(raw, ",")
	}
	args := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			args = append(args, part)
		}
	}
	if len(args) == 0 {
		return []string{"--pdf", "--no-fonts", "--exact", "--bbox=min", "-n"}
	}
	return args
}

func normalizedDiagramTypeFromPath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" || path == "/" || path == "/health" || path == "/api/render/diagrams" {
		return "", false
	}
	const legacyPrefix = "/api/render/diagrams/"
	var kind string
	if strings.HasPrefix(path, legacyPrefix) {
		kind = strings.TrimSpace(path[len(legacyPrefix):])
	} else {
		kind = strings.TrimPrefix(path, "/")
	}
	unescapedKind, err := url.PathUnescape(kind)
	if err != nil {
		return "", false
	}
	kind = unescapedKind
	if kind == "" {
		return "", false
	}
	return normalizeDiagramType(kind)
}

func normalizeDiagramType(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	aliases := map[string]string{
		"tikz":           "tikzpicture",
		"tikzpicture":    "tikzpicture",
		"tikzcd":         "tikzcd",
		"tikz-cd":        "tikzcd",
		"axis":           "axis",
		"pgfplots":       "axis",
		"pspicture":      "pspicture",
		"xymatrix":       "xymatrix",
		"xy":             "xymatrix",
		"cd":             "amscd",
		"amscd":          "amscd",
		"picture":        "picture",
		"forest":         "forest",
		"circuitikz":     "circuitikz",
		"chemfig":        "chemfig",
		"chemfig-scheme": "chemfig-scheme",
		"scheme":         "chemfig-scheme",
		"math":           "math",
	}
	normalized, ok := aliases[value]
	return normalized, ok
}

func readPayload(r io.Reader, max int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > max {
		return nil, errPayloadTooLarge
	}
	return payload, nil
}

func requireToken(token string, r *http.Request) error {
	if token == "" {
		return nil
	}
	headerToken := strings.TrimSpace(r.Header.Get("X-Rin-Renderer-Token"))
	if headerToken == "" {
		headerToken = strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(headerToken), "bearer ") {
			headerToken = strings.TrimSpace(headerToken[7:])
		}
	}
	if headerToken != token {
		return errUnauthorized
	}
	return nil
}

func renderDiagram(ctx context.Context, cfg config, kind, options, body, source string) (diagramResponse, error) {
	bodySource := normalizeDiagramSource(kind, options, body, source)
	if bodySource == "" {
		return diagramResponse{}, errEmptyDiagram
	}

	useXe := shouldUseXeLaTeX(bodySource)
	compiler := cfg.pdfLaTeX
	if useXe {
		compiler = cfg.xeLaTeX
	}
	texSource := buildTeXSource(kind, bodySource, useXe)

	workDir, err := os.MkdirTemp("", "rin-texsvg-")
	if err != nil {
		return diagramResponse{}, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(workDir)
	}()

	texPath := filepath.Join(workDir, "diagram.tex")
	if err := os.WriteFile(texPath, []byte(texSource), 0o600); err != nil {
		return diagramResponse{}, fmt.Errorf("write tex source: %w", err)
	}

	commandEnv := os.Environ()
	commandEnv = append(commandEnv, "openin_any=p", "openout_any=p")

	pdfPath := filepath.Join(workDir, "diagram.pdf")
	compileCtx, cancel := context.WithTimeout(ctx, cfg.commandTimeout)
	defer cancel()
	if err := runCommand(
		compileCtx,
		compiler,
		texCompileArgs(texPath),
		workDir,
		commandEnv,
	); err != nil {
		return diagramResponse{}, err
	}
	if _, err := os.Stat(pdfPath); err != nil {
		return diagramResponse{}, fmt.Errorf("compiled PDF not found: %w", err)
	}

	svgPath := filepath.Join(workDir, "diagram.svg")
	convertCtx, cancel := context.WithTimeout(ctx, cfg.commandTimeout)
	defer cancel()
	if err := runCommand(
		convertCtx,
		cfg.dvisvgm,
		append(append([]string{}, cfg.dvisvgmArgs...), "-o", svgPath, pdfPath),
		workDir,
		commandEnv,
	); err != nil {
		return diagramResponse{}, err
	}

	svgBytes, err := os.ReadFile(svgPath)
	if err != nil {
		return diagramResponse{}, fmt.Errorf("read svg: %w", err)
	}
	svg := strings.TrimSpace(string(svgBytes))
	if svg == "" {
		return diagramResponse{}, errors.New("rendered SVG is empty")
	}
	svg = tightenRenderedSVG(svg)
	svgBytes = []byte(svg)

	hash := sha256.Sum256(svgBytes)
	sum := hex.EncodeToString(hash[:])
	return diagramResponse{
		ID:   "diagram-" + sum[:12],
		URL:  "/diagrams/" + sum[:2] + "/" + sum + ".svg",
		SVG:  svg,
		Type: kind,
	}, nil
}

type svgBounds struct {
	MinX float64
	MinY float64
	MaxX float64
	MaxY float64
	Set  bool
}

type svgTransform struct {
	A float64
	B float64
	C float64
	D float64
	E float64
	F float64
}

type svgFrame struct {
	Transform svgTransform
	Hidden    bool
	InDefs    bool
}

type svgPathToken struct {
	Command   byte
	Number    float64
	IsCommand bool
}

func tightenRenderedSVG(svg string) string {
	bounds, ok := renderedSVGPathBounds(svg)
	if !ok {
		return svg
	}
	const padding = 1.0
	minX := bounds.MinX - padding
	minY := bounds.MinY - padding
	maxX := bounds.MaxX + padding
	maxY := bounds.MaxY + padding
	width := maxX - minX
	height := maxY - minY
	if width <= 0 || height <= 0 {
		return svg
	}
	svgStart := regexp.MustCompile(`(?is)<svg\b`).FindStringIndex(svg)
	if svgStart == nil {
		return svg
	}
	openEnd := strings.IndexByte(svg[svgStart[0]:], '>')
	if openEnd < 0 {
		return svg
	}
	openEnd += svgStart[0]
	openTag := svg[svgStart[0] : openEnd+1]
	openTag = setOrAddSVGAttribute(openTag, "viewBox", strings.Join([]string{
		formatSVGFloat(minX),
		formatSVGFloat(minY),
		formatSVGFloat(width),
		formatSVGFloat(height),
	}, " "))
	openTag = setOrAddSVGAttribute(openTag, "width", formatSVGFloat(width)+"pt")
	openTag = setOrAddSVGAttribute(openTag, "height", formatSVGFloat(height)+"pt")
	return svg[:svgStart[0]] + openTag + svg[openEnd+1:]
}

func renderedSVGPathBounds(svg string) (svgBounds, bool) {
	bounds := svgBounds{
		MinX: math.Inf(1),
		MinY: math.Inf(1),
		MaxX: math.Inf(-1),
		MaxY: math.Inf(-1),
	}
	defs := map[string]svgBounds{}
	decoder := xml.NewDecoder(strings.NewReader(svg))
	stack := []svgFrame{{
		Transform: identitySVGTransform(),
	}}
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return svgBounds{}, false
		}
		switch typed := token.(type) {
		case xml.StartElement:
			parent := stack[len(stack)-1]
			name := strings.ToLower(typed.Name.Local)
			transform := parent.Transform.multiply(parseSVGTransform(svgXMLAttrValue(typed.Attr, "transform")))
			frame := svgFrame{
				Transform: transform,
				Hidden:    parent.Hidden || isHiddenSVGElementForBounds(name, typed.Attr),
				InDefs:    parent.InDefs || name == "defs",
			}
			stack = append(stack, frame)
			switch name {
			case "path":
				pathBounds, ok := svgPathDataBounds(svgXMLAttrValue(typed.Attr, "d"))
				if !ok {
					continue
				}
				if id := strings.TrimSpace(svgXMLAttrValue(typed.Attr, "id")); id != "" {
					defs["#"+id] = pathBounds
				}
				if !frame.Hidden && !frame.InDefs {
					bounds.includeTransformedBounds(pathBounds, frame.Transform)
				}
			case "line":
				lineBounds := svgBounds{
					MinX: math.Inf(1),
					MinY: math.Inf(1),
					MaxX: math.Inf(-1),
					MaxY: math.Inf(-1),
				}
				lineBounds.include(svgLengthNumber(svgXMLAttrValue(typed.Attr, "x1")), svgLengthNumber(svgXMLAttrValue(typed.Attr, "y1")))
				lineBounds.include(svgLengthNumber(svgXMLAttrValue(typed.Attr, "x2")), svgLengthNumber(svgXMLAttrValue(typed.Attr, "y2")))
				if !frame.Hidden && !frame.InDefs {
					bounds.includeTransformedBounds(lineBounds, frame.Transform)
				}
			case "rect":
				rectBounds := svgBounds{
					MinX: math.Inf(1),
					MinY: math.Inf(1),
					MaxX: math.Inf(-1),
					MaxY: math.Inf(-1),
				}
				x := svgLengthNumber(svgXMLAttrValue(typed.Attr, "x"))
				y := svgLengthNumber(svgXMLAttrValue(typed.Attr, "y"))
				width := svgLengthNumber(svgXMLAttrValue(typed.Attr, "width"))
				height := svgLengthNumber(svgXMLAttrValue(typed.Attr, "height"))
				if width > 0 && height > 0 {
					rectBounds.include(x, y)
					rectBounds.include(x+width, y+height)
					if !frame.Hidden && !frame.InDefs {
						bounds.includeTransformedBounds(rectBounds, frame.Transform)
					}
				}
			case "circle", "ellipse":
				shapeBounds := svgBounds{
					MinX: math.Inf(1),
					MinY: math.Inf(1),
					MaxX: math.Inf(-1),
					MaxY: math.Inf(-1),
				}
				cx := svgLengthNumber(svgXMLAttrValue(typed.Attr, "cx"))
				cy := svgLengthNumber(svgXMLAttrValue(typed.Attr, "cy"))
				rx := svgLengthNumber(svgXMLAttrValue(typed.Attr, "rx"))
				ry := svgLengthNumber(svgXMLAttrValue(typed.Attr, "ry"))
				if name == "circle" {
					rx = svgLengthNumber(svgXMLAttrValue(typed.Attr, "r"))
					ry = rx
				}
				if rx > 0 && ry > 0 {
					shapeBounds.include(cx-rx, cy-ry)
					shapeBounds.include(cx+rx, cy+ry)
					if !frame.Hidden && !frame.InDefs {
						bounds.includeTransformedBounds(shapeBounds, frame.Transform)
					}
				}
			case "polyline", "polygon":
				pointBounds, ok := svgPointListBounds(svgXMLAttrValue(typed.Attr, "points"))
				if ok && !frame.Hidden && !frame.InDefs {
					bounds.includeTransformedBounds(pointBounds, frame.Transform)
				}
			case "use":
				if frame.Hidden || frame.InDefs {
					continue
				}
				href := svgXMLAttrValue(typed.Attr, "href")
				if href == "" {
					href = svgXMLAttrValue(typed.Attr, "xlink:href")
				}
				referenced, ok := defs[href]
				if !ok {
					continue
				}
				x := svgLengthNumber(svgXMLAttrValue(typed.Attr, "x"))
				y := svgLengthNumber(svgXMLAttrValue(typed.Attr, "y"))
				bounds.includeTransformedBounds(referenced.translated(x, y), frame.Transform)
			}
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if !bounds.Set {
		return svgBounds{}, false
	}
	return bounds, true
}

func (b *svgBounds) include(x float64, y float64) {
	if !b.Set {
		b.MinX, b.MaxX = x, x
		b.MinY, b.MaxY = y, y
		b.Set = true
		return
	}
	if x < b.MinX {
		b.MinX = x
	}
	if x > b.MaxX {
		b.MaxX = x
	}
	if y < b.MinY {
		b.MinY = y
	}
	if y > b.MaxY {
		b.MaxY = y
	}
}

func (b *svgBounds) includeTransformedBounds(other svgBounds, transform svgTransform) {
	if !other.Set {
		return
	}
	for _, point := range [][2]float64{
		{other.MinX, other.MinY},
		{other.MinX, other.MaxY},
		{other.MaxX, other.MinY},
		{other.MaxX, other.MaxY},
	} {
		x, y := transform.apply(point[0], point[1])
		b.include(x, y)
	}
}

func (b svgBounds) translated(x float64, y float64) svgBounds {
	if !b.Set {
		return b
	}
	return svgBounds{
		MinX: b.MinX + x,
		MinY: b.MinY + y,
		MaxX: b.MaxX + x,
		MaxY: b.MaxY + y,
		Set:  true,
	}
}

func identitySVGTransform() svgTransform {
	return svgTransform{A: 1, D: 1}
}

func (left svgTransform) multiply(right svgTransform) svgTransform {
	return svgTransform{
		A: left.A*right.A + left.C*right.B,
		B: left.B*right.A + left.D*right.B,
		C: left.A*right.C + left.C*right.D,
		D: left.B*right.C + left.D*right.D,
		E: left.A*right.E + left.C*right.F + left.E,
		F: left.B*right.E + left.D*right.F + left.F,
	}
}

func (transform svgTransform) apply(x float64, y float64) (float64, float64) {
	return transform.A*x + transform.C*y + transform.E, transform.B*x + transform.D*y + transform.F
}

func parseSVGTransform(raw string) svgTransform {
	result := identitySVGTransform()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return result
	}
	transformPattern := regexp.MustCompile(`(?is)(matrix|translate|scale|rotate)\s*\(([^)]*)\)`)
	for _, match := range transformPattern.FindAllStringSubmatch(raw, -1) {
		if len(match) < 3 {
			continue
		}
		values := svgNumberList(match[2])
		if len(values) == 0 {
			continue
		}
		next := identitySVGTransform()
		switch strings.ToLower(match[1]) {
		case "matrix":
			if len(values) < 6 {
				continue
			}
			next = svgTransform{A: values[0], B: values[1], C: values[2], D: values[3], E: values[4], F: values[5]}
		case "translate":
			next.E = values[0]
			if len(values) > 1 {
				next.F = values[1]
			}
		case "scale":
			next.A = values[0]
			next.D = values[0]
			if len(values) > 1 {
				next.D = values[1]
			}
		case "rotate":
			radians := values[0] * math.Pi / 180
			cosine := math.Cos(radians)
			sine := math.Sin(radians)
			rotation := svgTransform{A: cosine, B: sine, C: -sine, D: cosine}
			if len(values) >= 3 {
				toOrigin := svgTransform{A: 1, D: 1, E: -values[1], F: -values[2]}
				back := svgTransform{A: 1, D: 1, E: values[1], F: values[2]}
				next = back.multiply(rotation).multiply(toOrigin)
			} else {
				next = rotation
			}
		}
		result = result.multiply(next)
	}
	return result
}

func svgXMLAttrValue(attrs []xml.Attr, name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, attr := range attrs {
		local := strings.ToLower(attr.Name.Local)
		if local == name || (name == "xlink:href" && local == "href") {
			return strings.TrimSpace(attr.Value)
		}
	}
	return ""
}

func isHiddenSVGElementForBounds(name string, attrs []xml.Attr) bool {
	switch name {
	case "defs", "metadata", "script", "style", "title", "desc", "clippath", "mask", "pattern",
		"lineargradient", "radialgradient", "marker", "symbol", "filter":
		return true
	}
	for _, attr := range attrs {
		value := strings.ToLower(strings.TrimSpace(attr.Value))
		switch strings.ToLower(attr.Name.Local) {
		case "display":
			if value == "none" {
				return true
			}
		case "visibility":
			if value == "hidden" || value == "collapse" {
				return true
			}
		case "opacity":
			if value == "0" || value == "0.0" || value == ".0" {
				return true
			}
		case "style":
			compact := compactSVGStyle(value)
			if strings.Contains(compact, "display:none") || strings.Contains(compact, "visibility:hidden") || strings.Contains(compact, "opacity:0") {
				return true
			}
		}
	}
	return false
}

func svgPathDataBounds(pathData string) (svgBounds, bool) {
	tokens := tokenizeSVGPathData(pathData)
	bounds := svgBounds{
		MinX: math.Inf(1),
		MinY: math.Inf(1),
		MaxX: math.Inf(-1),
		MaxY: math.Inf(-1),
	}
	var command byte
	var currentX, currentY, startX, startY float64
	index := 0
	for index < len(tokens) {
		if tokens[index].IsCommand {
			command = tokens[index].Command
			index++
			if upperSVGPathCommand(command) == 'Z' {
				currentX, currentY = startX, startY
				bounds.include(currentX, currentY)
				command = 0
				continue
			}
		}
		if command == 0 {
			index++
			continue
		}
		relative := command >= 'a' && command <= 'z'
		switch upperSVGPathCommand(command) {
		case 'M':
			values, ok := readSVGPathValues(tokens, &index, 2)
			if !ok {
				index = advanceBadSVGPathToken(tokens, index)
				continue
			}
			x, y := values[0], values[1]
			if relative {
				x += currentX
				y += currentY
			}
			currentX, currentY = x, y
			startX, startY = x, y
			bounds.include(currentX, currentY)
			if relative {
				command = 'l'
			} else {
				command = 'L'
			}
		case 'L', 'T':
			values, ok := readSVGPathValues(tokens, &index, 2)
			if !ok {
				index = advanceBadSVGPathToken(tokens, index)
				continue
			}
			x, y := values[0], values[1]
			if relative {
				x += currentX
				y += currentY
			}
			currentX, currentY = x, y
			bounds.include(currentX, currentY)
		case 'H':
			values, ok := readSVGPathValues(tokens, &index, 1)
			if !ok {
				index = advanceBadSVGPathToken(tokens, index)
				continue
			}
			x := values[0]
			if relative {
				x += currentX
			}
			currentX = x
			bounds.include(currentX, currentY)
		case 'V':
			values, ok := readSVGPathValues(tokens, &index, 1)
			if !ok {
				index = advanceBadSVGPathToken(tokens, index)
				continue
			}
			y := values[0]
			if relative {
				y += currentY
			}
			currentY = y
			bounds.include(currentX, currentY)
		case 'C':
			values, ok := readSVGPathValues(tokens, &index, 6)
			if !ok {
				index = advanceBadSVGPathToken(tokens, index)
				continue
			}
			includeCurvePoints(&bounds, relative, currentX, currentY, values[:6]...)
			currentX, currentY = absoluteSVGPathPoint(relative, currentX, currentY, values[4], values[5])
		case 'S', 'Q':
			values, ok := readSVGPathValues(tokens, &index, 4)
			if !ok {
				index = advanceBadSVGPathToken(tokens, index)
				continue
			}
			includeCurvePoints(&bounds, relative, currentX, currentY, values[:4]...)
			currentX, currentY = absoluteSVGPathPoint(relative, currentX, currentY, values[2], values[3])
		case 'A':
			values, ok := readSVGPathValues(tokens, &index, 7)
			if !ok {
				index = advanceBadSVGPathToken(tokens, index)
				continue
			}
			endX, endY := absoluteSVGPathPoint(relative, currentX, currentY, values[5], values[6])
			rx := math.Abs(values[0])
			ry := math.Abs(values[1])
			bounds.include(currentX-rx, currentY-ry)
			bounds.include(currentX+rx, currentY+ry)
			bounds.include(endX-rx, endY-ry)
			bounds.include(endX+rx, endY+ry)
			currentX, currentY = endX, endY
		default:
			index = advanceBadSVGPathToken(tokens, index)
		}
	}
	if !bounds.Set {
		return svgBounds{}, false
	}
	return bounds, true
}

func includeCurvePoints(bounds *svgBounds, relative bool, currentX float64, currentY float64, values ...float64) {
	for index := 0; index+1 < len(values); index += 2 {
		x, y := absoluteSVGPathPoint(relative, currentX, currentY, values[index], values[index+1])
		bounds.include(x, y)
	}
}

func absoluteSVGPathPoint(relative bool, currentX float64, currentY float64, x float64, y float64) (float64, float64) {
	if relative {
		return currentX + x, currentY + y
	}
	return x, y
}

func readSVGPathValues(tokens []svgPathToken, index *int, count int) ([]float64, bool) {
	if *index+count > len(tokens) {
		return nil, false
	}
	values := make([]float64, count)
	for offset := 0; offset < count; offset++ {
		token := tokens[*index+offset]
		if token.IsCommand {
			return nil, false
		}
		values[offset] = token.Number
	}
	*index += count
	return values, true
}

func advanceBadSVGPathToken(tokens []svgPathToken, index int) int {
	if index < len(tokens) && !tokens[index].IsCommand {
		return index + 1
	}
	return index
}

func tokenizeSVGPathData(pathData string) []svgPathToken {
	tokens := make([]svgPathToken, 0)
	for index := 0; index < len(pathData); {
		ch := pathData[index]
		if ch == ',' || ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == '\f' {
			index++
			continue
		}
		if isSVGPathCommand(ch) {
			tokens = append(tokens, svgPathToken{Command: ch, IsCommand: true})
			index++
			continue
		}
		if isSVGNumberStart(ch) {
			raw, next, ok := scanSVGNumber(pathData, index)
			if ok {
				if value, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
					tokens = append(tokens, svgPathToken{Number: value})
				}
				index = next
				continue
			}
		}
		index++
	}
	return tokens
}

func scanSVGNumber(value string, start int) (string, int, bool) {
	index := start
	if index < len(value) && (value[index] == '+' || value[index] == '-') {
		index++
	}
	digits := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		index++
		digits++
	}
	if index < len(value) && value[index] == '.' {
		index++
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
			digits++
		}
	}
	if digits == 0 {
		return "", start, false
	}
	if index < len(value) && (value[index] == 'e' || value[index] == 'E') {
		expStart := index
		index++
		if index < len(value) && (value[index] == '+' || value[index] == '-') {
			index++
		}
		expDigits := 0
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
			expDigits++
		}
		if expDigits == 0 {
			index = expStart
		}
	}
	return value[start:index], index, true
}

func isSVGNumberStart(ch byte) bool {
	return ch == '+' || ch == '-' || ch == '.' || ch >= '0' && ch <= '9'
}

func isSVGPathCommand(ch byte) bool {
	switch ch {
	case 'M', 'm', 'Z', 'z', 'L', 'l', 'H', 'h', 'V', 'v', 'C', 'c', 'S', 's', 'Q', 'q', 'T', 't', 'A', 'a':
		return true
	default:
		return false
	}
}

func upperSVGPathCommand(command byte) byte {
	if command >= 'a' && command <= 'z' {
		return command - ('a' - 'A')
	}
	return command
}

func svgPointListBounds(points string) (svgBounds, bool) {
	values := svgNumberList(points)
	if len(values) < 2 {
		return svgBounds{}, false
	}
	bounds := svgBounds{
		MinX: math.Inf(1),
		MinY: math.Inf(1),
		MaxX: math.Inf(-1),
		MaxY: math.Inf(-1),
	}
	for index := 0; index+1 < len(values); index += 2 {
		bounds.include(values[index], values[index+1])
	}
	if !bounds.Set {
		return svgBounds{}, false
	}
	return bounds, true
}

func svgLengthNumber(value string) float64 {
	values := svgNumberList(value)
	if len(values) == 0 {
		return 0
	}
	return values[0]
}

func svgNumberList(raw string) []float64 {
	numberPattern := regexp.MustCompile(`[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?`)
	matches := numberPattern.FindAllString(raw, -1)
	values := make([]float64, 0, len(matches))
	for _, match := range matches {
		value, err := strconv.ParseFloat(match, 64)
		if err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
			values = append(values, value)
		}
	}
	return values
}

func compactSVGStyle(value string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(value) {
		if char == ' ' || char == '\t' || char == '\r' || char == '\n' || char == '\f' {
			continue
		}
		builder.WriteRune(char)
	}
	return builder.String()
}

func setOrAddSVGAttribute(tag string, attr string, value string) string {
	pattern := regexp.MustCompile(`(?is)\s` + regexp.QuoteMeta(attr) + `\s*=\s*("[^"]*"|'[^']*'|[^\s"'=<>` + "`" + `]+)`)
	if pattern.FindStringIndex(tag) != nil {
		return pattern.ReplaceAllString(tag, ` `+attr+`="`+value+`"`)
	}
	closeIndex := strings.LastIndex(tag, ">")
	if closeIndex < 0 {
		return tag
	}
	return tag[:closeIndex] + ` ` + attr + `="` + value + `"` + tag[closeIndex:]
}

func formatSVGFloat(value float64) string {
	if math.Abs(value) < 0.000001 {
		value = 0
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func shouldUseXeLaTeX(value string) bool {
	for _, r := range value {
		if r >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

func buildTeXSource(kind, diagramSource string, useXe bool) string {
	if kind == "math" {
		return buildMathTeXSource(diagramSource, useXe)
	}
	packages := []string{"amsmath", "amssymb", "mathrsfs", "tikz"}
	packages = append(packages, packagesForDiagramKind(kind)...)
	if useXe {
		packages = append(packages, "fontspec")
	}

	seen := make(map[string]struct{}, len(packages))
	uniq := make([]string, 0, len(packages))
	for _, pkg := range packages {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" {
			continue
		}
		if _, exists := seen[pkg]; exists {
			continue
		}
		seen[pkg] = struct{}{}
		uniq = append(uniq, pkg)
	}

	libraries := uniqueNonEmpty(tikzLibrariesForDiagramKind(kind))
	libraryLine := ""
	if len(libraries) > 0 {
		libraryLine = fmt.Sprintf("\\usetikzlibrary{%s}", strings.Join(libraries, ","))
	}

	return fmt.Sprintf(`\documentclass{article}
\pagestyle{empty}
\usepackage{%s}
%s
\setlength{\topmargin}{-0.8in}
\setlength{\oddsidemargin}{-0.8in}
\setlength{\evensidemargin}{-0.8in}
\setlength{\textwidth}{7.5in}
\setlength{\textheight}{10.5in}
\begin{document}
%s
\end{document}
`, strings.Join(uniq, ","), libraryLine, diagramSource)
}

func buildMathTeXSource(mathSource string, useXe bool) string {
	packages := []string{"amsmath", "amssymb", "mathtools", "bm", "graphicx", "xcolor"}
	if useXe {
		packages = append(packages, "fontspec")
	}
	return fmt.Sprintf(`\documentclass{article}
\pagestyle{empty}
\usepackage{%s}
\providecommand{\ket}[1]{\left|#1\right\rangle}
\providecommand{\bra}[1]{\left\langle#1\right|}
\providecommand{\braket}[2]{\left\langle#1\,\middle|\,#2\right\rangle}
\providecommand{\norm}[1]{\left\lVert#1\right\rVert}
\providecommand{\abs}[1]{\left\lvert#1\right\rvert}
\providecommand{\innerproduct}[2]{\left\langle#1,#2\right\rangle}
\providecommand{\xlongrightarrow}[1]{\xrightarrow{#1}}
\makeatletter
\providecommand{\xlongequal}{\@ifnextchar[{\rintexsvg@xlongequal@opt}{\rintexsvg@xlongequal@plain}}
\providecommand{\rintexsvg@xlongequal@opt}[2][]{\mathrel{\mathop{=}\limits^{#2}_{#1}}}
\providecommand{\rintexsvg@xlongequal@plain}[1]{\mathrel{\mathop{=}\limits^{#1}}}
\makeatother
\setlength{\topmargin}{-0.8in}
\setlength{\oddsidemargin}{-0.8in}
\setlength{\evensidemargin}{-0.8in}
\setlength{\textwidth}{7.5in}
\setlength{\textheight}{10.5in}
\begin{document}
%s
\end{document}
`, strings.Join(packages, ","), mathSource)
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	uniq := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		uniq = append(uniq, value)
	}
	return uniq
}

func texCompileArgs(texPath string) []string {
	pdfBaseName := filepath.Base(texPath)
	baseExt := filepath.Ext(pdfBaseName)
	jobName := "diagram"
	if baseExt != "" {
		jobName = strings.TrimSuffix(pdfBaseName, baseExt)
	}
	texArg := filepath.Base(texPath)
	return []string{
		"-interaction=nonstopmode",
		"-halt-on-error",
		"-no-shell-escape",
		"-output-directory",
		filepath.Dir(texPath),
		"-jobname",
		jobName,
		texArg,
	}
}

func normalizeDiagramSource(kind, options, body, source string) string {
	body = strings.TrimSpace(body)
	source = strings.TrimSpace(source)
	if kind == "math" {
		mathSource := body
		if mathSource == "" {
			mathSource = source
		}
		if strings.EqualFold(options, "inline") {
			return `\(` + mathSource + `\)`
		}
		return `\[` + mathSource + `\]`
	}
	if source != "" {
		if kind == "axis" {
			normalizedSource := strings.TrimSpace(source)
			if strings.Contains(normalizedSource, "\\begin{tikzpicture}") {
				return normalizedSource
			}
			return "\\begin{tikzpicture}\n" + normalizedSource + "\n\\end{tikzpicture}"
		}
		return source
	}

	switch kind {
	case "axis":
		return buildAxisDiagram(options, body)
	case "xymatrix":
		if strings.HasPrefix(body, "\\xymatrix") {
			return "$" + body + "$"
		}
		return fmt.Sprintf("$\\xymatrix{%s}$", body)
	case "chemfig-scheme":
		return buildWrappedEnvironment("schemestart", options, body)
	case "tikzpicture", "tikzcd", "pspicture", "picture", "forest", "circuitikz", "amscd":
		if kind == "amscd" {
			return buildWrappedEnvironment("CD", options, body)
		}
		return buildWrappedEnvironment(kind, options, body)
	case "chemfig":
		return buildCommandlessEnvironment("chemfig", options, body)
	default:
		return buildWrappedEnvironment("tikzpicture", options, body)
	}
}

func buildAxisDiagram(options, body string) string {
	axisOpen := "\\begin{axis}"
	if options != "" {
		axisOpen = axisOpen + "[" + options + "]"
	}
	return "\\begin{tikzpicture}\n" + axisOpen + "\n" + body + "\n\\end{axis}\n\\end{tikzpicture}"
}

func buildWrappedEnvironment(name, options, body string) string {
	if strings.TrimSpace(options) == "" {
		return fmt.Sprintf("\\begin{%s}\n%s\n\\end{%s}", name, body, name)
	}
	return fmt.Sprintf("\\begin{%s}[%s]\n%s\n\\end{%s}", name, options, body, name)
}

func buildCommandlessEnvironment(name, options, body string) string {
	if strings.TrimSpace(options) == "" {
		return fmt.Sprintf("\\%s{%s}", name, body)
	}
	return fmt.Sprintf("\\%s[%s]{%s}", name, options, body)
}

func packagesForDiagramKind(kind string) []string {
	switch kind {
	case "tikzpicture":
		return []string{"tikz"}
	case "tikzcd":
		return []string{"tikz-cd"}
	case "axis":
		return []string{"pgfplots"}
	case "pspicture":
		return []string{"pstricks", "auto-pst-pdf"}
	case "xymatrix":
		return []string{"xypic"}
	case "chemfig", "chemfig-scheme":
		return []string{"chemfig"}
	case "circuitikz":
		return []string{"circuitikz"}
	case "forest":
		return []string{"forest"}
	case "picture":
		return nil
	case "amscd":
		return []string{"amscd"}
	default:
		return nil
	}
}

func tikzLibrariesForDiagramKind(kind string) []string {
	switch kind {
	case "tikzpicture", "axis", "tikzcd", "circuitikz", "forest":
		// Project diagrams are compiled as isolated documents, so the wrapper
		// must preserve the standard TikZ context commonly declared by the
		// parent project. These libraries are safe to load for every TikZ-based
		// diagram and avoid silently dropping positioning and arrow-tip support.
		return []string{"calc", "positioning", "arrows.meta"}
	default:
		return nil
	}
}

func runCommand(ctx context.Context, command string, args []string, workdir string, env []string) error {
	cmd := exec.Command(command, args...)
	cmd.Dir = workdir
	cmd.Env = env
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := runProcess(ctx, cmd, childCancellationGrace)
	out := output.Bytes()
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 4096 {
			snippet = snippet[len(snippet)-4096:]
		}
		return fmt.Errorf("%s failed: %w: %s", command, err, snippet)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "failed to encode JSON", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func envDurationSeconds(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Second
}

func envInt64(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
