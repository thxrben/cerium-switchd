// Package version holds the build version, set at link time:
//
//	go build -ldflags "-X mclag/internal/version.Version=v1.2.3"
package version

// Version is the release version, or "dev" for local builds.
var Version = "dev"
