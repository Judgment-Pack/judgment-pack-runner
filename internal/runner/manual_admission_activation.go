package runner

import "context"

// The coordinator flips this one source constant only after both real gates
// are source-ready/reviewed, for the uniform final-candidate integration run.
const manualAdmissionReady = true

func ManualAdmissionAvailable() bool {
	return manualAdmissionReady && manualAdmissionPlatformSupported()
}

func (s *Service) validateManualStore() error {
	if s.manual == nil {
		return errManualProtocol
	}
	schema, err := s.manual.manualValue("schema")
	if err != nil || schema != manualStoreSchema || s.cfg.ExecutorAdmission == nil {
		return errManualProtocol
	}
	raw, err := s.manual.manualValue("manualExecutionDemand")
	if err != nil {
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
	for _, question := range []storeQuestion{askTriggers, askOpenOccurrences} {
		if count, e := s.manual.manualCount(question); e != nil || count != 0 {
			return manualUnsupported()
		}
	}
	return s.manual.queuedRunRecords(func(raw []byte, scanned error) error {
		if scanned != nil {
			return errManualProtocol
		}
		r, e := decodeRun(raw)
		if e != nil || r.manual == nil || r.manual.resource == nil || r.manual.registration == nil || r.Trigger != nil || r.Input.Source != nil || r.Input.Preparation != nil {
			return errManualProtocol
		}
		return nil
	})
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
