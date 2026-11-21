// Package web embeds the built dashboard so the binary serves it with no Node at run time.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// The bundle is committed under dist (docs/adr/0008), so this builds on a clean clone that
// has never run npm. all: includes names a bundler may emit with a leading dot or underscore.
//
//go:embed all:dist
var dist embed.FS

// Dist returns the dashboard rooted at the directory it was built into.
func Dist() http.FileSystem {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		// The embed directive guarantees the directory exists when this package builds.
		panic(err)
	}
	return http.FS(root)
}
