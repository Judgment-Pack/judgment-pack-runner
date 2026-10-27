package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These handwritten records exercise offline artifact admission, not an assertion
// that a published Runtime emitted a maximum record or arbitrary review metadata.
func columnOfflineBundle(t *testing.T, input Input, profiles []InputProfile, at time.Time) VerificationBundle {
	t.Helper()
	prepared, err := normalizeV2(input, profiles, at)
	if err != nil {
		t.Fatal(err)
	}
	pack := `{"specVersion":"0.2.0-draft"}`
	r := Release{ID: "offline-limits-release", Pack: pack, PackDigest: digest([]byte(pack)), PackID: "limits", PackVersion: "1", InputMapping: &prepared.Source.Mapping, InputProfiles: profiles, Sample: prepared}
	disposition := json.RawMessage(`{"kind":"unresolved","reasons":["unknown"]}`)
	audit := encode(map[string]any{
		"recordVersion": "1", "kind": "evaluation", "trail": "00112233445566778899aabbccddeeff", "sequence": 1, "previous": digest(nil),
		"pack": map[string]any{"digest": r.PackDigest}, "inputs": map[string]any{"facts": prepared.Facts, "evidenceSupplied": len(prepared.Evidence) > 0, "evidence": prepared.Evidence},
		"disposition": disposition, "cites": prepared.Preparation.Cites, "binding": json.RawMessage(bindingDocument(preparationDigest(prepared.Preparation))), "padding": "",
	})
	return VerificationBundle{Version: 2, Release: r, ReleaseDigest: releaseDigest(r), Run: Run{ID: "offline-limits-run", ReleaseID: r.ID, State: "completed", Input: prepared, Result: encode(map[string]any{"disposition": disposition}), Audit: audit}}
}

func columnOfflineVersion(t *testing.T, b VerificationBundle, version int) VerificationBundle {
	t.Helper()
	b.Version = version
	b.Run.AuditBytes, b.Run.AuditSignatures, b.Chain = nil, nil, nil
	if version >= 3 {
		// Compact original bytes can represent the same record as a parsed token
		// with optional JSON escapes. This keeps the two record bounds independent.
		var compact bytes.Buffer
		if err := json.Compact(&compact, b.Run.Audit); err != nil {
			t.Fatal(err)
		}
		var audit map[string]json.RawMessage
		if err := json.Unmarshal(compact.Bytes(), &audit); err != nil {
			t.Fatal(err)
		}
		var padding string
		if err := json.Unmarshal(audit["padding"], &padding); err != nil {
			t.Fatal(err)
		}
		audit["padding"] = encode(padding)
		b.Run.AuditBytes = encode(audit)
	}
	return columnOfflineEvidence(t, b)
}

func columnOfflineEvidence(t *testing.T, b VerificationBundle) VerificationBundle {
	t.Helper()
	version := b.Version
	if version >= 4 {
		entry := encode(ChainEntry{EntryVersion: "1", Trail: "00112233445566778899aabbccddeeff", Sequence: 1, Previous: digest(nil), Kind: "run", Run: b.Run.ID, AuditDigest: digest(b.Run.AuditBytes)})
		cp, err := checkpointOf(entry)
		if err != nil {
			t.Fatal(err)
		}
		b.Chain = &RunChainExport{Entry: entry, Checkpoint: cp}
	}
	if version == 5 {
		seed, _ := hex.DecodeString(vectorSeed1)
		key := ed25519.NewKeyFromSeed(seed)
		line := sidecarLine{keyID: vectorKeyID1, trail: "00112233445566778899aabbccddeeff", at: 1, value: digest(b.Run.AuditBytes)}
		sig := ed25519.Sign(key, line.signedBytes())
		b.Run.AuditSignatures = []byte(fmt.Sprintf(`{"keyId":%q,"kind":"record-signature","record":%q,"sequence":1,"sidecarVersion":"1","signature":%q,"trail":%q}`+"\n", line.keyID, line.value, hex.EncodeToString(sig), line.trail))
	}
	b.ReleaseDigest = releaseDigest(b.Release)
	return b
}

