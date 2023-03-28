//go:build tools

// A file for defining build tools.
// See https://play-with-go.dev/tools-as-dependencies_go119_en#adding-tool-dependencies for more.

package tools

import (
	_ "golang.org/x/lint"
	_ "honnef.co/go/tools"
)
