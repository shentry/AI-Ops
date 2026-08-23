package web

import "embed"

// DistFS contains the production static bundle. The checked-in fallback keeps
// the Go service buildable before the optional Vite build has run.
//
//go:embed dist
var DistFS embed.FS
