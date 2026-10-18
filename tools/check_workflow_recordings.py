#!/usr/bin/env python3
"""Compare test recordings with a separately provisioned experimental reader.

This harness supplies independently retained test pins and compares measurements.
It contains no workflow transition, preparation or admission implementation.
Inputs are test-controlled recordings and separate held pins, not a general
untrusted bundle loader. This experiment does not claim Spec conformance.
"""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import sys


def digest(raw):
    return 'sha256:' + hashlib.sha256(raw).hexdigest()


def signature(files):
    return json.dumps(files, sort_keys=True, separators=(',', ':'))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--spec', required=True, type=Path)
    ap.add_argument('--recordings', required=True, type=Path)
    ap.add_argument('--output', required=True, type=Path)
    args = ap.parse_args()
    sys.path.insert(0, str(args.spec.resolve()))
    from tools.workflow_reader import Expectations, verify

    results = []
    traces = {}
    comparisons = []
    unavailable = []
    unmaterialized = []
    failures = []
    manifests = sorted(args.recordings.rglob('expected-pins.json'))
    if not manifests:
        ap.error('no recording manifests found')
    for path in manifests:
        expected = json.loads(path.read_text())
        bundle = path.parent / 'bundle'
        files = {str(p.relative_to(bundle)): p.read_bytes()
                 for p in sorted(bundle.rglob('*')) if p.is_file()}
        name = str(path.parent.relative_to(args.recordings))
        # Detect changed artifacts before invoking a verifier. These checks do
        # not supply the verifier's historical candidate maps or byte totals.
        if ({p: digest(b) for p, b in files.items()} != expected['files'] or
                sum(map(len, files.values())) != expected['bytes'] or
                len(files) != expected['fileCount']):
            failures.append(dict(recording=name, error='recording changed since capture'))
            continue
        held = Expectations(expected['workflowDigest'],
                            expected.get('executionDigest', expected['files']['execution.json']),
                            {s['stage']: s['runtimeDigest'] for s in expected['frozen']},
                            (path.parent / 'historical-profiles.json').read_bytes())
        checked = verify(files, held, comparison_trace=True)
        result = dict(recording=name, exit=checked.exit_code, report=checked.report,
                      detail=checked.detail, comparisons=len(checked.trace))
        results.append(result)
        if checked.exit_code != 0:
            failures.append(result)
        elif 'activeOutputs' in expected:
            counts = dict(stages=expected['activeStages'], instances=expected['activeInstances'],
                          outputs=expected['activeOutputs'], selections=expected['activeSelections'])
            if checked.report.get('counts') != counts:
                failures.append(dict(recording=name, error='active report counts disagree',
                                     expected=counts, observed=checked.report.get('counts')))
        if checked.exit_code == 0:
            measured = {p: dict(bytes=len(b), digest=digest(b)) for p, b in files.items()}
            final = [t for t in checked.trace if t['completeCandidate'] and signature(t['files']) == signature(measured)]
            if not final:
                failures.append(dict(recording=name, error='no exact final inventory measurement'))
            elif any(t['retained']['instances'] != expected['retainedInstances'] or
                     t['retained']['outputs'] != expected['retainedOutputs'] for t in final):
                failures.append(dict(recording=name, error='retained count disagreement',
                                     expected=dict(instances=expected['retainedInstances'], outputs=expected['retainedOutputs']),
                                     observed=[t['retained'] for t in final]))
        for trace in checked.trace:
            traces.setdefault(trace['proofDigest'], []).append(trace)

    for path in sorted(args.recordings.rglob('comparisons/*.json')):
        comparison = json.loads(path.read_text())
        if not comparison['RecordingAvailable']:
            unavailable.append(dict(trace=str(path.relative_to(args.recordings)), kind=comparison['Kind'],
                                    cause=comparison['Cause'], reason='no valid portable export for this operation; only last prefix is verified'))
            continue
        proof_digest = digest(base64.b64decode(comparison['Proof'], validate=True))
        # Always compare the chosen carrier. For a refused aggregate candidate,
        # also compare the measured complete acceptance candidate. Unchosen
        # refusal carriers on successful paths need not be constructed by readers.
        names = []
        if comparison['Required'] is not None:
            names.append('Required')
        elif comparison['Acceptance'] is not None or comparison['Accepted']:
            names.append('Acceptance')
        else:
            # A count-only refusal can be decided before an acceptance file
            # map exists. The reader still verifies the recorded refusal;
            # there are no acceptance bytes to compare in this trace.
            unmaterialized.append(dict(trace=str(path.relative_to(args.recordings)),
                                       candidate='Acceptance', cause=comparison['Cause'],
                                       reason='no complete acceptance file map was materialized'))
        if not comparison['Accepted']:
            names.append('Refusal')
        for candidate in names:
            encoded = comparison[candidate]
            if encoded is None:
                failures.append(dict(trace=str(path.relative_to(args.recordings)),
                                     error='required comparison candidate missing', candidate=candidate))
                continue
            files = {p: base64.b64decode(raw, validate=True) for p, raw in encoded.items()}
            measured = {p: dict(bytes=len(raw), digest=digest(raw)) for p, raw in files.items()}
            matches = [t for t in traces.get(proof_digest, [])
                       if t['completeCandidate'] and signature(t['files']) == signature(measured)]
            result = dict(trace=str(path.relative_to(args.recordings)), candidate=candidate,
                          proofDigest=proof_digest, paths=len(files), bytes=sum(map(len, files.values())),
                          matches=len(matches))
            comparisons.append(result)
            if not matches:
                alternatives = traces.get(proof_digest, [])
                result['readerCandidates'] = [dict(candidate=t['candidate'], paths=t['paths'], bytes=t['bytes'])
                                              for t in alternatives]
                if alternatives:
                    closest = min(alternatives, key=lambda t: len(set(t['files']) ^ set(measured)))
                    result['differentPaths'] = [p for p in sorted(set(measured) | set(closest['files']))
                                                if measured.get(p) != closest['files'].get(p)][:12]
                failures.append(result)
    if not comparisons:
        failures.append(dict(error='no available writer candidates were compared'))
    output = dict(recordings=results, comparisons=comparisons, unavailableOperations=unavailable,
                  unmaterializedCandidates=unmaterialized, failures=failures)
    args.output.write_text(json.dumps(output, indent=2, default=str) + '\n')
    print(json.dumps(dict(recordings=len(results), comparisons=len(comparisons), unavailableOperations=len(unavailable),
                          unmaterializedCandidates=len(unmaterialized), failures=len(failures))))
    return int(bool(failures))


if __name__ == '__main__':
    raise SystemExit(main())
