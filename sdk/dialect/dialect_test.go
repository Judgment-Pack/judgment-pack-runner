package dialect_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/sdk/dialect"
)

// spec is the Spec's staged format 4.0.0 vectors, copied byte for byte
// (vectors/dialect-format-4/README.md). Every expected value in them was
// written by hand in the Spec; nothing here is recorded from this package.
const spec = "../../vectors/dialect-format-4/spec"

func specFile(t testing.TB, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{spec}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var e *dialect.Error
	if !errors.As(err, &e) {
		t.Fatalf("want an *Error, got %v", err)
	}
	return e.Code
}

func read(t testing.TB, document string) *dialect.Dialect {
	t.Helper()
	d, err := dialect.Read([]byte(document))
	if err != nil {
		t.Fatalf("%v\n%s", err, document)
	}
	return d
}

func instant(t testing.TB, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// facts is a preparation's facts document, its numbers kept as written.
func facts(t *testing.T, p *dialect.Preparation) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(p.Facts()))
	decoder.UseNumber()
	var f map[string]any
	if err := decoder.Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTheCaseRuleVectorPreparesByteForByte(t *testing.T) {
	var manifest struct {
		Cases []struct {
			Name              string `json:"name"`
			DialectDigest     string `json:"dialectDigest"`
			PreparationDigest string `json:"preparationDigest"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(specFile(t, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	var dialectDigest, preparationDigest string
	for _, c := range manifest.Cases {
		if c.Name == "case-rule-no-sources" {
			dialectDigest, preparationDigest = c.DialectDigest, c.PreparationDigest
		}
	}
	if dialectDigest == "" || preparationDigest == "" {
		t.Fatal("the manifest names no case-rule-no-sources digests")
	}
	want := specFile(t, "case-rule-no-sources", "preparation.json")
	var given struct {
		VerifiedAt string `json:"verifiedAt"`
	}
	if err := json.Unmarshal(want, &given); err != nil {
		t.Fatal(err)
	}
	at := instant(t, given.VerifiedAt)

	d, err := dialect.Read(specFile(t, "case-rule-no-sources", "dialect.json"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Digest() != dialectDigest {
		t.Fatalf("dialect digest %s, the manifest's %s", d.Digest(), dialectDigest)
	}
	caseDocument := specFile(t, "case-rule-no-sources", "case.json")
	// The instant is truncated to the second and read in UTC.
	for _, when := range []time.Time{at, at.Add(999 * time.Millisecond), at.In(time.FixedZone("", -5*3600))} {
		p, err := d.Prepare(caseDocument, when)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(p.Bytes(), want) {
			t.Fatalf("at %v the preparation differs from the vector's:\n%s\n%s", when, p.Bytes(), want)
		}
		sum := sha256.Sum256(want)
		if p.Digest() != preparationDigest || p.Digest() != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("preparation digest %s, the manifest's %s", p.Digest(), preparationDigest)
		}
		if got := string(p.Facts()); got != `{"request":{"id":"R-316"},"signIn":{"distanceMm":5570229873,"plausible":true,"speedMmPerS":193410}}` {
			t.Fatalf("facts %s", got)
		}
		if got := string(p.Evidence()); got != `{}` {
			t.Fatalf("evidence %s", got)
		}
		if got := string(p.ParameterText()); got != `{"allowedRanges":["192.0.2.0/24","2001:db8::/32"],"maxSpeed":250000}` {
			t.Fatalf("parameter text %s", got)
		}
	}
}

func TestThePackageRefusesWhatItDoesNotPrepare(t *testing.T) {
	plain := `"id":"urn:judgmentpack:dialect:sdk-tests","version":"0.1.0","case":{"facts":[],"evidence":[]}`
	for _, c := range []struct {
		name, document, code string
	}{
		{"a dialect with sources", string(specFile(t, "source-rule", "dialect.json")), dialect.CodeSourcesUnread},
		{"format 3.0.0", `{"formatVersion":"3.0.0",` + plain + `}`, dialect.CodeVersionUnread},
		{"format 2.0.0", `{"formatVersion":"2.0.0",` + plain + `}`, dialect.CodeVersionUnread},
		{"version 3", `{"version":3,"id":"urn:judgmentpack:dialect:sdk-tests","case":{"facts":[],"evidence":[]}}`, dialect.CodeVersionUnread},
		{"the extended profile", `{"profile":"extended","formatVersion":"4.0.0",` + plain + `}`, dialect.CodeProfileUnsupported},
		{"an unknown profile", `{"profile":"other","formatVersion":"4.0.0",` + plain + `}`, dialect.CodeMalformed},
		{"an unknown format", `{"formatVersion":"5.0.0",` + plain + `}`, dialect.CodeMalformed},
		{"no format", `{` + plain + `}`, dialect.CodeMalformed},
	} {
		if _, err := dialect.Read([]byte(c.document)); err == nil || codeOf(t, err) != c.code {
			t.Errorf("%s: want %s, got %v", c.name, c.code, err)
		}
	}
	// The base profile is read.
	if _, err := dialect.Read([]byte(`{"profile":"base","formatVersion":"4.0.0",` + plain + `}`)); err != nil {
		t.Fatal(err)
	}
}

// valueDialect is a dialect of no source whose case rule writes the derived
// value v at /value when it has one, and nothing otherwise, its case
// parameters given as JSON text.
func valueDialect(t testing.TB, parameters, derived string) *dialect.Dialect {
	t.Helper()
	declared, given := `{"runAt":"timestamp"}`, ``
	if parameters != "" {
		var p map[string]struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(parameters), &p); err != nil {
			t.Fatal(err)
		}
		types := map[string]string{"runAt": "timestamp"}
		for name, d := range p {
			types[name] = d.Type
		}
		b, _ := json.Marshal(types)
		declared, given = string(b), `"parameters":`+parameters+`,`
	}
	return read(t, `{"formatVersion":"4.0.0","id":"urn:judgmentpack:dialect:sdk-tests","version":"0.1.0","case":{`+given+
		`"facts":[],"evidence":[],"rule":{"ruleVersion":"2.0.0","parameters":`+declared+`,"derived":`+derived+`,"clauses":[`+
		`{"when":{"op":"exists","derived":"v"},"claim":{"facts":[{"pointer":"/value","derived":"v"}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"some"},`+
		`{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"resolved"},"reason":"none"}]}}}`)
}

// derivedValue is the value the dialect's rule writes at /value for the
// case, or false when it writes none.
func derivedValue(t *testing.T, d *dialect.Dialect, caseDocument string, at time.Time) (int64, bool) {
	t.Helper()
	p, err := d.Prepare([]byte(caseDocument), at)
	if err != nil {
		t.Fatalf("%v\n%s", err, caseDocument)
	}
	v, ok := facts(t, p)["value"]
	if !ok {
		return 0, false
	}
	n, err := v.(json.Number).Int64()
	if err != nil {
		t.Fatal(err)
	}
	return n, true
}

// The duration's expected value is the time package's difference between
// the two instants, in seconds; an instant time.Parse refuses has none.
func TestDurationAgainstTheTimePackage(t *testing.T) {
	d := valueDialect(t, "", `[{"name":"v","builtin":"duration","from":{"field":"/a"},"to":{"field":"/b"}}]`)
	at := instant(t, "2026-11-12T18:00:00Z")
	for _, pair := range [][2]string{
		{"2026-11-12T18:00:00Z", "2026-11-12T18:00:00Z"},
		{"2026-11-12T18:00:00Z", "2026-11-12T10:00:00Z"},
		{"1969-12-31T23:59:59Z", "1970-01-01T00:00:00Z"},
		{"0001-01-01T00:00:00Z", "9999-12-31T23:59:59Z"},
		{"9999-12-31T23:59:59Z", "0001-01-01T00:00:00Z"},
		{"2024-02-28T23:59:59Z", "2024-03-01T00:00:00Z"},
		{"2100-02-28T00:00:00Z", "2100-03-01T00:00:00Z"},
		{"2000-02-29T00:00:00Z", "2000-03-01T00:00:00Z"},
		{"2100-02-29T00:00:00Z", "2100-03-01T00:00:00Z"},
		{"2026-04-31T00:00:00Z", "2026-05-01T00:00:00Z"},
		{"2026-11-12T24:00:00Z", "2026-11-13T00:00:00Z"},
	} {
		a, errA := time.Parse(time.RFC3339, pair[0])
		b, errB := time.Parse(time.RFC3339, pair[1])
		got, some := derivedValue(t, d, fmt.Sprintf(`{"a":%q,"b":%q}`, pair[0], pair[1]), at)
		switch {
		case errA != nil || errB != nil:
			if some {
				t.Errorf("%v: an instant time.Parse refuses gave a duration %d", pair, got)
			}
		case !some || got != b.Unix()-a.Unix():
			t.Errorf("%v: duration %d (%v), the time package's %d", pair, got, some, b.Unix()-a.Unix())
		}
	}
}

// haversineMM is the great-circle distance in millimetres between two points
// on a sphere of the Spec's radius, 6371008.8 m (spec/dialect.md §2.2.5),
// in float64 arithmetic: an independent reckoning of the fixed-point one.
func haversineMM(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180
	sinLat, sinLon := math.Sin((lat2-lat1)*rad/2), math.Sin((lon2-lon1)*rad/2)
	h := sinLat*sinLat + math.Cos(lat1*rad)*math.Cos(lat2*rad)*sinLon*sinLon
	return 2 * 6371008800 * math.Asin(math.Min(1, math.Sqrt(h)))
}

var distancePairs = [][4]string{
	{"51.5074", "-0.1278", "51.5074", "-0.1278"},
	{"0", "0", "90", "0"},
	{"0", "0", "0", "180"},
	{"90", "0", "-90", "0"},
	{"40.7128", "-74.0060", "51.5074", "-0.1278"},
	{"-33.8688", "151.2093", "35.6762", "139.6503"},
	{"12.5", "179.5", "-12.5", "-179.5"},
	{"0", "0", "0", "0.00001"},
	{"0", "0", "0.000000001", "0"},
}

func float(t *testing.T, s string) float64 {
	t.Helper()
	var f float64
	if _, err := fmt.Sscan(s, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// The distance is in whole millimetres rounded down: within a millimetre
// below the float64 reckoning, and not above it.
func TestDistanceAgainstAFloatHaversine(t *testing.T) {
	d := valueDialect(t, "", `[{"name":"v","builtin":"distance","from":{"latitude":{"field":"/a/lat"},"longitude":{"field":"/a/lon"}},"to":{"latitude":{"field":"/b/lat"},"longitude":{"field":"/b/lon"}}}]`)
	at := instant(t, "2026-11-12T18:00:00Z")
	for _, p := range distancePairs {
		want := haversineMM(float(t, p[0]), float(t, p[1]), float(t, p[2]), float(t, p[3]))
		got, some := derivedValue(t, d, fmt.Sprintf(`{"a":{"lat":%q,"lon":%q},"b":{"lat":%q,"lon":%q}}`, p[0], p[1], p[2], p[3]), at)
		if !some || float64(got) > want+0.01 || float64(got) <= want-1.01 {
			t.Errorf("%v: distance %d (%v), the float64 haversine's %.4f", p, got, some, want)
		}
	}
}

// The speed is the distance over the duration's magnitude, rounded down; a
// zero duration, or a distance or duration with none, gives none.
func TestSpeedAgainstIndependentReckoning(t *testing.T) {
	d := valueDialect(t, "", `[{"name":"t","builtin":"duration","from":{"field":"/a/at"},"to":{"field":"/b/at"}},`+
		`{"name":"d","builtin":"distance","from":{"latitude":{"field":"/a/lat"},"longitude":{"field":"/a/lon"}},"to":{"latitude":{"field":"/b/lat"},"longitude":{"field":"/b/lon"}}},`+
		`{"name":"v","builtin":"speed","distance":"d","duration":"t"}]`)
	at := instant(t, "2026-11-12T18:00:00Z")
	for _, c := range []struct {
		pair     [4]string
		from, to string
	}{
		{distancePairs[4], "2026-11-12T10:00:00Z", "2026-11-12T18:00:00Z"},
		{distancePairs[4], "2026-11-12T18:00:00Z", "2026-11-12T10:00:00Z"},
		{distancePairs[5], "2026-11-12T00:00:00Z", "2026-11-12T09:37:13Z"},
		{distancePairs[1], "2026-11-12T17:59:59Z", "2026-11-12T18:00:00Z"},
		{distancePairs[0], "2026-11-12T17:00:00Z", "2026-11-12T18:00:00Z"},
		{distancePairs[4], "2026-11-12T18:00:00Z", "2026-11-12T18:00:00Z"},
		{[4]string{"91", "0", "0", "0"}, "2026-11-12T17:00:00Z", "2026-11-12T18:00:00Z"},
		{distancePairs[4], "2026-11-12T17:00:00Z", "2026-02-30T18:00:00Z"},
	} {
		p := c.pair
		from, errFrom := time.Parse(time.RFC3339, c.from)
		to, errTo := time.Parse(time.RFC3339, c.to)
		seconds := math.Abs(float64(to.Unix() - from.Unix()))
		got, some := derivedValue(t, d, fmt.Sprintf(`{"a":{"lat":%q,"lon":%q,"at":%q},"b":{"lat":%q,"lon":%q,"at":%q}}`, p[0], p[1], c.from, p[2], p[3], c.to), at)
		if errFrom != nil || errTo != nil || seconds == 0 || float(t, p[0]) > 90 {
			if some {
				t.Errorf("%v %s %s: a speed %d where there is none", p, c.from, c.to, got)
			}
			continue
		}
		distance := haversineMM(float(t, p[0]), float(t, p[1]), float(t, p[2]), float(t, p[3]))
		low, high := math.Floor((distance-1.01)/seconds), math.Floor((distance+0.01)/seconds)
		if !some || float64(got) < low || float64(got) > high {
			t.Errorf("%v %s %s: speed %d (%v), the independent reckoning's [%v, %v]", p, c.from, c.to, got, some, low, high)
		}
	}
}

func TestASpeedNamesAnEarlierDistanceAndDuration(t *testing.T) {
	duration := `{"name":"t","builtin":"duration","from":{"field":"/a"},"to":{"field":"/b"}}`
	distance := `{"name":"d","builtin":"distance","from":{"latitude":{"field":"/a/lat"},"longitude":{"field":"/a/lon"}},"to":{"latitude":{"field":"/b/lat"},"longitude":{"field":"/b/lon"}}}`
	for _, derived := range []string{
		`[` + duration + `,` + distance + `,{"name":"v","builtin":"speed","distance":"t","duration":"t"}]`,
		`[` + duration + `,` + distance + `,{"name":"v","builtin":"speed","distance":"d","duration":"d"}]`,
		`[` + duration + `,{"name":"v","builtin":"speed","distance":"d","duration":"t"},` + distance + `]`,
	} {
		document := `{"formatVersion":"4.0.0","id":"urn:judgmentpack:dialect:sdk-tests","version":"0.1.0","case":{"facts":[],"evidence":[],"rule":{"ruleVersion":"2.0.0","parameters":{"runAt":"timestamp"},"derived":` + derived +
			`,"clauses":[{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"resolved"},"reason":"none"}]}}}`
		if _, err := dialect.Read([]byte(document)); err == nil || codeOf(t, err) != dialect.CodeMalformed {
			t.Errorf("want %s, got %v\n%s", dialect.CodeMalformed, err, derived)
		}
	}
}

// matched is whether the condition when holds of the case, read from a
// dialect whose rule writes /matched true when it holds and false otherwise.
func matched(t *testing.T, parameters, derived, when, caseDocument string, at time.Time) bool {
	t.Helper()
	var p map[string]struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(parameters), &p); err != nil {
		t.Fatal(err)
	}
	types := map[string]string{"runAt": "timestamp"}
	for name, d := range p {
		types[name] = d.Type
	}
	declared, _ := json.Marshal(types)
	if derived != "" {
		derived = `"derived":` + derived + `,`
	}
	d := read(t, `{"formatVersion":"4.0.0","id":"urn:judgmentpack:dialect:sdk-tests","version":"0.1.0","case":{"parameters":`+parameters+
		`,"facts":[],"evidence":[],"rule":{"ruleVersion":"2.0.0","parameters":`+string(declared)+`,`+derived+`"clauses":[`+
		`{"when":`+when+`,"claim":{"facts":[{"pointer":"/matched","value":true}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"true"},`+
		`{"when":{"op":"always"},"claim":{"facts":[{"pointer":"/matched","value":false}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"false"}]}}}`)
	prepared, err := d.Prepare([]byte(caseDocument), at)
	if err != nil {
		t.Fatalf("%v\n%s", err, caseDocument)
	}
	return facts(t, prepared)["matched"] == true
}

// aheadWithin holds of an instant at most maxAhead seconds after asOf, and
// not before it: the boundaries are reckoned with the time package.
func TestAheadWithinAtItsBoundaries(t *testing.T) {
	at := instant(t, "2026-11-12T18:00:00Z")
	const window = 3600
	parameters := `{"window":{"pointer":"/window","type":"integer"}}`
	when := `{"op":"aheadWithin","field":"/at","asOf":"runAt","maxAhead":"window"}`
	for _, c := range []struct {
		offset time.Duration
		holds  bool
	}{
		{0, true},
		{window * time.Second, true},
		{window*time.Second + time.Second, false},
		{-time.Second, false},
	} {
		caseDocument := fmt.Sprintf(`{"window":%d,"at":%q}`, window, at.Add(c.offset).Format(time.RFC3339))
		if got := matched(t, parameters, "", when, caseDocument, at); got != c.holds {
			t.Errorf("an instant %v after runAt: aheadWithin %v, want %v", c.offset, got, c.holds)
		}
	}
}

// compareParam compares a derived duration with a limit equal to the time
// package's difference: less-than-or-equal holds at equality, less-than not.
func TestCompareParamAtEquality(t *testing.T) {
	at := instant(t, "2026-11-12T18:00:00Z")
	from := instant(t, "2026-11-11T06:29:31Z")
	limit := at.Unix() - from.Unix()
	parameters := `{"limit":{"pointer":"/limit","type":"integer"}}`
	derived := `[{"name":"d","builtin":"duration","from":{"field":"/from"},"to":{"param":"runAt"}}]`
	caseDocument := fmt.Sprintf(`{"limit":%d,"from":%q}`, limit, from.Format(time.RFC3339))
	for relation, holds := range map[string]bool{"less-than-or-equal": true, "greater-than-or-equal": true, "less-than": false, "greater-than": false} {
		when := `{"op":"compareParam","derived":"d","relation":"` + relation + `","param":"limit"}`
		if got := matched(t, parameters, derived, when, caseDocument, at); got != holds {
			t.Errorf("%s at equality: %v, want %v", relation, got, holds)
		}
	}
	// A decimal limit compares by value, whatever its scale.
	decimal := `{"limit":{"pointer":"/limit","type":"decimal"}}`
	caseDocument = fmt.Sprintf(`{"limit":"%d.000","from":%q}`, limit, from.Format(time.RFC3339))
	if !matched(t, decimal, derived, `{"op":"compareParam","derived":"d","relation":"greater-than-or-equal","param":"limit"}`, caseDocument, at) {
		t.Error("a decimal limit of the same value does not compare equal")
	}
}

// signInCases are variants of the end-to-end vector's case, among them some
// the dialect does not prepare.
func signInCases(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		switch i % 7 {
		case 5:
			out[i] = []byte(`{"request":{"id":"R-` + fmt.Sprint(i) + `"},"settings":{"allowedRanges":["192.0.2.0/24"]}}`)
		case 6:
			out[i] = []byte(`{"request":`)
		default:
			out[i] = []byte(fmt.Sprintf(`{"request":{"id":"R-%d"},"settings":{"allowedRanges":["192.0.2.0/24","2001:db8::/32"],"maxSpeed":%d},`+
				`"previous":{"at":"2026-11-12T%02d:00:00Z","latitude":"40.7128","longitude":"-74.0060"},`+
				`"current":{"at":"2026-11-12T18:00:00Z","latitude":"51.5074","longitude":"-0.1278","address":"2001:db8::%x"}}`, i, 100000+i*1000, i%18, i))
		}
	}
	return out
}

// A batch's results are those of Prepare, case by case, in the order of the
// cases, while several batches share one Dialect.
func TestABatchIsPrepareCaseByCase(t *testing.T) {
	d, err := dialect.Read(specFile(t, "case-rule-no-sources", "dialect.json"))
	if err != nil {
		t.Fatal(err)
	}
	at := instant(t, "2026-11-12T18:05:00Z")
	cases := signInCases(70)
	type answer struct{ bytes, parameters, code string }
	answerOf := func(p *dialect.Preparation, err error) answer {
		if err != nil {
			return answer{code: codeOf(t, err)}
		}
		return answer{bytes: string(p.Bytes()), parameters: p.ParameterDigest()}
	}
	want := make([]answer, len(cases))
	for i, c := range cases {
		want[i] = answerOf(d.Prepare(c, at))
		if again := answerOf(d.Prepare(c, at)); again != want[i] {
			t.Fatalf("case %d prepares differently a second time", i)
		}
	}
	if want[5].code != dialect.CodeParameter || want[6].code != dialect.CodeMalformed || want[0].bytes == "" {
		t.Fatalf("the cases are not the mix this test needs: %+v %+v", want[5], want[6])
	}
	var wg sync.WaitGroup
	results := make([][]dialect.Result, 4)
	for g := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[g] = d.PrepareBatch(cases, at)
		}()
	}
	wg.Wait()
	for g, batch := range results {
		if len(batch) != len(cases) {
			t.Fatalf("batch %d has %d results for %d cases", g, len(batch), len(cases))
		}
		for i, r := range batch {
			if got := answerOf(r.Preparation, r.Err); got != want[i] {
				t.Fatalf("batch %d, case %d: %+v, Prepare's %+v", g, i, got, want[i])
			}
		}
	}
	if got := d.PrepareBatch(nil, at); len(got) != 0 {
		t.Fatalf("an empty batch gave %d results", len(got))
	}
}

const digestDialect = `{"formatVersion":"4.0.0","id":"urn:judgmentpack:dialect:sdk-tests","version":"0.1.0","case":{` +
	`"parameters":{"amount":{"pointer":"/amount","type":"string"},"member":{"pointer":"/member","type":"string"}},` +
	`"facts":[{"target":"/note","source":"/note"}],"evidence":[],` +
	`"rule":{"ruleVersion":"2.0.0","parameters":{"amount":"string","member":"string","runAt":"timestamp"},"clauses":[` +
	`{"when":{"op":"always"},"claim":{"facts":[{"pointer":"/seen","value":true}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"seen"}]}}}`

// runtimeKey is the key of the Runtime's keyed decision record's test
// vector, whose digest of {"amount":"1250","member":"M-0042"} it names.
const (
	runtimeKey    = "5245434f474e495341424c452d544553542d4b45592d3766336139632d646f2d6e6f742d757365"
	runtimeDigest = "e7f602ba0b2a3002a6d8c6056e00c0a98f3b198467bb3dd5fc231563e88e1e23"
)

func TestTheParameterDigest(t *testing.T) {
	d := read(t, digestDialect)
	at := instant(t, "2026-11-12T18:00:00Z")
	prepare := func(caseDocument string, at time.Time) *dialect.Preparation {
		t.Helper()
		p, err := d.Prepare([]byte(caseDocument), at)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := prepare(`{"amount":"1250","member":"M-0042","note":"first"}`, at)
	text := `{"amount":"1250","member":"M-0042"}`
	if string(base.ParameterText()) != text {
		t.Fatalf("parameter text %s", base.ParameterText())
	}
	sum := sha256.Sum256([]byte(text))
	if base.ParameterDigest() != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("parameter digest %s", base.ParameterDigest())
	}

	// Not how the case was written, nor what else it holds, nor runAt.
	for _, same := range []*dialect.Preparation{
		prepare("{ \"member\" : \"M-0042\",\n\t\"note\": \"second\", \"amount\": \"1\\u0032\\u00350\" }", at),
		prepare(`{"amount":"1250","member":"M-0042","note":"first"}`, at.Add(48*time.Hour)),
	} {
		if same.ParameterDigest() != base.ParameterDigest() {
			t.Errorf("the parameter digest changed: %s", same.ParameterText())
		}
		if same.Digest() == base.Digest() {
			t.Error("the preparations should differ, which this check relies on")
		}
	}
	// Each parameter's value, and which parameter holds it.
	for _, other := range []string{
		`{"amount":"1251","member":"M-0042","note":"first"}`,
		`{"amount":"1250","member":"M-0043","note":"first"}`,
		`{"amount":"M-0042","member":"1250","note":"first"}`,
	} {
		if prepare(other, at).ParameterDigest() == base.ParameterDigest() {
			t.Errorf("the parameter digest did not change: %s", other)
		}
	}

	key, _ := hex.DecodeString(runtimeKey)
	keyed, err := base.KeyedParameterDigest("runtime-vector", key)
	if err != nil {
		t.Fatal(err)
	}
	if keyed.Algorithm != "hmac-sha256" || keyed.KeyID != "runtime-vector" || hex.EncodeToString(keyed.Digest) != runtimeDigest {
		t.Fatalf("keyed digest %s %s %x, the Runtime's vector %s", keyed.Algorithm, keyed.KeyID, keyed.Digest, runtimeDigest)
	}
	otherKey, _ := base.KeyedParameterDigest("runtime-vector", append([]byte{}, key[1:]...))
	if bytes.Equal(otherKey.Digest, keyed.Digest) {
		t.Error("another key gave the same keyed digest")
	}
	for _, id := range []string{strings.Repeat("k", 128), "a.b_c-d:e/F9"} {
		if _, err := base.KeyedParameterDigest(id, key); err != nil {
			t.Errorf("key identifier %q: %v", id, err)
		}
	}
	for _, c := range []struct {
		id  string
		key []byte
	}{
		{"", key}, {strings.Repeat("k", 129), key}, {"has space", key}, {"é", key}, {"k+1", key}, {"runtime-vector", nil}, {"runtime-vector", []byte{}},
	} {
		if got, err := base.KeyedParameterDigest(c.id, c.key); !errors.Is(err, dialect.ErrKeyedDigest) || got.Digest != nil {
			t.Errorf("key identifier %q, key of %d bytes: %v", c.id, len(c.key), err)
		}
	}
}

// A list parameter is its elements in their order, and a decimal its digits
// as written: the digest binds the values the case gave.
func TestTheParameterDigestKeepsListOrderAndDecimalSpelling(t *testing.T) {
	d := read(t, `{"formatVersion":"4.0.0","id":"urn:judgmentpack:dialect:sdk-tests","version":"0.1.0","case":{`+
		`"parameters":{"ranges":{"pointer":"/ranges","type":"address-range-list"},"rate":{"pointer":"/rate","type":"decimal"}},"facts":[],"evidence":[]}}`)
	at := instant(t, "2026-11-12T18:00:00Z")
	digest := func(caseDocument string) string {
		t.Helper()
		p, err := d.Prepare([]byte(caseDocument), at)
		if err != nil {
			t.Fatal(err)
		}
		return p.ParameterDigest()
	}
	base := digest(`{"ranges":["192.0.2.0/24","2001:db8::/32"],"rate":"0.75"}`)
	for _, other := range []string{
		`{"ranges":["2001:db8::/32","192.0.2.0/24"],"rate":"0.75"}`,
		`{"ranges":["192.0.2.0/24","2001:db8::/32"],"rate":"0.750"}`,
	} {
		if digest(other) == base {
			t.Errorf("the parameter digest did not change: %s", other)
		}
	}
}

// goCommand is the go command of the toolchain that built this test, by its
// path: no program is looked up by name.
func goCommand(t *testing.T) string {
	t.Helper()
	root := runtime.GOROOT()
	if root == "" {
		t.Skip("this test binary does not know its toolchain's root")
	}
	path := filepath.Join(root, "bin", "go")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	if _, err := os.Stat(path); err != nil {
		t.Skip("the go command is not at the toolchain's root: ", err)
	}
	return path
}

// The package's dependencies, as go list reports them, include no network
// package and none that starts a process.
func TestThePackageHasNoNetworkDependency(t *testing.T) {
	cmd := exec.Command(goCommand(t), "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly")
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if e, ok := err.(*exec.ExitError); ok {
			stderr = e.Stderr
		}
		t.Fatalf("go list: %v\n%s", err, stderr)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 10 || deps[len(deps)-1] != "github.com/Judgment-Pack/judgment-pack-runner/sdk/dialect" {
		t.Fatalf("go list -deps gave an unexpected list: %q", deps)
	}
	for _, p := range deps {
		if p == "net" || strings.HasPrefix(p, "net/") || p == "crypto/tls" || p == "crypto/x509" || p == "os/exec" || p == "plugin" ||
			strings.HasPrefix(p, "github.com/Judgment-Pack/judgment-pack-runner/internal/runner") {
			t.Errorf("the package depends on %s", p)
		}
	}
}

// BenchmarkPrepareBatch measures the cost of preparing one candidate through
// the end-to-end vector's dialect (a duration, a distance in fixed-point
// arithmetic, a speed and an address-range match), in batches of 1000 on
// every available processor.
func BenchmarkPrepareBatch(b *testing.B) {
	d, err := dialect.Read(specFile(b, "case-rule-no-sources", "dialect.json"))
	if err != nil {
		b.Fatal(err)
	}
	at := instant(b, "2026-11-12T18:05:00Z")
	var cases [][]byte
	for _, c := range signInCases(1400) {
		if _, err := d.Prepare(c, at); err == nil {
			cases = append(cases, c)
		}
	}
	cases = cases[:1000]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, r := range d.PrepareBatch(cases, at) {
			if r.Err != nil {
				b.Fatal(r.Err)
			}
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(cases)), "ns/candidate")
}

// BenchmarkPrepare measures one candidate on one goroutine.
func BenchmarkPrepare(b *testing.B) {
	d, err := dialect.Read(specFile(b, "case-rule-no-sources", "dialect.json"))
	if err != nil {
		b.Fatal(err)
	}
	at := instant(b, "2026-11-12T18:05:00Z")
	c := specFile(b, "case-rule-no-sources", "case.json")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Prepare(c, at); err != nil {
			b.Fatal(err)
		}
	}
}
