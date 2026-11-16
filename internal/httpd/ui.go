package httpd

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleUI 提供 Web 管理界面静态资源。
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	webDir := filepath.Clean(s.cfg.WebDir())
	upath := r.URL.Path
	if upath == "/" || upath == "" {
		upath = "/index.html"
	}
	fp := filepath.Join(webDir, filepath.Clean(upath))
	if !strings.HasPrefix(fp, webDir) {
		http.NotFound(w, r)
		return
	}
	fi, err := os.Stat(fp)
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, fp)
}
