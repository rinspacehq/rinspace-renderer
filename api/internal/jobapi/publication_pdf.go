package jobapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfinspect"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
	"github.com/rinspacehq/rinspace-renderer/api/internal/publicationpdf"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderstorage"
)

const publicationPDFReceiptHeader = "X-Rin-Publication-PDF-Receipt"

var errPublicationPDFAssetUnavailable = errors.New("publication PDF asset store is unavailable")

type PublicationPDFPrinter interface {
	Print(context.Context, contracts.DocumentBundle, bool, publicationpdf.ResolveImage) ([]byte, pdfinspect.Result, error)
}

type PublicationPDFConfig struct {
	ProfileSHA256 string
	Printer       PublicationPDFPrinter
	// PublicationPrincipal may submit an archived result after the original
	// Renderer job has expired. The primary product owns that reading binding.
	PublicationPrincipal string
	PublicAssets         interface {
		ReadPublicObject(context.Context, string, int64) ([]byte, error)
	}
}

func (config PublicationPDFConfig) validate() error {
	if config.Printer == nil || !controlHashPattern.MatchString(config.ProfileSHA256) || strings.Trim(config.ProfileSHA256, "0") == "" {
		return errors.New("publication PDF printer requires a verified profile and toolchain")
	}
	return nil
}

