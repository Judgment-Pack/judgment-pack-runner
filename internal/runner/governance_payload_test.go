package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const executionPayloadVectors = "testdata/governance-payload"
const executionPayloadManifestDigest = "sha256:84d7299260790b56146c094988bf429f0b7022562669f920fae9cabf92839b00"

type executionPayloadVector struct {
	Name, Facts, Evidence, Preparation, Source, PublicKey, Descriptor, Digest string
}
type executionPayloadManifest struct {
	Profile, CanonicalProfile, Release, ReleaseDigest string
	Vectors                                           []executionPayloadVector
}

func executionVectorBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(executionPayloadVectors, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func executionVectorManifest(t *testing.T) executionPayloadManifest {
	t.Helper()
	raw := executionVectorBytes(t, "manifest.json")
	if digest(raw) != executionPayloadManifestDigest {
		t.Fatal("fixed execution-payload manifest changed")
	}
	var manifest executionPayloadManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func executionVectorInput(t *testing.T, v executionPayloadVector) (Release, Input, signingInputs) {
	t.Helper()
	var release Release
	if err := strictJSON(executionVectorBytes(t, "release.json"), &release); err != nil {
		t.Fatal(err)
	}
	input := Input{Facts: executionVectorBytes(t, v.Facts)}
	if v.Evidence != "" {
		input.Evidence = executionVectorBytes(t, v.Evidence)
	}
	if v.Preparation != "" {
		if err := strictJSON(executionVectorBytes(t, v.Preparation), &input.Preparation); err != nil {
			t.Fatal(err)
		}
	}
	if v.Source != "" {
		if err := strictJSON(executionVectorBytes(t, v.Source), &input.Source); err != nil {
			t.Fatal(err)
		}
	}
	signing := signingInputs{}
	if v.PublicKey != "" {
		signing = signingInputs{path: "/synthetic/key", publicKey: v.PublicKey}
	}
	return release, input, signing
}

func TestExecutionPayloadGoldenVectors(t *testing.T) {
	manifest := executionVectorManifest(t)
	if manifest.Profile != executionPayloadProfile || manifest.CanonicalProfile != runnerCanonicalProfile {
		t.Fatal("fixture names another encoding profile")
	}
	seen := map[string]string{}
	for _, v := range manifest.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			release, input, signing := executionVectorInput(t, v)
			facts, evidence := bytes.Clone(input.Facts), bytes.Clone(input.Evidence)
			before := encode(input)
			payload, err := freezeExecutionPayload(release, input, signing)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(payload.Bytes(), executionVectorBytes(t, v.Descriptor)) || payload.Digest() != v.Digest {
				t.Fatalf("fixed vector differs: %s\n%s", payload.Digest(), payload.Bytes())
			}
			if releaseDigest(release) != manifest.ReleaseDigest {
				t.Fatal("payload must retain the existing legacy release digest")
			}
			if !bytes.Equal(facts, input.Facts) || !bytes.Equal(evidence, input.Evidence) || !bytes.Equal(before, encode(input)) {
				t.Fatal("freezing changed the supplied input")
			}
			// The canonical metadata files are fixed preimages too, with
			// integer/decimal/exponent lexemes deliberately beyond Policy's domain.
			for _, component := range []struct {
				name  string
				value any
			}{{v.Preparation, input.Preparation}, {v.Source, input.Source}} {
				if component.name != "" {
					raw, err := executionPayloadCanonical(component.value, MaxBody)
					if err != nil || !bytes.Equal(raw, executionVectorBytes(t, component.name)) {
						t.Fatalf("fixed canonical preimage %s differs: %v", component.name, err)
					}
				}
			}
			if earlier := seen[v.Digest]; earlier != "" {
				t.Fatalf("distinct vectors %s and %s have one digest", earlier, v.Name)
			}
			seen[v.Digest] = v.Name
		})
	}
}

