package jobapi

import (
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

func TestDocumentFailureDetailNeverBlamesAnImportWarning(t *testing.T) {
	failure := jobresult.StoredFailure{Error: "Document rendering failed", Diagnostics: []renderapi.Diagnostic{
		{Severity: "warning", Code: "project.file.unsupported", Message: "skip unsupported file: .gitea/ISSUE_TEMPLATE/config.yml"},
		{Severity: "error", Code: "latexml.undefined", Message: "Undefined control sequence: newfontfamily"},
	}}
	if detail := documentFailureDetail(failure); detail != "Undefined control sequence: newfontfamily" {
		t.Fatalf("wrong engine detail: %q", detail)
	}
	value := publicJobFailure(jobpostgres.Job{}, failure)
	if message, _ := value["message"].(string); strings.Contains(message, "skip unsupported") || !strings.Contains(message, "newfontfamily") {
		t.Fatalf("misleading public failure: %#v", value)
	}
	failure.Diagnostics = failure.Diagnostics[:1]
	if detail := documentFailureDetail(failure); detail != "" {
		t.Fatalf("warning is not a compiler failure: %q", detail)
	}
}
