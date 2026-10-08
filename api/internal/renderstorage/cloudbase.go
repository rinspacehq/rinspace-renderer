package renderstorage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const SVGContentType = "image/svg+xml; charset=utf-8"

type CloudBaseClient struct {
	EnvID         string
	AccessToken   string
	Bucket        string
	BaseURL       string
	PublicBaseURL string
	HTTPClient    *http.Client
}

type PutResult struct {
	ObjectID     string
	URL          string
	CloudBaseURL string
	Uploaded     bool
}

type cloudBaseUploadResponse struct {
	ID      string `json:"Id"`
	Key     string `json:"Key"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func SVGObjectID(svg []byte) string {
	hash := SVGHash(svg)
	return svgObjectIDFromHash(hash)
}

func svgObjectIDFromHash(hash string) string {
	return fmt.Sprintf("diagrams/v1/svg-sha256/%s/%s.svg", hash[:2], hash)
}

func SVGHash(svg []byte) string {
	sum := sha256.Sum256(svg)
	return hex.EncodeToString(sum[:])
}

func (c CloudBaseClient) PublicObjectURL(objectID string) string {
	return strings.TrimRight(c.publicStorageBaseURL(), "/") +
		"/v1/storages/object/public/" + url.PathEscape(c.bucket()) + "/" + encodeCloudObjectName(objectID)
}

func (c CloudBaseClient) objectReadURL(objectID string) string {
	return strings.TrimRight(c.storageBaseURL(), "/") +
		"/v1/storages/object/public/" + url.PathEscape(c.bucket()) + "/" + encodeCloudObjectName(objectID)
}

func (c CloudBaseClient) ObjectExists(ctx context.Context, objectID string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.objectReadURL(objectID), nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (c CloudBaseClient) ReadPublicObject(ctx context.Context, objectID string, maxBytes int64) ([]byte, error) {
	objectID = strings.TrimLeft(strings.TrimSpace(objectID), "/")
	if objectID == "" || maxBytes <= 0 {
		return nil, errors.New("CloudBase public object read requires an object ID and positive byte limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.objectReadURL(objectID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("CloudBase public object read failed: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("CloudBase public object exceeds %d bytes", maxBytes)
	}
	return body, nil
}

func (c CloudBaseClient) PutPublicObjectIfMissing(ctx context.Context, objectID string, contentType string, data []byte) (PutResult, error) {
	objectID = strings.TrimLeft(strings.TrimSpace(objectID), "/")
	if objectID == "" {
		return PutResult{}, errors.New("CloudBase object ID is required")
	}
	if err := c.Validate(); err != nil {
		return PutResult{}, err
	}
	publicURL := c.PublicObjectURL(objectID)
	if c.ObjectExists(ctx, objectID) {
		return PutResult{
			ObjectID:     objectID,
			URL:          publicURL,
			CloudBaseURL: publicURL,
			Uploaded:     false,
		}, nil
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return PutResult{}, errors.New("CloudBase access token is required")
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = SVGContentType
	}

	endpoint := strings.TrimRight(c.storageBaseURL(), "/") +
		"/v1/storages/object/" + url.PathEscape(c.bucket()) + "/" + encodeCloudObjectName(objectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return PutResult{}, err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Length", strconv.Itoa(len(data)))
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.AccessToken))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Upsert", "true")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return PutResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PutResult{}, fmt.Errorf("CloudBase object upload failed: %s %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var result cloudBaseUploadResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return PutResult{}, err
	}
	if strings.TrimSpace(result.Key) == "" {
		return PutResult{}, errors.New("CloudBase object upload returned empty key")
	}
	return PutResult{
		ObjectID:     objectID,
		URL:          publicURL,
		CloudBaseURL: publicURL,
		Uploaded:     true,
	}, nil
}

func (c CloudBaseClient) Validate() error {
	if strings.TrimSpace(c.BaseURL) == "" && strings.TrimSpace(c.EnvID) == "" {
		return errors.New("RIN_RENDERER_CLOUDBASE_ENV_ID or RIN_RENDERER_STORAGE_BASE_URL is required")
	}
	return nil
}

func (c CloudBaseClient) storageBaseURL() string {
	if strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	}
	return "https://" + strings.TrimSpace(c.EnvID) + ".api.tcloudbasegateway.com"
}

func (c CloudBaseClient) publicStorageBaseURL() string {
	if strings.TrimSpace(c.PublicBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/")
	}
	return c.storageBaseURL()
}

func (c CloudBaseClient) bucket() string {
	if strings.TrimSpace(c.Bucket) != "" {
		return strings.TrimSpace(c.Bucket)
	}
	return "rin-renderer"
}

func (c CloudBaseClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func encodeCloudObjectName(objectName string) string {
	parts := strings.Split(strings.TrimLeft(objectName, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
