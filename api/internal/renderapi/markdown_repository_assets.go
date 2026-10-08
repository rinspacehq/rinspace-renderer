package renderapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderstorage"

	_ "golang.org/x/image/webp"
)

const markdownRepositoryAssetPromotionVersion = "rin-markdown-repository-assets/v1"

const (
	markdownPublishedImageMaxDimension = 16384
	markdownPublishedImageMaxPixels    = 64 * 1024 * 1024
)

var markdownAssetTagPattern = regexp.MustCompile(`(?is)<(?:img|source)\b[^>]*\bdata-rin-asset-path\s*=\s*(?:"[^"]*"|'[^']*')[^>]*>`)
var markdownAssetSourcePattern = regexp.MustCompile(`(?is)(\bsrc\s*=\s*)(?:"[^"]*"|'[^']*'|[^\s>]+)`)

type markdownRepositoryManifest struct {
	Status           string `yaml:"status"`
	SourceVisibility string `yaml:"sourceVisibility"`
}

type promotedMarkdownAsset struct {
	ProjectPath string
	URL         string
	Artifact    *contracts.ArtifactReference
}

type markdownRepositoryAssetFailure struct {
	Code    string
	Message string
	Err     error
}

func (failure *markdownRepositoryAssetFailure) Error() string {
	if failure == nil || failure.Err == nil {
		return "Markdown repository asset processing failed"
	}
	return failure.Err.Error()
}

func (failure *markdownRepositoryAssetFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Err
}

func markdownAssetFailure(code, message string, err error) error {
	return &markdownRepositoryAssetFailure{Code: code, Message: message, Err: err}
}

