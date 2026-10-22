package runner

// Optional test evidence from actual candidate construction. This is not a
// reader, a baseline assertion, portable proof member or acquisition authority.
// Maps own complete reconstructed bytes; traces are never persisted as state.
type workflowV2Comparison struct {
	Kind                          string
	Epoch                         int
	Instance                      string
	Cause                         string
	Accepted                      bool
	RecordingAvailable            bool
	Proof                         []byte
	Acceptance, Refusal, Required map[string][]byte
}

func (w workflowV2Inventory) traceDecision(kind, instance, cause string, accepted, available bool, a, r *workflowV2Inventory) {
	if w.Trace == nil {
		return
	}
	e, _ := w.Proof.last()
	proof, _ := w.Proof.bytes()
	c := workflowV2Comparison{Kind: kind, Epoch: e.Number, Instance: instance, Cause: cause, Accepted: accepted, RecordingAvailable: available, Proof: proof}
	if a != nil {
		c.Acceptance, _ = a.recordingFiles()
	}
	if r != nil {
		c.Refusal, _ = r.recordingFiles()
	}
	w.Trace(c)
}
func (w workflowV2Inventory) traceRequired(kind string, err error) {
	if w.Trace == nil {
		return
	}
	e, _ := w.Proof.last()
	proof, _ := w.Proof.bytes()
	files, _ := w.recordingFiles()
	w.Trace(workflowV2Comparison{Kind: kind, Epoch: e.Number, Cause: "required-carrier", Accepted: err == nil, RecordingAvailable: err == nil, Proof: proof, Required: files})
}
