package finaloutput

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

type fixtureFile struct {
	SchemaVersion    string        `json:"schemaVersion"`
	ValidatorVersion string        `json:"validatorVersion"`
	Cases            []fixtureCase `json:"cases"`
}

type fixtureCase struct {
	Name              string                        `json:"name"`
	Fragment          string                        `json:"fragment"`
	MaxFragmentBytes  int64                         `json:"maxFragmentBytes,omitempty"`
	RequiredArtifacts []contracts.ArtifactReference `json:"requiredArtifacts,omitempty"`
	ClientOwnedAssets []string                      `json:"clientOwnedAssets,omitempty"`
	WantCode          string                        `json:"wantCode,omitempty"`
}

func TestSharedSecurityOutputFixtures(t *testing.T) {
	body, err := os.ReadFile("../../../packages/final-output/fixtures/security-output.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures fixtureFile
	if err := json.Unmarshal(body, &fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures.SchemaVersion != "rin-final-output-fixtures/v1" || fixtures.ValidatorVersion != ContractVersion || len(fixtures.Cases) < 15 {
		t.Fatalf("unexpected fixture contract: %#v", fixtures)
	}
	for _, current := range fixtures.Cases {
		current := current
		t.Run(current.Name, func(t *testing.T) {
			err := Validate(Input{
				Adapter: current.Name, Fragment: current.Fragment, MaxFragmentBytes: current.MaxFragmentBytes,
				RequiredArtifacts: current.RequiredArtifacts, ClientOwnedAssets: current.ClientOwnedAssets,
			})
			if current.WantCode == "" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			if err == nil || Code(err) != current.WantCode {
				t.Fatalf("Validate() = %v (%s), want %s", err, Code(err), current.WantCode)
			}
		})
	}
}

func TestValidateRejectsConflictingArtifactMetadata(t *testing.T) {
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	id := "diagrams/v1/svg-sha256/aa/" + hash + ".svg"
	base := contracts.ArtifactReference{ArtifactID: id, SHA256: hash, Bytes: 10, MediaType: "image/svg+xml", Visibility: "public"}
	conflict := base
	conflict.Bytes++
	err := Validate(Input{Fragment: `<img data-rin-artifact-id="` + id + `">`, RequiredArtifacts: []contracts.ArtifactReference{base, conflict}})
	if err == nil || Code(err) != "final_output.artifact_invalid" {
		t.Fatalf("Validate() = %v", err)
	}
}
