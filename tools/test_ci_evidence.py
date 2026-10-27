#!/usr/bin/env python3
"""The release evidence gate's configuration agrees with the workflows and the manifest.

Usage: python3 tools/test_ci_evidence.py

.github/ci-evidence.json names the CI jobs and matrix legs a release takes its evidence from, the
checks each of them runs, the files whose identity the evidence carries and the toolchain each ran
with. The gate refuses a release whose evidence disagrees with that (docs/releasing.md, "CI at the
tag"); these tests fail the pull request that makes the two drift apart instead. They also hold
scripts/dev/release_evidence.py to the development kit's lib/release_evidence.py of the release its
line 2 names.

The workflows are read as text, in the shapes this repository writes them. This is not a YAML
reader: a workflow rewritten in another shape fails here until the reading follows it.
"""
import hashlib
import os
import tempfile
import json
from pathlib import Path
import re
import subprocess
import sys
import unittest

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parent.parent
CONFIG = '.github/ci-evidence.json'
GATE = 'scripts/dev/release_evidence.py'
RELEASE = '.github/workflows/release.yml'

# The SHA-256 of the kit's lib/release_evidence.py at each release this repository has vendored it
# from. A re-vendor row adds the release it moves to.
KIT_GATE = {
    'v0.10.0': 'd44af3df07f64036013ba9967a54cf799cec678068f0937395d6a2eef370e0e8',
    'v0.11.0': 'f272b7a43272bac31bb9b0c37a583d9c40d376972b04c812e576bd82c1ef18e3',
}
RUNNER_OS = {'ubuntu': 'Linux', 'macos': 'macOS'}
GOOS = {'Linux': 'linux', 'macOS': 'darwin'}
GOARCH = {'X64': 'amd64', 'ARM64': 'arm64'}
ON_MAIN_PUSH = "github.ref == 'refs/heads/main' && github.event_name == 'push'"
CHECK_LOG = '| tee "${{ runner.temp }}/checks.log"'


def read(path):
    return (ROOT / path).read_text(encoding='utf-8')


def config():
    return json.loads(read(CONFIG))


def manifest_checks():
    return [row['name'] for row in json.loads(read('dev.json'))['checks']]


def jobs(text):
    """Each job of a workflow: its key under jobs: and the lines of its block."""
    lines = text.split('\n')
    result, key = {}, None
    for line in lines[lines.index('jobs:') + 1:]:
        found = re.fullmatch(r'  ([A-Za-z0-9_-]+):', line)
        if found:
            key = found.group(1)
            result[key] = []
        elif key is not None:
            result[key].append(line)
    return {key: '\n'.join(block) for key, block in result.items()}


def one(values, what):
    if len(values) != 1:
        raise AssertionError('expected one %s, found %r' % (what, values))
    return values[0]


def steps(block):
    """The steps of a job, each as the text from its leading dash to the next step."""
    return ['- ' + part for part in re.split(r'^      - ', block, flags=re.M)[1:]]


def legs(key, block):
    """The display name, labels and matrix value of each leg of a job."""
    name = one(re.findall(r'^    name: (.+)$', block, re.M), key + ' name')
    runs_on = one(re.findall(r'^    runs-on: (.+)$', block, re.M), key + ' runs-on')
    matrix = re.findall(r'^        os: \[(.*)\]$', block, re.M)
    values = [value.strip() for value in one(matrix, key + ' matrix').split(',')] if matrix else [None]
    result = []
    for value in values:
        def expand(text, value=value):
            return text if value is None else text.replace('${{ matrix.os }}', value)
        result.append({'job_id': key, 'name': expand(name), 'labels': [expand(runs_on)], 'expand': expand})
    return result


def ci_legs():
    workflow = config()['workflows']['ci']
    return {leg['name']: leg for key, block in jobs(read(workflow['path'])).items()
            for leg in legs(key, block)}


def push_selection(block):
    """The checks a push to main runs in a job: its check.sh call that is not --changed."""
    calls = [call for call in re.findall(r'scripts/dev/check\.sh --all (.*)$', block, re.M)
             if '--changed' not in call]
    words = one(calls, 'check.sh --all call on a push').split()
    everything = manifest_checks()
    if words[0] == '--only':
        named = words[1].split(',')
        return [name for name in everything if name in named]
    if words[0] == '--skip':
        named = words[1].split(',')
        return [name for name in everything if name not in named]
    raise AssertionError('check.sh --all is followed by %r' % words[0])


