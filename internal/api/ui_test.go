package api

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// uiMux builds just the UI routes on a bare mux. The UI is independent of
// Deps (it serves embedded static files), so this avoids standing up a
// full Server with subscriptions, scorer and data-plane controller.
func uiMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	(&Server{}).registerUI(mux)
	return mux
}

func TestUIServesAssets(t *testing.T) {
	mux := uiMux(t)

	cases := []struct {
		path     string
		wantType string
		wantBody string
	}{
		{"/ui/", "text/html", "Interchange"},
		{"/ui/assets/app.css", "text/css", "arco-"},
		{"/ui/assets/app.js", "javascript", "/api/subscriptions"},
		{"/ui/favicon.svg", "image/svg+xml", "<svg"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", c.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, c.wantType) {
			t.Errorf("GET %s Content-Type = %q, want to contain %q", c.path, ct, c.wantType)
		}
		body, _ := io.ReadAll(rec.Body)
		if !strings.Contains(string(body), c.wantBody) {
			t.Errorf("GET %s body missing %q", c.path, c.wantBody)
		}
	}
}

func TestUIRedirectsBarePath(t *testing.T) {
	rec := httptest.NewRecorder()
	uiMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui", nil))

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /ui = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("Location = %q, want /ui/", loc)
	}
}

func TestUISecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	uiMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", nil))

	// no-store: a cached app.js after a binary upgrade would talk to
	// endpoints that may have changed shape.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// The UI's source lives in web/src (React + Arco); internal/api/ui/ only
// holds the minified bundle built from it. Assertions about *how the code
// behaves* read the source, where identifiers and literals are intact.
const uiSrcDir = "../../web/src"

func readUISource(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(uiSrcDir, rel))
	if err != nil {
		t.Fatalf("read web/src/%s: %v", rel, err)
	}
	return string(b)
}

// The UI must not embed a token, and must not prefill the masked URL that
// GET /api/subscriptions returns. Writing a masked URL back through PUT
// would persist "token=c85b***0f02" into the live subscription, breaking
// it in a way that is tedious to diagnose.
func TestUIDoesNotReuseMaskedURL(t *testing.T) {
	src := readUISource(t, "pages/Subscriptions.tsx")

	// The edit path must clear the URL field rather than assign the
	// masked value from the subscription record.
	for _, bad := range []string{"url: editing.url", "url: sub.url", "url: s.url"} {
		if strings.Contains(src, bad) {
			t.Errorf("Subscriptions.tsx prefills masked URL via %q", bad)
		}
	}
	// And it must guard against an operator pasting one back.
	if !strings.Contains(src, `/\*\*\*/`) {
		t.Error("Subscriptions.tsx is missing the masked-URL (***) submit guard")
	}
}

// Token must stay in memory only. Checked against the shipped bundle —
// that is what runs in the browser, third-party code included — and
// against the source with comments stripped, because api.ts documents
// *why* web storage is avoided and that prose must not read as a call.
func TestUIDoesNotUseWebStorage(t *testing.T) {
	bundle, err := uiFS.ReadFile("ui/assets/app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	sources := map[string]string{"embedded assets/app.js": string(bundle)}
	err = filepath.WalkDir(uiSrcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tsx") && !strings.HasSuffix(path, ".ts") {
			return err
		}
		b, err := os.ReadFile(path)
		sources[path] = stripComments(string(b))
		return err
	})
	if err != nil {
		t.Fatalf("walk web/src: %v", err)
	}
	for name, code := range sources {
		if strings.Contains(code, "localStorage") || strings.Contains(code, "sessionStorage") {
			t.Errorf("%s touches web storage; the API token must stay in memory", name)
		}
	}
}

// Every /api path the UI calls must exist on the real server mux, so a
// renamed endpoint fails the build's tests instead of silently 404-ing in
// the browser.
func TestUIEndpointsAreRegistered(t *testing.T) {
	apiGo, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	muxSrc := string(apiGo)

	paths := extractAPIPaths(stripComments(readUISource(t, "api.ts")))
	if len(paths) == 0 {
		t.Fatal("extracted no /api paths from web/src/api.ts — the extractor is broken")
	}
	for _, path := range paths {
		if !strings.Contains(muxSrc, path+`"`) {
			t.Errorf("api.ts calls %s which is not registered in api.go", path)
		}
	}
}

// stripComments removes `/* */` blocks and `//` line comments so
// assertions about the code are not satisfied (or broken) by explanatory
// prose — api.ts documents *why* web storage is avoided, and that
// sentence must not read as a real call. Crude but adequate for the UI
// source: the only string literals containing `//` are URL placeholders,
// and truncating those lines does not affect the assertions above.
func stripComments(src string) string {
	// Block comments first, so a `//` inside one cannot confuse the
	// line pass.
	for {
		start := strings.Index(src, "/*")
		if start < 0 {
			break
		}
		end := strings.Index(src[start+2:], "*/")
		if end < 0 {
			src = src[:start]
			break
		}
		src = src[:start] + src[start+2+end+2:]
	}
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// extractAPIPaths pulls the distinct /api/... route paths out of the JS
// source. Query strings and runtime concatenation (`'/api/x/' + encode(y)`)
// are truncated at the first non-path character, so the result is
// comparable to the mux patterns registered in api.go.
func extractAPIPaths(src string) []string {
	re := regexp.MustCompile(`'(/api/[a-zA-Z0-9/_-]*)`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		path := strings.TrimSuffix(m[1], "/")
		if path == "/api" || path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}
