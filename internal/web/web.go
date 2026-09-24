// Package web serves the built SPA embedded from web/dist.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the page; without a build in dist it serves a one-line placeholder.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err == nil {
		if _, err := fs.Stat(sub, "index.html"); err == nil {
			return http.FileServer(http.FS(sub))
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "llamesh: the page is not built; run npm run build in web/ and rebuild", http.StatusNotFound)
	})
}
