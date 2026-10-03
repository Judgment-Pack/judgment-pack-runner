package runner

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The test vector of the Runtime guide's "Record signatures, exactly": the
// seeds of RFC 8032's first two test vectors, a trail of two lines, and a
// sidecar of three, copied from the guide as written.
const (
	vectorSeed1  = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	vectorKey1   = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
	vectorKeyID1 = "21fe31dfa154a261626bf854046fd227"
	vectorKey2   = "3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c"
	vectorKeyID2 = "39f713d0a644253f04529421b9f51b9b"
	vectorTrail1 = `{"recordVersion":"1","trail":"00112233445566778899aabbccddeeff","sequence":1,"previous":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","kind":"evaluation"}`
	vectorTrail2 = `{"recordVersion":"1","trail":"00112233445566778899aabbccddeeff","sequence":2,"previous":"sha256:9305565078dd0531a50e67ee2d227d4fd0cd0d504541b92801660236a0f0eaed","kind":"evaluation"}`
	vectorSigned = `judgment-pack-runtime/record-signature/1:{"record":"sha256:9305565078dd0531a50e67ee2d227d4fd0cd0d504541b92801660236a0f0eaed","sequence":1,"trail":"00112233445566778899aabbccddeeff"}`
	vectorLine1  = `{"keyId":"21fe31dfa154a261626bf854046fd227","kind":"record-signature","record":"sha256:9305565078dd0531a50e67ee2d227d4fd0cd0d504541b92801660236a0f0eaed","sequence":1,"sidecarVersion":"1","signature":"be509a591ce3d1ecc67a3bd2c35c914e001d2737a62ff8759e5be775448e5d5531370372971fdee8ac5b3c7c63fe16c9f83fed9243e114b8e6c02a2156ffbb0b","trail":"00112233445566778899aabbccddeeff"}`
	vectorLine2  = `{"at":1,"keyId":"21fe31dfa154a261626bf854046fd227","kind":"key-rotation","next":"3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c","sidecarVersion":"1","signature":"d6cf4da642a0f84dbaaf03a88b7afe0d7eded03897301ac80ab26f7248631502ebc482063a0dc37a2234fff2c62f8bff35ae825261e9d0b6d7908495a7f21201","trail":"00112233445566778899aabbccddeeff"}`
	vectorLine3  = `{"keyId":"39f713d0a644253f04529421b9f51b9b","kind":"record-signature","record":"sha256:d927c7b963914116f0f47219b0564b40741ae2b56de1c38ccaf27fd0f0630702","sequence":2,"sidecarVersion":"1","signature":"b28b5765844abecd9ca69b642ae1685597045babda87635b8760da9d747a6dc1c244cbbbb0b2c2d73766855665bad9487f61a373209244f7c41af0bb406d8400","trail":"00112233445566778899aabbccddeeff"}`
	// A signature only the cofactored check accepts, which must be refused.
	vectorCofactored = "2faf65a6e31c2e133985c42d3ca36eb1ffe2d8c859d0078a61a4188abb71a2aa310568784fb6c286895b994aba5032fd65558742dcdce63ea7f0c71fb8428904"
)

func mustKey(t *testing.T, text string) ed25519.PublicKey {
	t.Helper()
	key, err := ParsePublicKey(text)
	if err != nil {
		t.Fatal(text, err)
	}
	return key
}

// findingsAt is a check's findings as name@line, in order.
func findingsAt(c signatureCheck) []string {
	all := []string{}
	for _, f := range c.findings {
		all = append(all, fmt.Sprintf("%s@%d", f.Name, f.Line))
	}
	return all
}

