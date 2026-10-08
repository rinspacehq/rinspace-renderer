package jobresult

import "testing"

func TestStoredFailureRequiresSafeHTTPFailure(t *testing.T) {
	exitCode := 12
	failure := StoredFailure{SchemaVersion: StoredFailureSchemaVersion, Status: 502, Code: "document_render_failed", Error: "render failed", Retryable: true, ExitCode: &exitCode}
	if err := failure.Validate(); err != nil {
		t.Fatal(err)
	}
	failure.Status = 200
	if err := failure.Validate(); err == nil {
		t.Fatal("Validate() accepted successful failure status")
	}
	failure.Status = 502
	exitCode = 256
	if err := failure.Validate(); err == nil {
		t.Fatal("Validate() accepted an invalid process exit code")
	}
}
