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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/publicationpdf"
)

const maxPublicationReadingBytes = 64 << 20
const maxPublicationImageManifestBytes = 8 << 20
const maxPublicationArchiveImages = 48

var zhihuPublicationImageHost = regexp.MustCompile(`^pic([1-4]|x|a)\.zhimg\.com$`)
var zhihuPublicationImagePath = regexp.MustCompile(`^/([1-9][0-9]?/)?v2-[a-z0-9]{30,64}(_[a-z0-9]{1,12})?\.(jpg|jpeg|png|webp|gif)$`)
var zhihuPublicationImageQuery = regexp.MustCompile(`^source=[a-f0-9]{8}$`)
var archiveRasterExtensions = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
}

type publicationArchiveImage struct {
	Source      string                           `json:"source"`
	SHA256      string                           `json:"sha256"`
	Bytes       int64                            `json:"bytes"`
	MediaType   string                           `json:"mediaType"`
	Body        []byte                           `json:"body"`
	Attribution *publicationpdf.ImageAttribution `json:"attribution,omitempty"`
}

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
	request.Body = http.MaxBytesReader(response, request.Body, handler.config.MaxSourceBytes+maxPublicationReadingBytes+maxPublicationImageManifestBytes+(64<<10))
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
	if hex.EncodeToString(digest[:]) != input.SourceSHA256 {
		writeError(response, http.StatusConflict, "publication_pdf_source_mismatch", "archived source does not match the reading edition")
		return
	}
	images, err := readPublicationArchiveImages(form)
	if err != nil {
		writeError(response, http.StatusUnprocessableEntity, "invalid_publication_pdf_images", "archived publication images are invalid")
		return
	}
	resolver := handler.publicationImageResolver(reading, func(context.Context) ([]byte, error) { return archive, nil })
	resolveImage := func(ctx context.Context, ref publicationpdf.ImageReference) (publicationpdf.ResolvedImage, error) {
		if ref.RepositoryPath == "" && ref.DiagramObjectID == "" {
			if value, ok := images[ref.Source]; ok {
				return value, nil
			}
		}
		return resolver(ctx, ref)
	}
	handler.printPublicationPDF(ctx, response, input, readVersion, jobKey, *reading.Inline, strings.HasPrefix(input.ProjectID, "book:"), resolveImage)
}

func readPublicationArchiveImages(form *multipart.Reader) (map[string]publicationpdf.ResolvedImage, error) {
	part, err := form.NextPart()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil || part.FormName() != "verifiedImages" {
		return nil, errors.New("unexpected publication image field")
	}
	limiter := &io.LimitedReader{R: part, N: maxPublicationImageManifestBytes + 1}
	decoder := json.NewDecoder(limiter)
	decoder.DisallowUnknownFields()
	var records []publicationArchiveImage
	if err := decoder.Decode(&records); err != nil || len(records) == 0 || len(records) > maxPublicationArchiveImages {
		return nil, errors.New("invalid publication image manifest")
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF || limiter.N == 0 {
		return nil, errors.New("publication image manifest exceeds its limit or has trailing content")
	}
	if _, err := form.NextPart(); err != io.EOF {
		return nil, errors.New("unexpected field after the publication image manifest")
	}
	images := make(map[string]publicationpdf.ResolvedImage, len(records))
	for _, record := range records {
		location, err := url.Parse(record.Source)
		if err != nil || len(record.Source) > 1024 || location.Scheme != "https" || location.Hostname() == "" ||
			location.User != nil || location.Fragment != "" || record.Bytes <= 0 || record.Bytes > 4<<20 {
			return nil, errors.New("invalid archive image identity")
		}
		// Preserve the original source string as the offline lookup key. Legacy Zhihu images
		// include a fixed source code in that string. Renderer still only uses the verified bytes.
		zhihu := location.Port() == "" && zhihuPublicationImageHost.MatchString(location.Hostname()) &&
			zhihuPublicationImagePath.MatchString(location.EscapedPath())
		if location.RawQuery != "" && (!zhihu || !zhihuPublicationImageQuery.MatchString(location.RawQuery)) {
			return nil, errors.New("invalid archive image identity")
		}
		if _, duplicate := images[record.Source]; duplicate {
			return nil, errors.New("duplicate archive image source")
		}
		name := location.Path
		if zhihu {
			// CDN raster responses can use a different format than the historical URL suffix.
			// The supplied media type must still match the verified file bytes.
			extension, ok := archiveRasterExtensions[record.MediaType]
			if !ok {
				return nil, errors.New("invalid archived raster media type")
			}
			name = "image" + extension
		}
		image, err := verifiedPrintImage(record.Body, record.SHA256, record.Bytes, record.MediaType, name)
		if err != nil {
			return nil, err
		}
		if record.Attribution != nil {
			if err := record.Attribution.Validate(); err != nil {
				return nil, err
			}
			image.Attribution = record.Attribution
		}
		images[record.Source] = image
	}
	return images, nil
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