// The guide's keys, keyIds and signed bytes are reproduced, and a signature
// Runner makes with the guide's seed is the guide's line, byte for byte.
func TestTheGuidesVectorIsReproduced(t *testing.T) {
	seed, _ := hex.DecodeString(vectorSeed1)
	private := ed25519.NewKeyFromSeed(seed)
	public := private.Public().(ed25519.PublicKey)
	if hex.EncodeToString(public) != vectorKey1 || KeyID(public) != vectorKeyID1 || KeyID(mustKey(t, vectorKey2)) != vectorKeyID2 {
		t.Fatal("the keys or keyIds are not the guide's")
	}
	line, ok := readSidecarLine([]byte(vectorLine1))
	if !ok || string(line.signedBytes()) != vectorSigned || digest([]byte(vectorTrail1)) != line.value {
		t.Fatalf("the signed bytes are not the guide's: %s", line.signedBytes())
	}
	if hex.EncodeToString(ed25519.Sign(private, []byte(vectorSigned))) != hex.EncodeToString(line.signature) {
		t.Fatal("a signature with the guide's seed is not the guide's")
	}
}

// The guide's vector, verified by Runner's code, gives the answers the guide
// states for it.
func TestTheGuidesVectorVerifies(t *testing.T) {
	trail := [][]byte{[]byte(vectorTrail1), []byte(vectorTrail2)}
	sidecar := []byte(vectorLine1 + "\n" + vectorLine2 + "\n" + vectorLine3 + "\n")
	cofactored := []byte(strings.Replace(vectorLine1, "be509a591ce3d1ecc67a3bd2c35c914e001d2737a62ff8759e5be775448e5d5531370372971fdee8ac5b3c7c63fe16c9f83fed9243e114b8e6c02a2156ffbb0b", vectorCofactored, 1) + "\n" + vectorLine2 + "\n" + vectorLine3 + "\n")
	for name, c := range map[string]struct {
		sidecar  []byte
		key      string
		findings []string
		signed   []int64
		inForce  string
	}{
		// Every line holds: both records signed, the rotation followed, the
		// second key in force at the end, the coverage through sequence 2.
		"with the first public key alone": {sidecar, vectorKey1, []string{}, []int64{1, 2}, vectorKey2},
		// The first line and the rotation fail, both at line 1, and the
		// second record is signed.
		"with the second public key alone": {sidecar, vectorKey2, []string{"signature-invalid@1", "rotation-invalid@1"}, []int64{2}, vectorKey2},
		// The cofactored signature fails, the rotation after it holds, and
		// record 2 is signed.
		"with a signature only the cofactored check accepts": {cofactored, vectorKey1, []string{"signature-invalid@1"}, []int64{2}, vectorKey2},
	} {
		t.Run(name, func(t *testing.T) {
			got := checkSignatures(trail, c.sidecar, mustKey(t, c.key))
			var signed []int64
			for s := range got.signed {
				signed = append(signed, s)
			}
			slices.Sort(signed)
			if !slices.Equal(findingsAt(got), c.findings) || !slices.Equal(signed, c.signed) || got.through != 2 || hex.EncodeToString(got.inForce) != c.inForce || got.readable != 3 || got.unreadable != 0 {
				t.Fatalf("findings %v, signed %v, through %d, in force %x", findingsAt(got), signed, got.through, got.inForce)
			}
		})
	}
	// An attempt's trail holds one record: the guide's first record and its
	// own line are signed under the first key, and not under the second.
	one := [][]byte{[]byte(vectorTrail1)}
	if got := checkSignatures(one, []byte(vectorLine1+"\n"), mustKey(t, vectorKey1)); got.total != 0 || got.through != 1 {
		t.Fatal(findingsAt(got))
	}
	if got := checkSignatures(one, []byte(vectorLine1+"\n"), mustKey(t, vectorKey2)); !slices.Equal(findingsAt(got), []string{"signature-invalid@1"}) || got.through != 0 {
		t.Fatal(findingsAt(got))
	}
}

