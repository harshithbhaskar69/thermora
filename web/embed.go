// Package web contains the built, self-contained SPA served by the cloud binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var files embed.FS

// Shared public theme assets also serve the separate manufacturer origin.
//
//go:embed public/theme
var themeFiles embed.FS

func Assets() (fs.FS, error) { return fs.Sub(files, "dist") }

func ThemeAssets() (fs.FS, error) { return fs.Sub(themeFiles, "public") }