func TestExecutionPayloadPreservesExactRawDomain(t *testing.T) {
	v := executionVectorManifest(t).Vectors[0]
	r, input, signing := executionVectorInput(t, v)
	// Empty and nil evidence both omit the Runtime's file and flag.
	input.Evidence = json.RawMessage{}
	payload, err := freezeExecutionPayload(r, input, signing)
	if err != nil || payload.Digest() != v.Digest {
		t.Fatalf("empty evidence differs from omission: %v", err)
	}
	// Raw duplicates stay byte material. This codec makes no claim that input
	// admission or Runtime will accept their interpretation.
	input.Facts = json.RawMessage(`{"n":9007199254740993,"n":1.2300}`)
	before := bytes.Clone(input.Facts)
	duplicate, err := freezeExecutionPayload(r, input, signing)
	if err != nil || !bytes.Equal(input.Facts, before) {
		t.Fatalf("raw duplicate members were changed or interpreted: %v", err)
	}
	input.Facts = json.RawMessage(`{"n":1.2300}`)
	collapsed, err := freezeExecutionPayload(r, input, signing)
	if err != nil || duplicate.Digest() == collapsed.Digest() {
		t.Fatalf("raw duplicate member commitment was collapsed: %v", err)
	}
	if (executionPayload{}).Digest() != "" || len((executionPayload{}).Bytes()) != 0 {
		t.Fatal("uninitialized descriptor must not expose a payload digest")
	}
}

func TestExecutionPayloadCommitsEveryOperationalComponent(t *testing.T) {
	manifest := executionVectorManifest(t)
	v := manifest.Vectors[len(manifest.Vectors)-1]
	for _, change := range []struct {
		name string
		edit func(*Release, *Input, *signingInputs)
	}{
		{"release identity", func(r *Release, _ *Input, _ *signingInputs) { r.ID += "-changed" }},
		{"Pack", func(r *Release, _ *Input, _ *signingInputs) { r.Pack += " "; r.PackDigest = digest([]byte(r.Pack)) }},
		{"configuration bytes", func(r *Release, _ *Input, _ *signingInputs) { r.Config += " " }},
		{"lock bytes", func(r *Release, _ *Input, _ *signingInputs) { r.Lock += " " }},
		{"Runtime", func(r *Release, _ *Input, _ *signingInputs) { r.RuntimeDigest = digest([]byte("another binary")) }},
		{"facts bytes", func(_ *Release, i *Input, _ *signingInputs) { i.Facts = append(i.Facts, ' ') }},
		{"evidence bytes", func(_ *Release, i *Input, _ *signingInputs) { i.Evidence = append(i.Evidence, ' ') }},
		{"preparation instant", func(_ *Release, i *Input, _ *signingInputs) { i.Preparation.VerifiedAt = "2026-10-18T00:00:01Z" }},
		{"citations", func(_ *Release, i *Input, _ *signingInputs) { i.Preparation.Cites[0].CallIndex++ }},
		{"preparation number", func(_ *Release, i *Input, _ *signingInputs) {
			i.Preparation.Outcomes[0].Parameters = json.RawMessage(`{"big":9007199254740992,"decimal":1.2300}`)
		}},
		{"source case", func(_ *Release, i *Input, _ *signingInputs) {
			i.Source.Case = json.RawMessage(`{"big":9007199254740992,"decimal":1.2300,"exponent":1e+308}`)
		}},
		{"source import", func(_ *Release, i *Input, _ *signingInputs) {
			i.Source.Import.Binding = json.RawMessage(`{"sources":{},"synthetic":true}`)
		}},
		{"source response", func(_ *Release, i *Input, _ *signingInputs) {
			i.Source.Sources["synthetic"] = SourceValue{Response: json.RawMessage(`{"result":{"value":1}}`)}
		}},
		{"preparation omission", func(_ *Release, i *Input, _ *signingInputs) { i.Preparation = nil }},
		{"source omission", func(_ *Release, i *Input, _ *signingInputs) { i.Source = nil }},
		{"signing mode", func(_ *Release, _ *Input, s *signingInputs) { *s = signingInputs{} }},
		{"signing public key", func(_ *Release, _ *Input, s *signingInputs) {
			s.publicKey = "3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c"
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			release, input, signing := executionVectorInput(t, v)
			change.edit(&release, &input, &signing)
			payload, err := freezeExecutionPayload(release, input, signing)
			if err != nil || payload.Digest() == v.Digest {
				t.Fatalf("changed component was not committed: %v", err)
			}
		})
	}
	// Paths locate the same signing identity; no host path enters this profile.
	release, input, signing := executionVectorInput(t, v)
	signing.path = "/another/private/location"
	payload, err := freezeExecutionPayload(release, input, signing)
	if err != nil || payload.Digest() != v.Digest || bytes.Contains(payload.Bytes(), []byte(signing.path)) {
		t.Fatalf("signing path changed the payload: %v", err)
	}
	input.Facts[0] = 'x'
	input.Source.Case[0] = 'x'
	input.Preparation.VerifiedAt = "changed"
	release.Pack = "changed"
	returned := payload.Bytes()
	returned[0] = 'x'
	if payload.Digest() != v.Digest || !bytes.Equal(payload.Bytes(), executionVectorBytes(t, v.Descriptor)) {
		t.Fatal("retained descriptor can be mutated by an input or returned byte slice")
	}
}

