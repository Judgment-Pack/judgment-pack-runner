package runner

import "context"

// The coordinator flips this one source constant only after both real gates
// are source-ready/reviewed, for the uniform final-candidate integration run.
const manualAdmissionReady = true

func ManualAdmissionAvailable() bool {
	return manualAdmissionReady && manualAdmissionPlatformSupported()
}

func (s *Service) validateManualStore() error {
	var schema, raw string
	if s.db.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema) != nil || schema != manualStoreSchema || s.cfg.ExecutorAdmission == nil {
		return errManualProtocol
	}
	if s.db.QueryRow("SELECT value FROM metadata WHERE key='manualExecutionDemand'").Scan(&raw) != nil {
		return errManualProtocol
	}
	var d ManualExecutionDemand
	if manualDecode([]byte(raw), &d) != nil || d != s.cfg.ExecutorAdmission.demand() {
		return errManualProtocol
	}
	marker, err := readManualStateFile(s.executor, manualPrepareMarker, 16384)
	var held manualPrepareRecord
	if err != nil || manualDecode(marker, &held) != nil || held.Version != 1 || !manualHex(held.PrepareID) || !validDigest(held.RequestDigest) || held.Demand != d {
		return errManualProtocol
	}
	for _, query := range []string{"SELECT count(*) FROM triggers", "SELECT count(*) FROM occurrences WHERE state IN ('accepted','preparing','waiting','needs-attention')"} {
		var count int
		if s.db.QueryRow(query).Scan(&count) != nil || count != 0 {
			return manualUnsupported()
		}
	}
	rows, err := s.db.Query("SELECT record FROM runs WHERE state='queued'")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			return errManualProtocol
		}
		r, e := decodeRun([]byte(raw))
		if e != nil || r.manual == nil || r.manual.resource == nil || r.manual.registration == nil || r.Trigger != nil || r.Input.Source != nil || r.Input.Preparation != nil {
			return errManualProtocol
		}
	}
	return rows.Err()
}
func (s *Service) activateManualAuthority() error {
	if s.cfg.ExecutorAdmission == nil {
		return nil
	}
	if len(s.cfg.CloudConnections) != 0 || len(s.cfg.GatewayConnections) != 0 || len(s.cfg.InputProfiles) != 0 || len(s.storedProfiles) != 0 {
		return manualUnsupported()
	}
	signing := ManualSigning{Mode: "off"}
	if s.expectedKey != nil {
		signing = ManualSigning{Mode: "ed25519", PublicKey: hexPublic(s.expectedKey)}
	}
	_, err := s.authority.Activate(context.Background(), ManualActivateRequest{Challenge: s.cfg.ExecutorAdmission.Challenge, Signing: signing})
	return err
}