func (executor *markdownProjectExecutor) promoteRepositoryAssets(ctx context.Context, request ProjectExecutionRequest, bundle *contracts.DocumentBundle, files map[string][]byte) (artifacts []contracts.ArtifactReference, err error) {
	started := time.Now()
	visibility := "unknown"
	outcome := "failed"
	var operations *operational.Registry
	if executor != nil && executor.server != nil {
		operations = executor.server.operations
	}
	if operations != nil {
		defer func() {
			operations.Count("markdown_repository_asset_requests_total", visibility, outcome)
			operations.Observe("markdown_repository_asset_duration_seconds", time.Since(started), visibility, outcome)
		}()
	}
	if bundle == nil || bundle.ContentKind != contracts.ContentKindMarkdown {
		return nil, markdownAssetFailure("markdown.assets.validation_failed", "Markdown repository asset validation failed.", errors.New("Markdown repository asset promotion requires a Markdown bundle"))
	}
	referenced := map[string]bool{}
	for _, page := range bundle.Pages {
		for _, tag := range markdownAssetTagPattern.FindAllString(page.Fragment, -1) {
			assetPath := strings.TrimSpace(getHTMLAttribute(tag, "data-rin-asset-path"))
			if assetPath != "" {
				referenced[assetPath] = true
			}
		}
	}
	if len(referenced) == 0 {
		visibility = "none"
		outcome = "success"
		return []contracts.ArtifactReference{}, nil
	}

	manifest := markdownRepositoryManifest{}
	if raw := files["rinspace.yaml"]; len(raw) == 0 || yaml.Unmarshal(raw, &manifest) != nil {
		return nil, markdownAssetFailure("markdown.assets.validation_failed", "Markdown repository asset validation failed.", errors.New("Markdown repository assets require a valid rinspace.yaml"))
	}
	publicPromotion := manifest.Status == "published" && manifest.SourceVisibility == "open"
	visibility = "private"
	if publicPromotion {
		visibility = "public"
		if !executor.cfg.MarkdownRepoAssets {
			outcome = "disabled"
			return nil, markdownAssetFailure("markdown.assets.disabled", "Markdown repository asset publication is temporarily disabled.", errors.New("Markdown repository asset promotion is disabled"))
		}
	}
	assetsByPath := make(map[string]contracts.AssetReference, len(bundle.Assets))
	for _, asset := range bundle.Assets {
		if asset.Kind == "project-file" {
			assetsByPath[asset.ProjectPath] = asset
		}
	}
	paths := make([]string, 0, len(referenced))
	for assetPath := range referenced {
		paths = append(paths, assetPath)
	}
	sort.Strings(paths)

	promoted := make(map[string]promotedMarkdownAsset, len(paths))
	artifacts = make([]contracts.ArtifactReference, 0, len(paths))
	for _, assetPath := range paths {
		if !validMarkdownPromotablePath(assetPath) {
			return nil, markdownAssetFailure("markdown.assets.validation_failed", "Markdown repository asset validation failed.", fmt.Errorf("Markdown asset %q is outside the repository asset roots", assetPath))
		}
		asset, ok := assetsByPath[assetPath]
		body, found := files[assetPath]
		if !ok || !found {
			return nil, markdownAssetFailure("markdown.assets.missing", "A referenced Markdown repository asset is missing.", fmt.Errorf("Markdown asset %q is missing from the verified project", assetPath))
		}
		digest := sha256.Sum256(body)
		if asset.SHA256 != hex.EncodeToString(digest[:]) || asset.Bytes != int64(len(body)) {
			return nil, markdownAssetFailure("markdown.assets.validation_failed", "Markdown repository asset validation failed.", fmt.Errorf("Markdown asset %q does not match the project graph", assetPath))
		}
		storedBody, mediaType, extension, err := verifiedMarkdownAsset(body, asset.MediaType, assetPath)
		if err != nil {
			return nil, markdownAssetFailure("markdown.assets.validation_failed", "Markdown repository asset validation failed.", err)
		}
		item := promotedMarkdownAsset{ProjectPath: assetPath}
		if publicPromotion {
			storedHash := sha256.Sum256(storedBody)
			hash := hex.EncodeToString(storedHash[:])
			objectID := fmt.Sprintf("assets/v1/sha256/%s/%s%s", hash[:2], hash, extension)
			stored, putErr := executor.server.diagramStorage().PutPublicObjectIfMissing(ctx, objectID, mediaType, storedBody)
			if putErr != nil {
				return nil, markdownAssetFailure("markdown.assets.storage_failed", "Markdown repository asset storage failed.", fmt.Errorf("promote Markdown asset %q: %w", assetPath, putErr))
			}
			if stored.ObjectID != objectID || !executor.verifyMarkdownAssetStored(ctx, objectID, storedBody) {
				return nil, markdownAssetFailure("markdown.assets.storage_integrity_failed", "Markdown repository asset storage verification failed.", fmt.Errorf("promote Markdown asset %q: stored object failed integrity verification", assetPath))
			}
			item.URL = stored.URL
			item.Artifact = &contracts.ArtifactReference{ArtifactID: objectID, SHA256: hash, Bytes: int64(len(storedBody)), MediaType: mediaType, Visibility: "public"}
			artifacts = append(artifacts, *item.Artifact)
		} else {
			item.URL, err = markdownPrivateRepositoryAssetURL(request, assetPath)
			if err != nil {
				return nil, markdownAssetFailure("markdown.assets.validation_failed", "Markdown repository asset binding is invalid.", err)
			}
		}
		promoted[assetPath] = item
		if operations != nil {
			operations.Count("markdown_repository_asset_files_total", visibility)
		}
	}

	for index := range bundle.Pages {
		bundle.Pages[index].Fragment = markdownAssetTagPattern.ReplaceAllStringFunc(bundle.Pages[index].Fragment, func(tag string) string {
			assetPath := strings.TrimSpace(getHTMLAttribute(tag, "data-rin-asset-path"))
			item, ok := promoted[assetPath]
			if !ok {
				return tag
			}
			return replaceMarkdownAssetSource(tag, item.URL)
		})
	}
	hash, err := markdownBundleHash(*bundle)
	if err != nil {
		return nil, markdownAssetFailure("markdown.assets.finalization_failed", "Markdown repository asset finalization failed.", err)
	}
	bundle.BundleHash = hash
	outcome = "success"
	return artifacts, nil
}

