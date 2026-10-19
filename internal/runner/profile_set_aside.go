package runner

// A registered profile an earlier Runner admitted and this one's form refuses
// on its own, such as a scoped page profile with an entry RFC 0018 round 16
// refuses (runner #152), is set aside when the store opens (runner #154). It
// stays in the store's row byte for byte, its id stays taken and it counts
// toward the bound of 16; it is reported with its reason, at open and in the
// listing of the installation's profiles; and it is never bound. Every other
// registered profile loads and works as before. What fails as a set, not as
// one profile, still refuses to open the store (loadRegisteredProfiles), and
// a configured profile the form refuses still refuses to start (Open).
//
// A set-aside profile never enters the profiles a binding is resolved
// against (inputProfiles). So that a binding to one is refused by its name
// rather than as a profile the installation does not hold, each place
// Runner resolves the installation's profiles first refuses one that names
// it (refuseSetAside): planning and preparation, a preview, an import, job
// creation and upgrade, and a run's submission and its start. A workflow
// route, when there is one, binds through these.

import (
	"encoding/json"
	"fmt"
	"time"
)

// codeSetAside is the code a binding to a set-aside profile is refused with.
const codeSetAside = "input_profile_set_aside"

// runProfileSetAside is the Reason of a run whose release names a profile
// that was set aside after the run was queued: it ends without starting.
const runProfileSetAside = "profile-set-aside"

// SetAsideProfile is a registered profile set aside when the store opened:
// its id and the refusal of this Runner's form, with its digest and its bytes
// as the store holds them.
type SetAsideProfile struct {
	ID, Reason string
	digest     string
	stored     json.RawMessage
}

// String is the line Runner reports the profile with when it opens its store.
func (p SetAsideProfile) String() string {
	return fmt.Sprintf("the registered input profile %q is set aside, kept and never bound: %s", p.ID, p.Reason)
}

// refusal is the refusal of a binding that names the profile.
func (p SetAsideProfile) refusal() error {
	return bad(codeSetAside, fmt.Sprintf("The input profile %s was set aside when Runner opened its store, and is never bound: %s. Its id stays taken. Register the profile anew under another id and review a new binding.", p.ID, p.Reason))
}

// SetAside is each registered profile set aside when the store opened, in
// the order of the store's row.
func (s *Service) SetAside() []SetAsideProfile {
	return append([]SetAsideProfile(nil), s.setAside...)
}

// refuseSetAsideIDs refuses the first of ids that names a set-aside profile.
func (s *Service) refuseSetAsideIDs(ids []string) error {
	for _, id := range ids {
		for _, p := range s.setAside {
			if p.ID == id {
				return p.refusal()
			}
		}
	}
	return nil
}

// refuseSetAside refuses an input that names a set-aside profile: in its
// mapping, or in its import's binding document.
func (s *Service) refuseSetAside(i Input) error {
	if i.Source == nil {
		return nil
	}
	if e := s.refuseSetAsideMapping(&i.Source.Mapping); e != nil {
		return e
	}
	if i.Source.Import != nil {
		return s.refuseSetAsideIDs(bindingProfileIDs(i.Source.Import.Binding))
	}
	return nil
}

// refuseSetAsideMapping refuses a mapping that names a set-aside profile.
func (s *Service) refuseSetAsideMapping(m *InputMapping) error {
	if m == nil {
		return nil
	}
	return s.refuseSetAsideIDs(mappingProfileIDs(*m))
}

// nextInput is nextInput under the installation's profiles, refusing first an
// input that names a set-aside profile.
func (s *Service) nextInput(i Input, at time.Time) (InputPlan, error) {
	if s.cfg.ExecutorAdmission != nil {
		return InputPlan{}, manualUnsupported()
	}
	if e := s.refuseSetAside(i); e != nil {
		return InputPlan{}, e
	}
	return nextInput(i, s.inputProfiles(), at)
}

// mappingProfileIDs is each profile id a mapping's sources name, read
// leniently, before any form: a source's profile, and a research source's web
// profiles. A source of another form is refused for that later.
func mappingProfileIDs(m InputMapping) []string {
	var ids []string
	for _, source := range m.Sources {
		if source.Profile != "" {
			ids = append(ids, source.Profile)
		}
		ids = append(ids, webProfileIDs(source.WebProfiles)...)
	}
	return ids
}

// bindingProfileIDs is each profile id a binding document names, read
// leniently, before any form, in the order of its sources' names: each
// source's profile and, for a research source, its web profiles.
func bindingProfileIDs(raw json.RawMessage) []string {
	var binding struct {
		Sources map[string]json.RawMessage `json:"sources"`
	}
	_ = json.Unmarshal(raw, &binding)
	var ids []string
	for _, name := range sortedKeys(binding.Sources) {
		var entry struct {
			Profile     any             `json:"profile"`
			WebProfiles json.RawMessage `json:"webProfiles"`
		}
		_ = json.Unmarshal(binding.Sources[name], &entry)
		if id, ok := entry.Profile.(string); ok && id != "" {
			ids = append(ids, id)
		}
		ids = append(ids, webProfileIDs(entry.WebProfiles)...)
	}
	return ids
}

// webProfileIDs is each profile id a research source's web profiles name,
// read leniently, in the order of their capabilities' names.
func webProfileIDs(raw json.RawMessage) []string {
	var web map[string]json.RawMessage
	_ = json.Unmarshal(raw, &web)
	var ids []string
	for _, capability := range sortedKeys(web) {
		var assignment struct {
			Profile any `json:"profile"`
		}
		_ = json.Unmarshal(web[capability], &assignment)
		if id, ok := assignment.Profile.(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
