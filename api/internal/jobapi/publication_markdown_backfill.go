package jobapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

const maxPublicationReadingBytes = 64 << 20

func (handler *Handler) publicationMarkdownBackfill(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	if identity.PrincipalID != handler.config.PublicationPDF.PublicationPrincipal {
		writeError(response, http.StatusForbidden, "publication_pdf_forbidden", "publication service authentication is required")
		return
	}
	if !handler.takePublicationPDFSlot(response) {
		return
	}
	defer func() { <-handler.printSlot }()
	ctx, cancel := context.WithTimeout(request.Context(), 80*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(response)
	_ = controller.SetReadDeadline(deadline)
	defer controller.SetReadDeadline(time.Time{})
	request.Body = http.MaxBytesReader(response, request.Body, handler.config.MaxSourceBytes+maxPublicationReadingBytes+(64<<10))
	form, err := request.MultipartReader()
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid_publication_pdf_input", "archived publication input is invalid")
		return
	}
	raw, err := readPublicationPart(form, "input", 2048)
	var input contracts.PublicationPDFInput
	if err != nil || json.Unmarshal(raw, &input) != nil {
		writeError(response, http.StatusBadRequest, "invalid_publication_pdf_input", "archived publication input is invalid")
		return
	}
	readVersion, jobKey, err := input.Identity()
	if err != nil || input.ProjectID != identity.OwnerScope || !strings.HasPrefix(input.SourceRevision, "git:") ||
		strings.HasPrefix(input.ProjectID, "pdf:") || request.Header.Get("Idempotency-Key") != jobKey {
		writeError(response, http.StatusUnprocessableEntity, "invalid_publication_pdf_identity", "archived publication input does not match the admitted edition")
		return
	}
	if input.ProfileSHA256 != handler.config.PublicationPDF.ProfileSHA256 {
		writeError(response, http.StatusConflict, "publication_pdf_profile_mismatch", "publication PDF profile is unavailable")
		return
	}
	raw, err = readPublicationPart(form, "result", maxPublicationReadingBytes)
	var reading contracts.RenderResult
	// The publication principal supplies the result verified and persisted with
	// the active binding. Legacy bundles have producer-specific hash encodings.
	if err != nil || json.Unmarshal(raw, &reading) != nil || reading.Validate() != nil ||
		reading.Inline == nil || reading.ContentKind != contracts.ContentKindMarkdown || reading.ResultHash != input.ReadingSHA256 {
		writeError(response, http.StatusConflict, "publication_pdf_result_mismatch", "archived output does not match the reading edition")
		return
	}
	archive, err := readPublicationPart(form, "source", handler.config.MaxSourceBytes)
	if err != nil || len(archive) == 0 {
		writeError(response, http.StatusBadRequest, "invalid_publication_pdf_source", "archived publication source is invalid")
		return
	}
	digest := sha256.Sum256(archive)
	if _, err := form.NextPart(); err != io.EOF || hex.EncodeToString(digest[:]) != input.SourceSHA256 {
		writeError(response, http.StatusConflict, "publication_pdf_source_mismatch", "archived source does not match the reading edition")
		return
	}
	resolver := handler.publicationImageResolver(reading, func(context.Context) ([]byte, error) { return archive, nil })
	handler.printPublicationPDF(ctx, response, input, readVersion, jobKey, *reading.Inline, strings.HasPrefix(input.ProjectID, "book:"), resolver)
}

func readPublicationPart(form *multipart.Reader, name string, limit int64) ([]byte, error) {
	part, err := form.NextPart()
	if err != nil {
		return nil, err
	}
	if part.FormName() != name {
		return nil, errors.New("unexpected publication field")
	}
	raw, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err == nil && int64(len(raw)) > limit {
		return nil, errors.New("publication field exceeds its limit")
	}
	return raw, err
}
