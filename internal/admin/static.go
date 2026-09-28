package admin

import (
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
)

// serveStatic gives out the compiled Vue bundle. The app itself is public - it
// holds no secrets - and every piece of data it can reach comes back through the
// cookie-guarded JSON API.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "只接受 GET")
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/admin")
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		name = "index.html"
	}

	f, err := s.dist.Open(name)
	if err != nil {
		// A missing entry document means the bundle was never built, and a path
		// without an extension is the app's own route rather than a file, so both
		// fall through to index.html. Anything else is genuinely absent.
		if name == "index.html" || path.Ext(name) == "" {
			s.serveIndex(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "文件不存在")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		s.serveIndex(w, r)
		return
	}

	w.Header().Set("content-type", contentTypeOf(name))
	w.Header().Set("cache-control", cacheControlOf(name))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		s.log.Error("static file copy failed", "name", name, "error", err)
	}
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := s.dist.Open("index.html")
	if err != nil {
		// Only ever reached when the bundle was not built into the binary.
		http.Error(w, "后台前端未构建：仓库里跑 npm --prefix web run build，镜像里由 Dockerfile 的 node 阶段生成", http.StatusServiceUnavailable)
		return
	}
	defer f.Close()

	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		s.log.Error("index copy failed", "error", err)
	}
}

func contentTypeOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	switch ext {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	}
	return "application/octet-stream"
}

// cacheControlOf splits the hashed assets from the entry document: the first
// never changes name, the second must never be reused, or an operator keeps
// loading the bundle they just replaced.
func cacheControlOf(name string) string {
	if strings.HasPrefix(name, "assets/") {
		return "public, max-age=31536000, immutable"
	}
	return "no-store"
}
