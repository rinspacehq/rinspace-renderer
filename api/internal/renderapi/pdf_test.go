package renderapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfinspect"
)

func TestPDFInspectionEndpointValidatesAndDoesNotReencode(t *testing.T) {
	server := NewServer(testConfig())
	body := validInspectionPDF()
	request := httptest.NewRequest(http.MethodPost, "/api/render/pdf/inspect", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/pdf")
	request.Header.Set("X-Rin-Renderer-Token", "test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PDF inspection failed: %d %s", response.Code, response.Body.String())
	}
	result := pdfinspect.Result{}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != pdfinspect.SchemaVersion || result.PageCount != 1 || result.OriginalReencoded || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("unexpected PDF inspection response: %#v headers=%v", result, response.Header())
	}

	unsafeRequest := httptest.NewRequest(http.MethodPost, "/api/render/pdf/inspect", bytes.NewReader(activeInspectionPDF()))
	unsafeRequest.Header.Set("X-Rin-Renderer-Token", "test-token")
	unsafeResponse := httptest.NewRecorder()
	server.ServeHTTP(unsafeResponse, unsafeRequest)
	if unsafeResponse.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unsafe PDF was not quarantined: %d %s", unsafeResponse.Code, unsafeResponse.Body.String())
	}
}

func activeInspectionPDF() []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	offsets := make([]int, 6)
	for index, object := range []string{
		"<< /Type /Catalog /Pages 2 0 R /OpenAction 5 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
		"<< /S /JavaScript /JS (app.alert\\(1\\)) >>",
	} {
		offsets[index+1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := body.Len()
	body.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for index := 1; index <= 5; index++ {
		fmt.Fprintf(&body, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&body, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return body.Bytes()
}

func validInspectionPDF() []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	offsets := make([]int, 5)
	for index, object := range []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	} {
		offsets[index+1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := body.Len()
	body.WriteString("xref\n0 5\n0000000000 65535 f \n")
	for index := 1; index <= 4; index++ {
		fmt.Fprintf(&body, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&body, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return body.Bytes()
}
