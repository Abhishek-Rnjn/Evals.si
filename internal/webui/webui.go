// Package webui serves the read-only web UI: static files, embedded in the
// binary, that read evalsid's REST API with the viewer's own credential.
// The files hold no data, so they are served without authentication; every
// API call they make goes through the gate like any other client's.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Prefix is where the UI is served.
const Prefix = "/ui/"

// csp allows only the UI's own files and same-origin API calls: no inline
// script, no third-party content, no framing.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Register mounts the UI on mux: /ui/ and a redirect from /ui.
func Register(mux *http.ServeMux) {
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // the embed is static
	}
	fileServer := http.StripPrefix(Prefix, http.FileServer(http.FS(files)))
	mux.Handle("GET "+Prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	}))
	mux.Handle("GET /ui", http.RedirectHandler(Prefix, http.StatusMovedPermanently))
}