// A public key is refused unless it is the canonical encoding of a point of
// the curve whose order does not divide 8, before anything signed with it is
// read; no key a seed derives is refused.
func TestAPublicKeyIsHeldToTheKeyCheck(t *testing.T) {
	for _, accepted := range []string{vectorKey1, vectorKey2} {
		if _, err := ParsePublicKey(accepted); err != nil {
			t.Fatal(accepted, err)
		}
	}
	for i := range 64 {
		seed := bytes.Repeat([]byte{byte(i * 7)}, 32)
		public := hex.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
		if _, err := ParsePublicKey(public); err != nil {
			t.Fatal("a key a seed derives was refused:", public, err)
		}
	}
	refused := map[string]error{
		"":                          errKeyForm,
		strings.ToUpper(vectorKey1): errKeyForm,
		vectorKey1[:62]:             errKeyForm,
		vectorKey1 + "00":           errKeyForm,
		" " + vectorKey1[1:]:        errKeyForm,
		strings.Repeat("g", 64):     errKeyForm,
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f": errKeyNotCanonical, // y = p, a lenient reader's y = 0
		"f0ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f": errKeyNotCanonical, // y = p + 3, a point of y = 3
		"0100000000000000000000000000000000000000000000000000000000000080": errKeyNotCanonical, // y = 1, x = 0 with its sign set
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff": errKeyNotCanonical, // y = p - 1, x = 0 with its sign set
		"0200000000000000000000000000000000000000000000000000000000000000": errKeyNoPoint,      // y = 2
	}
	for _, small := range smallOrderKeys {
		refused[small] = errKeySmallOrder
	}
	if len(refused) != 19 {
		t.Fatal("the cases overlap")
	}
	for text, want := range refused {
		if _, err := ParsePublicKey(text); err != want {
			t.Errorf("%q: %v, not %v", text, err, want)
		}
	}
	// Under the all-zero key, a key of small order, crypto/ed25519 accepts a
	// signature anyone can make for most messages: R a point of small order and
	// s zero. The key check is what refuses such a key.
	forged := 0
	for _, r := range smallOrderKeys {
		sig, _ := hex.DecodeString(r + strings.Repeat("0", 64))
		if ed25519.Verify(make([]byte, 32), []byte(vectorSigned), sig) {
			forged++
		}
	}
	if forged == 0 {
		t.Fatal("the fixture's forgery does not show what the key check guards against")
	}
}

