package renderapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestTypstGeneratedImagesSurviveCanonicalOnlyDelivery(t *testing.T) {
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><path d="M0 0L10 10"/></svg>`
	html := []byte(`<html><body><h1>Image</h1>` + svg + `</body></html>`)
	result, err := TypstHTMLResult("job-image", "request-image", strings.Repeat("a", 64), "main.typ", "Images", "article", "test", "0.15.1", html, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the actual API: keep only the canonical JSON, not AssetFiles or
	// the compatibility response, which is never sent to the Control Plane.
	encoded, err := json.Marshal(result.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	var delivered struct {
		Inline struct {
			Schema string `json:"schemaVersion"`
			Assets []struct {
				Kind   string `json:"kind"`
				SHA256 string `json:"sha256"`
				Bytes  int    `json:"bytes"`
				Body   string `json:"bodyBase64"`
			} `json:"assets"`
		} `json:"inline"`
	}
	if err := json.Unmarshal(encoded, &delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.Inline.Schema != "rin-document-bundle/v3" || len(delivered.Inline.Assets) != 1 {
		t.Fatalf("canonical delivery has no persistent resource contract: %s", encoded)
	}
	asset := delivered.Inline.Assets[0]
	body, err := base64.StdEncoding.Strict().DecodeString(asset.Body)
	digest := sha256.Sum256(body)
	if err != nil || asset.Kind != "embedded" || len(body) == 0 || asset.Bytes != len(body) || asset.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("canonical resource bytes missing or inconsistent: %#v, %v", asset, err)
	}
}
