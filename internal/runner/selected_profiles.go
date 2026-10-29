package runner

// The selected-profile namespace (runner #191): explicitly selected retained
// generated-input profiles, admitted beside the installation's ordinary
// profiles and outside their bound of 16, for preparation only. It is served
// by the same Service, so the same store and the same executor lease and
// authority hold it; nothing here opens a store, takes a lease or starts a
// worker. A selected profile is a retained-column producer profile
// (columnProducerProfile) that declares the installed producer-carrier
// capability. It is immutable once admitted, kept for the store's life, and
// bound only by the selected input planning route, which prepares and checks
// supplied inputs and acquires nothing. No release, job or run resolves it:
// those resolve the ordinary profiles, and refuse a selected one by name
// (refuseSetAsideIDs). The two namespaces never share an id.
//
// docs/design/selected-profile-preparation.md is the design.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"
)

const selectedProfilesKey = "selected-input-profiles-v1"

// selectedProfileBound is the namespace's bound of profiles. A mapping names
// at most 16 sources (InputMapping.validate), and every retained preparation
// is planned under at most 16 profiles, so 16 selected profiles cover any one
// retained preparation's profiles without narrowing that domain.
const selectedProfileBound = 16

// selectedProfilesBytes bounds the namespace's stored row, the JSON array of
// its profiles. A retained preparation is refused when the profiles it is
// planned under exceed profilesBytes (prepareV2Mode), so a namespace larger
// than that could never be planned under at all.
const selectedProfilesBytes = columnProfilesLimit

// selectedPreparationContract names the namespace's contract in its report.
const selectedPreparationContract = "selected-profile-preparation/1"

// The named refusals of the namespace and of its boundary.
const (
	codeSelectedUnselected   = "selected_preparation_unselected"
	codeSelectedUnsupported  = "selected_profile_unsupported"
	codeSelectedConflict     = "selected_profile_conflict"
	codeSelectedOrdinary     = "selected_profile_ordinary"
	codeSelectedLimit        = "selected_profile_limit"
	codeInputProfileSelected = "input_profile_selected"
)

// selectedProfileForm is the refusal of a profile the namespace does not
// admit, or nil: a profile the installation's admission refuses on its own, a
// profile that is not the retained-column producer, and one that declares no
// installed producer-carrier capability are refused alike.
func selectedProfileForm(p InputProfile) error {
	if validateProfiles([]InputProfile{p}) != nil || columnProducerProfile(p) != nil {
		return bad(codeSelectedUnsupported, "Only a valid retained generated-input producer profile can be selected.")
	}
	if _, e := conversionCapabilityLimits(p.ConversionContracts, "producer-carrier", true); e != nil {
		return bad(codeSelectedUnsupported, "A selected profile declares the installed retained-column producer-carrier capability.")
	}
	return nil
}

// selectedRowBound is the refusal of a namespace whose stored elements exceed
// its bound of profiles or bytes, or nil.
func selectedRowBound(stored []json.RawMessage) error {
	if len(stored) > selectedProfileBound || len(storedRow(stored)) > selectedProfilesBytes {
		return bad(codeSelectedLimit, fmt.Sprintf("At most %d selected profiles of at most %d bytes together are supported, apart from the 16 ordinary profiles. Existing profiles were retained.", selectedProfileBound, selectedProfilesBytes))
	}
	return nil
}

// selectedInputProfiles is a deep snapshot of the admitted selected profiles.
func (s *Service) selectedInputProfiles() []InputProfile {
	held := s.selectedProfiles.Load()
	if held == nil {
		return nil
	}
	var snapshot []InputProfile
	_ = json.Unmarshal(encode(*held), &snapshot)
	return snapshot
}

// ordinaryProfileID reports whether id names an ordinary profile: configured,
// registered, or registered and set aside.
func (s *Service) ordinaryProfileID(id string) bool {
	for _, p := range s.cfg.InputProfiles {
		if p.ID == id {
			return true
		}
	}
	for _, p := range s.registeredInputProfiles() {
		if p.ID == id {
			return true
		}
	}
	for _, p := range s.setAside {
		if p.ID == id {
			return true
		}
	}
	return false
}

