// Package static embeds the vendored web assets — the Tailwind runtime and the
// UI fonts — so pages render with zero external network requests (works
// offline / air-gapped, and no CDN sees your page loads). The assets ship
// inside the binary, matching the embedded-migrations deploy model.
//
// tailwind.js is the Tailwind Play build (v3.4.16 from cdn.tailwindcss.com,
// MIT — see assets/tailwind.LICENSE.txt), kept because the templates are
// hand-maintained Go with class strings — a build-time Tailwind pipeline would
// add a toolchain for no benefit at this scale. Fonts are the latin +
// latin-ext subsets from Google Fonts (SIL OFL 1.1 — see assets/fonts/OFL.txt).
package static

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assetsFS embed.FS

// Handler serves the embedded assets under the /static/ URL prefix.
func Handler() http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// Unreachable: "assets" is embedded at compile time.
		panic(err)
	}
	fileServer := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Embedded assets only change with the binary; a day of caching keeps
		// page loads light without sticky-stale risk across upgrades.
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fileServer.ServeHTTP(w, r)
	})
}
