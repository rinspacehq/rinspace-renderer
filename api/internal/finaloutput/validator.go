// Package finaloutput enforces source-format-neutral invariants after an adapter has inserted all
// trusted work-unit output. It is intentionally a validator, not a replacement for an adapter's
// HTML/HAST sanitizer.
package finaloutput

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

const (
	ContractVersion         = "rin-final-output/v1"
	DefaultMaxFragmentBytes = int64(32 << 20)
)

type Input struct {
	Adapter           string
	Fragment          string
	MaxFragmentBytes  int64
	RequiredArtifacts []contracts.ArtifactReference
	ClientOwnedAssets []string
}

type Error struct {
	Code    string
	Message string
}

func (err *Error) Error() string { return err.Message }

func Code(err error) string {
	var invariant *Error
	if errors.As(err, &invariant) {
		return invariant.Code
	}
	return "final_output.invalid"
}

var (
	reservedPlaceholderText = regexp.MustCompile(`(?i)(RINRENDERER[A-Z0-9_-]*(?:PLACEHOLDER|SHAREDMATH)|@@RIN[-_][A-Z0-9_-]*@@)`)
	cssURLPattern           = regexp.MustCompile(`(?is)url\s*\(\s*([^)]*?)\s*\)`)
	windowsPathPattern      = regexp.MustCompile(`(?i)^[a-z]:[\\/]`)
)

var domClobberingNames = map[string]struct{}{
	"__proto__": {}, "constructor": {}, "prototype": {}, "window": {}, "document": {},
	"location": {}, "top": {}, "parent": {}, "self": {}, "frames": {}, "forms": {},
	"images": {}, "scripts": {}, "children": {}, "contentwindow": {}, "contentdocument": {},
}

var forbiddenElements = map[string]struct{}{
	"script": {}, "iframe": {}, "frame": {}, "frameset": {}, "base": {}, "meta": {}, "portal": {},
}

var URLAttributes = map[string]struct{}{
	"href": {}, "src": {}, "xlink:href": {}, "action": {}, "formaction": {}, "poster": {}, "data": {}, "cite": {},
}

func Validate(input Input) error {
	limit := input.MaxFragmentBytes
	if limit <= 0 {
		limit = DefaultMaxFragmentBytes
	}
	if int64(len(input.Fragment)) > limit {
		return reject("final_output.too_large", "final HTML fragment exceeds %d bytes", limit)
	}
	if reservedPlaceholderText.MatchString(input.Fragment) {
		return reject("final_output.unknown_placeholder", "final HTML contains a reserved renderer placeholder")
	}

	artifacts, err := requiredArtifactMap(input.RequiredArtifacts)
	if err != nil {
		return err
	}
	clientAssets, err := clientAssetMap(input.ClientOwnedAssets)
	if err != nil {
		return err
	}
	seenArtifacts := map[string]struct{}{}
	seenClientAssets := map[string]struct{}{}
	seenIDs := map[string]struct{}{}

	tokenizer := html.NewTokenizer(strings.NewReader(input.Fragment))
	styleDepth := 0
	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			if errors.Is(tokenizer.Err(), io.EOF) {
				for artifactID := range artifacts {
					if _, ok := seenArtifacts[artifactID]; !ok {
						return reject("final_output.artifact_unreferenced", "required artifact %q is not referenced by final HTML", artifactID)
					}
				}
				return nil
			}
			return reject("final_output.invalid_html", "final HTML tokenization failed: %v", tokenizer.Err())
		case html.TextToken:
			if styleDepth > 0 {
				if err := validateCSS(string(tokenizer.Text())); err != nil {
					return err
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			tag := strings.ToLower(token.Data)
			if err := renderingError(token); err != nil {
				return err
			}
			if _, forbidden := forbiddenElements[tag]; forbidden {
				return reject("final_output.executable_element", "final HTML contains forbidden <%s> element", tag)
			}
			if tag == "rin-work" || strings.Contains(tag, "placeholder") {
				return reject("final_output.unknown_placeholder", "final HTML contains unresolved <%s> placeholder", tag)
			}
			if tag == "style" && tokenType == html.StartTagToken {
				styleDepth++
			}
			if err := validateAttributes(tag, token.Attr, artifacts, clientAssets, seenArtifacts, seenClientAssets, seenIDs); err != nil {
				return err
			}
		case html.EndTagToken:
			token := tokenizer.Token()
			if strings.EqualFold(token.Data, "style") && styleDepth > 0 {
				styleDepth--
			}
		}
	}
}

// ValidateRenderingErrors checks individual reader pages and already-canonical
// bundles without requiring every whole-document artifact to occur on each page.
// It supplements, and never replaces, the complete fragment's security gate.
func ValidateRenderingErrors(fragment string) error {
	if int64(len(fragment)) > DefaultMaxFragmentBytes {
		return reject("final_output.too_large", "reader page exceeds %d bytes", DefaultMaxFragmentBytes)
	}
	tokenizer := html.NewTokenizer(strings.NewReader(fragment))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if errors.Is(tokenizer.Err(), io.EOF) {
				return nil
			}
			return reject("final_output.invalid_html", "reader HTML tokenization failed: %v", tokenizer.Err())
		case html.StartTagToken, html.SelfClosingTagToken:
			if err := renderingError(tokenizer.Token()); err != nil {
				return err
			}
		}
	}
}

