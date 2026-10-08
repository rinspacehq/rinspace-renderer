package renderstorage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSVGObjectIDUsesContentAddressedPath(t *testing.T) {
	svg := []byte(`<svg viewBox="0 0 10 10"><path stroke="#000"/></svg>`)
	sum := sha256.Sum256(svg)
	hash := hex.EncodeToString(sum[:])
	want := "diagrams/v1/svg-sha256/" + hash[:2] + "/" + hash + ".svg"
	if got := SVGObjectID(svg); got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestPutPublicObjectIfMissingUploadsSVGWithUpsert(t *testing.T) {
	const token = "cloudbase-token"
	svg := []byte(`<svg viewBox="0 0 10 10"></svg>`)
	objectID := SVGObjectID(svg)
	var headPath string
	var postPath string
	var postBody string
	var upsert string
	var contentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			headPath = r.URL.Path
			http.NotFound(w, r)
		case http.MethodPost:
			postPath = r.URL.Path
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "missing auth", http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			postBody = string(body)
			upsert = r.Header.Get("X-Upsert")
			contentType = r.Header.Get("Content-Type")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"Id":"object-id","Key":%q}`, "rin-renderer/"+objectID)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := CloudBaseClient{AccessToken: token, Bucket: "rin-renderer", BaseURL: server.URL, PublicBaseURL: "https://cdn.example"}
	result, err := client.PutPublicObjectIfMissing(context.Background(), objectID, SVGContentType, svg)
	if err != nil {
		t.Fatal(err)
	}

	publicPath := "/v1/storages/object/public/rin-renderer/" + objectID
	objectPath := "/v1/storages/object/rin-renderer/" + objectID
	if headPath != publicPath || postPath != objectPath {
		t.Fatalf("unexpected storage paths: head=%q post=%q", headPath, postPath)
	}
	if postBody != string(svg) || upsert != "true" || contentType != SVGContentType {
		t.Fatalf("unexpected upload body/headers: body=%q upsert=%q contentType=%q", postBody, upsert, contentType)
	}
	wantURL := "https://cdn.example" + publicPath
	if result.URL != wantURL || result.CloudBaseURL != wantURL || result.ObjectID != objectID || !result.Uploaded {
		t.Fatalf("unexpected upload result: %#v", result)
	}
}

func TestPutPublicObjectIfMissingSkipsExistingObject(t *testing.T) {
	svg := []byte(`<svg viewBox="0 0 10 10"></svg>`)
	objectID := SVGObjectID(svg)
	var postRequests int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && strings.HasPrefix(r.URL.Path, "/v1/storages/object/public/rin-renderer/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodPost {
			postRequests++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := CloudBaseClient{AccessToken: "cloudbase-token", Bucket: "rin-renderer", BaseURL: server.URL}
	result, err := client.PutPublicObjectIfMissing(context.Background(), objectID, SVGContentType, svg)
	if err != nil {
		t.Fatal(err)
	}
	if postRequests != 0 {
		t.Fatalf("expected no upload for existing object, got %d POST requests", postRequests)
	}
	if result.Uploaded || result.ObjectID != objectID {
		t.Fatalf("unexpected result for existing object: %#v", result)
	}
}

func TestReadPublicObjectIsBoundedAndUsesPublicPath(t *testing.T) {
	body := []byte(`<svg viewBox="0 0 1 1"></svg>`)
	objectID := SVGObjectID(body)
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := CloudBaseClient{Bucket: "rin-renderer", BaseURL: server.URL, PublicBaseURL: "https://cdn.example"}
	got, err := client.ReadPublicObject(context.Background(), objectID, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) || path != "/v1/storages/object/public/rin-renderer/"+objectID {
		t.Fatalf("read body/path = %q %q", got, path)
	}
	if gotURL := client.PublicObjectURL(objectID); !strings.HasPrefix(gotURL, "https://cdn.example/") {
		t.Fatalf("public object URL did not use reader origin: %s", gotURL)
	}
	if _, err := client.ReadPublicObject(context.Background(), objectID, int64(len(body)-1)); err == nil {
		t.Fatal("oversized public object was accepted")
	}
}

func TestReadPublicObjectRejectsMissingObject(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	client := CloudBaseClient{Bucket: "rin-renderer", BaseURL: server.URL}
	if _, err := client.ReadPublicObject(context.Background(), "diagrams/v1/missing.svg", 1024); err == nil {
		t.Fatal("missing public object was accepted")
	}
}
