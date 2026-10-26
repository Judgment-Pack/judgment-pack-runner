package retainedcolumns

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type corpusCase struct {
	ID               string          `json:"id"`
	Kind             string          `json:"kind"`
	Column           string          `json:"column"`
	InputJSON        *string         `json:"inputJson"`
	InputState       string          `json:"inputState"`
	Rule             map[string]any  `json:"rule"`
	Origin           string          `json:"origin"`
	Type             string          `json:"type"`
	Status           string          `json:"status"`
	Declaration      json.RawMessage `json:"declaration"`
	RevisionMismatch bool            `json:"revisionMismatch"`
	Undeclared       bool            `json:"undeclared"`
	FieldEntryAbsent bool            `json:"fieldEntryAbsent"`
	RecordCount      *int            `json:"recordCount"`
	Preview          *struct {
		ValueJSON *string  `json:"valueJson"`
		Omitted   string   `json:"omitted"`
		Issues    []string `json:"issues"`
	} `json:"preview"`
	Preparation     string `json:"preparation"`
	ClaimAdmissible *bool  `json:"claimAdmissible"`
}
type corpus struct {
	Cases    []corpusCase `json:"cases"`
	Boundary []struct {
		ID, Kind, Repeat, Expected, ExpectedIssue, ResearchType string
		Count                                                   int
	} `json:"boundaryCases"`
	Shortest  []struct{ ID, Binary64Hex, ExpectedRational string } `json:"shortestCases"`
	Interface []struct {
		ID       string
		Request  map[string]json.RawMessage
		Expected json.RawMessage
	} `json:"interfaceCases"`
}

