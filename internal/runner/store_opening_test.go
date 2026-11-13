package runner

import (
	"errors"
	"testing"
)

// TestManualAdmissionStaysOnTheSQLiteKind: under manual admission, the store
// opening refuses a kind that hands over no manual-admission state, which
// would leave the store unchecked (docs/design/store-postgresql.md, D4), and
// opens the sqlite kind, which hands it over. Without manual admission, the
// same kind opens.
func TestManualAdmissionStaysOnTheSQLiteKind(t *testing.T) {
	const keepsNone = "keeps-no-manual-state"
	storeKinds[keepsNone] = func(storeOpening) (runStore, error) { return newMemoryStore(), nil }
	t.Cleanup(func() { delete(storeKinds, keepsNone) })
	var handed manualState
	opening := func(admission bool) storeOpening {
		handed = nil
		return storeOpening{
			dir: privateStateDir(t), workspace: "conformance", owner: "owner",
			migrate: !admission, manualAdmission: admission,
			manual: func(m manualState) error { handed = m; return nil },
		}
	}
	if st, err := openStore(keepsNone, opening(true)); !errors.Is(err, errManualProtocol) || st != nil {
		t.Fatalf("a kind keeping no manual-admission state opened under manual admission: %v", err)
	}
	st, err := openStore(keepsNone, opening(false))
	if err != nil {
		t.Fatalf("a kind keeping no manual-admission state refused without manual admission: %v", err)
	}
	st.close()
	st, err = openStore(defaultStoreKind, opening(true))
	if err != nil || handed == nil {
		t.Fatalf("the sqlite kind under manual admission: %v, state handed over: %v", err, handed != nil)
	}
	st.close()
}
