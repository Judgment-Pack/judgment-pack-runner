package retainedcolumns

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSelectedLeafPublishedCorpus(t *testing.T) {
	raw, e := os.ReadFile("testdata/v0.3.0/cases.json")
	if e != nil {
		t.Fatal(e)
	}
	if digest(raw) != "sha256:17b16ff7a9b74fe4cc81eb6f6129a6aabdd9834621d9e756aa2d4c1a3f245863" {
		t.Fatal("published selected corpus changed")
	}
	var c corpus
	if e = json.Unmarshal(raw, &c); e != nil {
		t.Fatal(e)
	}
	if len(c.Cases) != 72 || len(c.Boundary) != 7 || len(c.Shortest) != 7 {
		t.Fatal("selected conversion inventory changed")
	}
	t.Run("conversions", func(t *testing.T) {
		for _, row := range c.Cases {
			t.Run(row.ID, func(t *testing.T) { assertCase(t, row) })
		}
	})
	t.Run("type-bounds", func(t *testing.T) { assertCorpusTypeBounds(t, c) })
	t.Run("shortest", func(t *testing.T) { assertCorpusShortest(t, c) })
	t.Run("references", func(t *testing.T) { assertCorpusReferences(t, raw) })
	var bounds struct {
		Rows []struct {
			ID, Origin, Unit, Profile, Suffix           string
			Repeat, ListItems, ItemBytes, ExpectedBytes int
			Expected                                    struct {
				Refusal         string
				ValueJSON       *string
				ClaimAdmissible bool
				Issues          []string
			}
		} `json:"rawValueBounds"`
	}
	if e = json.Unmarshal(raw, &bounds); e != nil {
		t.Fatal(e)
	}
	if len(bounds.Rows) != 11 {
		t.Fatal("selected origin bounds changed")
	}
	for _, row := range bounds.Rows {
		t.Run(row.ID, func(t *testing.T) {
			value := strings.Repeat(row.Unit, row.Repeat) + row.Suffix
			if row.ListItems > 0 {
				items := make([]string, row.ListItems)
				for i := range items {
					items[i] = strings.Repeat("x", row.ItemBytes)
				}
				value = string(encodeTest(t, items))
			}
			if row.ExpectedBytes > 0 && len(value) != row.ExpectedBytes {
				t.Fatal("handwritten raw-value recipe size")
			}
			var research any
			if row.Origin == "generated" {
				research = map[string]any{"recordRecipeRevision": "recipe:1", "declaration": nil, "fields": map[string]any{}}
			}
			frame := encodeTest(t, map[string]any{"contractVersion": "1", "origin": row.Origin, "values": map[string]any{"Selected": "ok", "Unselected": value}, "research": research})
			profile := Profile(row.Profile)
			if profile == "" {
				profile = Base
			}
			f, e := ParseFrame(frame, profile)
			if row.Expected.Refusal != "" {
				assertRefusal(t, e, row.Expected.Refusal)
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			p, e := ParsePlan([]byte(`{"contractVersion":"1","column":"Selected","kind":"text"}`))
			if e != nil {
				t.Fatal(e)
			}
			got, e := Preview(p, f)
			if e != nil || got.ValueJSON == nil || row.Expected.ValueJSON == nil || *got.ValueJSON != *row.Expected.ValueJSON || got.ClaimAdmissible != row.Expected.ClaimAdmissible || len(got.Issues) != len(row.Expected.Issues) {
				t.Fatalf("selected raw recipe differs: %+v %v", got, e)
			}
		})
	}
}
