package web

import "embed"

// DistFS contains the production static bundle produced by `npm run build`.
// The bundle is a build artifact and is not committed; web/dist/.gitkeep keeps
// the directory present so the Go service still compiles before the frontend
// has been built. Serving then yields 404s until the bundle exists.
//
//go:embed all:dist
var DistFS embed.FS
