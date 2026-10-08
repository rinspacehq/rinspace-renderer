package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestControlPlaneCompletionSharedVectors(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve shared completion contract path")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "packages", "protocol", "contracts", "rin-control-plane", "v1", "renderer-completion.test-vectors.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		SchemaVersion string `json:"schemaVersion"`
		Vectors       []struct {
			Name  string          `json:"name"`
			Valid bool            `json:"valid"`
			Value json.RawMessage `json:"value"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.SchemaVersion != "rin-renderer-completion/v2-test-vectors" {
		t.Fatalf("unexpected shared completion schema %q", vectors.SchemaVersion)
	}
	for _, vector := range vectors.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			_, err := ParseControlPlaneCompletionEvent(vector.Value)
			if vector.Valid && err != nil {
				t.Fatalf("valid completion rejected: %v", err)
			}
			if !vector.Valid && err == nil {
				t.Fatal("invalid completion accepted")
			}
		})
	}
}
