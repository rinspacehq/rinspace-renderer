package typstadapter

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestEmbeddedRasterKeepsCorrectMIMEAndExactAsset(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	source := `<html><body><h2>Image</h2><img src="data:image/png;base64,` + base64.StdEncoding.EncodeToString(encoded.Bytes()) + `"></body></html>`
	result, err := AdaptHTML([]byte(source), "article", map[string][]byte{"images/example.png": encoded.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Assets) != 1 || result.Assets[0].MediaType != "image/png" || result.Assets[0].SourcePath != "images/example.png" ||
		!strings.HasSuffix(result.Assets[0].Path, ".png") || !bytes.Equal(result.Assets[0].Body, encoded.Bytes()) {
		t.Fatalf("raster was dropped or mislabelled: %#v", result.Assets)
	}
	if _, err := AdaptHTML([]byte(strings.Replace(source, "image/png", "image/jpeg", 1)), "article", nil); err == nil {
		t.Fatal("raster MIME spoof was accepted")
	}
}
