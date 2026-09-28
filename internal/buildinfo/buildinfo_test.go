package buildinfo

import "testing"

// Holds whether or not the build under test was given a version, and leaves
// the variable as it found it.
func TestReleaseVersionLeads(t *testing.T) {
	given := releaseVersion
	t.Cleanup(func() { releaseVersion = given })

	releaseVersion = ""
	without := Version()
	if without == "" || without == "(devel)" {
		t.Fatalf("Version() is %q without a release version", without)
	}
	releaseVersion = "v9.8.7"
	if got := Version(); got != "v9.8.7" {
		t.Fatalf("Version() is %q with a release version set", got)
	}
	releaseVersion = ""
	if got := Version(); got != without {
		t.Fatalf("Version() is %q after the release version was cleared, and was %q", got, without)
	}
}
