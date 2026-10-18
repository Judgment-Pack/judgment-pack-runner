package runner

// Trusted hosts may add an immutable profile without restarting the dispatcher.
// This is installation configuration, never imported Dialect authority. The
// private control listener's bearer and Origin checks apply before this handler.
import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
)

const registeredProfilesKey = "registered-input-profiles-v1"
const registeredProfilesBound = 16*(32<<10) + 17 // array brackets and commas

// combinedProfiles is the installation's profiles, configured and registered,
// or the refusal of the set: setAside more profiles are stored, set aside,
// and count toward the bound of 16.
func combinedProfiles(configured, registered []InputProfile, setAside int) ([]InputProfile, error) {
	profiles := append([]InputProfile(nil), configured...)
	for _, p := range registered {
		found := false
		for _, current := range profiles {
			if current.ID != p.ID {
				continue
			}
			if profileHash(current) != profileHash(p) {
				return nil, &apiError{409, "input_profile_conflict", "This profile ID already names another definition. Use a new profile ID and review a new binding."}
			}
			found = true
		}
		if !found {
			profiles = append(profiles, p)
		}
	}
	if len(profiles)+setAside > 16 {
		return nil, bad("input_profile_limit", "At most 16 installation input profiles are supported, a stored profile set aside included. Existing profiles were retained.")
	}
	if err := validateProfiles(profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

// A snapshot read never takes the registration lock: release/upgrade checks can
// run inside SQLite transactions while another caller waits to persist a profile.
func (s *Service) registeredInputProfiles() []InputProfile {
	if held := s.registeredProfiles.Load(); held != nil {
		return *held
	}
	return nil
}

func (s *Service) inputProfiles() []InputProfile {
	// Configuration is frozen at Open; registration is additive and checked before
	// persistence. Return a deep snapshot so a caller cannot mutate trusted pins.
	profiles := append([]InputProfile(nil), s.cfg.InputProfiles...)
	for _, p := range s.registeredInputProfiles() {
		found := false
		for _, current := range profiles {
			found = found || current.ID == p.ID
		}
		if !found {
			profiles = append(profiles, p)
		}
	}
	var snapshot []InputProfile
	_ = json.Unmarshal(encode(profiles), &snapshot)
	return snapshot
}

// loadRegisteredProfiles reads the registered profiles the store's row holds.
// Each that this Runner's admission refuses on its own, as validateProfiles
// refuses that profile alone, is set aside (profile_set_aside.go) and the
// others load. What fails as a set refuses to open the store: a row over its
// storage bound, unreadable, or not an array of JSON objects each with exactly
// a profile's members and every member registration requires;
// more than 16 stored profiles, or more than 16 with the configured ones, a
// set-aside one counted; an id stored twice; and a stored profile, admitted or
// set aside, whose id a configured profile of another definition has.
func (s *Service) loadRegisteredProfiles() error {
	var raw string
	err := s.db.QueryRow("SELECT value FROM metadata WHERE key=?", registeredProfilesKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) > registeredProfilesBound {
		return errors.New("registered input profiles exceed their storage bound")
	}
	var profiles []InputProfile
	if err = strictJSON([]byte(raw), &profiles); err != nil {
		return fmt.Errorf("registered input profiles are invalid: %w", err)
	}
	var stored []json.RawMessage
	if profiles == nil || json.Unmarshal([]byte(raw), &stored) != nil || len(stored) != len(profiles) {
		return errors.New("registered input profiles are invalid")
	}
	for i, element := range stored {
		if e := storedProfileForm(element); e != nil {
			return fmt.Errorf("registered input profiles are invalid: element %d is not a profile in its exact form: %w", i, e)
		}
	}
	if len(profiles) > 16 {
		return errors.New("registered input profiles are invalid: at most 16 input profiles")
	}
	ids := map[string]bool{}
	for _, p := range profiles {
		if ids[p.ID] {
			return fmt.Errorf("registered input profiles are invalid: the id %q is stored twice", p.ID)
		}
		ids[p.ID] = true
	}
	admitted := []InputProfile{}
	var setAside []SetAsideProfile
	for i, p := range profiles {
		if e := validateProfiles([]InputProfile{p}); e != nil {
			setAside = append(setAside, SetAsideProfile{ID: p.ID, Reason: e.Error(), digest: profileHash(p), stored: stored[i]})
			continue
		}
		admitted = append(admitted, p)
	}
	for _, p := range setAside {
		for _, c := range s.cfg.InputProfiles {
			if c.ID == p.ID {
				return fmt.Errorf("registered input profile %q, set aside, has the id of a configured profile of another definition", p.ID)
			}
		}
	}
	if _, err = combinedProfiles(s.cfg.InputProfiles, admitted, len(setAside)); err != nil {
		return err
	}
	s.storedProfiles, s.setAside = stored, setAside
	s.registeredProfiles.Store(&admitted)
	return nil
}

// requiredProfileMembers are the members a profile is registered with, each
// present, endpoint null included.
var requiredProfileMembers = []string{"id", "publicKey", "class", "source", "authority", "shape", "adapter", "endpoint"}

// storedProfileForm holds an element of the row to the form registration
// takes: a JSON object of exactly a profile's members, named as written, with
// every member registration requires. An element of another form, null or
// {} included, is a row Runner did not write, and the store does not open;
// only a profile of this form that admission refuses is set aside.
func storedProfileForm(raw json.RawMessage) error {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || members == nil {
		return errors.New("it is not a JSON object")
	}
	if e := exactPreparationKeys(raw, reflect.TypeOf(InputProfile{})); e != nil {
		return e
	}
	for _, key := range requiredProfileMembers {
		if _, present := members[key]; !present {
			return fmt.Errorf("it has no member %s", key)
		}
	}
	return nil
}

// profileRefusal is the refusal of an installation's configured profiles that
// admission (validateProfiles) refuses with err: it names the first profile at
// which the set fails, the first whose admission with those before it fails,
// and gives that refusal as the reason. A configured profile an earlier Runner
// admitted and this one refuses, such as a scope entry RFC 0018 round 16
// refuses, is not set aside as a registered one is: Runner does not start,
// and the operator edits the configuration.
func profileRefusal(whose string, profiles []InputProfile, err error) error {
	for i := range profiles {
		if e := validateProfiles(profiles[:i+1]); e != nil {
			return fmt.Errorf("%s input profile %q is refused: %w", whose, profiles[i].ID, e)
		}
	}
	return fmt.Errorf("%s input profiles are invalid: %w", whose, err)
}

func (s *Service) registerInputProfile(p InputProfile) (bool, error) {
	if err := validateProfiles([]InputProfile{p}); err != nil {
		return false, bad("invalid_input_profile", "Supply one valid installation input profile.")
	}
	// Copy all nested values before holding the new authority in memory.
	var frozen InputProfile
	if err := json.Unmarshal(encode(p), &frozen); err != nil {
		return false, err
	}
	s.profilesMu.Lock()
	defer s.profilesMu.Unlock()
	for _, held := range s.registeredInputProfiles() {
		if held.ID != frozen.ID {
			continue
		}
		if profileHash(held) != profileHash(frozen) {
			return false, &apiError{409, "input_profile_conflict", "A registered profile is immutable. Use a new profile ID and review a new binding."}
		}
		return false, nil
	}
	// A set-aside profile's id stays taken: it is stored, and bindings and
	// releases made before name it.
	for _, held := range s.setAside {
		if held.ID == frozen.ID {
			return false, &apiError{409, "input_profile_conflict", "A registered profile is immutable, and this ID names one that was set aside when Runner opened its store. Use a new profile ID and review a new binding."}
		}
	}
	proposed := append(append([]InputProfile(nil), s.registeredInputProfiles()...), frozen)
	if _, err := combinedProfiles(s.cfg.InputProfiles, proposed, len(s.setAside)); err != nil {
		return false, err
	}
	// The row is rewritten with every profile it stores as stored, a set-aside
	// one's bytes unchanged, and the new profile after them.
	stored := append(append([]json.RawMessage(nil), s.storedProfiles...), encode(frozen))
	row := storedRow(stored)
	if len(row) > registeredProfilesBound {
		return false, bad("input_profile_limit", "The registered input profiles would exceed their storage bound. Existing profiles were retained.")
	}
	// One atomic, synchronous SQLite write precedes making the profile visible.
	// A retry after an uncertain response can only observe this same definition.
	if _, err := s.db.Exec("INSERT INTO metadata(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", registeredProfilesKey, row); err != nil {
		return false, err
	}
	s.storedProfiles = stored
	s.registeredProfiles.Store(&proposed)
	return true, nil
}

// storedRow is the row that stores profiles: their bytes as given, in order,
// in one JSON array, as encode writes one.
func storedRow(profiles []json.RawMessage) string {
	row := []byte{'['}
	for i, p := range profiles {
		if i > 0 {
			row = append(row, ',')
		}
		row = append(row, p...)
	}
	return string(append(row, ']'))
}

func (s *Service) registerInputProfileHandler(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
	var p InputProfile
	if err != nil || strictJSON(raw, &p) != nil || exactPreparationKeys(raw, reflect.TypeOf(p)) != nil {
		failure(w, &apiError{400, "invalid_request", "Supply one bounded input profile with its exact supported members."})
		return
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		failure(w, bad("invalid_input_profile", "Supply one input profile."))
		return
	}
	for _, key := range requiredProfileMembers {
		if _, present := members[key]; !present {
			failure(w, bad("invalid_input_profile", "The input profile omits a required member."))
			return
		}
	}
	created, err := s.registerInputProfile(p)
	if err != nil {
		failure(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	write(w, status, map[string]any{"profile": p, "digest": profileHash(p)})
}
