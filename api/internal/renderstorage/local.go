package renderstorage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const LocalAssetPath = "/local-assets/"

var localSVGObjectPattern = regexp.MustCompile(`^diagrams/v1/svg-sha256/([0-9a-f]{2})/([0-9a-f]{64})\.svg$`)

// LocalFileStore serves immutable, content-addressed SVGs from a private
// directory through the loopback-only Renderer API. It is never a product
// publication store.
type LocalFileStore struct {
	root    string
	baseURL string
}

func NewLocalFileStore(root, baseURL string) (*LocalFileStore, error) {
	if !filepath.IsAbs(root) || strings.TrimSpace(root) == "" {
		return nil, errors.New("local public asset root must be an absolute path")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed == nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return nil, errors.New("local public asset base URL must be a loopback HTTP origin")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("local public asset base URL must use a loopback host")
	}
	if parsed.Port() == "" {
		return nil, errors.New("local public asset base URL requires an explicit port")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	return &LocalFileStore{root: filepath.Clean(root), baseURL: strings.TrimRight(baseURL, "/")}, nil
}

func (store *LocalFileStore) PublicObjectURL(objectID string) string {
	return store.baseURL + LocalAssetPath + objectID
}

func (store *LocalFileStore) objectPath(objectID string) (string, error) {
	match := localSVGObjectPattern.FindStringSubmatch(objectID)
	if match == nil || match[1] != match[2][:2] {
		return "", errors.New("invalid local SVG object ID")
	}
	return filepath.Join(store.root, filepath.FromSlash(objectID)), nil
}

func (store *LocalFileStore) ReadPublicObject(ctx context.Context, objectID string, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, errors.New("local public object byte limit must be positive")
	}
	path, err := store.objectPath(objectID)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes || SVGObjectID(data) != objectID {
		return nil, errors.New("local public object size or digest is invalid")
	}
	return data, nil
}

func (store *LocalFileStore) PutPublicObjectIfMissing(ctx context.Context, objectID, contentType string, data []byte) (PutResult, error) {
	if err := ctx.Err(); err != nil {
		return PutResult{}, err
	}
	path, err := store.objectPath(objectID)
	if err != nil {
		return PutResult{}, err
	}
	if contentType != SVGContentType || len(data) == 0 || len(data) > 4<<20 || SVGObjectID(data) != objectID {
		return PutResult{}, errors.New("local public asset must be a bounded content-addressed SVG")
	}
	normalized, err := NormalizeSVGObject(data)
	if err != nil || !bytes.Equal(normalized.Bytes, data) {
		return PutResult{}, errors.New("local public asset must already be sanitized and normalized")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return PutResult{}, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := store.ReadPublicObject(ctx, objectID, int64(len(data)))
		if readErr != nil || !bytes.Equal(existing, data) {
			return PutResult{}, errors.New("existing local public asset failed digest verification")
		}
		return PutResult{ObjectID: objectID, URL: store.PublicObjectURL(objectID), Uploaded: false}, nil
	}
	if err != nil {
		return PutResult{}, err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return PutResult{}, errors.Join(writeErr, closeErr)
	}
	stored, err := store.ReadPublicObject(ctx, objectID, int64(len(data)))
	if err != nil || !bytes.Equal(stored, data) {
		_ = os.Remove(path)
		return PutResult{}, errors.New("local public asset failed post-write verification")
	}
	return PutResult{ObjectID: objectID, URL: store.PublicObjectURL(objectID), Uploaded: true}, nil
}

func (store *LocalFileStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	objectID := strings.TrimPrefix(r.URL.Path, LocalAssetPath)
	data, err := store.ReadPublicObject(r.Context(), objectID, 4<<20)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", SVGContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
