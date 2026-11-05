// Package version holds build metadata injected at link time (see Makefile and .goreleaser.yaml).
package version

// Set via -ldflags "-X github.com/Foncifelo/sidecar-proxy/internal/version.Version=…".
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a single-line description for --version and the heartbeat.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
