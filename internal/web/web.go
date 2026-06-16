package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var staticFS embed.FS

// StaticFS returns the embedded static asset filesystem rooted at "static".
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded path is a compile-time constant; cannot fail at runtime
	}
	return sub
}

// maskKey hides all but the last 4 characters of a secret for display.
func maskKey(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}
