package api

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gogf/gf/v2/net/ghttp"
)

// StaticAPI serves embedded frontend assets without allowing SPA fallback to
// swallow backend 404s. The fs may be an embed.FS rooted at "dist" or an FS
// already rooted at its web output directory.
type StaticAPI struct {
	files fs.FS
}

func NewStaticAPI(content fs.FS) *StaticAPI {
	content = normalizeStaticFS(content)
	return &StaticAPI{files: content}
}

func normalizeStaticFS(content fs.FS) fs.FS {
	if content == nil {
		return nil
	}
	if _, err := fs.Stat(content, "index.html"); err == nil {
		return content
	}
	if _, err := fs.Stat(content, "dist/index.html"); err == nil {
		if sub, subErr := fs.Sub(content, "dist"); subErr == nil {
			return sub
		}
	}
	return content
}

// NewStaticHandler is a descriptive alias used by route assembly code.
func NewStaticHandler(content fs.FS) *StaticAPI { return NewStaticAPI(content) }

func (h *StaticAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *StaticAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.files == nil || r == nil {
		if isBackendPath(r) {
			writeNotFound(w)
		} else {
			writeError(w, http.StatusNotFound, "static assets unavailable")
		}
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w)
		return
	}
	if isBackendPath(r) {
		writeNotFound(w)
		return
	}
	requestPath, valid := staticRequestPath(r.URL.Path)
	if !valid {
		writeNotFound(w)
		return
	}
	if info, err := fs.Stat(h.files, requestPath); err == nil && !info.IsDir() {
		h.serveFile(w, r, requestPath)
		return
	}
	if isMissingStaticAsset(requestPath) {
		writeNotFound(w)
		return
	}
	// Unknown non-backend paths are client-side routes. They receive the
	// committed index fallback; API-like paths above always remain JSON 404s.
	h.serveFile(w, r, "index.html")
}

func (h *StaticAPI) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	file, err := h.files.Open(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "static asset not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusNotFound, "static asset not found")
		return
	}
	seeker, ok := file.(io.ReadSeeker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "static asset is not readable")
		return
	}
	if strings.EqualFold(path.Ext(name), ".html") {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
	}
	http.ServeContent(w, r, path.Base(name), info.ModTime(), seeker)
}

func staticRequestPath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "/" {
		return "index.html", true
	}
	clean := path.Clean(strings.TrimPrefix(raw, "/"))
	if clean == "." || clean == "" {
		return "index.html", true
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

func isBackendPath(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	clean := "/" + strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	for _, prefix := range []string{"/api", "/auth", "/debug", "/webhook", "/integrations", "/metrics"} {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return true
		}
	}
	return false
}

func isMissingStaticAsset(name string) bool {
	return strings.HasPrefix(name, "assets/") || path.Ext(name) != ""
}
