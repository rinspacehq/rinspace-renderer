package renderapi

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstadapter"
)

// Submission only: even a synchronous API runtime cannot execute fragments.
// Authentication, scheduling, source/result privacy and recovery stay with the
// existing job API. No public publication identity is constructed for a lesson.
func (s *Server) handleFragments(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	if !s.requireServiceToken(w, r, requestID) {
		return
	}
	if s.jobs == nil || TypstFragmentsProfileID(s.cfg) == "" {
		writeError(w, http.StatusServiceUnavailable, requestID, "isolated fragment jobs are unavailable", nil)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.URL.RawQuery != "" || r.Header.Get("Content-Encoding") != "" {
		writeError(w, 400, requestID, "invalid fragment request", nil)
		return
	}
	var input struct {
		Schema    string                       `json:"schema"`
		ProfileID string                       `json:"renderProfileId"`
		Blocks    []typstadapter.FragmentBlock `json:"blocks"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF || input.Schema != "typst-fragments/v1" || input.ProfileID != TypstFragmentsProfileID(s.cfg) {
		writeError(w, 400, requestID, "invalid fragment schema or profile", nil)
		return
	}
	files, ids, err := typstadapter.BuildFragmentProject(input.Blocks)
	if err != nil {
		writeError(w, 400, requestID, "invalid fragment blocks", err)
		return
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			writeError(w, 500, requestID, "fragment archive failed", nil)
			return
		}
		if _, err := entry.Write(files[name]); err != nil {
			writeError(w, 500, requestID, "fragment archive failed", nil)
			return
		}
	}
	if err := writer.Close(); err != nil {
		writeError(w, 500, requestID, "fragment archive failed", nil)
		return
	}
	sourceDigest := sha256.Sum256(archive.Bytes())
	// The final Typst bundle identifies its canonical project graph. The ZIP
	// byte digest belongs to the source artifact, not to that graph identity.
	// Use the same importer as the executor so consumers can bind the result
	// to this accepted source without weakening their equality checks.
	project, err := projectidentity.Import("fragments.zip", archive.Bytes(), contracts.ContentKindTypst, "main.typ", projectLimitsFromConfig(s.cfg))
	if err != nil {
		writeError(w, 400, requestID, "fragment project identity failed", nil)
		return
	}
	options, _ := json.Marshal(map[string]any{"typstFragments": typstadapter.FragmentOptions{TemplateVersion: typstadapter.FragmentTemplateVersion, BlockIDs: ids}})
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := map[string]string{"contentKind": "typst", "documentEngine": "typst", "priorityIntent": "publish", "documentMode": "article",
		"entrypoint": "main.typ", "renderProfileId": input.ProfileID, "options": string(options)}
	for name, value := range fields {
		if err := form.WriteField(name, value); err != nil {
			writeError(w, 500, requestID, "fragment envelope failed", nil)
			return
		}
	}
	file, err := form.CreateFormFile("source", "fragments.zip")
	if err != nil {
		writeError(w, 500, requestID, "fragment envelope failed", nil)
		return
	}
	if _, err := io.Copy(file, &archive); err != nil {
		writeError(w, 500, requestID, "fragment envelope failed", nil)
		return
	}
	if err := form.Close(); err != nil {
		writeError(w, 500, requestID, "fragment envelope failed", nil)
		return
	}
	queued := r.Clone(r.Context())
	target := *r.URL
	target.Path = "/api/render/jobs"
	target.RawPath = ""
	queued.URL = &target
	queued.Header = r.Header.Clone()
	queued.Header.Set("Content-Type", form.FormDataContentType())
	queued.Header.Del("Content-Length")
	queued.Header.Del("Transfer-Encoding")
	queued.ContentLength = int64(body.Len())
	queued.TransferEncoding = nil
	queued.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
	queued.GetBody = nil
	queued.Form = nil
	queued.PostForm = nil
	queued.MultipartForm = nil
	// An absent key is rejected by the existing job API, before storage/admission.
	if strings.TrimSpace(queued.Header.Get("Idempotency-Key")) == "" {
		writeError(w, 400, requestID, "Idempotency-Key is required", nil)
		return
	}
	w.Header().Set("X-Rin-Fragment-Project-SHA256", project.Graph.ProjectHash)
	w.Header().Set("X-Rin-Fragment-Source-SHA256", hex.EncodeToString(sourceDigest[:]))
	s.jobs.ServeHTTP(w, queued)
}