func (handler *Handler) publicationPDF(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 2048)
	var input contracts.PublicationPDFInput
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		writeError(response, http.StatusBadRequest, "invalid_publication_pdf_input", "publication PDF input is invalid")
		return
	}
	readVersion, jobKey, err := input.Identity()
	if err != nil || input.ProjectID != identity.OwnerScope || request.Header.Get("Idempotency-Key") != jobKey {
		writeError(response, http.StatusUnprocessableEntity, "invalid_publication_pdf_identity", "publication PDF input does not match the admitted job")
		return
	}
	if input.ProfileSHA256 != handler.config.PublicationPDF.ProfileSHA256 {
		writeError(response, http.StatusConflict, "publication_pdf_profile_mismatch", "publication PDF profile is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 80*time.Second)
	defer cancel()
	job, err := handler.repository.AuthorizedJob(ctx, request.PathValue("jobId"), identity.PrincipalID, identity.OwnerScope)
	if err != nil {
		writeRepositoryError(response, err)
		return
	}
	book, err := publicationMarkdownJob(job, input)
	if err != nil {
		writeError(response, http.StatusConflict, "publication_pdf_source_mismatch", "publication PDF source is not the completed edition")
		return
	}
	if !handler.takePublicationPDFSlot(response) {
		return
	}
	defer func() { <-handler.printSlot }()

	source, err := handler.repository.Artifact(ctx, job.SourceArtifactID)
	if err != nil || source.Visibility != "private" || source.SHA256 != input.SourceSHA256 ||
		source.ExpiresAt == nil || !source.ExpiresAt.After(handler.config.Now()) {
		writeError(response, http.StatusConflict, "publication_pdf_source_mismatch", "publication source does not match the edition")
		return
	}
	raw, resultArtifact, err := handler.resultArtifactBody(ctx, job)
	if err != nil || resultArtifact.SchemaVersion != jobresult.StoredOutputSchemaVersion {
		writeError(response, http.StatusGone, "publication_pdf_result_unavailable", "completed publication output is unavailable")
		return
	}
	output, err := jobresult.Decode(raw)
	if err != nil || output.Result.Inline == nil || output.Result.JobID != job.ID ||
		output.Result.ResultHash != input.ReadingSHA256 || output.Result.ContentKind != contracts.ContentKindMarkdown {
		writeError(response, http.StatusConflict, "publication_pdf_result_mismatch", "completed output does not match the reading edition")
		return
	}
	resolver := handler.publicationImageResolver(output.Result, func(ctx context.Context) ([]byte, error) {
		raw, _, err := handler.privateArtifactBody(ctx, job.SourceArtifactID)
		return raw, err
	})
	handler.printPublicationPDF(ctx, response, input, readVersion, jobKey, *output.Result.Inline, book, resolver)
}

func (handler *Handler) publicationTagPDF(response http.ResponseWriter, request *http.Request) {
	identity, ok := handler.identity(response, request)
	if !ok {
		return
	}
	var payload struct {
		Input    contracts.PublicationPDFInput       `json:"input"`
		Snapshot contracts.PublicationTagPDFSnapshot `json:"snapshot"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		writeError(response, http.StatusBadRequest, "invalid_publication_pdf_input", "tag PDF input is invalid")
		return
	}
	input := payload.Input
	edition, editionErr := payload.Snapshot.Edition(identity.OwnerScope)
	readVersion, jobKey, identityErr := input.Identity()
	if editionErr != nil || identityErr != nil || input.PublicationPDFEdition != edition ||
		request.Header.Get("Idempotency-Key") != jobKey {
		writeError(response, http.StatusUnprocessableEntity, "invalid_publication_pdf_identity", "tag fields do not match the admitted edition")
		return
	}
	if input.ProfileSHA256 != handler.config.PublicationPDF.ProfileSHA256 {
		writeError(response, http.StatusConflict, "publication_pdf_profile_mismatch", "publication PDF profile is unavailable")
		return
	}
	if !handler.takePublicationPDFSlot(response) {
		return
	}
	defer func() { <-handler.printSlot }()
	ctx, cancel := context.WithTimeout(request.Context(), 80*time.Second)
	defer cancel()
	title := payload.Snapshot.Title
	fragment := "<h1>" + html.EscapeString(title) + "</h1>"
	if payload.Snapshot.Excerpt != "" {
		fragment += "<p>" + html.EscapeString(payload.Snapshot.Excerpt) + "</p>"
	}
	bundle := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersionV1, State: contracts.DocumentBundleStateFinal,
		ContentKind: contracts.ContentKindMarkdown, DocumentEngine: "markdown",
		Title: title, ProjectHash: input.SourceSHA256, BundleHash: input.ReadingSHA256,
		Pages: []contracts.DocumentPage{{
			ID: "tag", SourcePath: "tag.md", Title: title, Fragment: fragment, FragmentFormat: contracts.FragmentFormatHTML,
		}},
		Provenance: contracts.Provenance{
			Adapter: "markdown", AdapterVersion: "1", EngineVersion: "1", ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion,
		},
	}
	handler.printPublicationPDF(ctx, response, input, readVersion, jobKey, bundle, false, nil)
}

func (handler *Handler) takePublicationPDFSlot(response http.ResponseWriter) bool {
	select {
	case handler.printSlot <- struct{}{}:
		return true
	default:
		response.Header().Set("Retry-After", "2")
		writeError(response, http.StatusServiceUnavailable, "publication_pdf_busy", "publication PDF printer is busy")
		return false
	}
}

func (handler *Handler) printPublicationPDF(ctx context.Context, response http.ResponseWriter, input contracts.PublicationPDFInput, readVersion, jobKey string, bundle contracts.DocumentBundle, book bool, resolver publicationpdf.ResolveImage) {
	pdf, inspected, err := handler.config.PublicationPDF.Printer.Print(ctx, bundle, book, resolver)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			writeError(response, http.StatusGatewayTimeout, "publication_pdf_timeout", "publication PDF printing timed out")
			return
		}
		if errors.Is(err, errPublicationPDFAssetUnavailable) {
			writeError(response, http.StatusServiceUnavailable, "publication_pdf_asset_unavailable", "publication PDF asset is temporarily unavailable")
			return
		}
		writeError(response, http.StatusUnprocessableEntity, "publication_pdf_print_failed", "publication PDF printing or asset verification failed")
		return
	}
	digest := sha256.Sum256(pdf)
	receipt := contracts.PublicationPDFResult{
		SchemaVersion: contracts.PublicationPDFResultSchemaV1, Input: input, ReadVersion: readVersion, JobKey: jobKey,
		SHA256: inspected.SourceSHA256, Bytes: inspected.SourceBytes, Pages: inspected.PageCount, MediaType: "application/pdf",
	}
	if err := receipt.ValidateFor(input); err != nil || receipt.Bytes != int64(len(pdf)) || receipt.SHA256 != hex.EncodeToString(digest[:]) {
		writeError(response, http.StatusInternalServerError, "publication_pdf_integrity", "printed PDF failed integrity verification")
		return
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "publication_pdf_receipt", "publication PDF receipt could not be encoded")
		return
	}
	response.Header().Set(publicationPDFReceiptHeader, base64.RawURLEncoding.EncodeToString(encoded))
	response.Header().Set("Content-Type", receipt.MediaType)
	response.Header().Set("Content-Length", strconv.FormatInt(receipt.Bytes, 10))
	response.Header().Set("Cache-Control", "private, no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = response.Write(pdf)
}

func publicationMarkdownJob(job jobpostgres.Job, input contracts.PublicationPDFInput) (bool, error) {
	var meta struct {
		OutputKind   string `json:"outputKind"`
		ProjectID    string `json:"controlProjectId"`
		SourceCommit string `json:"sourceCommit"`
		DocumentMode string `json:"documentMode"`
	}
	if job.State != "succeeded" || job.ContentKind != "markdown" ||
		json.Unmarshal(job.RequestMetadata, &meta) != nil ||
		meta.OutputKind != "document" || meta.ProjectID != input.ProjectID ||
		meta.SourceCommit == "" || input.SourceRevision != "git:"+meta.SourceCommit ||
		meta.DocumentMode != "article" && meta.DocumentMode != "book" ||
		(meta.DocumentMode == "book") != strings.HasPrefix(input.ProjectID, "book:") {
		return false, errors.New("job is not the requested Markdown publication")
	}
	return meta.DocumentMode == "book", nil
}

func (handler *Handler) publicationImageResolver(result contracts.RenderResult, source func(context.Context) ([]byte, error)) publicationpdf.ResolveImage {
	repositoryAssets := make(map[string]contracts.AssetReference)
	for _, asset := range result.Inline.Assets {
		if asset.Kind == "project-file" {
			repositoryAssets[asset.ProjectPath] = asset
		}
	}
	publicAssets := make(map[string]contracts.ArtifactReference)
	for _, asset := range result.Assets {
		if asset.Visibility == "public" {
			publicAssets[asset.ArtifactID] = asset
		}
	}
	var files map[string]projectcore.File
	return func(ctx context.Context, image publicationpdf.ImageReference) (string, error) {
		switch {
		case image.RepositoryPath != "" && image.DiagramObjectID == "":
			asset, exists := repositoryAssets[image.RepositoryPath]
			if !exists {
				return "", errors.New("repository image is not in the completed result")
			}
			if files == nil {
				raw, err := source(ctx)
				if err != nil {
					return "", errors.Join(errPublicationPDFAssetUnavailable, err)
				}
				items, _, err := projectcore.ImportMarkdownArchive("project.zip", raw, projectcore.Limits{
					ArchiveMaxBytes: handler.config.MaxSourceBytes, FileMaxCount: handler.config.ProjectFileMaxCount,
					FileMaxBytes: handler.config.ProjectFileMaxBytes,
				})
				if err != nil {
					return "", err
				}
				files = make(map[string]projectcore.File, len(items))
				for _, item := range items {
					files[item.Path] = item
				}
			}
			file, exists := files[image.RepositoryPath]
			if !exists {
				return "", errors.New("repository image is missing from the verified source")
			}
			body, err := projectidentity.FileBytes(file)
			if err != nil {
				return "", err
			}
			return printAssetDataURL(body, asset.SHA256, asset.Bytes, asset.MediaType, image.RepositoryPath)
		case image.DiagramObjectID != "" && image.RepositoryPath == "":
			asset, exists := publicAssets[image.DiagramObjectID]
			reader := handler.config.PublicationPDF.PublicAssets
			if !exists || reader == nil || asset.Bytes <= 0 || asset.Bytes > 4<<20 {
				return "", errors.New("diagram object is not available in the completed result")
			}
			body, err := reader.ReadPublicObject(ctx, asset.ArtifactID, asset.Bytes)
			if err != nil {
				return "", errors.Join(errPublicationPDFAssetUnavailable, err)
			}
			return printAssetDataURL(body, asset.SHA256, asset.Bytes, asset.MediaType, asset.ArtifactID)
		default:
			return "", errors.New("print image has no verified repository or diagram binding")
		}
	}
}

func printAssetDataURL(body []byte, expectedHash string, size int64, mediaType, name string) (string, error) {
	digest := sha256.Sum256(body)
	if int64(len(body)) != size || hex.EncodeToString(digest[:]) != expectedHash {
		return "", errors.New("print asset integrity failed")
	}
	verified, mediaType, _, err := renderstorage.VerifyRepositoryImage(body, mediaType, name)
	if err != nil {
		return "", err
	}
	return "data:" + strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]) + ";base64," + base64.StdEncoding.EncodeToString(verified), nil
}
