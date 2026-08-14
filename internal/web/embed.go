// Package web embeds the built SPA. `go:embed` patterns are always relative
// to this file's own directory and cannot reach outside it (no `../`), so
// the SPA build cannot be embedded straight from web/dist where Vite writes
// it; `make web` builds there and then copies the result into dist here.
// `make web` must run before `go build`; CI enforces the ordering. dist/
// starts as a placeholder .gitkeep so a clean checkout still compiles before
// the first `make web`, since an empty go:embed directory is a build error.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var assets embed.FS

// Static returns the built SPA rooted at dist.
func Static() (fs.FS, error) {
	return fs.Sub(assets, "dist")
}
