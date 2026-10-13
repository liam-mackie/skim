// Package web embeds the built Svelte app so the Skim binaries need no asset
// directory at runtime. Build it first with `npm --prefix web run build`.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the contents of web/dist captured at compile time.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // "dist" is a valid, embedded path.
	}
	return sub
}
