package runner

// Trusted hosts may add an immutable profile without restarting the dispatcher.
// This is installation configuration, never imported Dialect authority. The
// private control listener's bearer and Origin checks apply before this handler.
import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
)

const registeredProfilesKey = "registered-input-profiles-v1"
const registeredProfilesBound = 16*(32<<10) + 17 // array brackets and commas

func combinedProfiles(configured, registered []InputProfile) ([]InputProfile, error) {
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
	if len(profiles) > 16 {
		return nil, bad("input_profile_limit", "At most 16 installation input profiles are supported. Existing profiles were retained.")
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
	profiles, err := ParseInputProfiles([]byte(raw))
	if err != nil || profiles == nil {
		return errors.New("registered input profiles are invalid")
	}
	if _, err = combinedProfiles(s.cfg.InputProfiles, profiles); err != nil {
		return err
	}
	s.registeredProfiles.Store(&profiles)
	return nil
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
	proposed := append(append([]InputProfile(nil), s.registeredInputProfiles()...), frozen)
	if _, err := combinedProfiles(s.cfg.InputProfiles, proposed); err != nil {
		return false, err
	}
	// One atomic, synchronous SQLite write precedes making the profile visible.
	// A retry after an uncertain response can only observe this same definition.
	if _, err := s.db.Exec("INSERT INTO metadata(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", registeredProfilesKey, string(encode(proposed))); err != nil {
		return false, err
	}
	s.registeredProfiles.Store(&proposed)
	return true, nil
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
	for _, key := range []string{"id", "publicKey", "class", "source", "authority", "shape", "adapter", "endpoint"} {
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