def go_versions(text):
    return re.findall(r"^ *go-version: *['\"]?([0-9][0-9.x]*)['\"]? *$", text, re.M)



def unit_partition_trace(script, *, script_text=None, fail_call=None, conformance='sqlite,memory'):
    """Execute only shell wiring: the fake Go records selections, never builds."""
    with tempfile.TemporaryDirectory(prefix='runner-unit-selection-') as temporary:
        directory = Path(temporary)
        trace = directory / 'trace.jsonl'
        if script_text is not None:
            script = directory / 'unit-tests.sh'
            script.write_text(script_text, encoding='utf-8')
        fake = directory / 'go'
        fake.write_text('#!' + sys.executable + '\n' + r'''
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
with Path(os.environ['UNIT_SELECTION_TRACE']).open('a') as output:
    output.write(json.dumps({'args': args, 'env': {key: os.environ.get(key) for key in (
        'JPACK_TEST_BIN', 'JPACK_RETAINED_TEST_BIN', 'JPACK_EARLIER_TEST_BIN',
        'JPACK_GATEWAY_TEST_BIN', 'JPACK_MCP_ADAPTER_TEST_BIN', 'JPACK_OLD_RUNNER_TEST_BIN')}}) + '\n')
number = len(Path(os.environ['UNIT_SELECTION_TRACE']).read_text().splitlines())
if str(number) == os.environ.get('UNIT_SELECTION_FAIL_CALL'):
    print('fake Go failure at call %d' % number)
    sys.exit(7)
if '^TestStoreKindsConform$' in args:
    for kind in os.environ['UNIT_SELECTION_CONFORMANCE'].split(','):
        if kind:
            print('--- PASS: TestStoreKindsConform/%s (0.00s)' % kind)
''', encoding='utf-8')
        fake.chmod(0o700)
        env = dict(os.environ, PATH=str(directory) + os.pathsep + os.environ['PATH'],
                   UNIT_SELECTION_TRACE=str(trace),
                   UNIT_SELECTION_FAIL_CALL=str(fail_call),
                   UNIT_SELECTION_CONFORMANCE=conformance)
        process = subprocess.run(['bash', str(script)], cwd=ROOT, env=env,
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        calls = [json.loads(line) for line in trace.read_text().splitlines()]
        return process, calls


class Jobs(unittest.TestCase):
    def test_every_job_and_leg_of_ci_is_required_by_its_display_name(self):
        workflow = config()['workflows']['ci']
        self.assertTrue((ROOT / workflow['path']).is_file(), workflow['path'])
        text = read(workflow['path'])
        self.assertEqual(text.split('\n')[0], 'name: ' + workflow['name'])
        # On a push to main nothing is skipped, and a skip would have to be named here on purpose.
        self.assertNotIn('skipped_jobs', workflow)
        found = ci_legs()
        self.assertEqual(sorted(spec['name'] for spec in workflow['jobs'].values()), sorted(found))
        for job_key, spec in workflow['jobs'].items():
            with self.subTest(job=job_key):
                leg = found[spec['name']]
                self.assertEqual(spec['job_id'], leg['job_id'])
                self.assertEqual(spec['labels'], leg['labels'])

    def test_only_a_push_to_the_trusted_branch_is_evidence(self):
        settings = config()
        workflow = settings['workflows']['ci']
        self.assertEqual(workflow['events'], ['push'])
        self.assertIn('\n  push:\n    branches: [%s]\n' % settings['branch'], read(workflow['path']))

    def test_each_leg_records_its_evidence_under_the_name_the_gate_reads(self):
        workflow = config()['workflows']['ci']
        found = ci_legs()
        blocks = jobs(read(workflow['path']))
        for job_key, spec in workflow['jobs'].items():
            with self.subTest(job=job_key):
                leg = found[spec['name']]
                listed = steps(blocks[leg['job_id']])
                captures = [index for index, step in enumerate(listed)
                            if 'scripts/dev/release_evidence.py capture' in step]
                capture = listed[one(captures, 'capture step')]
                self.assertIn('\n        if: %s\n' % ON_MAIN_PUSH, capture)
                self.assertIn('run: python3 scripts/dev/release_evidence.py capture --workflow ci '
                              '--job %s --check-log "${{ runner.temp }}/checks.log" '
                              '--output "${{ runner.temp }}/evidence.json"\n'
                              % job_key, leg['expand'](capture))
                upload = listed[captures[0] + 1]
                self.assertIn('\n        if: %s\n' % ON_MAIN_PUSH, upload)
                self.assertRegex(upload, r'\n        uses: actions/upload-artifact@[0-9a-f]{40} ')
                self.assertIn('\n          name: ci-evidence-ci-%s-${{ github.run_attempt }}\n'
                              '          path: ${{ runner.temp }}/evidence.json\n'
                              '          if-no-files-found: error\n' % job_key, leg['expand'](upload))
                # The checks' summary is what the capture reads, and a failed check fails its step.
                checks = [step for step in listed[:captures[0]] if 'scripts/dev/check.sh --all' in step]
                self.assertTrue(checks)
                for step in checks:
                    self.assertRegex(step, r'run: \|\n +set -o pipefail\n')
                    for call in re.findall(r'scripts/dev/check\.sh --all .*$', step, re.M):
                        self.assertTrue(call.endswith(CHECK_LOG), call)


class Checks(unittest.TestCase):
    def test_each_leg_lists_the_checks_its_push_run_reports(self):
        workflow = config()['workflows']['ci']
        found = ci_legs()
        blocks = jobs(read(workflow['path']))
        covered = set()
        for job_key, spec in workflow['jobs'].items():
            with self.subTest(job=job_key):
                self.assertNotIn('optional_skips', spec)
                self.assertEqual(spec['checks'], push_selection(blocks[found[spec['name']]['job_id']]))
                covered.update(spec['checks'])
        self.assertEqual(sorted(covered), sorted(manifest_checks()))

    def test_the_pins_and_the_must_run_tests_are_in_every_runner_leg(self):
        """A step script that fetches a pinned companion, or fails when a named test did not run and
        pass, is an input the evidence carries, and its check is in every Runner leg."""
        settings = config()
        manifest = json.loads(read('dev.json'))['checks']
        named = []
        for row in manifest:
            script = re.fullmatch(r'(scripts/checks/[A-Za-z0-9_-]+\.sh)', row['run'])
            if script and re.search(r'^fetch_pin |did not run and pass', read(script.group(1)), re.M):
                named.append((row['name'], script.group(1)))
        # Named too: a script that lost its must-run assertions would drop out of the derivation.
        self.assertTrue({'build-pinned', 'manual-gate', 'unit-tests', 'chain-tests'} <= {name for name, _ in named},
                        named)
        runner = [spec for spec in settings['workflows']['ci']['jobs'].values() if spec['job_id'] == 'check']
        self.assertEqual(len(runner), 2)
        for name, script in named:
            with self.subTest(check=name):
                self.assertIn(script, settings['inputs'])
                for spec in runner:
                    self.assertIn(name, spec['checks'])

    def assert_unit_partition_calls(self, calls):
        main = [call for call in calls if '^TestStoreKindsConform$' not in call['args']]
        # Check every current top-level case and handwritten representatives of
        # future families, alphabet boundaries, examples and fuzz seed corpus.
        sources = subprocess.check_output(
            ['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard', '--', '*_test.go'],
            cwd=ROOT, text=True).split('\0')
        names = {name for source in sources if source
                 for name in re.findall(r'^func ((?:Test|Example|Fuzz)\w*)\(',
                                        (ROOT / source).read_text(encoding='utf-8'), re.M)
                 if name != 'TestMain'}
        names.update({'TestColumnRoleFuture', 'TestColumnFuture', 'TestRC2Future',
                      'TestWorkflowFuture', 'TestAFuture', 'TestMFuture', 'TestNFuture',
                      'TestZFuture', 'TestFutureOrdinary', 'TestΩFuture',
                      'ExampleFuture', 'ExampleColumn', 'FuzzFuture', 'FuzzRC2'})
        groups = []
        for call in main:
            args = call['args']
            self.assertEqual(args[:2], ['test', '-race'], 'unit partition lost race instrumentation')
            self.assertEqual(args[-1], './...')
            self.assertFalse(any(arg.startswith(('-timeout', '-count', '-fuzz')) for arg in args),
                             'unit partitions changed default timeout, cache or fuzz execution')
            options = args[2:-1]
            self.assertEqual(len(options) % 2, 0, 'unit partition has an unexpected option')
            self.assertTrue(all(option in ('-run', '-skip') for option in options[::2]),
                            'unit partition has an unexpected option')
            self.assertEqual(len(options[::2]), len(set(options[::2])),
                             'unit partition repeats an option')
            matches = set(names)
            if '-run' in args:
                matches &= {name for name in names if re.search(args[args.index('-run') + 1], name)}
            if '-skip' in args:
                matches -= {name for name in names if re.search(args[args.index('-skip') + 1], name)}
            groups.append(matches)
            for key, binary in {'JPACK_TEST_BIN': 'jpack', 'JPACK_RETAINED_TEST_BIN': 'jpack-retained',
                                'JPACK_EARLIER_TEST_BIN': 'jpack-earlier', 'JPACK_GATEWAY_TEST_BIN': 'gateway',
                                'JPACK_MCP_ADAPTER_TEST_BIN': 'adapter-mcp', 'JPACK_OLD_RUNNER_TEST_BIN': 'jpack-runner-old'}.items():
                self.assertEqual(call['env'][key], str(ROOT / '.ci/bin' / binary), key)
        self.assertEqual(set().union(*groups), names, 'unit partitions omit cases')
        for index, group in enumerate(groups):
            for other in groups[index + 1:]:
                self.assertFalse(group & other, 'unit partitions repeat cases')
        self.assertEqual(len(groups), 5)
        for name in ('ExampleFuture', 'ExampleColumn', 'FuzzFuture', 'FuzzRC2'):
            self.assertIn(name, groups[-1])
        conformance = [call for call in calls if '^TestStoreKindsConform$' in call['args']]
        self.assertEqual(len(conformance), 1)
        self.assertEqual(conformance[0]['args'],
                         ['test', '-race', '-count=1', '-v', '-run',
                          '^TestStoreKindsConform$', './internal/runner'])

    def test_unit_selections_are_complementary_and_keep_pinned_environments(self):
        process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh')
        self.assertEqual(process.returncode, 0, process.stdout)
        self.assert_unit_partition_calls(calls)
        unit = [row for row in json.loads(read('dev.json'))['checks'] if row['name'] == 'unit-tests']
        self.assertEqual(len(unit), 1)
        self.assertEqual(unit[0]['timeout'], 3000)

    def test_unit_selection_controls_catch_omission_overlap_and_changed_execution(self):
        script = read('scripts/checks/unit-tests.sh')
        mutations = [
            ("run_unit -run '^TestWorkflow'", ': # omitted workflow partition', 'unit partitions omit cases'),
            ("'^(TestColumn|TestRC2|TestWorkflow|Test[A-M])'",
             "'^(TestColumn|TestRC2|Test[A-M])'", 'unit partitions repeat cases'),
            ('go test -race "$@"', 'go test "$@"', 'unit partition lost race instrumentation'),
            ('go test -race "$@"', 'go test -race -timeout=700s "$@"', 'unit partitions changed default timeout'),
            ('JPACK_TEST_BIN="$bin/jpack"', 'JPACK_TEST_BIN="$bin/wrong"', 'JPACK_TEST_BIN'),
        ]
        for before, after, message in mutations:
            with self.subTest(safeguard=message):
                self.assertEqual(script.count(before), 1, before)
                process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh',
                                                       script_text=script.replace(before, after))
                self.assertEqual(process.returncode, 0, process.stdout)
                with self.assertRaisesRegex(AssertionError, message):
                    self.assert_unit_partition_calls(calls)

    def test_unit_script_propagates_each_partition_and_conformance_failure(self):
        for failed in range(1, 7):
            with self.subTest(call=failed):
                process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh', fail_call=failed)
                self.assertNotEqual(process.returncode, 0)
                self.assertEqual(len(calls), failed, 'script continued after Go failed')
                self.assertIn('fake Go failure at call %d' % failed, process.stdout)

    def test_unit_script_requires_both_actual_store_conformance_kinds(self):
        for kinds, missing in [('sqlite', 'memory'), ('memory', 'sqlite'), ('', 'sqlite')]:
            with self.subTest(kinds=kinds):
                process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh', conformance=kinds)
                self.assertNotEqual(process.returncode, 0)
                self.assertEqual(len(calls), 6)
                self.assertIn('TestStoreKindsConform/%s did not run and pass' % missing, process.stdout)

    def test_every_input_is_a_tracked_file(self):
        inputs = config()['inputs']
        self.assertTrue(inputs)
        for path in inputs:
            with self.subTest(path=path):
                self.assertTrue((ROOT / path).is_file(), path)
                tracked = subprocess.run(['git', 'ls-files', '--error-unmatch', '--', path], cwd=ROOT,
                                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                         stderr=subprocess.DEVNULL)
                self.assertEqual(tracked.returncode, 0, path + ' is not tracked')