func encodeTest(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func loadCorpus(t *testing.T) corpus {
	t.Helper()
	b, e := os.ReadFile("testdata/cases.json")
	if e != nil {
		t.Fatal(e)
	}
	if digest(b) != "sha256:c443229939deafc1c3f5020a9b3e9bdcd69707bc3a42c9571a6a59f81dfaa69d" {
		t.Fatal("pinned handwritten corpus changed")
	}
	var c corpus
	if e = json.Unmarshal(b, &c); e != nil {
		t.Fatal(e)
	}
	return c
}
func fixture(t *testing.T, c corpusCase) (Plan, Frame) {
	t.Helper()
	column := c.Column
	if column == "" {
		column = "Column"
	}
	rule := map[string]any{"contractVersion": "1", "column": column}
	if c.Kind != "evidence-availability" {
		rule["kind"] = c.Kind
	}
	for k, v := range c.Rule {
		rule[k] = v
	}
	p, e := ParsePlan(encodeTest(t, rule))
	if e != nil {
		t.Fatalf("fixture plan: %v", e)
	}
	origin := c.Origin
	if origin == "" {
		origin = "asserted"
		if c.Kind == "research-value" {
			origin = "generated"
		}
	}
	values := map[string]any{}
	if c.Kind != "research-value" && c.InputJSON != nil {
		var value any
		if e = json.Unmarshal([]byte(*c.InputJSON), &value); e != nil {
			t.Fatal(e)
		}
		values[column] = value
	}
	frame := map[string]any{"contractVersion": "1", "origin": origin, "values": values, "research": nil}
	if origin == "generated" {
		kind := c.Type
		if kind == "" && c.Preview == nil {
			kind = "text"
		}
		entries := []any{}
		if !c.Undeclared {
			field := string(encodeTest(t, map[string]any{"name": column, "type": kind}))
			entries = append(entries, map[string]any{"name": column, "fieldJson": field})
		}
		revision := "recipe:1"
		if c.RevisionMismatch {
			revision = "recipe:2"
		}
		var declaration any = map[string]any{"recipeRevision": revision, "fields": entries}
		if string(c.Declaration) == "null" {
			declaration = nil
		}
		status := c.Status
		if status == "" {
			status = "supported"
		}
		field := map[string]any{"status": status}
		if c.InputJSON != nil && c.InputState != "missing" {
			field["valueJson"] = *c.InputJSON
		}
		fields := map[string]any{}
		if !c.FieldEntryAbsent {
			fields[column] = field
		}
		frame["research"] = map[string]any{"recordRecipeRevision": "recipe:1", "declaration": declaration, "fields": fields}
	}
	f, e := ParseFrame(encodeTest(t, frame), Base)
	if e != nil {
		t.Fatalf("fixture frame: %v", e)
	}
	return p, f
}
func assertCase(t *testing.T, c corpusCase) {
	t.Helper()
	if c.RecordCount != nil && *c.RecordCount == 0 {
		// Validate only the row-free plan; invent neither a frame nor a declaration.
		if _, err := ParsePlan([]byte(`{"contractVersion":"1","column":"Planned","kind":"research-value"}`)); err != nil {
			t.Fatal(err)
		}
		return
	}
	p, f := fixture(t, c)
	got, e := Preview(p, f)
	if e != nil {
		t.Fatal(e)
	}
	wantValue, wantOmit, wantIssues := (*string)(nil), "", []string{"research-context"}
	if c.Preview != nil {
		wantValue, wantOmit, wantIssues = c.Preview.ValueJSON, c.Preview.Omitted, c.Preview.Issues
	}
	// The shared corpus spells some ordinary strings with JSON Unicode escapes.
	// Normalize string quoting with the independent standard JSON encoder only;
	// research/number bytes must compare verbatim.
	if wantValue != nil && (c.Kind == "text" || c.Kind == "decimal" || c.Kind == "evidence-availability") {
		var s string
		if e = json.Unmarshal([]byte(*wantValue), &s); e != nil {
			t.Fatal(e)
		}
		encoded := string(encodeTest(t, s))
		wantValue = &encoded
	}
	if !reflect.DeepEqual(got.ValueJSON, wantValue) || got.Omitted != wantOmit {
		t.Fatalf("exact preview mismatch: got=%+v wantValue=%v wantOmit=%q", got, wantValue, wantOmit)
	}
	codes := []string{}
	for _, i := range got.Issues {
		codes = append(codes, i.Code)
		if i.Message == "" {
			t.Fatal("missing pinned message")
		}
	}
	if !reflect.DeepEqual(codes, wantIssues) {
		t.Fatalf("issue mismatch: got=%v want=%v", codes, wantIssues)
	}
	admissible := wantValue != nil && c.Preparation != "read-failed-no-evaluation"
	if c.ClaimAdmissible != nil {
		admissible = *c.ClaimAdmissible
	}
	if got.ClaimAdmissible != admissible {
		t.Fatalf("claim boundary got=%v want=%v", got.ClaimAdmissible, admissible)
	}
	lineage, err := Derive(p, f)
	refuseExpected := len(wantIssues) > 0 || wantValue != nil && !admissible
	if refuseExpected {
		if lineage != nil {
			t.Fatal("failed preview produced success lineage")
		}
		assertRefusal(t, err, "read-failed")
	} else if err != nil || lineage == nil {
		t.Fatalf("missing recomputed lineage: %v", err)
	}
}
func TestPinnedCompatibilityCorpus(t *testing.T) {
	c := loadCorpus(t)
	if len(c.Cases) != 72 {
		t.Fatal("unexpected corpus size")
	}
	for _, row := range c.Cases {
		t.Run(row.ID, func(t *testing.T) { assertCase(t, row) })
	}
}
func TestPinnedTypeBounds(t *testing.T) {
	c := loadCorpus(t)
	for _, b := range c.Boundary {
		t.Run(b.ID, func(t *testing.T) {
			value := any(strings.Repeat(b.Repeat, b.Count))
			if b.ResearchType == "text-list" {
				a := make([]string, b.Count)
				value = a
			}
			raw := string(encodeTest(t, value))
			kind := b.Kind
			if b.ResearchType != "" {
				kind = "research-value"
			}
			row := corpusCase{Kind: kind, Type: b.ResearchType, Status: "supported", InputJSON: &raw}
			p, f := fixture(t, row)
			got, e := Preview(p, f)
			if e != nil {
				t.Fatal(e)
			}
			if b.ExpectedIssue != "" {
				if len(got.Issues) != 1 || got.Issues[0].Code != b.ExpectedIssue {
					t.Fatalf("expected bound issue %s, got %+v", b.ExpectedIssue, got)
				}
			} else if got.ValueJSON == nil || *got.ValueJSON != raw || len(got.Issues) != 0 {
				t.Fatalf("expected exact at-bound value, got %+v", got)
			}
		})
	}
}
func TestPinnedShortestRationals(t *testing.T) {
	for _, c := range loadCorpus(t).Shortest {
		t.Run(c.ID, func(t *testing.T) {
			bits, e := strconv.ParseUint(c.Binary64Hex, 16, 64)
			if e != nil {
				t.Fatal(e)
			}
			got := shortestMagnitude(math.Float64frombits(bits))
			want := rat(c.ExpectedRational)
			if got.Cmp(want) != 0 {
				t.Fatalf("shortest rational got=%s want=%s", got.RatString(), want.RatString())
			}
		})
	}
}
func TestPinnedLineageFragments(t *testing.T) {
	count := 0
	for _, c := range loadCorpus(t).Interface {
		raw, ok := c.Request["lineage"]
		if !ok || len(raw) == 0 || raw[0] != '{' {
			continue
		}
		count++
		t.Run(c.ID, func(t *testing.T) {
			var r struct {
				Rule    json.RawMessage
				Frame   json.RawMessage
				Profile Profile
			}
			if e := json.Unmarshal(raw, &r); e != nil {
				t.Fatal(e)
			}
			profile := r.Profile
			if profile == "" {
				profile = Base
			}
			p, e := ParsePlan(r.Rule)
			if e != nil {
				t.Fatal(e)
			}
			f, e := ParseFrame(r.Frame, profile)
			if e != nil {
				t.Fatal(e)
			}
			got, e := Derive(p, f)
			var refusal struct{ Refusal string }
			if err := json.Unmarshal(c.Expected, &refusal); err != nil {
				t.Fatal(err)
			}
			if refusal.Refusal != "" {
				if got != nil {
					t.Fatal("refusal emitted success lineage")
				}
				assertRefusal(t, e, refusal.Refusal)
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			// Historical corpus input is an inline diagnostic, not the new wire form.
			var historic struct {
				ContractVersion, Column string
				Input                   struct {
					State string
					JSON  *string
				}
				Result           ResultState
				ResearchIdentity *ResearchIdentity
			}
			if e = json.Unmarshal(c.Expected, &historic); e != nil {
				t.Fatal(e)
			}
			resolved, e := ResolveInput(p, f, got)
			if e != nil || got.ContractVersion != historic.ContractVersion || got.Column != historic.Column || got.Input.State != historic.Input.State || !reflect.DeepEqual(resolved, historic.Input.JSON) || !reflect.DeepEqual(got.Result, historic.Result) || !reflect.DeepEqual(got.ResearchIdentity, historic.ResearchIdentity) {
				t.Fatalf("historical diagnostic semantics differ: %v got=%s want=%s", e, encodeTest(t, got), c.Expected)
			}
			if got.Input.State == "present" && (got.Input.Ref == nil || got.Input.Ref.FrameSHA256 != digest(r.Frame)) {
				t.Fatal("reference does not bind received frame")
			}

		})
	}
	if count != 9 {
		t.Fatalf("expected 9 lineage fragments, read %d", count)
	}
}
