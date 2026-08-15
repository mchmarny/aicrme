// Package testfs provides a minimal static filesystem for API tests.
package testfs

import (
	"io/fs"
	"testing/fstest"
)

// Static returns a one-file SPA stand-in.
func Static() fs.FS {
	return fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>aicrme</title>")}}
}
