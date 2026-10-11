package runner

// The instant freshness is judged against (runner #55).
//
// Preparation judges freshness against one instant, which it retains as
// preparation.verifiedAt: each verified acquisition's observation and serving
// times, from the source's maxAge before that instant to 30 seconds after it;
// and each table of a calculator that computed, from the table's limit before
// it to 30 seconds after it. Offline verification recomputes the preparation
// at the instant the export retains, and nothing else gives it one.
//
// No signature covers that member. The Runtime's record signature covers the
// audit record's bytes, which do not carry it; a receipt signs its source's
// own times, not the instant they were judged against; and the run's chain
// entry binds the record's digest, not the export. A preparation recomputed at
// another instant verifies as the original does, signed and witnessed, when
// its facts, evidence and citations come out the same, as they do unless a
// rule reads runAt or the instant leaves a limit's window. So freshness is
// consistent with the instant the export supplies; nothing establishes that
// instant was the admission instant. verify-run says so for every run whose
// preparation judged freshness, a copy mapping's as much as a rule's.

// freshnessAgainst names the export's member that holds the instant.
const freshnessAgainst = "preparation.verifiedAt"

// The checks a source's freshness was judged by.
const (
	// freshnessAcquisition is a verified acquisition's observation and serving
	// times.
	freshnessAcquisition = "acquisition"
	// freshnessCalculation is a computed calculator answer's table instants.
	freshnessCalculation = "calculation"
)

// UnsignedFreshness is verify-run's report of that dependency: the member the
// instant is taken from, the instant, and each source whose freshness was
// judged against it, in the mapping's order of sources.
type UnsignedFreshness struct {
	Against    string            `json:"against"`
	VerifiedAt string            `json:"verifiedAt"`
	Sources    []FreshnessSource `json:"sources"`
}

// FreshnessSource is one source whose freshness was judged against the
// instant: by its acquisition, and, for a calculator that computed, by its
// tables too; and how many fact and evidence targets the source maps, with a
// value or without.
type FreshnessSource struct {
	Source  string   `json:"source"`
	Checks  []string `json:"checks"`
	Targets int      `json:"targets"`
}

// judgedFreshness is a source whose freshness preparation judged against its
// instant, as prepareV2 lists it: its acquisition always, and its tables when
// it is a calculator that computed.
type judgedFreshness struct {
	source      string
	calculation bool
}

// unsignedFreshness is the report of a verified preparation's dependency on
// its instant, from what its recomputation judged. It is nil when nothing was
// judged against the instant: no source was acquired.
func unsignedFreshness(p *Preparation, judged []judgedFreshness) *UnsignedFreshness {
	if len(judged) == 0 {
		return nil
	}
	targets := map[string]int{}
	for _, l := range p.Lineage {
		targets[l.Source]++
	}
	u := &UnsignedFreshness{Against: freshnessAgainst, VerifiedAt: p.VerifiedAt, Sources: []FreshnessSource{}}
	for _, j := range judged {
		checks := []string{freshnessAcquisition}
		if j.calculation {
			checks = append(checks, freshnessCalculation)
		}
		u.Sources = append(u.Sources, FreshnessSource{j.source, checks, targets[j.source]})
	}
	return u
}

// Calculations counts the sources whose calculator's tables were judged
// against the instant.
func (u *UnsignedFreshness) Calculations() (n int) {
	for _, s := range u.Sources {
		for _, c := range s.Checks {
			if c == freshnessCalculation {
				n++
			}
		}
	}
	return n
}
