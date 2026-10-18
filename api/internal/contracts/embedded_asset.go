package contracts

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"

	"golang.org/x/net/html"
)

const (
	DocumentBundleSchemaVersionV3 = "rin-document-bundle/v3"
	EmbeddedAssetMaxBytes         = 16 << 20
	EmbeddedAssetsMaxBytes        = 32 << 20
	EmbeddedAssetsMaxCount        = 4096
)

// DecodeEmbeddedAsset verifies the wire bytes, not an ambient file or URL.
// Sanitization belongs to the producing image adapter; this contract binds its
// already-validated output to an immutable, self-contained publication.
func DecodeEmbeddedAsset(asset AssetReference) ([]byte, error) {
	if asset.Kind != "embedded" || asset.ArtifactKey != "" || validateCanonicalPath(asset.ProjectPath) != nil ||
		asset.Bytes <= 0 || asset.Bytes > EmbeddedAssetMaxBytes || !isSHA256(asset.SHA256) ||
		len(asset.BodyBase64) != base64.StdEncoding.EncodedLen(int(asset.Bytes)) {
		return nil, errors.New("embedded image identity, path or size is invalid")
	}
	media := map[string]string{".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif"}[path.Ext(asset.ProjectPath)]
	if media == "" || media != asset.MediaType {
		return nil, errors.New("embedded image type does not match its path")
	}
	if asset.ProjectPath != "typst-assets/"+asset.SHA256+path.Ext(asset.ProjectPath) {
		return nil, errors.New("embedded image path is not content-addressed")
	}
	body, err := base64.StdEncoding.Strict().DecodeString(asset.BodyBase64)
	if err != nil || len(body) != int(asset.Bytes) || base64.StdEncoding.EncodeToString(body) != asset.BodyBase64 {
		return nil, errors.New("embedded image encoding or length is invalid")
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != asset.SHA256 {
		return nil, errors.New("embedded image digest does not match its bytes")
	}
	return body, nil
}

func validateEmbeddedReferences(pages []DocumentPage, assets []AssetReference) error {
	paths, seen := map[string]bool{}, map[string]bool{}
	for _, asset := range assets {
		paths[asset.ProjectPath] = true
	}
	for _, page := range pages {
		tokens := html.NewTokenizer(strings.NewReader(page.Fragment))
		for {
			kind := tokens.Next()
			if kind == html.ErrorToken {
				if err := tokens.Err(); err != io.EOF {
					return err
				}
				break
			}
			if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
				continue
			}
			token := tokens.Token()
			if token.Data != "img" {
				continue
			}
			var source, marker string
			sourceCount, markerCount := 0, 0
			for _, attr := range token.Attr {
				if attr.Key == "src" {
					source = attr.Val
					sourceCount++
				}
				if attr.Key == "data-rin-asset-path" {
					marker = attr.Val
					markerCount++
				}
			}
			if sourceCount != 1 || markerCount != 1 || source != marker || !paths[marker] {
				return errors.New("embedded image reference does not match its manifest")
			}
			seen[marker] = true
		}
	}
	if len(seen) != len(paths) {
		return errors.New("embedded image manifest has unused resources")
	}
	return nil
}

// EmbeddedBundleHash uses sorted JSON object keys with bundleHash="". It is
// deliberately versioned: the existing v1/v2 hash algorithms do not change.
func EmbeddedBundleHash(bundle DocumentBundle) (string, error) {
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return "", err
	}
	value["bundleHash"] = ""
	encoded, err = json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