class Toolchain(unittest.TestCase):
    def test_every_leg_ran_the_release_toolchain(self):
        settings = config()
        release = sorted(set(go_versions(read(RELEASE))))
        self.assertEqual(len(release), 1, release)
        version = release[0]
        blocks = jobs(read(settings['workflows']['ci']['path']))
        for key, block in blocks.items():
            with self.subTest(job=key):
                self.assertEqual(go_versions(block), [version])
        found = ci_legs()
        for job_key, spec in settings['workflows']['ci']['jobs'].items():
            with self.subTest(leg=job_key):
                profile = settings['profiles'][spec['profile']]
                platform = profile['platform']
                label = one(found[spec['name']]['labels'], 'label')
                self.assertEqual(RUNNER_OS[label.split('-')[0]], platform['RUNNER_OS'])
                self.assertEqual(profile['pins']['go'], 'go version go%s %s/%s' % (
                    version, GOOS[platform['RUNNER_OS']], GOARCH[platform['RUNNER_ARCH']]))
                # A leg the release job cannot compare with its own runner is held to its image family.
                if profile['compare'] == 'pins':
                    self.assertRegex(platform.get('ImageOS', ''), r'^[a-z]+[0-9]+$')

    def test_a_leg_compared_with_the_release_runner_runs_where_the_release_job_does(self):
        settings = config()
        verify = jobs(read(RELEASE))['verify']
        self.assertIn('\n    runs-on: ubuntu-latest\n', verify)
        for job_key, spec in settings['workflows']['ci']['jobs'].items():
            if settings['profiles'][spec['profile']]['compare'] == 'release-runner':
                with self.subTest(leg=job_key):
                    self.assertEqual(spec['labels'], ['ubuntu-latest'])


