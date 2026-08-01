// Package web serves the static site, compiled into the binary.
//
// Embedding rather than mounting a volume means one artifact to deploy and no
// chance of the binary and the HTML drifting apart. The whole site is a few KB;
// if it grows past a megabyte or two, revisit.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// StaticHandler serves the embedded site with cache headers appropriate to
// each file type.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Only possible if the embed directive and this path disagree, which
		// is a compile-time-ish error — fail loudly at startup, not at request.
		panic("web: static subtree missing: " + err.Error())
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTML changes every deploy; assets are effectively immutable because
		// they'd get a new filename if they changed. Different TTLs for each.
		if isHTML(r.URL.Path) {
			w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func isHTML(path string) bool {
	if path == "" || path == "/" {
		return true
	}
	n := len(path)
	return n >= 5 && path[n-5:] == ".html"
}
