package typstpdfexecutor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func typstFixtureDir(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(sourceFile), "../../../packages/protocol/fixtures/typst-pdf")
}

func TestTypstPDFResultFixtures(t *testing.T) {
	dir := typstFixtureDir(t)
	for _, name := range []string{"result-preview.json", "result-export.json"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			var result Result
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatal(err)
			}
			if err := result.Validate(); err != nil {
				t.Fatalf("fixture must validate: %v", err)
			}
			roundTrip, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Result
			if err := json.Unmarshal(roundTrip, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.SchemaVersion != result.SchemaVersion || decoded.OutputKind != result.OutputKind ||
				decoded.IdempotencyKey != result.IdempotencyKey || decoded.TotalArtifactBytes != result.TotalArtifactBytes {
				t.Fatal("Typst PDF result changed during JSON round trip")
			}
		})
	}
}

func TestTypstPDFPreviewIdempotencyKeyMatchesFixture(t *testing.T) {
	dir := typstFixtureDir(t)
	body, err := os.ReadFile(filepath.Join(dir, "result-preview.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	derived := PreviewIdempotencyKey(result.SessionID, result.SnapshotHash, result.Entrypoint, result.EnginePolicyID, result.ImageDigest)
	if derived != result.IdempotencyKey {
		t.Fatalf("derived idempotency key %q does not match fixture %q", derived, result.IdempotencyKey)
	}
}

func TestTypstPDFResultRejectsInvalidVectors(t *testing.T) {
	dir := typstFixtureDir(t)
	manifestBody, err := os.ReadFile(filepath.Join(dir, "invalid-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion string `json:"schemaVersion"`
		Base          string `json:"base"`
		Cases         []struct {
			Name string `json:"name"`
			Path []any  `json:"path"`
			// Value is applied verbatim onto the JSON document.
			Value json.RawMessage `json:"value"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != "rin-typst-pdf-invalid-fixtures/v1" || manifest.Base != "result-preview.json" || len(manifest.Cases) == 0 {
		t.Fatalf("invalid fixture manifest is malformed: %#v", manifest)
	}
	baseBody, err := os.ReadFile(filepath.Join(dir, manifest.Base))
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range manifest.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var document any
			if err := json.Unmarshal(baseBody, &document); err != nil {
				t.Fatal(err)
			}
			if err := setFixturePath(document, fixture.Path, fixture.Value); err != nil {
				t.Fatal(err)
			}
			mutated, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			var result Result
			if err := json.Unmarshal(mutated, &result); err != nil {
				t.Fatalf("mutation must remain decodable: %v", err)
			}
			if err := result.Validate(); err == nil {
				t.Fatalf("mutation %q was not rejected", fixture.Name)
			}
		})
	}
}

func setFixturePath(target any, parts []any, value json.RawMessage) error {
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return err
	}
	cursor := target
	for index, raw := range parts {
		key, ok := raw.(float64)
		last := index == len(parts)-1
		if ok {
			list, listOK := cursor.([]any)
			position := int(key)
			if !listOK || position < 0 || position >= len(list) {
				return errFixturePath
			}
			if last {
				list[position] = decoded
				return nil
			}
			cursor = list[position]
			continue
		}
		name, nameOK := raw.(string)
		object, objectOK := cursor.(map[string]any)
		if !nameOK || !objectOK {
			return errFixturePath
		}
		if last {
			object[name] = decoded
			return nil
		}
		cursor = object[name]
	}
	return errFixturePath
}

var errFixturePath = errFixturePathType{}

type errFixturePathType struct{}

func (errFixturePathType) Error() string { return "invalid fixture mutation path" }
