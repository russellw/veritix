package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// externalRef finds a src or href pointing at another origin, including the
// protocol-relative form.
var externalRef = regexp.MustCompile(`(?i)(src|href)\s*=\s*["'](https?:)?//`)

// TestBundleLoadsNothingFromAnotherOrigin checks the page Veritix serves.
//
// The product's claim is that a customer's data stays on their own machine. A
// <script src> pointing at a CDN would undo that at delivery time: every install
// would fetch code from somebody else's server, and that code would run next to
// the one endpoint that serves raw customer rows. The CSP in internal/api/spa.go
// is what enforces this at runtime; this is the build-time warning, because a
// reference that the CSP blocks is a page that silently half-works rather than
// an obvious failure.
//
// It checks index.html, which is where such a reference would be written, and
// it proves the absence of an accidental one — not the absence of a determined
// one, which no grep can do. The cooldown and the frozen lockfile are what
// address that; see docs/frontend-stack.md.
func TestBundleLoadsNothingFromAnotherOrigin(t *testing.T) {
	index, err := fs.ReadFile(FS(), "index.html")
	if err != nil {
		t.Skip("no web build embedded; run `make web` to include this check")
	}

	if m := externalRef.FindString(string(index)); m != "" {
		t.Errorf("index.html loads something from another origin: %q", m)
	}

	// Everything it does load has to be one of the files shipped beside it.
	for _, ref := range regexp.MustCompile(`(?i)(?:src|href)="([^"]+)"`).FindAllStringSubmatch(string(index), -1) {
		path := strings.TrimPrefix(ref[1], "/")
		if path == "" || strings.HasPrefix(path, "data:") {
			continue
		}
		if _, err := fs.Stat(FS(), path); err != nil {
			t.Errorf("index.html references %q, which is not in the build", ref[1])
		}
	}
}

// TestBuiltReportsWhetherThereIsAnInterface guards the placeholder arrangement:
// dist/.gitkeep keeps the embed compiling on a clean checkout, and Built is how
// the server tells that apart from a real build.
func TestBuiltReportsWhetherThereIsAnInterface(t *testing.T) {
	_, err := fs.Stat(FS(), "index.html")
	if got, want := Built(), err == nil; got != want {
		t.Errorf("Built() = %v, but index.html present = %v", got, want)
	}
}
