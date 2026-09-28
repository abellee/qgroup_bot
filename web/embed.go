// Package web carries the compiled admin front-end into the binary.
//
// dist/ is built by Vite from the sources next to this file: the Dockerfile's
// node stage fills it before go build runs, and locally
// `npm --prefix web run build` does the same. A checkout that has not built it
// still compiles, because dist/.gitkeep is committed, and the panel then answers
// 503 with the build command instead of serving a stale bundle.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var bundle embed.FS

// Dist is the bundle root.
func Dist() (fs.FS, error) {
	return fs.Sub(bundle, "dist")
}
