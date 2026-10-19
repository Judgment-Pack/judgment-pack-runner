package runner

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
)

func hexPublic(raw []byte) string { return hex.EncodeToString(raw) }

// All retained preimages are immutable strings. The common starter repeats this
// verification after the authority commitment; any change consumes/refuses.
type manualLaunchMaterial struct {
	files     map[string]string
	evidence  bool
	config    Config
	publicKey string
	resource  func() error
}

func (s *Service) prepareGovernedLaunch(q ManualStartRequest, cmd *exec.Cmd, r Release, in Input) (*manualPreparedLaunch, error) {
	if s.authority == nil || s.cfg.ExecutorAdmission == nil || !manualRelease(r) || in.Source != nil || in.Preparation != nil {
		return nil, errManualProtocol
	}
	if verifyManualLease(s.executor) != nil {
		return nil, errManualProtocol
	}
	signing := signingInputs{}
	if s.expectedKey != nil {
		signing = signingInputs{s.cfg.SigningKey, hexPublic(s.expectedKey)}
	}
	payload, err := freezeExecutionPayload(r, in, signing)
	if err != nil || q.Payload.Digest != payload.Digest() || !bytes.Equal(q.Payload.Bytes, payload.Bytes()) {
		return nil, errManualProtocol
	}
	m := &manualLaunchMaterial{files: map[string]string{"pack.json": r.Pack, "jpack.json": r.Config, "jpack.lock.json": r.Lock, "facts.json": string(in.Facts)}, evidence: len(in.Evidence) > 0, config: s.cfg, publicKey: signing.publicKey}
	m.resource = func() error {
		binding, e := s.manualExecutionBinding(q.Resource.JobID)
		if e != nil || manualResourceOf(binding) != q.Resource {
			return errManualProtocol
		}
		return nil
	}
	if m.evidence {
		m.files["evidence.json"] = string(in.Evidence)
	}
	if m.verify(cmd) != nil {
		return nil, errManualProtocol
	}
	p, err := prepareManualLaunch(q, cmd, s.executor)
	if err != nil {
		return nil, err
	}
	p.client = s.authority
	p.material = m
	return p, nil
}

func (m *manualLaunchMaterial) verify(cmd *exec.Cmd) error {
	if m == nil || cmd == nil || !filepath.IsAbs(cmd.Dir) {
		return errManualProtocol
	}
	if checkManualDemand(m.config.ExecutorOwnership, m.config.ExecutorAdmission) != nil {
		return errManualProtocol
	}
	if m.resource == nil || m.resource() != nil {
		return errManualProtocol
	}
	for name, want := range m.files {
		got, err := readLimit(filepath.Join(cmd.Dir, name), MaxBody)
		if err != nil || string(got) != want {
			return errManualProtocol
		}
	}
	if !m.evidence {
		if _, err := os.Lstat(filepath.Join(cmd.Dir, "evidence.json")); !os.IsNotExist(err) {
			return errManualProtocol
		}
	}
	key, err := expectedSigningKey(m.config)
	if err != nil || hexPublic(key) != m.publicKey {
		return errManualProtocol
	}
	return nil
}
