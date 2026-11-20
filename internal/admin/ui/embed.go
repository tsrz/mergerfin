package ui

import "embed"

// FS holds the embedded static assets for the MergerFin admin dashboard.
//
//go:embed index.html
var FS embed.FS