// A sidecar line is read by the rule: one JSON object of exactly the seven
// members, each once and of its form, whatever its whitespace and escapes,
// with names and strings read as JSON decodes them, and at most 4096 bytes.
func TestASidecarLineIsReadByTheRule(t *testing.T) {
	one := [][]byte{[]byte(vectorTrail1)}
	key := mustKey(t, vectorKey1)
	padded := strings.Replace(vectorLine1, `{"keyId"`, `{`+strings.Repeat(" ", 4096-len(vectorLine1))+`"keyId"`, 1)
	for name, c := range map[string]struct {
		line   string
		signed bool
	}{
		"as written":                     {vectorLine1, true},
		"spaced":                         {strings.ReplaceAll(strings.ReplaceAll(vectorLine1, `":`, `" : `), `,"`, ` , "`), true},
		"a name escaped":                 {strings.Replace(vectorLine1, `"kind"`, `"kind"`, 1), true},
		"a value escaped":                {strings.Replace(vectorLine1, `"trail":"00`, `"trail":"00`, 1), true},
		"of exactly 4096 bytes":          {padded, true},
		"of 4097 bytes":                  {strings.Replace(padded, `{`, `{ `, 1), false},
		"a name given twice":             {strings.Replace(vectorLine1, `"kind":"record-signature",`, `"kind":"record-signature","kind":"record-signature",`, 1), false},
		"a member more":                  {strings.Replace(vectorLine1, `{`, `{"note":"x",`, 1), false},
		"a member less":                  {strings.Replace(vectorLine1, `"sidecarVersion":"1",`, ``, 1), false},
		"another sidecarVersion":         {strings.Replace(vectorLine1, `"sidecarVersion":"1"`, `"sidecarVersion":"2"`, 1), false},
		"another kind":                   {strings.Replace(vectorLine1, `"record-signature"`, `"record-signatures"`, 1), false},
		"a sequence of 1.0":              {strings.Replace(vectorLine1, `"sequence":1,`, `"sequence":1.0,`, 1), false},
		"a sequence of 1e0":              {strings.Replace(vectorLine1, `"sequence":1,`, `"sequence":1e0,`, 1), false},
		"a sequence of 0":                {strings.Replace(vectorLine1, `"sequence":1,`, `"sequence":0,`, 1), false},
		"a sequence of 2^53-1":           {strings.Replace(vectorLine1, `"sequence":1,`, `"sequence":9007199254740991,`, 1), false},
		"a sequence as a string":         {strings.Replace(vectorLine1, `"sequence":1,`, `"sequence":"1",`, 1), false},
		"an uppercase signature":         {strings.Replace(vectorLine1, `"signature":"be509a`, `"signature":"BE509A`, 1), false},
		"a short keyId":                  {strings.Replace(vectorLine1, `"keyId":"21fe`, `"keyId":"21f`, 1), false},
		"a record without sha256:":       {strings.Replace(vectorLine1, `"record":"sha256:`, `"record":"`, 1), false},
		"a torn line ended with a tilde": {vectorLine1[:100] + "~", false},
		"two objects":                    {vectorLine1 + vectorLine1, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, readable := readSidecarLine([]byte(c.line))
			got := checkSignatures(one, []byte(c.line+"\n"), key)
			if readable != c.signed || (got.through == 1) != c.signed || got.total != 0 || got.readable+got.unreadable != 1 {
				t.Fatalf("readable %v, through %d, findings %v", readable, got.through, findingsAt(got))
			}
		})
	}
	// A line with no newline after it was never written, as a verifier reads
	// it, and an empty sidecar signs nothing. A carriage return before a
	// line's newline is JSON's whitespace, and the line still signs.
	for sidecar, want := range map[string]struct {
		through              int64
		readable, unreadable int
	}{
		vectorLine1:                      {0, 0, 1},
		"":                               {0, 0, 0},
		"\n":                             {0, 0, 1},
		vectorLine1 + "\r\n":             {1, 1, 0},
		vectorLine1 + "\n" + vectorLine1: {1, 1, 1},
	} {
		got := checkSignatures(one, []byte(sidecar), key)
		readable, unreadable := sidecarLineCounts([]byte(sidecar))
		if got.through != want.through || got.total != 0 || got.readable != want.readable || got.unreadable != want.unreadable || readable != want.readable || unreadable != want.unreadable {
			t.Fatalf("%q: through %d, lines %d and %d, findings %v", sidecar, got.through, got.readable, got.unreadable, findingsAt(got))
		}
	}
}

