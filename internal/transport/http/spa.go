package httpapi

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SPA serves the built frontend from an embedded filesystem.
//
// Any path that is not an API route falls back to index.html, because the
// router is client-side: a browser opening /vms directly must receive the app,
// not a 404.
type SPA struct {
	files fs.FS
}

// NewSPA builds the handler. A nil filesystem disables it, which is what the
// dev build does while Vite serves the frontend on its own port.
func NewSPA(files fs.FS) *SPA {
	if files == nil {
		return nil
	}
	if _, err := fs.Stat(files, "index.html"); err != nil {
		return nil
	}
	return &SPA{files: files}
}

func (s *SPA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}

	file, err := s.files.Open(name)
	if err != nil {
		// Unknown path: hand the client-side router its entry point.
		s.serveIndex(w, r)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		s.serveIndex(w, r)
		return
	}

	// Vite fingerprints asset filenames, so they can be cached indefinitely
	// while index.html must always be revalidated or a deploy would not take
	// effect until every cache expired.
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}

	http.ServeContent(w, r, name, info.ModTime(), file.(io.ReadSeeker))
}

func (s *SPA) serveIndex(w http.ResponseWriter, r *http.Request) {
	index, err := s.files.Open("index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer index.Close()

	info, err := index.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", info.ModTime(), index.(io.ReadSeeker))
}