class Release(unittest.TestCase):
    def test_the_release_takes_the_evidence_and_does_not_call_ci_again(self):
        text = read(RELEASE)
        self.assertNotIn('.github/workflows/ci.yml', text)
        found = jobs(text)
        verify = found['verify']
        self.assertTrue(verify.startswith('    name: CI at the tag\n    needs: admit\n'), verify[:80])
        self.assertIn('\n      contents: read\n', verify)
        self.assertIn('\n      actions: read\n', verify)
        listed = steps(verify)
        self.assertEqual(len(listed), 4)
        self.assertIn('ref: ${{ github.sha }}', listed[0])
        self.assertIn('uses: actions/setup-go@', listed[1])
        self.assertIn('run: python3 scripts/dev/release_evidence.py verify '
                      '--output "${{ runner.temp }}/release-evidence.json"\n', listed[2])
        # The decision record is kept on a refusal too: the upload runs unless the run was cancelled.
        self.assertIn('- name: Keep the decision record\n        if: ${{ !cancelled() }}\n'
                      '        uses: actions/upload-artifact@', listed[3])
        self.assertIn('\n          name: release-evidence-${{ github.run_attempt }}\n'
                      '          path: ${{ runner.temp }}/release-evidence.json\n'
                      '          if-no-files-found: error\n', listed[3] + '\n')
        self.assertIn('\n      - verify\n', found['package'])


class Vendored(unittest.TestCase):
    def test_the_gate_is_the_kits_of_the_release_line_2_names(self):
        lines = (ROOT / GATE).read_bytes().split(b'\n')
        found = re.fullmatch(r'# from judgment-pack-devkit (v[0-9]+\.[0-9]+\.[0-9]+) \(lib/release_evidence\.py\)',
                             lines[1].decode('utf-8'))
        self.assertIsNotNone(found, lines[1])
        release = found.group(1)
        # Re-vendored with the other scripts, from one kit release.
        runner = read('scripts/dev/check.sh').split('\n')[1]
        self.assertEqual(runner, '# from judgment-pack-devkit %s (templates/check.sh)' % release)
        self.assertIn(release, KIT_GATE, 'no digest of the kit release %s' % release)
        self.assertEqual(hashlib.sha256(b'\n'.join(lines[:1] + lines[2:])).hexdigest(), KIT_GATE[release],
                         '%s is not the kit\'s lib/release_evidence.py of %s after line 2' % (GATE, release))


if __name__ == '__main__':
    unittest.main()
