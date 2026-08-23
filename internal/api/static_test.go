package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticIndexDoesNotRedirect(t *testing.T) {
	files := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte(`<!doctype html><div id="root"></div>`)},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	handler := NewStaticAPI(files)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET / = %d", resp.Code)
	}
	if loc := resp.Header().Get("Location"); loc != "" {
		t.Fatalf("redirected to %s", loc)
	}
	if !strings.Contains(resp.Body.String(), `id="root"`) {
		t.Fatalf("body = %s", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/incidents/1", nil))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `id="root"`) {
		t.Fatalf("SPA fallback = %d %s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/v1/incidents/1/control-room", nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("backend path = %d", resp.Code)
	}

	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if resp.Code != http.StatusOK || resp.Body.String() != "console.log(1)" {
		t.Fatalf("asset = %d %s", resp.Code, resp.Body.String())
	}

	var _ fs.FS = files
}

func TestStaticRejectsMissingAssetsAndBackendNamespaces(t *testing.T) {
	files := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(`<!doctype html><div id="root"></div>`)}}
	handler := NewStaticAPI(files)

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d", resp.Code)
	}

	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/auth/feishu/start", nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("auth fallback = %d", resp.Code)
	}

	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/", nil))
	if resp.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("index response has no CSP")
	}
}
