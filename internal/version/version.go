// Package version holds build metadata injected at link time.
package version

import "runtime"

// These are set with -ldflags at build time. The defaults make an unstamped
// local build obviously distinguishable from a released one, which matters
// because experiment results record the tool version alongside their data.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// GoVersion reports the toolchain the binary was built with.
func GoVersion() string { return runtime.Version() }
