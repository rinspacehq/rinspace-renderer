package typstadapter

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestImageStyleDimensionsAndSecurity(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"width: 24pt", "height: auto; width: 24pt"},
		{"display: block; width: 25%", "display: block; height: auto; width: 25%"},
		{"height: 34.0157pt", "height: 34.0157pt; width: auto"},
		{"width: 32px; height: 40px; object-fit: cover", "height: 40px; object-fit: cover; width: 32px"},
		{"display: inline; width: 0", "display: inline; height: auto; width: 0"},
		{"width: calc(50% + 1em - 12pt)", "height: auto; width: calc(50% + 1em - 12pt)"},
		{"image-rendering: pixelated; width: 24pt", "height: auto; image-rendering: pixelated; width: 24pt"},
		{"", ""},
	} {
		got, err := sanitizeImageStyle(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, raw := range []string{
		"width: url(https://evil.invalid/track)", "width: expression(alert(1))",
		"width: 24pt !important", "width: -24pt", "width: 1e30px",
		"position: fixed", "background: url(//evil.invalid)",
		"width: 24pt; width: 40pt", "w\\69dth: 24pt", "width: 24pt/*x*/",
		"display: none", "--size: 24pt", "width: var(--size)", "invalid",
		"width: calc(50% + url(x))", "width: calc(24pt * 100)",
	} {
		if _, err := sanitizeImageStyle(raw); err == nil {
			t.Fatalf("unsafe or unsupported image CSS accepted: %q", raw)
		}
	}
}

func TestAdaptHTMLKeepsRasterDimensionsAndExactBytes(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 6, 8))); err != nil {
		t.Fatal(err)
	}
	source := `<html><body><img width="6" height="8" style="width: 24pt" src="data:image/png;base64,` + base64.StdEncoding.EncodeToString(data.Bytes()) + `"></body></html>`
	result, err := AdaptHTML([]byte(source), "article", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Assets) != 1 || result.Assets[0].MediaType != "image/png" || !bytes.Equal(result.Assets[0].Body, data.Bytes()) || !strings.Contains(result.Pages[0].HTML, `style="height: auto; width: 24pt"`) {
		t.Fatalf("raster image bytes or dimensions lost: %#v", result)
	}
}

func TestAdaptHTMLRetainsImageDimensionsInArticlesAndBooks(t *testing.T) {
	asset := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 600 800"><rect width="600" height="800"/></svg>`)
	image := `<img src="data:image/svg+xml;base64,` + base64.StdEncoding.EncodeToString(asset) + `" width="600" height="800" style="width: 24pt">`
	for _, mode := range []string{"article", "book"} {
		result, err := AdaptHTML([]byte(`<html><body><h2>Images</h2><p>`+image+image+`</p></body></html>`), mode, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Pages) != 1 || len(result.Assets) != 1 || strings.Count(result.Pages[0].HTML, `style="height: auto; width: 24pt"`) != 2 || strings.Contains(result.Pages[0].HTML, "position") {
			t.Fatalf("%s lost safe image dimensions or retained global styling: %#v", mode, result)
		}
	}
	if _, err := AdaptHTML([]byte(`<html><body>`+strings.ReplaceAll(image, "width: 24pt", "width: url(https://evil.invalid)")+`</body></html>`), "article", nil); err == nil {
		t.Fatal("unsafe image CSS crossed the final-output boundary")
	}
}
