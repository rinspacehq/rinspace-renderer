package contracts

import (
	"os"
	"strings"
	"testing"
)

func TestMarkdownRuntimeVersionsMatchNodeFinalizer(t *testing.T) {
	source, err := os.ReadFile("../../../engines/markdown/finalizer.mjs")
	if err != nil {
		t.Fatal(err)
	}
	for name, version := range map[string]string{
		"MARKDOWN_SANITIZER_VERSION":         MarkdownSanitizerVersion,
		"MARKDOWN_BOOK_CACHE_SCHEMA_VERSION": MarkdownBookPageCacheSchemaVersion,
	} {
		if !strings.Contains(string(source), "export const "+name+" = '"+version+"';") {
			t.Fatalf("Go %s=%s does not match the Node finalizer", name, version)
		}
	}
}