func TestColumnRoleOfflineIndependentArtifactLimits(t *testing.T) {
	input := columnCase(t, "text", `"A"`)
	input.Source.ConversionContract = selectedContract()
	base := columnOfflineBundle(t, input, nil, time.Now())
	for _, artifact := range []string{"parsed-record", "original-record", "release", "run"} {
		t.Run(artifact, func(t *testing.T) {
			for version := 2; version <= 5; version++ {
				if artifact == "original-record" && version == 2 {
					continue
				} // v2 has no original bytes
				t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
					b := base
					var over VerificationBundle
					want := ""
					switch artifact {
					case "parsed-record":
						// Repeated optional escapes grow only the parsed token, not the exact
						// compact record in v3+. A one-byte change completes the boundary pair.
						room := 9437184 - len(base.Run.Audit)
						padding := strings.Repeat(`\u0061`, room/6) + strings.Repeat("a", room%6)
						b.Run.Audit = bytes.Replace(base.Run.Audit, []byte(`"padding":""`), []byte(`"padding":"`+padding+`"`), 1)
						b = columnOfflineVersion(t, b, version)
						over = b
						over.Run.Audit = bytes.Replace(b.Run.Audit, []byte(`"padding":"`), []byte(`"padding":"a`), 1)
						// The changed character is also kept in the compact original, chain
						// and signature, so the only violated budget is the parsed record.
						over = columnOfflineVersion(t, over, version)
						if len(b.Run.Audit) != 9437184 || len(over.Run.Audit) != 9437185 || version >= 3 && len(over.Run.AuditBytes) >= 9437184 {
							t.Fatal("parsed-record boundary setup")
						}
						want = "parsed audit record exceeds selected record limit"
					case "original-record":
						b = columnOfflineVersion(t, b, version)
						b.Run.AuditBytes = paddedJSON(b.Run.AuditBytes, 9437184)
						b = columnOfflineEvidence(t, b)
						over = b
						over.Run.AuditBytes = append(append([]byte(nil), b.Run.AuditBytes...), ' ')
						over = columnOfflineEvidence(t, over)
						if len(b.Run.AuditBytes) != 9437184 || len(over.Run.AuditBytes) != 9437185 || len(over.Run.Audit) >= 9437184 {
							t.Fatal("original-record boundary setup")
						}
						want = "audit record's bytes exceed 8 MiB"
					case "release":
						b = columnOfflineVersion(t, b, version)
						b.Release.Preview = json.RawMessage(`""`)
						b.Release.Preview = json.RawMessage(`"` + strings.Repeat("a", 41943040-len(encode(b.Release))) + `"`)
						over = b
						over.Release.Preview = append(append(json.RawMessage(nil), b.Release.Preview[:len(b.Release.Preview)-1]...), 'a', '"')
						if len(encode(b.Release)) != 41943040 || len(encode(over.Release)) != 41943041 {
							t.Fatal("release boundary setup")
						}
						want = "retained release exceeds selected storage limit"
					case "run":
						b = columnOfflineVersion(t, b, version)
						b.Run.Problem = "x" // retain the otherwise omitted member in the size equation
						b.Run.Problem = strings.Repeat("a", 41943040-len(runText(b.Run))+1)
						over = b
						over.Run.Problem += "a"
						if len(runText(b.Run)) != 41943040 || len(runText(over.Run)) != 41943041 {
							t.Fatal("run boundary setup")
						}
						want = "retained run exceeds selected storage limit"
					}
					b.ReleaseDigest, over.ReleaseDigest = releaseDigest(b.Release), releaseDigest(over.Release)
					raw := encode(b)
					if len(raw) >= 75497472 {
						t.Fatal("exact artifact exceeds independent export base")
					}
					v, err := VerifyInputs(raw, nil, b.ReleaseDigest, selectedContract())
					if err != nil || !v.Checked() {
						t.Fatalf("exact %s v%d refused: %v", artifact, version, err)
					}
					if version == 5 && v.CheckSignature(mustKey(t, vectorKey1)).Status != SignatureSigned {
						t.Fatal("exact artifact lost its signature")
					}
					if _, err := VerifyInputs(encode(over), nil, over.ReleaseDigest, selectedContract()); err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("one-over %s v%d escaped independent guard: %v", artifact, version, err)
					}
				})
			}
		})
	}
}

