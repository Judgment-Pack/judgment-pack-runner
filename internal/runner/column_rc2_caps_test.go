package runner

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// Declaration/minimum/selector cases exercise the real private admission owners,
// not producer effects or measurement of invented artifact bytes. Extended rows
// belong to separately supporting owners; Runner advertises Base only.
func TestRC2PublishedBaseCapabilityCorpus(t *testing.T) {
	var data struct {
		Caps, Interface []struct {
			ID       string
			Request  map[string]json.RawMessage
			Expected struct {
				Refusal            string
				Admitted, Selected bool
			}
		}
	}
	// Published JSON group names differ from the intentionally small Go adapter.
	var groups map[string]json.RawMessage
	if e := json.Unmarshal(rc2Read(t, "cases.json"), &groups); e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal(groups["capCases"], &data.Caps); e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal(groups["interfaceCases"], &data.Interface); e != nil {
		t.Fatal(e)
	}
	saved := installedConversionSpec
	fixture := rc2SyntheticInstallation()
	fixture.Version = "draft-rfc0029"
	installedConversionSpec = fixture
	t.Cleanup(func() { installedConversionSpec = saved })
	checked, extended := 0, 0
	for _, row := range append(data.Caps, data.Interface...) {
		if a := row.Request["admission"]; a != nil {
			if _, e := exactMembers(a); e != nil {
				checked++
				t.Run(row.ID, func(t *testing.T) { rc2ExpectedRefusal(t, row.Expected.Refusal, conversionFailure(codeMalformed)) })
				continue
			}
			var request struct {
				Capabilities json.RawMessage
				Required     json.RawMessage
				Sizes        map[string]int
			}
			if e := json.Unmarshal(a, &request); e != nil {
				t.Fatal(e)
			}
			var identity struct {
				Profile               string
				Role, ProducerGrammar string
				Limits                map[string]int
			}
			if e := json.Unmarshal(request.Required, &identity); e != nil {
				t.Fatal(e)
			}
			if identity.Profile == "extended" {
				extended++
				continue
			}
			checked++
			t.Run(row.ID, func(t *testing.T) {
				var capRoot map[string]json.RawMessage
				var cs []ConversionCapability
				e := strictJSON(request.Capabilities, &capRoot)
				if e == nil {
					if raw, exists := capRoot["conversionContracts"]; !exists {
						e = conversionFailure("capability-unsupported")
					} else {
						e = strictJSON(raw, &cs)
					}
				}
				var required ConversionCapability
				if e == nil {
					e = strictJSON(request.Required, &required)
				}
				if e == nil {
					var limits map[string]int
					limits, e = conversionCapabilityLimits(cs, identity.Role, identity.ProducerGrammar != "")
					if e == nil {
						for k, n := range limits {
							v, ok := request.Sizes[k]
							if !ok || v < 0 {
								e = conversionFailure(codeMalformed)
								break
							}
							if n > identity.Limits[k] {
								limits[k] = identity.Limits[k]
							}
							if err := conversionArtifactBound(limits, k, v); err != nil {
								e = err
								break
							}
						}
					}
				}
				rc2ExpectedRefusal(t, row.Expected.Refusal, e)
			})
		} else if a := row.Request["selection"]; a != nil {
			var request struct {
				Selector json.RawMessage
				Role     string
			}
			if e := json.Unmarshal(a, &request); e != nil {
				t.Fatal(e)
			}
			var header struct{ Profile any }
			json.Unmarshal(request.Selector, &header)
			if header.Profile == "extended" {
				extended++
				continue
			}
			checked++
			t.Run(row.ID, func(t *testing.T) {
				_, e := ParseConversionSelector(base64.RawURLEncoding.EncodeToString(request.Selector), request.Role)
				rc2ExpectedRefusal(t, row.Expected.Refusal, e)
			})
		}
	}
	if checked == 0 || extended == 0 {
		t.Fatal("published capability applicability inventory empty")
	}
	t.Logf("actual Base declaration/minimum/selection rows=%d; Extended role rows outside Runner support=%d", checked, extended)
}
func rc2ExpectedRefusal(t *testing.T, want string, e error) {
	t.Helper()
	if want == "" {
		if e != nil {
			t.Fatal("published positive refused", e)
		}
		return
	}
	if e == nil {
		t.Fatal("published refusal admitted", want)
	}
	if _, got := verificationOutcome(e); got != want {
		t.Fatalf("published refusal differs: got%s want%s: %v", got, want, e)
	}
}
