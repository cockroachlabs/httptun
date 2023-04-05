//go:build tools

// A file for defining build tools used for CI.
// See https://play-with-go.dev/tools-as-dependencies_go119_en#adding-tool-dependencies for more.

package tools

import (
	_ "golang.org/x/lint"
	_ "honnef.co/go/tools/cmd/staticcheck"
)