// Each check of the rule, with the trail known, as an attempt's is: its one
// record.
func TestARecordSignatureIsCheckedAgainstItsTrail(t *testing.T) {
	seed, _ := hex.DecodeString(vectorSeed1)
	private := ed25519.NewKeyFromSeed(seed)
	key := mustKey(t, vectorKey1)
	trail := "00112233445566778899aabbccddeeff"
	sign := func(record []byte, sequence int64, trailID string) string {
		l := sidecarLine{keyID: vectorKeyID1, trail: trailID, at: sequence, value: digest(record)}
		l.signature = ed25519.Sign(private, l.signedBytes())
		return fmt.Sprintf(`{"keyId":%q,"kind":"record-signature","record":%q,"sequence":%d,"sidecarVersion":"1","signature":%q,"trail":%q}`, l.keyID, l.value, sequence, hex.EncodeToString(l.signature), trailID)
	}
	rotate := func(at int64, next, trailID string) string {
		l := sidecarLine{rotation: true, keyID: vectorKeyID1, trail: trailID, at: at, value: next}
		l.signature = ed25519.Sign(private, l.signedBytes())
		return fmt.Sprintf(`{"at":%d,"keyId":%q,"kind":"key-rotation","next":%q,"sidecarVersion":"1","signature":%q,"trail":%q}`, at, l.keyID, next, hex.EncodeToString(l.signature), trailID)
	}
	record := []byte(vectorTrail1)
	changed := []byte(strings.Replace(vectorTrail1, `"kind":"evaluation"`, `"kind":"evaluation","note":"changed"`, 1))
	unchained := []byte(`{"recordVersion":"1","kind":"evaluation"}`)
	atTwo := []byte(strings.Replace(vectorTrail1, `"sequence":1`, `"sequence":2`, 1))
	linked := []byte(strings.Replace(vectorTrail1, `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`, strings.Repeat("0", 64), 1))
	for name, c := range map[string]struct {
		record   []byte
		sidecar  string
		findings []string
		through  int64
	}{
		"signed":                            {record, sign(record, 1, trail), []string{}, 1},
		"the record changed":                {changed, sign(record, 1, trail), []string{"signature-record-mismatch@1"}, 0},
		"the record changed and re-signed":  {changed, sign(changed, 1, trail), []string{}, 1},
		"signed as another sequence":        {record, sign(record, 2, trail), []string{"signature-no-record@2"}, 0},
		"signed as another trail's":         {record, sign(record, 1, strings.Repeat("a", 32)), []string{"signature-no-record@1"}, 0},
		"a record that is not chained":      {unchained, sign(unchained, 1, trail), []string{"signature-no-record@1"}, 0},
		"a record not at its sequence":      {atTwo, sign(atTwo, 1, trail), []string{"sequence-mismatch@1"}, 0},
		"a record not linked to nothing":    {linked, sign(linked, 1, trail), []string{"previous-mismatch@1"}, 0},
		"the same line twice":               {record, sign(record, 1, trail) + "\n" + sign(record, 1, trail), []string{"sidecar-out-of-order@1"}, 1},
		"a rotation, then the record":       {record, rotate(1, vectorKey2, trail) + "\n" + sign(record, 1, trail), []string{"sidecar-out-of-order@1"}, 0},
		"the record, then a rotation":       {record, sign(record, 1, trail) + "\n" + rotate(1, vectorKey2, trail), []string{}, 1},
		"a rotation past the trail":         {record, rotate(2, vectorKey2, trail), []string{"signature-no-record@2"}, 0},
		"a rotation of another trail":       {record, rotate(1, vectorKey2, strings.Repeat("a", 32)), []string{"rotation-invalid@1"}, 0},
		"a rotation to a small-order key":   {record, sign(record, 1, trail) + "\n" + rotate(1, smallOrderKeys[2], trail), []string{"rotation-invalid@1"}, 1},
		"a rotation to a non-canonical key": {record, sign(record, 1, trail) + "\n" + rotate(1, "edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", trail), []string{"rotation-invalid@1"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			got := checkSignatures([][]byte{c.record}, []byte(c.sidecar+"\n"), key)
			if !slices.Equal(findingsAt(got), c.findings) || got.through != c.through {
				t.Fatalf("findings %v, through %d", findingsAt(got), got.through)
			}
		})
	}
	// A record signature made with another key, or naming another keyId.
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	l := sidecarLine{keyID: vectorKeyID1, trail: trail, at: 1, value: digest(record)}
	forged := fmt.Sprintf(`{"keyId":%q,"kind":"record-signature","record":%q,"sequence":1,"sidecarVersion":"1","signature":%q,"trail":%q}`, vectorKeyID1, l.value, hex.EncodeToString(ed25519.Sign(other, l.signedBytes())), trail)
	renamed := strings.Replace(sign(record, 1, trail), vectorKeyID1, vectorKeyID2, 1)
	for _, line := range []string{forged, renamed} {
		if got := checkSignatures([][]byte{record}, []byte(line+"\n"), key); !slices.Equal(findingsAt(got), []string{"signature-invalid@1"}) {
			t.Fatal(findingsAt(got))
		}
	}
}