func renderingError(token html.Token) error {
	tag := strings.ToLower(token.Data)
	if tag == "merror" || tag == "mjx-merror" {
		return reject("final_output.render_error", "final HTML contains a <%s> rendering error", tag)
	}
	for _, attribute := range token.Attr {
		if attribute.Key == "class" {
			for _, class := range strings.Fields(attribute.Val) {
				if class == "rin-math-source-fallback" {
					return reject("final_output.render_error", "final HTML contains unrendered math source")
				}
				if class == "ltx_ERROR" || class == "mjx-merror" || class == "katex-error" {
					return reject("final_output.render_error", "final HTML contains a %s rendering error", class)
				}
			}
		}
	}
	return nil
}

func requiredArtifactMap(items []contracts.ArtifactReference) (map[string]contracts.ArtifactReference, error) {
	result := make(map[string]contracts.ArtifactReference, len(items))
	for _, artifact := range items {
		if err := artifact.Validate(); err != nil {
			return nil, reject("final_output.artifact_invalid", "required artifact is invalid: %v", err)
		}
		if artifact.Visibility != "public" || artifact.ExpiresAt != "" {
			return nil, reject("final_output.artifact_invalid", "publishable artifact %q must be immutable and public", artifact.ArtifactID)
		}
		if artifact.Bytes <= 0 {
			return nil, reject("final_output.artifact_invalid", "publishable artifact %q must have a positive byte size", artifact.ArtifactID)
		}
		if strings.HasPrefix(artifact.ArtifactID, "diagrams/v1/svg-sha256/") {
			expectedID := "diagrams/v1/svg-sha256/" + artifact.SHA256[:2] + "/" + artifact.SHA256 + ".svg"
			if artifact.ArtifactID != expectedID || artifact.MediaType != "image/svg+xml; charset=utf-8" {
				return nil, reject("final_output.artifact_invalid", "diagram artifact %q does not match its content-addressed SVG metadata", artifact.ArtifactID)
			}
		}
		if previous, duplicate := result[artifact.ArtifactID]; duplicate && previous != artifact {
			return nil, reject("final_output.artifact_invalid", "artifact %q has conflicting metadata", artifact.ArtifactID)
		}
		result[artifact.ArtifactID] = artifact
	}
	return result, nil
}

func clientAssetMap(items []string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(items))
	for _, item := range items {
		cleaned, ok := contracts.CleanProjectPath(item)
		if !ok || cleaned != item {
			return nil, reject("final_output.client_asset_invalid", "client-owned asset path %q is not canonical", item)
		}
		result[item] = struct{}{}
	}
	return result, nil
}

func validateAttributes(tag string, attrs []html.Attribute, artifacts map[string]contracts.ArtifactReference, clientAssets map[string]struct{}, seenArtifacts, seenClientAssets, seenIDs map[string]struct{}) error {
	values := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		name := strings.ToLower(attr.Key)
		if attr.Namespace != "" {
			name = strings.ToLower(attr.Namespace) + ":" + name
		}
		value := strings.TrimSpace(attr.Val)
		if _, duplicate := values[name]; duplicate {
			return reject("final_output.duplicate_attribute", "final HTML <%s> contains duplicate %q attribute", tag, name)
		}
		values[name] = value
		if strings.HasPrefix(name, "on") {
			return reject("final_output.event_handler", "final HTML contains event handler attribute %q", name)
		}
		if _, ok := URLAttributes[name]; ok {
			if err := validateURL(value, tag, name); err != nil {
				return err
			}
		}
		if (strings.Contains(name, "path") || strings.Contains(name, "file")) && rendererLocalPath(value) {
			return reject("final_output.renderer_local_path", "final HTML <%s> exposes a renderer-local path in %s", tag, name)
		}
		if name == "srcset" {
			if err := validateSrcset(value, tag); err != nil {
				return err
			}
		}
		if name == "style" {
			if err := validateCSS(value); err != nil {
				return err
			}
		}
		if name == "id" || name == "name" {
			normalized := strings.ToLower(value)
			if _, reserved := domClobberingNames[normalized]; reserved {
				return reject("final_output.dom_clobbering_identifier", "final HTML contains DOM-clobbering %s %q", name, value)
			}
			if name == "id" && value != "" {
				if _, duplicate := seenIDs[value]; duplicate {
					return reject("final_output.dom_clobbering_identifier", "final HTML contains duplicate id %q", value)
				}
				seenIDs[value] = struct{}{}
			}
		}
	}

	for _, marker := range []string{"data-rin-diagram-object-id", "data-rin-math-object-id", "data-rin-artifact-id"} {
		if artifactID, present := values[marker]; present {
			if artifactID == "" {
				return reject("final_output.artifact_missing", "final HTML contains an empty %s marker", marker)
			}
			if _, ok := artifacts[artifactID]; !ok {
				return reject("final_output.artifact_missing", "final HTML references undeclared artifact %q", artifactID)
			}
			seenArtifacts[artifactID] = struct{}{}
		}
	}
	if projectPath, present := values["data-rin-asset-path"]; present {
		if projectPath == "" {
			return reject("final_output.client_asset_missing", "final HTML contains an empty client asset marker")
		}
		if _, ok := clientAssets[projectPath]; !ok {
			return reject("final_output.client_asset_missing", "final HTML references asset %q outside the client upload manifest", projectPath)
		}
		seenClientAssets[projectPath] = struct{}{}
	}
	if attribute, raw, required := relativeResourceReference(tag, values); required && !hasResourceDeclaration(values) {
		return reject("final_output.artifact_missing", "final HTML <%s> has an undeclared relative %s resource %q", tag, attribute, raw)
	}
	return nil
}

