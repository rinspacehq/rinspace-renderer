package renderapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

func canonicalEmbeddedImages(files []AssetFile) ([]contracts.AssetReference, error) {
	if len(files) > contracts.EmbeddedAssetsMaxCount {
		return nil, errors.New("too many embedded image resources")
	}
	assets := make([]contracts.AssetReference, 0, len(files))
	var totalBytes int64
	for index, file := range files {
		if file.Encoding != "base64" || file.Bytes <= 0 || file.Bytes > contracts.EmbeddedAssetMaxBytes || len(file.Body) != base64.StdEncoding.EncodedLen(int(file.Bytes)) {
			return nil, errors.New("embedded image has invalid encoding or size")
		}
		totalBytes += file.Bytes
		if totalBytes > contracts.EmbeddedAssetsMaxBytes {
			return nil, errors.New("embedded images exceed the aggregate byte limit")
		}
		body, err := base64.StdEncoding.Strict().DecodeString(file.Body)
		if err != nil || int64(len(body)) != file.Bytes {
			return nil, errors.New("embedded image length is invalid")
		}
		verified, media, _, err := verifiedMarkdownAsset(body, file.MIME, file.Path)
		if err != nil || !bytes.Equal(verified, body) {
			return nil, errors.New("embedded image is not validated canonical image output")
		}
		media = strings.TrimSpace(strings.Split(media, ";")[0])
		digest := sha256.Sum256(body)
		assets = append(assets, contracts.AssetReference{ID: fmt.Sprintf("asset-%d", index+1), Kind: "embedded",
			ProjectPath: file.Path, SHA256: hex.EncodeToString(digest[:]), Bytes: file.Bytes, MediaType: media, BodyBase64: file.Body})
	}
	return assets, nil
}
