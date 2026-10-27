// Package web holds the browser UI, embedded into the binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Static returns the UI files with "static/" stripped from their paths.
func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the embedded directory is fixed at build time
	}
	return sub
}