func TestColumnRoleReplayNegotiatedLimits(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	for _, origin := range []string{"asserted", "generated"} {
		t.Run(origin, func(t *testing.T) {
			input := columnCase(t, "text", `"A"`)
			input.Source.ConversionContract = selectedContract()
			var profiles []InputProfile
			if origin == "generated" {
				f := selectedProducer(t)
				input, profiles, at = f.input, []InputProfile{f.profile}, f.at
			}
			b := columnOfflineBundle(t, input, profiles, at)
			// A valid document larger than the mapping independently exercises pack
			// admission, while the first document case is limited by the mapping.
			for _, packDominates := range []bool{false, true} {
				if packDominates {
					b.Release.Pack = `{"specVersion":"0.2.0-draft","note":"` + strings.Repeat("a", 8192) + `"}`
					b.Release.PackDigest = digest([]byte(b.Release.Pack))
					b.Run.Audit = bytes.ReplaceAll(b.Run.Audit, []byte(digest([]byte(`{"specVersion":"0.2.0-draft"}`))), []byte(b.Release.PackDigest))
				}
				frameRaw, _ := rawPointer(mustColumnCaseBytes(b.Run.Input.Source), "/retained")
				if origin == "generated" {
					source := b.Run.Input.Source.Sources["desk"]
					raw := source.Response
					if source.RetainedResponseJSON != "" {
						raw = []byte(source.RetainedResponseJSON)
					}
					parts, err := exactMembers(raw)
					if err != nil {
						t.Fatal(err)
					}
					frameRaw, _ = rawPointer(parts["result"], "/retained")
				}
				if len(frameRaw) == 0 {
					t.Fatal("missing frame boundary")
				}
				docSize := max(len(b.Release.Pack), len(encode(b.Run.Input.Source.Mapping)))
				dir := t.TempDir()
				reportPath, calls, outputPath := filepath.Join(dir, "contract.json"), filepath.Join(dir, "calls"), filepath.Join(dir, "output.json")
				result := []byte(`{"outputVersion":"2","status":"evaluated","packId":"limits","packVersion":"1","evaluatorSpecVersion":"0.2.0-draft","rehearsal":true,"disposition":{"kind":"unresolved","reasons":["unknown"]}}`)
				if err := os.WriteFile(outputPath, result, 0600); err != nil {
					t.Fatal(err)
				}
				script := "#!/bin/sh\nif [ \"$1\" = dialect ]; then exec /bin/cat '" + reportPath + "'; fi\n/bin/echo evaluation >> '" + calls + "'\nexec /bin/cat '" + outputPath + "'\n"
				runtime := filepath.Join(dir, "runtime")
				if err := os.WriteFile(runtime, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				b.Release.RuntimeDigest = digest([]byte(script))
				b.ReleaseDigest = releaseDigest(b.Release)
				for _, tc := range []struct {
					name        string
					doc, frame  int
					unsupported bool
					want        string
				}{
					{"exact", docSize, len(frameRaw), false, ""},
					{"document-one-over", docSize - 1, len(frameRaw), false, "documentBytes"},
					{"frame-one-over", docSize, len(frameRaw) - 1, false, "frameBytes"},
					{"unknown-tuple", docSize, len(frameRaw), true, "support the exact retained-column selection"},
				} {
					t.Run(fmt.Sprintf("pack-%t/%s", packDominates, tc.name), func(t *testing.T) {
						capability := ConversionCapabilities("document-validation")[0]
						capability.Limits["documentBytes"], capability.Limits["frameBytes"] = tc.doc, tc.frame
						if tc.unsupported {
							capability.Spec.Revision = strings.Repeat("0", 40)
						}
						report := encode(map[string]any{"capabilities": map[string]any{"conversionContracts": []ConversionCapability{capability}}})
						if err := os.WriteFile(reportPath, report, 0600); err != nil {
							t.Fatal(err)
						}
						before, _ := os.ReadFile(calls)
						err := VerifyDisposition(context.Background(), encode(b), profiles, b.ReleaseDigest, runtime, selectedContract())
						after, _ := os.ReadFile(calls)
						if tc.want == "" {
							if err != nil || string(after) != string(before)+"evaluation\n" {
								t.Fatalf("exact negotiated artifacts refused/evaluation missing: %v", err)
							}
						} else if err == nil || !strings.Contains(err.Error(), tc.want) || !bytes.Equal(before, after) {
							t.Fatalf("negotiated replay %s bypassed before-effect refusal: %v calls %q -> %q", tc.name, err, before, after)
						}
					})
				}
			}
		})
	}
}