func (executor *markdownProjectExecutor) verifyMarkdownAssetStored(ctx context.Context, objectID string, expected []byte) bool {
	expectedHash := sha256.Sum256(expected)
	for attempt, delay := range []time.Duration{0, 50 * time.Millisecond, 150 * time.Millisecond} {
		if attempt > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false
			case <-timer.C:
			}
		}
		body, err := executor.server.diagramStorage().ReadPublicObject(ctx, objectID, int64(len(expected)))
		actualHash := sha256.Sum256(body)
		if err == nil && len(body) == len(expected) && actualHash == expectedHash {
			return true
		}
	}
	return false
}

func validMarkdownPromotablePath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value != path.Clean(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	if strings.HasPrefix(value, ".") || strings.Contains(value, "/.") {
		return false
	}
	switch strings.ToLower(path.Ext(value)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg":
		return true
	default:
		return false
	}
}

func verifiedMarkdownAsset(body []byte, mediaType, projectPath string) ([]byte, string, string, error) {
	mediaType = strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
	extension := strings.ToLower(path.Ext(projectPath))
	valid := false
	switch extension {
	case ".png":
		valid = mediaType == "image/png" && len(body) >= 8 && bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	case ".jpg", ".jpeg":
		valid, extension = mediaType == "image/jpeg" && len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[2] == 0xff, ".jpg"
	case ".gif":
		valid = mediaType == "image/gif" && len(body) >= 6 && (bytes.Equal(body[:6], []byte("GIF87a")) || bytes.Equal(body[:6], []byte("GIF89a")))
	case ".webp":
		valid = mediaType == "image/webp" && len(body) >= 12 && bytes.Equal(body[:4], []byte("RIFF")) && bytes.Equal(body[8:12], []byte("WEBP"))
	case ".svg":
		if mediaType != "image/svg+xml" {
			break
		}
		normalized, err := renderstorage.NormalizeSVG(body)
		if err != nil {
			return nil, "", "", fmt.Errorf("Markdown SVG %q is unsafe: %w", projectPath, err)
		}
		return normalized, renderstorage.SVGContentType, extension, nil
	}
	if !valid {
		return nil, "", "", fmt.Errorf("Markdown asset %q does not match its media type", projectPath)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	expectedFormat := map[string]string{".png": "png", ".jpg": "jpeg", ".gif": "gif", ".webp": "webp"}[extension]
	if err != nil || format != expectedFormat || config.Width <= 0 || config.Height <= 0 ||
		config.Width > markdownPublishedImageMaxDimension || config.Height > markdownPublishedImageMaxDimension ||
		int64(config.Width)*int64(config.Height) > markdownPublishedImageMaxPixels {
		return nil, "", "", fmt.Errorf("Markdown asset %q has invalid or excessive dimensions", projectPath)
	}
	return body, mediaType, extension, nil
}

func markdownPrivateRepositoryAssetURL(request ProjectExecutionRequest, assetPath string) (string, error) {
	projectKind, objectID, ok := strings.Cut(strings.TrimSpace(request.ProjectID), ":")
	owner := map[string]string{"article": "a", "book": "b"}[projectKind]
	commit := strings.ToLower(strings.TrimSpace(request.SourceCommit))
	if !ok || owner == "" || objectID == "" || !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(commit) {
		return "", errors.New("private Markdown asset repository identity is invalid")
	}
	parts := strings.Split(assetPath, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return "/repos/" + owner + "/" + url.PathEscape(objectID) + "/raw/commit/" + commit + "/" + strings.Join(parts, "/"), nil
}

func replaceMarkdownAssetSource(tag, source string) string {
	escaped := stdhtml.EscapeString(strings.TrimSpace(source))
	if escaped == "" {
		return tag
	}
	if markdownAssetSourcePattern.MatchString(tag) {
		return markdownAssetSourcePattern.ReplaceAllString(tag, `${1}"`+escaped+`"`)
	}
	return addHTMLAttribute(tag, "src", source)
}

func markdownBundleHash(bundle contracts.DocumentBundle) (string, error) {
	bundle.BundleHash = ""
	raw, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	var canonical map[string]any
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return "", err
	}
	canonical["bundleHash"] = ""
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(canonical); err != nil {
		return "", err
	}
	digest := sha256.Sum256(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
	return hex.EncodeToString(digest[:]), nil
}
