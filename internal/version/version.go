// Package version reports the build version of Evals.si binaries.
package version

// Version is overridden at build time with
// -ldflags "-X github.com/abhishek-rnjn/evals.si/internal/version.Version=v0.1.0".
var Version = "0.0.0-dev"

// APIVersion is the protobuf API version this build serves.
const APIVersion = "evalsi.v1alpha1"
