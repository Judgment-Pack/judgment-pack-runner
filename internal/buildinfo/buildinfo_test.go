package buildinfo

import "testing"

func TestReleaseVersionLeads(t *testing.T) {
	before := Version()
	if before == "" || before == "(devel)" {
		t.Fatalf("Version() is %q without a release version", before)
	}
	releaseVersion = "v9.8.7"
	t.Cleanup(func() { releaseVersion = "" })
	if got := Version(); got != "v9.8.7" {
		t.Fatalf("Version() is %q with a release version set", got)
	}
	releaseVersion = ""
	if got := Version(); got != before {
		t.Fatalf("Version() is %q after the release version was cleared, and was %q", got, before)
	}
}
