// Package version exposes build-time identity stamped via -ldflags.
//
// At build time the Makefile / Dockerfile / release pipeline inject:
//
//	-X github.com/novaworkbench/backend/internal/version.Version=<vX.Y.Z>
//	-X github.com/novaworkbench/backend/internal/version.Commit=<git SHA>
//	-X github.com/novaworkbench/backend/internal/version.BuildDate=<RFC3339>
//
// Local dev builds without ldflags report "dev" so the field is never
// empty in /api/health.
package version

var (
	// Version is the release tag (e.g. "v0.2.0") or "dev" for local builds.
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"
	// BuildDate is the RFC3339 UTC build timestamp.
	BuildDate = "unknown"
)
