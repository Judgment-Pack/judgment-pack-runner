package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/retainedcolumns"
)

func TestRC2PublishedProducerConsistencyCorpus(t *testing.T) {
	// Expand ONLY the published handwritten literal patches, exactly as the
	// publisher's fixture driver does. No interpreter is asked for expectations;
	// no signed/preparation fixture is modified or re-signed.
	script := `import json,copy,sys,pathlib
p=pathlib.Path(sys.argv[1]);examples=json.loads((p/'examples.json').read_text());data=json.loads((p/'cases.json').read_text());out=[]
for row in data['producerCases']:
 c=copy.deepcopy(examples['producerConsistency'])
 for member in ('request','expected'):
  if member in row:c[member]=copy.deepcopy(row[member])
  for edit in row.get(member+'Patch',[]):
   parent=c[member]
   for token in edit['path'][:-1]:parent=parent[token]
   key=edit['path'][-1]
   if edit.get('remove'):del parent[key]
   else:parent[key]=copy.deepcopy(edit['value'])
 out.append({'id':row['id'],'requestJson':row.get('wireJson') or json.dumps(c['request'],ensure_ascii=True,separators=(',',':')),'expected':c['expected']})
print(json.dumps(out))`
	raw, e := exec.Command("python3", "-c", script, rc2FixtureDirectory).Output()
	if e != nil {
		t.Fatal("literal producer fixture expansion", e)
	}
	var rows []struct {
		ID, RequestJSON string
		Expected        struct {
			Consistent                                   bool
			Refusal, OriginalSnapshotSHA256, FrameSHA256 string
			Transformation                               json.RawMessage
		}
	}
	if e = json.Unmarshal(raw, &rows); e != nil || len(rows) != 52 {
		t.Fatal("published producer inventory", e)
	}
	checked, extended := 0, 0
	for _, row := range rows {
		var request struct{ GeneratedConsistency json.RawMessage }
		if e = json.Unmarshal([]byte(row.RequestJSON), &request); e != nil {
			t.Fatal(e)
		}
		var header struct{ Profile string }
		json.Unmarshal(request.GeneratedConsistency, &header)
		if header.Profile == "extended" {
			extended++
			continue
		}
		checked++
		t.Run(row.ID, func(t *testing.T) {
			c, tr, e := rc2ProducerConsistency(request.GeneratedConsistency)
			rc2ExpectedRefusal(t, row.Expected.Refusal, e)
			if e == nil {
				if !row.Expected.Consistent || c.OriginalSnapshotSHA256 != row.Expected.OriginalSnapshotSHA256 || digest(c.RawFrame) != row.Expected.FrameSHA256 || !sameJSON(encode(tr), row.Expected.Transformation) {
					t.Fatalf("published original-inventory/reference result differs: frame%s want%s transformation%s", digest(c.RawFrame), row.Expected.FrameSHA256, encode(tr))
				}
			}
		})
	}
	t.Logf("actual producer tuple/inventory/frame/reference checks=%d; Extended consistency outside Runner=%d; acquisition/history are NOT established", checked, extended)
}

// This is the corpus's explicit prevalidated trust projection, not profile
// registration or a receipt verifier. Actual production entry tests separately
// require the complete trusted profile, request, signature and freshness.
func rc2ProducerConsistency(raw []byte) (retainedColumnCarrier, *retainedcolumns.Transformation, error) {
	fail := func(code string) (retainedColumnCarrier, *retainedcolumns.Transformation, error) {
		return retainedColumnCarrier{}, nil, withCode(code, errors.New("published producer consistency refusal"))
	}
	var b struct {
		Carrier, TrustedProducer, SourceBinding, Rule json.RawMessage
		Profile, ReceiptResultDigest                  string
		Limits                                        map[string]int
	}
	if strictJSON(raw, &b) != nil || b.Profile != "base" {
		return fail(codeMalformed)
	}
	var trusted map[string]json.RawMessage
	if exact, err := exactMembers(b.TrustedProducer); err != nil || len(exact) != 10 {
		return fail(codeMalformed)
	} else {
		trusted = exact
	}
	var profileDigest string
	json.Unmarshal(trusted["profileDigest"], &profileDigest)
	delete(trusted, "profileDigest")
	var p InputProfile
	if strictJSON(encode(trusted), &p) != nil {
		return fail(codeMalformed)
	}
	if e := columnProducerProfile(p); e != nil {
		return retainedColumnCarrier{}, nil, e
	}
	var binding struct{ Kind, Profile, ProfileDigest string }
	if strictJSON(b.SourceBinding, &binding) != nil {
		return fail(codeMalformed)
	}
	if binding.Kind != "operation" || binding.Profile != p.ID || binding.ProfileDigest != profileDigest {
		return fail(codeMismatch)
	}
	c, e := readColumnCarrierLimits(b.Carrier, b.Limits, selectedContract())
	if e != nil {
		return c, nil, e
	}
	canonical, e := scopedJSON(b.Carrier, columnCarrierLimit)
	if e != nil || digest(canonical) != b.ReceiptResultDigest {
		return fail(codeInvalid)
	}
	if e = validateColumnProducerRelation(c); e != nil {
		return c, nil, e
	}
	rule := b.Rule
	var members map[string]json.RawMessage
	if json.Unmarshal(rule, &members) != nil {
		return fail(codeMalformed)
	}
	if bytes.Equal(members["kind"], []byte(`"evidence-availability"`)) {
		delete(members, "kind")
		rule = encode(members)
	}
	plan, e := retainedcolumns.ParsePlan(rule)
	if e != nil {
		return c, nil, columnError(e)
	}
	tr, e := retainedcolumns.Derive(plan, c.Frame)
	return c, tr, columnError(e)
}
