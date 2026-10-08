package renderstorage

import (
	"strings"
	"testing"
)

func TestNormalizeSVGRequiresNonEmptyViewBox(t *testing.T) {
	normalized, err := NormalizeSVG([]byte(`<svg width="12pt" height="8pt"><path d="M0 0L1 1"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(normalized), `viewBox="0 0 12 8"`) {
		t.Fatalf("expected derived viewBox, got %s", normalized)
	}

	if _, err := NormalizeSVG([]byte(`<svg><path d="M0 0L1 1"/></svg>`)); err == nil {
		t.Fatal("expected SVG without viewBox or dimensions to fail")
	}
	if _, err := NormalizeSVG([]byte(`<svg viewBox="   "><path d="M0 0L1 1"/></svg>`)); err == nil {
		t.Fatal("expected SVG with blank viewBox to fail")
	}
}

func TestNormalizeSVGStripsUnsafeContentAndHelperText(t *testing.T) {
	raw := []byte(`<?xml version="1.0"?>
<svg viewBox="0 0 10 10" onload="alert(1)" xmlns:xlink="http://www.w3.org/1999/xlink">
  <script>alert(1)</script>
  <a href="javascript:alert(1)" xlink:href="javascript:alert(2)">
    <animate attributeName="href" values="javascript:alert(3)" />
  </a>
  <text class="rin-renderer-helper">RIN_RENDERER_HELPER</text>
  <path d="M0 0L10 10" onclick="alert(4)" style="stroke:#000"/>
</svg>`)

	normalized, err := NormalizeSVG(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := string(normalized)
	for _, unexpected := range []string{
		"<script",
		"<animate",
		"javascript:",
		"onload=",
		"onclick=",
		"RIN_RENDERER_HELPER",
		"rin-renderer-helper",
	} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(unexpected)) {
			t.Fatalf("expected normalized SVG to remove %q, got %s", unexpected, got)
		}
	}
	if !strings.Contains(got, `<path`) || !strings.Contains(got, `viewBox="0 0 10 10"`) {
		t.Fatalf("expected visible path and viewBox to remain, got %s", got)
	}
}

func TestNormalizeSVGRejectsInvisibleSVG(t *testing.T) {
	for _, raw := range []string{
		`<svg viewBox="0 0 10 10"><defs><path id="p" d="M0 0L1 1"/></defs></svg>`,
		`<svg viewBox="0 0 10 10"><path display="none" d="M0 0L1 1"/></svg>`,
		`<svg viewBox="0 0 10 10"><text class="rin-texsvg-helper">helper</text></svg>`,
	} {
		if _, err := NormalizeSVG([]byte(raw)); err == nil {
			t.Fatalf("expected invisible SVG to fail: %s", raw)
		}
	}
}

func TestNormalizeSVGObjectUsesNormalizedBytesForHash(t *testing.T) {
	a, err := NormalizeSVGObject([]byte(" \r\n<svg height=\"10\" width=\"10\">\r\n  <path d=\"M0 0L10 10\"/>\r\n</svg>\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeSVGObject([]byte(`<svg width="10" height="10"><path d="M0 0L10 10"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Bytes) != string(b.Bytes) {
		t.Fatalf("expected deterministic normalized bytes:\na=%s\nb=%s", a.Bytes, b.Bytes)
	}
	if a.Hash != b.Hash || a.ObjectID != b.ObjectID {
		t.Fatalf("expected deterministic hash/object ID: a=%#v b=%#v", a, b)
	}
	if a.Hash != SVGHash(a.Bytes) || a.ObjectID != SVGObjectID(a.Bytes) {
		t.Fatalf("expected hash/object ID to be based on normalized bytes: %#v", a)
	}
}
