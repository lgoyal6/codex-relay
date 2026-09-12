package httpapi

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// dashboardFS holds the compiled React/TypeScript frontend.
//
// Node is a contributor build dependency only: `npm run build` in web/ writes into
// internal/httpapi/dist, which is embedded here, so an end user runs a single executable
// with no Node runtime. All assets are local; nothing is fetched from a CDN.
//
//go:embed all:dist
var dashboardFS embed.FS

// Dashboard serves the embedded frontend and hands the page its session token.
//
// The token is delivered once, in the HTML, so the page can call the API. It is not a
// cookie set for every request, and it is never placed in a URL we log.
func Dashboard(token, version string) http.Handler {
	sub, err := fs.Sub(dashboardFS, "dist")
	if err != nil {
		return placeholder(err, version)
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		// The frontend has not been built into this binary. Say so plainly rather than
		// serving a blank page.
		return placeholder(fmt.Errorf("the dashboard was not compiled into this build"), version)
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A strict local policy: no remote scripts, styles, fonts, images or connections.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; font-src 'self'; connect-src 'self'; "+
				"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		clean := strings.TrimPrefix(r.URL.Path, "/")
		if clean == "" || !hasFile(sub, clean) {
			serveIndex(w, sub, token, version)
			return
		}
		files.ServeHTTP(w, r)
	})
}

func hasFile(sub fs.FS, name string) bool {
	st, err := fs.Stat(sub, name)
	return err == nil && !st.IsDir()
}

// serveIndex injects the session token into the page as a JSON script block. Using a
// data block rather than string interpolation into JavaScript keeps the token out of any
// executable context.
func serveIndex(w http.ResponseWriter, sub fs.FS, token, version string) {
	raw, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "dashboard is unavailable", http.StatusInternalServerError)
		return
	}
	boot := fmt.Sprintf(
		`<script type="application/json" id="codexrelay-boot">{"token":%q,"version":%q}</script>`,
		token, version)
	html := strings.Replace(string(raw), "</head>", boot+"</head>", 1)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
}

func placeholder(reason error, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>codex-relay</title>
<body style="font:14px -apple-system,BlinkMacSystemFont,Segoe UI,sans-serif;margin:0;display:grid;place-items:center;height:100vh;background:#f6f7f9;color:#16181d">
<div style="max-width:460px;background:#fff;border:1px solid #dfe2e8;border-radius:10px;padding:26px 30px">
<h1 style="font-size:16px;margin:0 0 10px">Dashboard not built</h1>
<p style="margin:0 0 10px;color:#5b6270">%s. The service and its CLI are running normally; only the browser dashboard is missing from this build.</p>
<p style="margin:0;color:#5b6270">Contributors: run <code>npm install &amp;&amp; npm run build</code> in <code>web/</code>, then rebuild. Version %s.</p>
</div>`, reason, version)
	})
}
