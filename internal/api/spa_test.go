package api

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

// stubWeb stands in for a Vite build. The real bundle is not used here because
// these tests are about how the interface is served, not what is in it, and a
// test that needs `make web` to have been run first is a test that gets skipped.
func stubWeb() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><div id=root></div>")},
		"assets/index.js":  {Data: []byte("console.log(1)")},
		"assets/index.css": {Data: []byte("body{}")},
		"favicon.svg":      {Data: []byte("<svg/>")},
	}
}

// TestWebInterfaceIsServedUnderAStrictCSP pins the browser-side half of the
// promise that customer data does not leave the process.
//
// The interface can read a finding's offending rows, so a compromised script in
// the bundle would be sitting next to exactly the data the product exists to
// keep in. connect-src 'self' is what stops it reaching anywhere with them, and
// this test is here so that loosening it has to be deliberate — the same reason
// TestDefaultReportContainsNoRawValues exists for the report.
func TestWebInterfaceIsServedUnderAStrictCSP(t *testing.T) {
	ts := newTestServerWith(t, "", stubWeb())

	for _, path := range []string{"/", "/datasets/abc", "/assets/index.js"} {
		resp := ts.get(path)
		if resp.Status != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200", path, resp.Status)
		}

		csp := resp.Header.Get("Content-Security-Policy")
		if csp == "" {
			t.Fatalf("GET %s: no Content-Security-Policy header", path)
		}

		for _, want := range []string{
			"default-src 'self'",
			"connect-src 'self'", // the one that keeps data in
			"script-src 'self'",
			"object-src 'none'",
			"base-uri 'self'",
			"frame-ancestors 'none'",
		} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %s: policy is missing %q\n  got: %s", path, want, csp)
			}
		}

		// Not merely absent from script-src: an inline style attribute is a
		// place to smuggle an exfiltrating url(), and the interface is built
		// with no dependency that needs one. See internal/api/spa.go.
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("GET %s: policy has been loosened with unsafe-*: %s", path, csp)
		}

		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s: X-Content-Type-Options is %q, want nosniff", path, got)
		}
	}
}

// TestClientSideRoutesFallBackToTheApp covers reloading a deep link. A finding's
// id is stable across runs, so /runs/{id}/findings/{fid} is a URL somebody
// sends to a colleague, and it has to survive being pasted into a fresh tab.
func TestClientSideRoutesFallBackToTheApp(t *testing.T) {
	ts := newTestServerWith(t, "", stubWeb())

	for _, path := range []string{
		"/",
		"/datasets/01234567",
		"/runs/01234567",
		"/runs/01234567/findings/abcdef",
		"/nothing-like-a-route",
	} {
		resp := ts.get(path)
		if resp.Status != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, resp.Status)
			continue
		}
		if !strings.Contains(string(resp.Body), `id=root`) {
			t.Errorf("GET %s: did not serve the app shell", path)
		}
	}
}

// TestAssetsAreServedAndCached checks that a real file wins over the fallback,
// and that fingerprinted assets say they can be cached while index.html says it
// cannot — index.html is what names the current fingerprints.
func TestAssetsAreServedAndCached(t *testing.T) {
	ts := newTestServerWith(t, "", stubWeb())

	asset := ts.get("/assets/index.js")
	if asset.Status != http.StatusOK {
		t.Fatalf("GET /assets/index.js: status %d", asset.Status)
	}
	if string(asset.Body) != "console.log(1)" {
		t.Errorf("GET /assets/index.js served %q, not the asset", asset.Body)
	}
	if cc := asset.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset Cache-Control is %q, want it cacheable", cc)
	}

	index := ts.get("/")
	if cc := index.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("index.html Cache-Control is %q, want no-store", cc)
	}
}

// TestTheAppDoesNotShadowTheAPI is the reason the SPA fallback is registered on
// "/" rather than in place of the 404: every /api/v1 path must still reach a
// handler, and an unknown one must still be a JSON error rather than an HTML
// page a script cannot parse.
func TestTheAppDoesNotShadowTheAPI(t *testing.T) {
	ts := newTestServerWith(t, "", stubWeb())

	health := ts.get("/api/v1/health")
	if health.Status != http.StatusOK {
		t.Fatalf("GET /api/v1/health: status %d", health.Status)
	}
	if ct := health.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("health Content-Type is %q, want JSON", ct)
	}

	missing := ts.get("/api/v1/no-such-thing")
	if missing.Status != http.StatusNotFound {
		t.Errorf("GET an unknown API path: status %d, want 404", missing.Status)
	}
	if ct := missing.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("unknown API path returned %q, want a JSON error", ct)
	}
}

// TestServerWithoutAWebBuildStillServesTheAPI covers the binary somebody builds
// with plain `go build` and no `make web`. The API has to keep working, and the
// interface has to say what is wrong rather than serve a blank page.
func TestServerWithoutAWebBuildStillServesTheAPI(t *testing.T) {
	// Nil: no interface was embedded at all.
	ts := newTestServer(t, "")
	if resp := ts.get("/api/v1/health"); resp.Status != http.StatusOK {
		t.Fatalf("GET /api/v1/health: status %d", resp.Status)
	}
	if resp := ts.get("/"); resp.Status != http.StatusNotFound {
		t.Errorf("GET / with no interface: status %d, want 404", resp.Status)
	}

	// Embedded, but holding only the committed placeholder — what `go build`
	// produces on a clean checkout.
	placeholder := newTestServerWith(t, "", fstest.MapFS{".gitkeep": {Data: nil}})
	if resp := placeholder.get("/api/v1/health"); resp.Status != http.StatusOK {
		t.Fatalf("GET /api/v1/health: status %d", resp.Status)
	}

	resp := placeholder.get("/")
	if resp.Status != http.StatusServiceUnavailable {
		t.Errorf("GET / with an unbuilt interface: status %d, want 503", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "make web") {
		t.Errorf("the unbuilt-interface message does not say how to fix it: %s", resp.Body)
	}
}
