package contracts

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestEmbeddedImageV3WireVector(t *testing.T) {
	body, err := os.ReadFile("../../../packages/document-contract/fixtures/document-bundle/typst-final-images-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var bundle DocumentBundle
	if err := json.Unmarshal(body, &bundle); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Validate(); err != nil {
		t.Fatal(err)
	}
	if bundle.BundleHash != "d9c0472869a84e85009babc5979d191b50e98a8e0171dfb7f2cc8f1ff4328af4" {
		t.Fatal("wire vector changed")
	}
	for name, mutate := range map[string]func(*DocumentBundle){
		"missing bytes":    func(b *DocumentBundle) { b.Assets[0].BodyBase64 = "" },
		"digest":           func(b *DocumentBundle) { b.Assets[0].SHA256 = strings.Repeat("b", 64) },
		"length":           func(b *DocumentBundle) { b.Assets[0].Bytes++ },
		"media":            func(b *DocumentBundle) { b.Assets[0].MediaType = "image/png" },
		"path traversal":   func(b *DocumentBundle) { b.Assets[0].ProjectPath = "../escape.svg" },
		"wrong version":    func(b *DocumentBundle) { b.SchemaVersion = DocumentBundleSchemaVersionV1 },
		"wrong format":     func(b *DocumentBundle) { b.ContentKind = ContentKindMarkdown },
		"no deduplication": func(b *DocumentBundle) { a := b.Assets[0]; a.ID = "asset-2"; b.Assets = append(b.Assets, a) },
		"changed html":     func(b *DocumentBundle) { b.Pages[0].Fragment = "<p>tampered</p>" },
		"unbounded bytes":  func(b *DocumentBundle) { b.Assets[0].Bytes = EmbeddedAssetMaxBytes + 1 },
		"semantic claim":   func(b *DocumentBundle) { b.Pages[0].Blocks = []DocumentBlock{} },
	} {
		t.Run(name, func(t *testing.T) {
			var changed DocumentBundle
			if err := json.Unmarshal(body, &changed); err != nil {
				t.Fatal(err)
			}
			mutate(&changed)
			if err := changed.Validate(); err == nil {
				t.Fatal("accepted invalid embedded resource bundle")
			}
		})
	}
}
