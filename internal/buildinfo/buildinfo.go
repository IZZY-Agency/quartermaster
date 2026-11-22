// Package buildinfo holds the version and commit the Makefile stamps into the
// binary with -ldflags. A plain "go build" leaves the defaults.
package buildinfo

var (
	// Version is the release or git-describe version; "dev" without LDFLAGS.
	Version = "dev"
	// Commit is the short commit hash; "unknown" without LDFLAGS.
	Commit = "unknown"
)
