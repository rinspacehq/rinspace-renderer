package contracts

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPublicationPDFFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/publication-pdf-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name   string               `json:"name"`
		Result PublicationPDFResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil || len(fixtures) < 3 {
		t.Fatalf("invalid publication receipt fixtures: %v", err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			if err := fixture.Result.ValidateFor(fixture.Result.Input); err != nil {
				t.Fatal(err)
			}
		})
	}
}
