package renderstorage

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"path"
	"strings"

	_ "golang.org/x/image/webp"
)

const (
	repositoryImageMaxDimension = 16384
	repositoryImageMaxPixels    = 64 * 1024 * 1024
)

// VerifyRepositoryImage validates repository media bytes and normalizes SVGs.
func VerifyRepositoryImage(body []byte, mediaType, projectPath string) ([]byte, string, string, error) {
	mediaType = strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
	extension := strings.ToLower(path.Ext(projectPath))
	valid := false
	switch extension {
	case ".png":
		valid = mediaType == "image/png" && len(body) >= 8 && bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	case ".jpg", ".jpeg":
		valid, extension = mediaType == "image/jpeg" && len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[2] == 0xff, ".jpg"
	case ".gif":
		valid = mediaType == "image/gif" && len(body) >= 6 && (bytes.Equal(body[:6], []byte("GIF87a")) || bytes.Equal(body[:6], []byte("GIF89a")))
	case ".webp":
		valid = mediaType == "image/webp" && len(body) >= 12 && bytes.Equal(body[:4], []byte("RIFF")) && bytes.Equal(body[8:12], []byte("WEBP"))
	case ".svg":
		if mediaType != "image/svg+xml" {
			break
		}
		normalized, err := NormalizeSVG(body)
		if err != nil {
			return nil, "", "", fmt.Errorf("Markdown SVG %q is unsafe: %w", projectPath, err)
		}
		return normalized, SVGContentType, extension, nil
	}
	if !valid {
		return nil, "", "", fmt.Errorf("Markdown asset %q does not match its media type", projectPath)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	expectedFormat := map[string]string{".png": "png", ".jpg": "jpeg", ".gif": "gif", ".webp": "webp"}[extension]
	if err != nil || format != expectedFormat || config.Width <= 0 || config.Height <= 0 ||
		config.Width > repositoryImageMaxDimension || config.Height > repositoryImageMaxDimension ||
		int64(config.Width)*int64(config.Height) > repositoryImageMaxPixels {
		return nil, "", "", fmt.Errorf("Markdown asset %q has invalid or excessive dimensions", projectPath)
	}
	return body, mediaType, extension, nil
}