func TestExecutionPayloadRefusesMalformedMaterial(t *testing.T) {
	manifest := executionVectorManifest(t)
	v := manifest.Vectors[len(manifest.Vectors)-1]
	changes := []struct {
		name string
		edit func(*Release, *Input, *signingInputs)
	}{
		{"missing facts", func(_ *Release, i *Input, _ *signingInputs) { i.Facts = nil }},
		{"invalid facts", func(_ *Release, i *Input, _ *signingInputs) { i.Facts = json.RawMessage(`{"n":NaN}`) }},
		{"multiple facts", func(_ *Release, i *Input, _ *signingInputs) { i.Facts = json.RawMessage(`0 false`) }},
		{"invalid UTF8 facts", func(_ *Release, i *Input, _ *signingInputs) { i.Facts = []byte{'"', 0xff, '"'} }},
		{"unpaired surrogate facts", func(_ *Release, i *Input, _ *signingInputs) { i.Facts = json.RawMessage(`"\ud800"`) }},
		{"invalid evidence", func(_ *Release, i *Input, _ *signingInputs) { i.Evidence = json.RawMessage(` `) }},
		{"uppercase Runtime digest", func(r *Release, _ *Input, _ *signingInputs) { r.RuntimeDigest = "sha256:" + strings.Repeat("A", 64) }},
		{"unknown Runtime digest", func(r *Release, _ *Input, _ *signingInputs) { r.RuntimeDigest = "sha512:" + strings.Repeat("a", 64) }},
		{"invalid Pack digest", func(r *Release, _ *Input, _ *signingInputs) { r.PackDigest = "sha256:0" }},
		{"changed Pack bytes", func(r *Release, _ *Input, _ *signingInputs) { r.Pack += " " }},
		{"invalid Pack", func(r *Release, _ *Input, _ *signingInputs) { r.Pack = "{"; r.PackDigest = digest([]byte(r.Pack)) }},
		{"invalid configuration", func(r *Release, _ *Input, _ *signingInputs) { r.Config = "{" }},
		{"invalid lock", func(r *Release, _ *Input, _ *signingInputs) { r.Lock = "{" }},
		{"invalid release JSON", func(r *Release, _ *Input, _ *signingInputs) { r.Preview = json.RawMessage(`{"bad":`) }},
		{"invalid preparation JSON", func(_ *Release, i *Input, _ *signingInputs) {
			i.Preparation.Outcomes[0].Parameters = json.RawMessage(`{"bad":`)
		}},
		{"duplicate preparation member", func(_ *Release, i *Input, _ *signingInputs) {
			i.Preparation.Outcomes[0].Claim = json.RawMessage(`{"value":0,"value":1}`)
		}},
		{"invalid source JSON", func(_ *Release, i *Input, _ *signingInputs) { i.Source.Case = json.RawMessage(`{"bad":`) }},
		{"duplicate source member", func(_ *Release, i *Input, _ *signingInputs) { i.Source.Case = json.RawMessage(`{"value":0,"value":1}`) }},
		{"source nesting", func(_ *Release, i *Input, _ *signingInputs) {
			i.Source.Case = json.RawMessage(strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65))
		}},
		{"public key without signing", func(_ *Release, _ *Input, s *signingInputs) { s.path = "" }},
		{"missing signing public key", func(_ *Release, _ *Input, s *signingInputs) { s.publicKey = "" }},
		{"invalid signing public key", func(_ *Release, _ *Input, s *signingInputs) { s.publicKey = strings.Repeat("0", 64) }},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			release, input, signing := executionVectorInput(t, v)
			change.edit(&release, &input, &signing)
			payload, err := freezeExecutionPayload(release, input, signing)
			if err == nil || len(payload.Bytes()) != 0 || payload.Digest() != "" {
				t.Fatal("malformed material produced a descriptor")
			}
		})
	}
}

