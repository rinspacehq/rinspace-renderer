package renderstorage

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalFileStorePersistsVerifiedSVG(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalFileStore(root, "http://127.0.0.1:8090")
	if err != nil {
		t.Fatal(err)
	}
	svg, err := NormalizeSVGObject([]byte(`<svg viewBox="0 0 10 10"><path d="M0 0L10 10"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.PutPublicObjectIfMissing(context.Background(), svg.ObjectID, SVGContentType, svg.Bytes)
	if err != nil || !first.Uploaded || first.URL != "http://127.0.0.1:8090/local-assets/"+svg.ObjectID {
		t.Fatalf("first local upload = %#v, %v", first, err)
	}
	restarted, err := NewLocalFileStore(root, "http://127.0.0.1:8090")
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.PutPublicObjectIfMissing(context.Background(), svg.ObjectID, SVGContentType, svg.Bytes)
	if err != nil || second.Uploaded {
		t.Fatalf("reused local upload = %#v, %v", second, err)
	}
	request := httptest.NewRequest(http.MethodGet, "/local-assets/"+svg.ObjectID, nil)
	response := httptest.NewRecorder()
	restarted.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), svg.Bytes) || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("local SVG read = %d %q", response.Code, response.Body.String())
	}
	if _, err := restarted.PutPublicObjectIfMissing(context.Background(), svg.ObjectID, SVGContentType, []byte(`<svg/>`)); err == nil {
		t.Fatal("mismatched SVG digest was accepted")
	}
	if _, err := restarted.ReadPublicObject(context.Background(), "../outside.svg", 4<<20); err == nil {
		t.Fatal("path traversal was accepted")
	}
}

func TestLocalFileStoreRejectsNonLoopbackAndUnsafeSVG(t *testing.T) {
	for _, origin := range []string{"http://example.com:8090", "https://127.0.0.1:8090", "http://127.0.0.1:8090/path"} {
		if _, err := NewLocalFileStore(t.TempDir(), origin); err == nil {
			t.Fatalf("unsafe local origin %q was accepted", origin)
		}
	}
	store, err := NewLocalFileStore(t.TempDir(), "http://localhost:8090")
	if err != nil {
		t.Fatal(err)
	}
	unsafe := []byte(`<svg viewBox="0 0 10 10"><script>alert(1)</script><path d="M0 0L1 1"/></svg>`)
	if _, err := store.PutPublicObjectIfMissing(context.Background(), SVGObjectID(unsafe), SVGContentType, unsafe); err == nil || !strings.Contains(err.Error(), "sanitized") {
		t.Fatalf("unsafe SVG rejection = %v", err)
	}
}
