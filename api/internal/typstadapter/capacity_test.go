package typstadapter

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestAdaptHTMLAcceptsLargeRepeatedImagesAndDeduplicatesAssets(t *testing.T) {
	// Native HTML repeats data URLs; the published bundle must not repeat the
	// image bytes. Synthetic noise keeps this PNG realistic but deterministic.
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	random := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(random.Uint32())
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	tag := `<img src="data:image/png;base64,` + base64.StdEncoding.EncodeToString(encoded.Bytes()) + `" style="width: 24pt">`
	const count = 420
	source := []byte(`<html><body><h2>Repeated images</h2>` + strings.Repeat(tag, count) + `</body></html>`)
	if len(source) <= 32<<20 || len(source) > MaxNativeHTMLBytes {
		t.Fatalf("regression must cross the old limit: %d bytes", len(source))
	}
	result, err := AdaptHTML(source, "article", map[string][]byte{"assets/tile.png": encoded.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pages) != 1 || len(result.Assets) != 1 || !bytes.Equal(result.Assets[0].Body, encoded.Bytes()) {
		t.Fatal("large native HTML lost the single exact image asset")
	}
	fragment := result.Pages[0].HTML
	if strings.Count(fragment, "<img ") != count || strings.Contains(fragment, "data:image/") || len(fragment) > 256<<10 {
		t.Fatal("large native HTML did not preserve and compact every image")
	}
}

func TestAdaptHTMLRetainsHardNativeOutputCeiling(t *testing.T) {
	_, err := AdaptHTML(make([]byte, MaxNativeHTMLBytes+1), "article", nil)
	if err == nil || !strings.Contains(err.Error(), "67108865 bytes; limit is 67108864 bytes") {
		t.Fatalf("oversized HTML was not rejected precisely: %v", err)
	}
}