func TestExecutionPayloadByteBounds(t *testing.T) {
	v := executionVectorManifest(t).Vectors[0]
	for _, component := range []string{"facts", "evidence", "Pack", "configuration", "lock", "release", "preparation", "source"} {
		for _, over := range []bool{false, true} {
			name := "at limit"
			if over {
				name = "over limit"
			}
			t.Run(component+"/"+name, func(t *testing.T) {
				r, i, s := executionVectorInput(t, v)
				extra := 0
				if over {
					extra = 1
				}
				quoted := func(n int) []byte { return []byte(`"` + strings.Repeat("a", n-2) + `"`) }
				switch component {
				case "facts":
					i.Facts = quoted(MaxBody + extra)
				case "evidence":
					i.Evidence = quoted(MaxBody + extra)
				case "Pack":
					r.Pack = string(quoted(1<<20 + extra))
					r.PackDigest = digest([]byte(r.Pack))
				case "configuration":
					r.Config = string(quoted(MaxBody + extra))
				case "lock":
					r.Lock = string(quoted(MaxBody + extra))
				case "release":
					r.Title = ""
					r.Title = strings.Repeat("a", maxExecutionRelease-len(encode(r))+extra)
				case "preparation":
					i.Preparation = &Preparation{}
					i.Preparation.Verification = strings.Repeat("a", MaxBody-len(encode(i.Preparation))+extra)
				case "source":
					i.Source = &SourceInput{MappingDigest: "x"}
					i.Source.MappingDigest += strings.Repeat("a", MaxBody-len(encode(i.Source))+extra)
				}
				_, err := freezeExecutionPayload(r, i, s)
				if (err != nil) != over {
					t.Fatalf("bound result: %v", err)
				}
			})
		}
	}
}

// Exercise the existing file-writing boundary, not a second codec. The child
// exits before evaluation; no audit, decision or eligibility is fabricated.
func TestExecutionPayloadMatchesOperationalFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture requires /bin/sh")
	}
	vectors := executionVectorManifest(t).Vectors
	for _, v := range []executionPayloadVector{vectors[0], vectors[1], vectors[len(vectors)-1]} {
		t.Run(v.Name, func(t *testing.T) {
			r, input, signing := executionVectorInput(t, v)
			bin := filepath.Join(t.TempDir(), "capture-runtime")
			script := "#!/bin/sh\nif [ \"$1\" = version ]; then printf '%s' '{\"tool\":{\"version\":\"0.31.0\"}}'; exit 0; fi\nprintf '%s\\n' \"$*\" > captured-args\nprintf '%s\\n' \"${JPACK_SIGNING_PUBLIC_KEY-off}\" > captured-key\nexit 42\n"
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "attempt")
			if _, _, _, _, err := evaluateWith(context.Background(), bin, r, input, dir, false, signing); err == nil {
				t.Fatal("capturing child must stop before evaluation")
			}
			for filename, expected := range map[string][]byte{"pack.json": []byte(r.Pack), "jpack.json": []byte(r.Config), "jpack.lock.json": []byte(r.Lock), "facts.json": input.Facts} {
				actual, err := os.ReadFile(filepath.Join(dir, filename))
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("%s changed: %v", filename, err)
				}
			}
			evidence, err := os.ReadFile(filepath.Join(dir, "evidence.json"))
			if len(input.Evidence) == 0 {
				if !os.IsNotExist(err) {
					t.Fatal("omitted evidence was written")
				}
			} else if err != nil || !bytes.Equal(evidence, input.Evidence) {
				t.Fatalf("evidence changed: %v", err)
			}
			args, err := os.ReadFile(filepath.Join(dir, "captured-args"))
			if err != nil || strings.Contains(string(args), "--evidence") != (len(input.Evidence) > 0) {
				t.Fatalf("evidence argument differs: %v", err)
			}
			key, err := os.ReadFile(filepath.Join(dir, "captured-key"))
			expectedKey := "off\n"
			if signing.path != "" {
				expectedKey = signing.publicKey + "\n"
			}
			if err != nil || string(key) != expectedKey {
				t.Fatalf("signing environment differs: %v", err)
			}
			if input.Preparation != nil {
				cites, err := os.ReadFile(filepath.Join(dir, "cites.json"))
				if err != nil || !bytes.Equal(cites, encode(input.Preparation.Cites)) {
					t.Fatalf("citations differ: %v", err)
				}
				binding, err := os.ReadFile(filepath.Join(dir, bindingFile))
				if err != nil || !bytes.Equal(binding, bindingDocument(preparationDigest(input.Preparation))) {
					t.Fatalf("preparation binding differs: %v", err)
				}
			}
		})
	}
}
