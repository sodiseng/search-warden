// Package buildinfo holds version metadata injected at build time via -ldflags.
package buildinfo

// Overridden with: -ldflags "-X github.com/sodiseng/search-warden/gateway/internal/buildinfo.Version=v1.2.3 ..."
var (
	Version = "dev"
	Commit  = "none"
	Date    = ""
)

// String returns a single-line human-readable version.
func String() string {
	s := Version + " (" + Commit
	if Date != "" {
		s += ", " + Date
	}
	return s + ")"
}
