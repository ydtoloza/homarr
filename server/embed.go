// Package server embeds the homarr-go frontend assets into the binary.
//
// CI copies the Next.js static export into frontend/dist before building,
// so the compiled React app ships inside the single homarr-go binary.
// The placeholder committed in git keeps `go build` working without a
// frontend build (until the M1 export spike lands).
package server

import (
	"embed"
	"io/fs"
)

//go:embed all:frontend/dist
var distFS embed.FS

// Sub returns the embedded frontend dist as an fs.FS rooted at dist.
func Sub() fs.FS {
	sub, err := fs.Sub(distFS, "frontend/dist")
	if err != nil {
		// Static path; unreachable unless the embed directive changes.
		panic("server: embed frontend/dist missing: " + err.Error())
	}
	return sub
}
