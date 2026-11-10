// Package web embeds the browser UI (no build step needed at runtime).
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Static is the UI root (index.html, app.js, app.css, vendor/).
func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
