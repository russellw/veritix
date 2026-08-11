package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// contentSecurityPolicy is the browser-side half of Veritix's central promise.
//
// The product exists so that commercially sensitive data never reaches a
// vendor, and internal/report and the rows endpoint enforce that inside the
// process. But the interface a customer actually looks at runs in a browser,
// where a single compromised dependency in the bundle could read a finding's
// rows and POST them anywhere. `connect-src 'self'` is what stops that: the
// page may talk to the server it came from and to nothing else.
//
// The rest closes the doors that go with it. `default-src 'self'` means no
// font, image, or frame from another origin either. `object-src 'none'` and
// `frame-ancestors 'none'` remove plugin embedding and clickjacking.
// `base-uri 'self'` stops an injected <base> re-pointing every relative URL,
// which would otherwise route the whole API elsewhere without touching a fetch.
//
// There is deliberately no 'unsafe-inline' anywhere, for scripts or for styles.
// The web interface has no runtime dependency that sets inline style attributes
// — which is a large part of why it has no UI framework — so the strict policy
// costs nothing here. Keep it that way: adding 'unsafe-inline' to make a
// component work would trade the guarantee for a convenience.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"frame-ancestors 'none'"

// spaHandler serves the embedded web interface: a built asset when the request
// names one, and index.html otherwise so that client-side routes such as
// /runs/{id}/findings/{fid} survive a reload or a pasted link.
//
// It is only ever reached for paths that did not match /api/v1/, so it cannot
// shadow the API.
func (s *Server) spaHandler(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// The interface can navigate to another origin, but it should never
		// tell that origin which Veritix instance sent it there.
		w.Header().Set("Referrer-Policy", "same-origin")

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		// fs.ValidPath rejects anything with a .. element or a leading slash,
		// and the filesystem is the embedded build rather than the disk, so a
		// request cannot name a file outside it.
		if fs.ValidPath(name) && name != "index.html" {
			if info, err := fs.Stat(dist, name); err == nil && !info.IsDir() {
				// Vite fingerprints asset filenames, so a given URL's content
				// never changes and can be cached hard. index.html deliberately
				// is not cached: it is what points at the current fingerprints.
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}

		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			// Only reachable when the binary was built without running the web
			// build. Say which command fixes it rather than serving a blank page.
			writeError(w, http.StatusServiceUnavailable,
				"the web interface was not built into this binary: run `make web` and rebuild")
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(index)
	})
}
