// Package web serves the embedded frontend SPA from the Go binary so a
// release ships as a single executable. The frontend is built to
// `frontend/dist` (Vite default), copied into this directory by the root
// `Makefile` (or the multi-stage Dockerfile), and pulled into the binary
// via `//go:embed all:dist`.
//
// The package lives in backend/web/ (sibling of dist/) because //go:embed
// cannot traverse upward with `..` paths.
//
// Routing: the same http.ServeMux also serves every `/api/...` route. Go 1.22
// ServeMux gives exact patterns priority over the `/` catch-all, so all
// registered API handlers run first; this package only fires for paths the
// API mux didn't claim.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// HasFrontend reports whether the embed bundle contains the SPA index.
// When NOVA_SKIP_FRONTEND=1 (or when no frontend was built into the binary)
// the dist directory only has the .gitkeep placeholder; we skip registering
// the static handler so the API server still runs in backend-only mode.
func HasFrontend() bool {
	_, err := embedded.ReadFile("dist/index.html")
	return err == nil
}

// SPA fallback handler: serves files from dist; when a path doesn't match a
// concrete asset (e.g. client-side routes like /projects/:id, /settings/...),
// fall back to index.html so the React router can take over. The frontend's
// own vite.config.ts / nginx config previously had no SPA fallback, which
// caused hard 404s on refresh; this handler is the single-binary equivalent.
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Anything under /api/ is the API mux's job. Defensive: this handler
		// is registered as the `/` catch-all (Go 1.22 ServeMux only falls
		// through to it for non-API paths), but if anyone ever mounts it
		// differently we still skip here.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}

		// Strip the leading "/" so the embedded FS sees "index.html", "assets/...".
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		// Try the concrete file first.
		if _, err := fs.Stat(fsys, path); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA fallback: serve index.html so react-router handles the route.
		// Don't cache the fallback — vite-built index.html is small but must
		// pick up new deployments promptly.
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

// Register attaches the SPA handler at "/" as a catch-all. Must be called
// AFTER all precise `/api/...` routes are registered on mux so the catch-all
// only fires for paths the API mux didn't claim.
func Register(mux *http.ServeMux) {
	if !HasFrontend() {
		return
	}
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		return
	}
	mux.Handle("/", spaHandler(sub))
}