// selectedProfileID reports whether id names a selected profile, admitted or
// set aside.
func (s *Service) selectedProfileID(id string) bool {
	if held := s.selectedProfiles.Load(); held != nil {
		for _, p := range *held {
			if p.ID == id {
				return true
			}
		}
	}
	for _, p := range s.selectedSetAside {
		if p.ID == id {
			return true
		}
	}
	return false
}

// selectedRefusal is the refusal of an ordinary binding or request that names
// the selected profile id.
func selectedRefusal(id string) error {
	return bad(codeInputProfileSelected, fmt.Sprintf("The input profile %s is a selected profile, bound only by selected preparation. It is never an ordinary profile, and no release, job or run binds it.", id))
}

// loadSelectedProfiles reads the namespace's row when the store opens, after
// the ordinary profiles. A row Runner did not write refuses to open the store,
// as the ordinary row does: over its bound, unreadable, not an array of
// profiles in their exact form, an id stored twice, or an id an ordinary
// profile has. A stored profile this Runner's selection refuses on its own is
// set aside: kept byte for byte, its id taken, counted, listed with its
// reason, and never bound.
func (s *Service) loadSelectedProfiles() error {
	var raw string
	err := s.db.QueryRow("SELECT value FROM metadata WHERE key=?", selectedProfilesKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var profiles []InputProfile
	var stored []json.RawMessage
	if strictJSON([]byte(raw), &profiles) != nil || profiles == nil || json.Unmarshal([]byte(raw), &stored) != nil || len(stored) != len(profiles) {
		return errors.New("selected input profiles are invalid")
	}
	for i, element := range stored {
		if e := storedProfileForm(element); e != nil {
			return fmt.Errorf("selected input profiles are invalid: element %d is not a profile in its exact form: %w", i, e)
		}
	}
	if len(raw) > selectedProfilesBytes || len(stored) > selectedProfileBound {
		return errors.New("selected input profiles exceed their bound")
	}
	ids := map[string]bool{}
	admitted := []InputProfile{}
	for i, p := range profiles {
		if ids[p.ID] || s.ordinaryProfileID(p.ID) {
			return fmt.Errorf("selected input profiles are invalid: the id %q is stored twice or is an ordinary profile's", p.ID)
		}
		ids[p.ID] = true
		if e := selectedProfileForm(p); e != nil {
			s.selectedSetAside = append(s.selectedSetAside, SetAsideProfile{ID: p.ID, Reason: e.Error(), digest: profileHash(p), stored: stored[i]})
			continue
		}
		admitted = append(admitted, p)
	}
	s.selectedStored = stored
	s.selectedProfiles.Store(&admitted)
	return nil
}

// admitSelectedProfile admits p to the namespace: true when it is new, false
// when the namespace already holds this same definition.
func (s *Service) admitSelectedProfile(p InputProfile) (bool, error) {
	if s.cfg.ExecutorAdmission != nil {
		return false, manualUnsupported()
	}
	if e := selectedProfileForm(p); e != nil {
		return false, e
	}
	var frozen InputProfile
	if err := json.Unmarshal(encode(p), &frozen); err != nil {
		return false, err
	}
	// Ordinary registration takes the same lock, so the two namespaces
	// never admit one id together.
	s.profilesMu.Lock()
	defer s.profilesMu.Unlock()
	for _, held := range s.selectedInputProfiles() {
		if held.ID != frozen.ID {
			continue
		}
		if profileHash(held) != profileHash(frozen) {
			return false, &apiError{409, codeSelectedConflict, "A selected profile is immutable. Use a new profile ID."}
		}
		return false, nil
	}
	for _, held := range s.selectedSetAside {
		if held.ID == frozen.ID {
			return false, &apiError{409, codeSelectedConflict, "A selected profile is immutable, and this ID names one that was set aside when Runner opened its store. Use a new profile ID."}
		}
	}
	if s.ordinaryProfileID(frozen.ID) {
		return false, &apiError{409, codeSelectedOrdinary, "This ID names an ordinary installation profile, which is never selected. Use a new profile ID."}
	}
	stored := append(append([]json.RawMessage(nil), s.selectedStored...), encode(frozen))
	if e := selectedRowBound(stored); e != nil {
		return false, e
	}
	// One synchronous write precedes making the profile visible, as an
	// ordinary registration's does.
	if _, err := s.db.Exec("INSERT INTO metadata(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", selectedProfilesKey, storedRow(stored)); err != nil {
		return false, err
	}
	admitted := append(s.selectedInputProfiles(), frozen)
	s.selectedStored = stored
	s.selectedProfiles.Store(&admitted)
	return true, nil
}

// inputProfileIDs is each profile id an input names: in its mapping, and in
// its import's binding document.
func inputProfileIDs(i Input) []string {
	if i.Source == nil {
		return nil
	}
	ids := mappingProfileIDs(i.Source.Mapping)
	if i.Source.Import != nil {
		ids = append(ids, bindingProfileIDs(i.Source.Import.Binding)...)
	}
	return ids
}

// selectedNextInput is next-input planning under the selected profiles alone.
// It prepares and checks the supplied input, persists nothing and acquires
// nothing. An input that names an ordinary profile is refused by that name,
// and one that names a selected profile set aside by its own.
func (s *Service) selectedNextInput(i Input, at time.Time) (InputPlan, error) {
	if s.cfg.ExecutorAdmission != nil {
		return InputPlan{}, manualUnsupported()
	}
	for _, id := range inputProfileIDs(i) {
		if s.ordinaryProfileID(id) {
			return InputPlan{}, bad(codeSelectedOrdinary, fmt.Sprintf("The input profile %s is an ordinary installation profile. Selected preparation binds selected profiles only.", id))
		}
		for _, p := range s.selectedSetAside {
			if p.ID == id {
				return InputPlan{}, p.refusal()
			}
		}
	}
	return nextInput(i, s.selectedInputProfiles(), at)
}

// selectedUnselected is the refusal of a selected-profile route reached
// without the retained-column prefix and its exact selector.
func selectedUnselected() error {
	return bad(codeSelectedUnselected, "Selected preparation is served under /v1/retained-column/1/ with the exact installed JPack-Conversion-Contract selector.")
}

// selectedReport is the namespace's capability report and listing.
func (s *Service) selectedReport() map[string]any {
	profiles := []any{}
	for _, p := range s.selectedInputProfiles() {
		profiles = append(profiles, map[string]any{"profile": p, "digest": profileHash(p)})
	}
	for _, p := range s.selectedSetAside {
		profiles = append(profiles, map[string]any{"profile": p.stored, "digest": p.digest, "state": "set-aside", "reason": p.Reason})
	}
	return map[string]any{"contract": selectedPreparationContract, "bound": map[string]int{"profiles": selectedProfileBound, "profilesBytes": selectedProfilesBytes}, "profiles": profiles}
}

func (s *Service) selectedProfileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/selected-profiles", func(w http.ResponseWriter, r *http.Request) {
		if requestConversion(r) == nil {
			failure(w, selectedUnselected())
			return
		}
		write(w, 200, s.selectedReport())
	})
	mux.HandleFunc("POST /v1/selected-profiles", func(w http.ResponseWriter, r *http.Request) {
		if requestConversion(r) == nil {
			failure(w, selectedUnselected())
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
		var p InputProfile
		if err != nil || strictJSON(raw, &p) != nil || exactPreparationKeys(raw, reflect.TypeOf(p)) != nil {
			failure(w, &apiError{400, "invalid_request", "Supply one bounded input profile with its exact supported members."})
			return
		}
		var members map[string]json.RawMessage
		if json.Unmarshal(raw, &members) != nil {
			failure(w, bad(codeSelectedUnsupported, "Supply one input profile."))
			return
		}
		for _, key := range requiredProfileMembers {
			if _, present := members[key]; !present {
				failure(w, bad(codeSelectedUnsupported, "The input profile omits a required member."))
				return
			}
		}
		created, err := s.admitSelectedProfile(p)
		if err != nil {
			failure(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		write(w, status, map[string]any{"profile": p, "digest": profileHash(p)})
	})
	mux.HandleFunc("POST /v1/selected-inputs/next", func(w http.ResponseWriter, r *http.Request) {
		if requestConversion(r) == nil {
			failure(w, selectedUnselected())
			return
		}
		var input Input
		if err := decodeInput(w, r, &input); err != nil {
			failure(w, err)
			return
		}
		plan, err := s.selectedNextInput(input, time.Now().UTC().Truncate(time.Second))
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, plan)
	})
}