func relativeResourceReference(tag string, values map[string]string) (string, string, bool) {
	attribute := ""
	switch tag {
	case "img", "embed", "source", "audio", "track", "input":
		attribute = "src"
	case "object":
		attribute = "data"
	case "video":
		attribute = "poster"
	}
	value := strings.TrimSpace(values[attribute])
	if attribute == "" || value == "" || strings.HasPrefix(value, "#") {
		return "", "", false
	}
	parsed, err := url.Parse(value)
	return attribute, value, err == nil && parsed.Scheme == "" && parsed.Host == ""
}

func hasResourceDeclaration(values map[string]string) bool {
	for _, marker := range []string{"data-rin-asset-path", "data-rin-diagram-object-id", "data-rin-math-object-id", "data-rin-artifact-id"} {
		if strings.TrimSpace(values[marker]) != "" {
			return true
		}
	}
	return false
}

func validateURL(raw, tag, attribute string) error {
	value := strings.TrimSpace(raw)
	if value == "" || strings.HasPrefix(value, "#") {
		return nil
	}
	if containsControl(value) {
		return reject("final_output.unsafe_url", "final HTML <%s> has a control character in %s", tag, attribute)
	}
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
	if strings.HasPrefix(compact, "//") {
		return reject("final_output.unsafe_url", "final HTML <%s> uses a protocol-relative %s", tag, attribute)
	}
	if rendererLocalPath(value) {
		return reject("final_output.renderer_local_path", "final HTML <%s> exposes a renderer-local path in %s", tag, attribute)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return reject("final_output.unsafe_url", "final HTML <%s> has an invalid %s URL", tag, attribute)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "" && scheme != "http" && scheme != "https" && scheme != "mailto" && scheme != "tel" {
		return reject("final_output.unsafe_url", "final HTML <%s> uses disallowed URL scheme %q", tag, scheme)
	}
	if (scheme == "http" || scheme == "https") && localHost(parsed.Hostname()) {
		return reject("final_output.renderer_local_path", "final HTML <%s> exposes renderer-local host %q", tag, parsed.Hostname())
	}
	if scheme == "" && escapesProjectRoot(parsed.Path) {
		return reject("final_output.renderer_local_path", "final HTML <%s> URL escapes the project root", tag)
	}
	return nil
}

func validateSrcset(value, tag string) error {
	for _, candidate := range strings.Split(value, ",") {
		fields := strings.Fields(strings.TrimSpace(candidate))
		if len(fields) == 0 {
			continue
		}
		if err := validateURL(fields[0], tag, "srcset"); err != nil {
			return err
		}
	}
	return nil
}

func validateCSS(value string) error {
	lower := strings.ToLower(strings.Map(func(r rune) rune {
		if r == 0 || r == '\r' || r == '\n' || r == '\f' {
			return -1
		}
		return r
	}, value))
	for _, forbidden := range []string{"expression(", "@import", "javascript:", "vbscript:", "-moz-binding", "behavior:"} {
		if strings.Contains(lower, forbidden) {
			return reject("final_output.unsafe_style", "final HTML contains unsafe CSS construct %q", forbidden)
		}
	}
	for _, match := range cssURLPattern.FindAllStringSubmatch(value, -1) {
		raw := strings.Trim(strings.TrimSpace(match[1]), "\"'")
		if err := validateURL(raw, "style", "url()"); err != nil {
			return err
		}
	}
	return nil
}

func rendererLocalPath(value string) bool {
	lower := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	if windowsPathPattern.MatchString(value) || strings.HasPrefix(lower, "file:") {
		return true
	}
	for _, prefix := range []string{"/tmp/", "/var/tmp/", "/home/", "/root/", "/proc/", "/sys/", "/dev/", "/workspace/"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func localHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasPrefix(host, "rin-renderer") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast())
}

func escapesProjectRoot(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") {
		return false
	}
	depth := 0
	for _, segment := range strings.Split(strings.ReplaceAll(value, "\\", "/"), "/") {
		switch segment {
		case "", ".":
		case "..":
			depth--
		default:
			depth++
		}
		if depth < 0 {
			return true
		}
	}
	return strings.HasPrefix(path.Clean(value), "../")
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func reject(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
