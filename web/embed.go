// Package web embeds the built web interface so that Veritix ships as one
// binary with nothing to install alongside it.
//
// The single-binary promise is about runtime, not build time: Vite still has to
// run to produce dist/, but no Node is needed to run the result. `make web`
// builds it; a committed dist/.gitkeep keeps this package compiling before any
// build has happened, and the server reports the interface as not built.
package web

import (
	"embed"
	"io/fs"
)

// all: is needed rather than a plain pattern because the placeholder is a
// dotfile, and Vite's asset names are not known here.
//
//go:embed all:dist
var dist embed.FS

// FS returns the built interface, rooted at web/dist.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// dist is always embedded — at minimum the committed placeholder — so
		// this cannot fail without the binary having been built wrong.
		panic(err)
	}
	return sub
}

// Built reports whether a real build was embedded, as opposed to just the
// placeholder. The server uses it to say something useful instead of serving a
// blank page.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}
