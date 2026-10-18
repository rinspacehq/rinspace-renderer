package typstadapter

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// Compiler-emitted raster images retain their exact bytes and declared MIME,
// rather than being rejected or incorrectly published as SVG. Bound dimensions
// before a browser is allowed to decode the object.
func normalizeRaster(body []byte, mediaType, sourcePath string, assets map[string]Asset) (string, error) {
	formats := map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif", "image/webp": "webp"}
	want, ok := formats[mediaType]
	if !ok {
		return "", fmt.Errorf("Typst embedded raster media type is unsupported: %s", mediaType)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || format != want || config.Width < 1 || config.Height < 1 ||
		int64(config.Width)*int64(config.Height) > 40_000_000 {
		return "", fmt.Errorf("Typst embedded raster image is invalid or too large")
	}
	hash := sha256.Sum256(body)
	name := fmt.Sprintf("typst-assets/%x.%s", hash, want)
	assets[name] = Asset{Path: name, SourcePath: sourcePath, Body: body, MediaType: mediaType}
	return name, nil
}
