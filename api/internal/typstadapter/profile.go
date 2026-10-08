package typstadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ProfileSchemaVersion identifies how a Typst HTML render profile id is derived.
const ProfileSchemaVersion = "rin-typst-html-profile/v2"

// AdapterVersion is the semantic version of the Go HTML adapter and sanitizer.
// Bump it whenever the accepted element/attribute/URL policy changes so a new
// renderProfileId is produced and stale cached results are not reused.
const AdapterVersion = "rin-typst-html-adapter/v1"

// MathPresentation values recorded in a profile.
const (
	MathPresentationNative  = "typst-mathml-native"
	MathPresentationMathJax = "mathjax-chtml"
)

// Profile is the immutable set of inputs that decide whether a previously
// produced Typst HTML bundle may be reused for the same source commit. It is
// deliberately explicit: compiler, font, sealed package set, adapter/sanitizer,
// final-output contract and math presentation must all match before a cache or
// publication decision can reuse a result.
type Profile struct {
	CompilerVersion    string
	CompilerHash       string
	FontHash           string
	PackageHash        string
	AdapterVersion     string
	FinalOutputVersion string
	MathPresentation   string
}

// ID returns a stable, content-addressed profile identifier. Unknown or empty
// fields collapse to a fixed placeholder so an incomplete profile still yields
// a deterministic (never silently reused) id.
func (profile Profile) ID() string {
	parts := []string{
		ProfileSchemaVersion,
		firstNonEmpty(profile.CompilerVersion, "compiler-unreported"),
		firstNonEmpty(strings.ToLower(profile.CompilerHash), "compiler-hash-unreported"),
		firstNonEmpty(strings.ToLower(profile.FontHash), "font-hash-unreported"),
		firstNonEmpty(strings.ToLower(profile.PackageHash), "package-hash-unreported"),
		firstNonEmpty(profile.AdapterVersion, AdapterVersion),
		firstNonEmpty(profile.FinalOutputVersion, "final-output-unreported"),
		firstNonEmpty(profile.MathPresentation, MathPresentationNative),
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "typst-html-" + hex.EncodeToString(digest[:16])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
