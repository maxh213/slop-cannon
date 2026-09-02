package cli

// Build metadata injected by the Makefile through -ldflags. The defaults are
// used by plain "go build" / "go run".
var (
	// Version is the semantic version or git describe output of this build.
	Version = "dev"
	// Commit is the short git commit hash of this build.
	Commit = "none"
	// Date is the UTC build timestamp in RFC 3339 format.
	Date = "unknown"
)
