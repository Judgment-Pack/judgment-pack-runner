package runner

// Briefs are derived documents, never evaluation or review authority.
// Wire contract mirrored in Desk and Runner; changes require both conformance tests.
import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type BriefText struct {
	Context     string `json:"context"`
	Findings    string `json:"findings"`
	Uncertainty string `json:"uncertainty"`
	NextAction  string `json:"nextAction"`
}
type BriefRevision struct {
	ID       string          `json:"id"`
	Revision int             `json:"revision"`
	At       string          `json:"at"`
	Snapshot json.RawMessage `json:"snapshot"`
	Text     BriefText       `json:"text"`
	Model    string          `json:"model"`
	Method   string          `json:"method"`
}
type BriefPending struct {
	Token    string          `json:"token"`
	Expires  string          `json:"expires"`
	Snapshot json.RawMessage `json:"snapshot"`
	Model    string          `json:"model"`
}
type BriefHistory struct {
	Revisions []BriefRevision `json:"revisions"`
	Pending   *BriefPending   `json:"pending,omitempty"`
}
type BriefStore struct {
	Version  int                     `json:"version"`
	Subjects map[string]BriefHistory `json:"subjects"`
}
type BriefCommand struct {
	Project          string          `json:"project,omitempty"`
	Action           string          `json:"action"`
	Subject          string          `json:"subject"`
	Owner            string          `json:"owner,omitempty"`
	CaseID           string          `json:"caseId,omitempty"`
	ExpectedRevision int             `json:"expectedRevision"`
	Token            string          `json:"token,omitempty"`
	Snapshot         json.RawMessage `json:"snapshot,omitempty"`
	Model            string          `json:"model,omitempty"`
	Text             BriefText       `json:"text"`
}

func briefCanonical(v json.RawMessage) []byte {
	var x any
	d := json.NewDecoder(bytes.NewReader(v))
	d.UseNumber()
	if d.Decode(&x) != nil {
		return nil
	}
	b, _ := json.Marshal(x)
	return b
}
func sameBriefJSON(a, b json.RawMessage) bool {
	return bytes.Equal(briefCanonical(a), briefCanonical(b))
}
func briefTextValid(t BriefText) bool {
	count := 0
	for _, s := range []string{t.Context, t.Findings, t.Uncertainty, t.NextAction} {
		if strings.TrimSpace(s) == "" || len(s) > 3000 {
			return false
		}
		count += len([]rune(s))
	}
	return count <= 4500
}
func validateBriefs(data []byte) error {
	var d BriefStore
	if json.Unmarshal(data, &d) != nil || d.Version != 1 || d.Subjects == nil || len(d.Subjects) > 1024 {
		return errors.New("Invalid brief storage.")
	}
	for key, h := range d.Subjects {
		if key == "" || len(h.Revisions) > 100 {
			return errors.New("Invalid brief history.")
		}
		for i, b := range h.Revisions {
			if b.Revision != i+1 || b.ID == "" || !json.Valid(b.Snapshot) || !briefTextValid(b.Text) {
				return errors.New("Invalid brief revision.")
			}
		}
		if h.Pending != nil {
			if _, err := time.Parse(time.RFC3339, h.Pending.Expires); err != nil || h.Pending.Token == "" || !json.Valid(h.Pending.Snapshot) {
				return errors.New("Invalid brief reservation.")
			}
		}
	}
	return nil
}
func applyBrief(d *BriefStore, c BriefCommand, at time.Time) error {
	if c.Subject == "" || len(c.Subject) > 1024 {
		return errors.New("Choose a brief subject.")
	}
	h := d.Subjects[c.Subject]
	if h.Revisions == nil {
		h.Revisions = []BriefRevision{}
	}
	// Retrying a completed save is idempotent, including after the lease expired.
	if c.Action == "complete" && c.Token != "" {
		for _, v := range h.Revisions {
			if v.ID == c.Token {
				if v.Text != c.Text {
					return errors.New("This generation was already saved with different text.")
				}
				return nil
			}
		}
	}
	switch c.Action {
	case "begin":
		if len(h.Revisions) != c.ExpectedRevision {
			return errors.New("A newer brief exists. Reload before regenerating.")
		}
		if len(h.Revisions) >= 100 {
			return errors.New("This brief reached its 100 revision limit.")
		}
		if h.Pending != nil {
			expires, _ := time.Parse(time.RFC3339, h.Pending.Expires)
			if at.Before(expires) {
				return errors.New("Brief generation is already in progress. Try again when it finishes or its reservation expires.")
			}
		}
		if !json.Valid(c.Snapshot) || len(c.Snapshot) > 1<<20 || strings.TrimSpace(c.Model) == "" || len(c.Model) > 256 {
			return errors.New("Supply a bounded source snapshot and model.")
		}
		token := make([]byte, 16)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		h.Pending = &BriefPending{Token: hex.EncodeToString(token), Expires: at.Add(10 * time.Minute).Format(time.RFC3339), Snapshot: append(json.RawMessage(nil), c.Snapshot...), Model: c.Model}
	case "complete", "cancel":
		if h.Pending == nil || c.Token == "" || h.Pending.Token != c.Token {
			return errors.New("This brief reservation is no longer current. Reload and try again.")
		}
		if c.Action == "complete" {
			expires, _ := time.Parse(time.RFC3339, h.Pending.Expires)
			if !at.Before(expires) {
				return errors.New("Brief generation expired. The previous brief is unchanged.")
			}
			if !briefTextValid(c.Text) {
				return errors.New("The brief must contain four concise sections, within 4,500 characters.")
			}
			h.Revisions = append(h.Revisions, BriefRevision{ID: c.Token, Revision: len(h.Revisions) + 1, At: at.UTC().Format(time.RFC3339), Snapshot: h.Pending.Snapshot, Text: c.Text, Model: h.Pending.Model, Method: "ai-summary-v1"})
		}
		h.Pending = nil
	default:
		return fmt.Errorf("Unsupported brief action: %s", c.Action)
	}
	if d.Subjects == nil {
		d.Subjects = map[string]BriefHistory{}
	}
	d.Subjects[c.Subject] = h
	return nil
}
