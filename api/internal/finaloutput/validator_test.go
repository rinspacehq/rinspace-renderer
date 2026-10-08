package finaloutput

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

type fixtureFile struct {
	SchemaVersion    string        `json:"schemaVersion"`
	ValidatorVersion string        `json:"validatorVersion"`
	Cases            []fixtureCase `json:"cases"`
}

func TestLocalDiagramURLRequiresExactDeclaredSVGAndLocalProfile(t *testing.T) {
	hash := strings.Repeat("a", 64)
	id := "diagrams/v1/svg-sha256/aa/" + hash + ".svg"
	artifact := contracts.ArtifactReference{ArtifactID: id, SHA256: hash, Bytes: 10, MediaType: "image/svg+xml; charset=utf-8", Visibility: "public"}
	base := "http://127.0.0.1:8090"
	url := base + "/local-assets/" + id
	fragment := `<img src="` + url + `" data-rin-diagram-object-id="` + id + `">`
	if err := Validate(Input{Fragment: fragment, RequiredArtifacts: []contracts.ArtifactReference{artifact}, LocalAssetBaseURL: base}); err != nil {
		t.Fatalf("declared local SVG rejected: %v", err)
	}
	for name, input := range map[string]Input{
		"product profile":     {Fragment: fragment, RequiredArtifacts: []contracts.ArtifactReference{artifact}},
		"other loopback URL":  {Fragment: strings.Replace(fragment, url, "http://127.0.0.1:8091/local-assets/"+id, 1), RequiredArtifacts: []contracts.ArtifactReference{artifact}, LocalAssetBaseURL: base},
		"query suffix":        {Fragment: strings.Replace(fragment, url, url+"?x=1", 1), RequiredArtifacts: []contracts.ArtifactReference{artifact}, LocalAssetBaseURL: base},
		"different artifact":  {Fragment: strings.Replace(fragment, "data-rin-diagram-object-id=\""+id, "data-rin-diagram-object-id=\"other", 1), RequiredArtifacts: []contracts.ArtifactReference{artifact}, LocalAssetBaseURL: base},
		"anchor":              {Fragment: `<a href="` + url + `" data-rin-diagram-object-id="` + id + `">x</a>`, RequiredArtifacts: []contracts.ArtifactReference{artifact}, LocalAssetBaseURL: base},
		"style":               {Fragment: `<img src="` + url + `" data-rin-diagram-object-id="` + id + `" style="background:url(` + url + `)">`, RequiredArtifacts: []contracts.ArtifactReference{artifact}, LocalAssetBaseURL: base},
		"srcset":              {Fragment: `<img src="` + url + `" srcset="` + url + `" data-rin-diagram-object-id="` + id + `">`, RequiredArtifacts: []contracts.ArtifactReference{artifact}, LocalAssetBaseURL: base},
		"non-loopback origin": {Fragment: fragment, RequiredArtifacts: []contracts.ArtifactReference{artifact}, LocalAssetBaseURL: "http://example.com:8090"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(input); err == nil {
				t.Fatal("unsafe local asset reference accepted")
			}
		})
	}
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
