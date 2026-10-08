package orchestration

import (
	"strings"
	"testing"
)

func TestBuildCacheKeyCoversEveryStageAndRequiredVersion(t *testing.T) {
	for _, stage := range CacheStages() {
		stage := stage
		t.Run(string(stage), func(t *testing.T) {
			versions := map[string]string{}
			for _, name := range RequiredCacheVersions(stage) {
				versions[name] = name + "/v1"
			}
			input := CacheKeyInput{
				Stage: stage,
				NormalizedInputs: map[string]string{
					"normalized-source": "private source must never enter the printable key",
					"options-hash":      strings.Repeat("a", 64),
				},
				ProjectContext: map[string]string{"project-hash": strings.Repeat("b", 64)},
				Versions:       versions,
			}
			key, err := BuildCacheKey(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := key.Validate(); err != nil || !strings.HasPrefix(key.String(), "rin-cache/v1/"+string(stage)+"/") ||
				strings.Contains(key.String(), "private source") {
				t.Fatalf("cache key = %#v, validate = %v", key, err)
			}
			for _, name := range RequiredCacheVersions(stage) {
				changed := cloneStrings(versions)
				changed[name] += ".changed"
				other, err := BuildCacheKey(CacheKeyInput{
					Stage: stage, NormalizedInputs: input.NormalizedInputs,
					ProjectContext: input.ProjectContext, Versions: changed,
				})
				if err != nil || other == key {
					t.Fatalf("version %q did not invalidate %q key: %#v, %v", name, stage, other, err)
				}
			}
		})
	}
}

func TestBuildCacheKeyIsCanonicalAndRejectsIncompleteInputs(t *testing.T) {
	versions := map[string]string{}
	for _, name := range RequiredCacheVersions(CacheStageMath) {
		versions[name] = "v1"
	}
	first, err := BuildCacheKey(CacheKeyInput{
		Stage:            CacheStageMath,
		NormalizedInputs: map[string]string{"source": `\not\exists`, "display": "false"},
		ProjectContext:   map[string]string{"macro-hash": strings.Repeat("c", 64)},
		Versions:         versions,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildCacheKey(CacheKeyInput{
		Stage:            CacheStageMath,
		NormalizedInputs: map[string]string{"display": "false", "source": `\not\exists`},
		ProjectContext:   map[string]string{"macro-hash": strings.Repeat("c", 64)},
		Versions:         cloneStrings(versions),
	})
	if err != nil || second != first {
		t.Fatalf("canonical keys = %#v / %#v, %v", first, second, err)
	}
	changedSource, err := BuildCacheKey(CacheKeyInput{
		Stage:            CacheStageMath,
		NormalizedInputs: map[string]string{"display": "false", "source": `\exists`},
		ProjectContext:   map[string]string{"macro-hash": strings.Repeat("c", 64)}, Versions: versions,
	})
	if err != nil || changedSource == first {
		t.Fatalf("normalized input did not invalidate key: %#v, %v", changedSource, err)
	}
	changedContext, err := BuildCacheKey(CacheKeyInput{
		Stage:            CacheStageMath,
		NormalizedInputs: map[string]string{"display": "false", "source": `\not\exists`},
		ProjectContext:   map[string]string{"macro-hash": strings.Repeat("d", 64)}, Versions: versions,
	})
	if err != nil || changedContext == first {
		t.Fatalf("project context did not invalidate key: %#v, %v", changedContext, err)
	}
	missing := cloneStrings(versions)
	delete(missing, "font")
	if _, err := BuildCacheKey(CacheKeyInput{Stage: CacheStageMath, NormalizedInputs: map[string]string{"source": "x"}, Versions: missing}); err == nil {
		t.Fatal("missing font version was accepted")
	}
	for _, invalid := range []CacheKeyInput{
		{Stage: "unknown", NormalizedInputs: map[string]string{"source": "x"}, Versions: versions},
		{Stage: CacheStageMath, Versions: versions},
		{Stage: CacheStageMath, NormalizedInputs: map[string]string{"Bad Name": "x"}, Versions: versions},
		{Stage: CacheStageMath, NormalizedInputs: map[string]string{"source": ""}, Versions: versions},
	} {
		if _, err := BuildCacheKey(invalid); err == nil {
			t.Fatalf("invalid cache input was accepted: %#v", invalid)
		}
	}
}

func TestCacheArtifactIDIsStageScopedAndContentAddressed(t *testing.T) {
	body := []byte("immutable cache body")
	mathID, err := CacheArtifactID(CacheStageMath, body)
	parts := strings.Split(mathID, "/")
	if err != nil || len(parts) != 5 || strings.Join(parts[:3], "/") != "render-cache/v1/math" ||
		len(parts[4]) != 64 || parts[3] != parts[4][:2] {
		t.Fatalf("math cache artifact ID = %q, %v", mathID, err)
	}
	second, err := CacheArtifactID(CacheStageMath, append([]byte(nil), body...))
	if err != nil || second != mathID {
		t.Fatalf("cache artifact IDs are not stable: %q / %q, %v", mathID, second, err)
	}
	diagramID, err := CacheArtifactID(CacheStageDiagram, body)
	if err != nil || diagramID == mathID {
		t.Fatalf("stage did not scope cache artifact: %q / %q, %v", mathID, diagramID, err)
	}
	if _, err := CacheArtifactID("unknown", body); err == nil {
		t.Fatal("unknown cache artifact stage was accepted")
	}
}

func cloneStrings(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for name, value := range values {
		cloned[name] = value
	}
	return cloned
}
