package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiFS holds the control-plane web UI. Embedded into the binary so the
// deployment story stays "ship one file" — install.sh copies a single
// leap-gateway binary and there is no separate asset directory to keep in
// sync with it.
//
// internal/api/ui/ is BUILD OUTPUT, committed on purpose: the source is the
// React + Arco Design app under web/, and `make ui` (npm run build) writes
// the bundle here. Committing it keeps `go build`, CI and stage.sh free of
// any Node toolchain. Never hand-edit files in ui/ — edit web/src and
// rebuild, or the next build silently reverts the change.
//
//go:embed ui
var uiFS embed.FS

// registerUI mounts the web UI at /ui/ (and redirects /ui → /ui/).
//
// AUTH: these routes are deliberately NOT wrapped in s.auth. They serve
// static assets only — no node data, no config, no secrets. Every byte of
// real data the page displays comes from the /api/* endpoints, which stay
// behind s.auth; the page asks the operator for the token and sends it as
// a Bearer header. Requiring auth for the HTML itself would be a worse
// design: a browser cannot attach a Bearer header to a plain navigation,
// so it would force the token into a query string or cookie, i.e. into
// server logs and browser history.
//
// Reachability is governed by api.listen (default 127.0.0.1:18080) and the
// node's nft rules, which unconditionally deny the FeiLian client subnet
// from reaching the API port. Exposing the UI does not widen that surface.
func (s *Server) registerUI(mux *http.ServeMux) {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		// Only reachable if the embed directive and this path disagree,
		// which is a build-time mistake, not a runtime condition.
		panic("api: embedded ui subtree missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	mux.Handle("GET /ui/", noStoreHTML(http.StripPrefix("/ui/", fileServer)))
}

// noStoreHTML keeps browsers from caching the shell across upgrades. The
// bundle is ~1 MB fetched once per page load from a LAN / ssh-tunnelled
// node, so revalidating costs little — while a stale cached app.js after a
// binary upgrade produces confusing "the UI is out of date but the
// endpoint changed" bug reports.
func noStoreHTML(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// Static, self-contained page: no inline script or style, no
		// remote origins, no framing. Kept in sync with what index.html
		// actually loads (assets/app.js, assets/app.css, favicon.svg).
		// web/vite.config.ts serves `npm run preview` with this same
		// policy so a violation shows up before it reaches a node.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; script-src 'self'; "+
				"connect-src *; img-src 'self' data:; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}
