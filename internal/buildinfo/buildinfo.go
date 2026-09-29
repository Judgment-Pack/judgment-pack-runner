// Package buildinfo names the repository release an executable was built from.
package buildinfo

import "runtime/debug"

// releaseVersion is the repository tag, set by the release build alone
// (docs/releasing.md). It is unrelated to a pack's version, a frozen pack
// release, or the jobs protocol.
var releaseVersion string

// Version is the tag of a release build, else the module version the toolchain
// recorded (a tag or a pseudo-version, when VCS stamping is available), else
// "unversioned build" where it recorded none, as under -buildvcs=false.
func Version() string {
	if releaseVersion != "" {
		return releaseVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "unversioned build"
}
