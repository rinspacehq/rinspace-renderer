package jobapi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFragmentJobRequiresDistinctConfiguredProfile(t *testing.T) {
	for _, test := range []struct {
		name, profile, configured, options string
		want                               int
	}{
		{"disabled", "typst-fragments-reviewed", "", `{"typstFragments":{"templateVersion":"typst-fragment-template/v1","blockIds":["one"]}}`, 422},
		{"legacy profile", "typst-html-testprofile", "typst-fragments-reviewed", `{"typstFragments":{"templateVersion":"typst-fragment-template/v1","blockIds":["one"]}}`, 422},
		{"reviewed", "typst-fragments-reviewed", "typst-fragments-reviewed", `{"typstFragments":{"templateVersion":"typst-fragment-template/v1","blockIds":["one"]}}`, 202},
		{"duplicate IDs", "typst-fragments-reviewed", "typst-fragments-reviewed", `{"typstFragments":{"templateVersion":"typst-fragment-template/v1","blockIds":["one","one"]}}`, 400},
		{"legacy opaque options", "typst-html-testprofile", "", "legacy-options", 202},
	} {
		t.Run(test.name, func(t *testing.T) {
			admit := &fakeAdmission{}
			handler := newTestHandler(t, admit, newFakeRepository(), &fakeStore{})
			handler.config.TypstFragmentsProfileID = test.configured
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for name, value := range map[string]string{"contentKind": "typst", "documentEngine": "typst", "priorityIntent": "publish", "entrypoint": "main.typ", "documentMode": "article", "renderProfileId": test.profile, "options": test.options} {
				if err := writer.WriteField(name, value); err != nil {
					t.Fatal(err)
				}
			}
			file, _ := writer.CreateFormFile("source", "fragments.zip")
			_, _ = file.Write([]byte("synthetic bounded archive"))
			writer.Close()
			request := authorizedRequest(http.MethodPost, "/api/render/jobs", &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			request.Header.Set("Idempotency-Key", strings.Repeat("f", 64))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("fragment admission: %d %s", response.Code, response.Body.String())
			}
			if test.want != 202 && admit.request.ContentKind != "" {
				t.Fatal("invalid profile consumed an admission slot")
			}
		})
	}
}
