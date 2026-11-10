// Package bundle contains release assets prepared by npm run assets:prepare.
package bundle

import (
	"embed"
	"io/fs"
)

//go:embed all:frontend
var frontend embed.FS

//go:embed scrcpy-server-v5.0.1
var ScrcpyServer []byte

// Frontend returns the built application rooted at index.html.
func Frontend() (fs.FS, error) { return fs.Sub(frontend, "frontend") }
