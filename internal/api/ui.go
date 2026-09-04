package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiFS holds the control-plane web UI. Embedded into the binary so the
// deployment story stays "ship one file" — install.sh copies a single
// leap-gateway binary and there is no separate asset directory to keep in
// sync with it. The UI is plain HTML/CSS/JS with no build step, so what is
// in internal/api/ui/ is exactly what is served.
//
//go:embed ui/index.html ui/app.css ui/app.js
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
// three assets are a few KB each and are fetched once per page load, so
// revalidating costs nothing measurable — while a stale cached app.js
// after a binary upgrade produces confusing "the UI is out of date but
// the endpoint changed" bug reports.
func noStoreHTML(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// Static, self-contained page: no inline handlers, no remote
		// origins, no framing. Kept in sync with what index.html
		// actually loads (its own app.css / app.js and nothing else).
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; script-src 'self'; "+
				"connect-src *; img-src 'self' data:; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}
