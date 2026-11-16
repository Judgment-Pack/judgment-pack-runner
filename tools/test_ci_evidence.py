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
import platform
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


MATRIX_AXES = ('os', 'shard')


def legs(key, block):
    """The display name, labels and matrix values of each leg of a job: one leg for each
    combination of the values of its matrix axes (os, and shard for the unit-test shards)."""
    name = one(re.findall(r'^    name: (.+)$', block, re.M), key + ' name')
    runs_on = one(re.findall(r'^    runs-on: (.+)$', block, re.M), key + ' runs-on')
    combinations = [{}]
    for axis in MATRIX_AXES:
        matrix = re.findall(r'^        %s: \[(.*)\]$' % axis, block, re.M)
        if matrix:
            values = [value.strip() for value in one(matrix, key + ' matrix ' + axis).split(',')]
            combinations = [dict(combination, **{axis: value})
                            for combination in combinations for value in values]
    result = []
    for combination in combinations:
        def expand(text, combination=combination):
            for axis, value in combination.items():
                text = text.replace('${{ matrix.%s }}' % axis, value)
            return text
        result.append({'job_id': key, 'name': expand(name), 'labels': [expand(runs_on)], 'expand': expand,
                       'matrix': combination})
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



def unit_partition_trace(script, *, script_text=None, fail_call=None, fail_args=None, jobs=None,
                         conformance='sqlite,memory,twice', shard=None):
    """Execute only shell wiring: the fake Go records selections, never builds.

    jobs sets JPACK_UNIT_TEST_JOBS (None leaves the script's default) and shard sets
    JPACK_UNIT_SHARD (None runs every group, as a local run does). Call numbers
    follow the order in which Go starts, so fail_call is exact only with jobs='1';
    fail_args fails the call with exactly these arguments, whatever its number."""
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
        'JPACK_TEST_BIN', 'JPACK_RETAINED_TEST_BIN', 'JPACK_COVERS_TEST_BIN', 'JPACK_CORE050_TEST_BIN',
        'JPACK_EARLIER_TEST_BIN', 'JPACK_GATEWAY_TEST_BIN', 'JPACK_MCP_ADAPTER_TEST_BIN',
        'JPACK_OLD_RUNNER_TEST_BIN')}}) + '\n')
number = len(Path(os.environ['UNIT_SELECTION_TRACE']).read_text().splitlines())
if str(number) == os.environ.get('UNIT_SELECTION_FAIL_CALL') or args == json.loads(os.environ['UNIT_SELECTION_FAIL_ARGS']):
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
                   UNIT_SELECTION_FAIL_ARGS=json.dumps(fail_args),
                   UNIT_SELECTION_CONFORMANCE=conformance)
        env.pop('JPACK_UNIT_TEST_JOBS', None)
        env.pop('JPACK_UNIT_SHARD', None)
        if jobs is not None:
            env['JPACK_UNIT_TEST_JOBS'] = jobs
        if shard is not None:
            env['JPACK_UNIT_SHARD'] = shard
        process = subprocess.run(['bash', str(script)], cwd=ROOT, env=env,
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        calls = [json.loads(line) for line in trace.read_text().splitlines()] if trace.exists() else []
        return process, calls


UNIT_SCRIPT = 'scripts/checks/unit-tests.sh'
UNIT_TABLE = re.compile(r"^unit_shards=\(('[^']*'(?: '[^']*')*)\)$", re.M)


def unit_shard_rows(script):
    """The rows of unit-tests.sh's shard table, each the groups (and store) one shard runs."""
    return re.findall(r"'([^']*)'", one(UNIT_TABLE.findall(script), 'shard table'))


def with_unit_shard_rows(script, rows):
    return UNIT_TABLE.sub("unit_shards=(%s)" % ' '.join("'%s'" % row for row in rows), script, count=1)


def without_unit_group(script, group):
    """The script with group (a number) left out of its shard table: what a script that has one
    group fewer needs for its table to name every group once."""
    rows = [' '.join(item for item in row.split() if item != str(group)) for row in unit_shard_rows(script)]
    return with_unit_shard_rows(script, rows)


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
        legs = settings['workflows']['ci']['jobs'].values()
        runner = [spec for spec in legs if spec['job_id'] == 'check']
        self.assertEqual(len(runner), 2)
        for spec in runner:
            # unit-tests runs in the Unit tests shards of the Runner leg's system, not in the leg: each
            # check is in the leg or in every shard of its system, and unit-tests in every shard.
            # That the shards of a system run every group is test_the_unit_shards_of_ci_run_every_group_once.
            shards = [shard for shard in legs if shard['job_id'] == 'unit' and shard['labels'] == spec['labels']]
            self.assertTrue(shards, spec['name'])
            in_every_shard = set.intersection(*(set(shard['checks']) for shard in shards))
            self.assertIn('unit-tests', in_every_shard)
            self.assertNotIn('unit-tests', spec['checks'])
            for name, script in named:
                with self.subTest(leg=spec['name'], check=name):
                    self.assertIn(script, settings['inputs'])
                    self.assertIn(name, set(spec['checks']) | in_every_shard)
                    # The pinned binaries are built where the tests that run them run.
                    if name != 'build-pinned' and name in in_every_shard:
                        self.assertIn('build-pinned', in_every_shard)

    def test_a_pull_request_or_merge_group_selects_only_checks_its_job_runs_on_a_push(self):
        """A job's --changed calls leave out exactly what its push call leaves out, so that a pull
        request never runs a check in a job that does not run it on main (a Unit tests shard running
        go-vet, say), and never skips one that job owns."""
        everything = manifest_checks()
        found = 0
        for key, block in jobs(read(config()['workflows']['ci']['path'])).items():
            calls = re.findall(r'scripts/dev/check\.sh --all --changed \S+ (.*)$', block, re.M)
            if not calls:
                continue
            with self.subTest(job=key):
                self.assertEqual(len(calls), 2, 'one for a pull request and one for a merge group')
                for call in calls:
                    words = call.split()
                    self.assertEqual(words[0], '--skip', call)
                    left_out = words[1].split(',')
                    self.assertEqual([name for name in everything if name not in left_out], push_selection(block))
                found += 1
        self.assertEqual(found, 2, 'the Runner and Unit tests jobs')

    def test_a_runner_leg_fails_unless_every_unit_shard_succeeded(self):
        """The Runner legs are the required names: one that a failed shard skipped would count as
        passing, so they run after the shards whatever happened, and fail first unless all
        succeeded."""
        block = jobs(read(config()['workflows']['ci']['path']))['check']
        self.assertIn('\n    needs: unit\n    if: ${{ always() }}\n', block)
        gate = steps(block)[0]
        self.assertIn('\n        env:\n          UNIT: ${{ needs.unit.result }}\n        run: |\n', gate)
        lines = gate.split('run: |\n', 1)[1].split('\n')
        ends = [index for index, line in enumerate(lines) if line and not line.startswith(' ' * 10)]
        body = '\n'.join(line[10:] for line in lines[:ends[0] if ends else len(lines)])
        self.assertIn('exit 1', body)
        for result, status in (('success', 0), ('failure', 1), ('cancelled', 1), ('skipped', 1), ('', 1)):
            with self.subTest(result=result):
                process = subprocess.run(['bash', '-e', '-c', body], env=dict(os.environ, UNIT=result),
                                         stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
                self.assertEqual(process.returncode, status, process.stdout)

    def test_the_unit_shards_of_ci_run_every_group_once(self):
        """On each system the shards ci.yml names, with the JPACK_UNIT_SHARD each sets, run every group
        and the store conformance run exactly once between them: the evidence of the Unit tests legs
        is the evidence of the whole unit-tests step."""
        script = ROOT / UNIT_SCRIPT
        block = jobs(read(config()['workflows']['ci']['path']))['unit']
        setting = one(re.findall(r'^          JPACK_UNIT_SHARD: (.+)$', block, re.M), 'JPACK_UNIT_SHARD')
        serial, everything = unit_partition_trace(script, jobs='1')
        self.assertEqual(serial.returncode, 0, serial.stdout)
        shards = {}
        for leg in ci_legs().values():
            if leg['job_id'] == 'unit':
                self.assertNotIn('${{', leg['name'])
                shards.setdefault(leg['labels'][0], []).append(leg['expand'](setting))
        self.assertEqual(sorted(shards), ['macos-latest', 'ubuntu-latest'])
        for system, selections in shards.items():
            with self.subTest(system=system):
                self.assertEqual(len(selections), len(set(selections)))
                union = []
                for selection in selections:
                    process, calls = unit_partition_trace(script, jobs='1', shard=selection)
                    self.assertEqual(process.returncode, 0, process.stdout)
                    self.assertTrue(calls, selection + ' runs nothing')
                    self.assertIn('unit-tests: shard %s: ' % selection, process.stdout)
                    union += calls
                self.assert_unit_partition_calls(union)
                self.assertEqual(sorted(json.dumps(call['args']) for call in union),
                                 sorted(json.dumps(call['args']) for call in everything))

    def test_unit_script_refuses_a_shard_table_that_does_not_name_every_group_once(self):
        script = read(UNIT_SCRIPT)
        rows = unit_shard_rows(script)
        longest = max(range(len(rows)), key=lambda index: len(rows[index].split()))
        first = rows[longest].split()[0]
        other = (longest + 1) % len(rows)

        def changed(index, row):
            return [row if number == index else value for number, value in enumerate(rows)]
        mutants = [
            (changed(longest, ' '.join(rows[longest].split()[1:])), 'the shard table names %s in no shard' % first),
            ([' '.join(item for item in row.split() if item != 'store') for row in rows],
             'the shard table names store in no shard'),
            (changed(other, rows[other] + ' ' + first), 'the shard table names %s twice' % first),
            (changed(other, rows[other] + ' 12'), 'the shard table names group 12 of 11'),
            (changed(other, rows[other] + ' x'), "the shard table names 'x', which is not a group"),
            (rows + [''], 'a shard of the table has no group'),
        ]
        for mutated, message in mutants:
            for shard in (None, '1/%d' % len(mutated)):
                with self.subTest(table=mutated, shard=shard):
                    process, calls = unit_partition_trace(ROOT / UNIT_SCRIPT, shard=shard,
                                                          script_text=with_unit_shard_rows(script, mutated))
                    self.assertEqual(process.returncode, 2, process.stdout)
                    self.assertEqual(calls, [])
                    self.assertIn(message, process.stdout)

    def test_unit_script_refuses_a_shard_that_is_not_one_of_its_table(self):
        count = len(unit_shard_rows(read(UNIT_SCRIPT)))
        for shard in ('0/%d' % count, '%d/%d' % (count + 1, count), '1/%d' % (count + 1), '1/%d' % (count - 1),
                      '1', 'a/%d' % count, '01/%d' % count, ' 1/%d' % count, '1/%d ' % count, '1/%d/1' % count):
            with self.subTest(shard=shard):
                process, calls = unit_partition_trace(ROOT / UNIT_SCRIPT, shard=shard)
                self.assertEqual(process.returncode, 2, process.stdout)
                self.assertEqual(calls, [])
                self.assertIn('not i/%d with i from 1 to %d' % (count, count), process.stdout)

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
        names.update({'TestColumnRoleFuture', 'TestColumnFuture', 'TestSelectedLeafFuture',
                      'TestWorkflowFuture', 'TestAFuture', 'TestMFuture', 'TestNFuture',
                      'TestZFuture', 'TestFutureOrdinary', 'TestΩFuture',
                      'ExampleFuture', 'ExampleColumn', 'FuzzFuture', 'FuzzSelectedLeaf'})
        # Go matches slash-separated run/skip expressions component by component.
        # Represent the split parent's leaves so sibling/future subtests must still
        # run exactly once, including versions below the selected artifact level.
        offline = 'TestColumnRoleOfflineIndependentArtifactLimits'
        names.remove(offline)
        names.update(offline + '/' + artifact + '/' + version
                     for artifact in ('parsed-record', 'original-record', 'release', 'run', 'future')
                     for version in ('v2', 'v5', 'future'))
        names.update({offline + 'Future', offline + '/releaseFuture/v5',
                      offline + '/runFuture/v5', 'TestColumnRoleStoredAndExportBoundariesFuture',
                      'TestColumnRoleLiteralSeparatorWireCustodyFuture'})

        def selected(pattern, name):
            expressions, components = pattern.split('/'), name.split('/')
            return len(expressions) <= len(components) and all(
                re.search(expression, component)
                for expression, component in zip(expressions, components))

        # The groups run side by side, so the trace holds them in the order
        # they started, which is not fixed: the group that takes what the
        # others leave is known by having no -run, not by being last.
        groups, rest = [], []
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
                matches &= {name for name in names if selected(args[args.index('-run') + 1], name)}
            if '-skip' in args:
                matches -= {name for name in names if selected(args[args.index('-skip') + 1], name)}
            groups.append(matches)
            if '-run' not in args:
                rest.append(matches)
            for key, binary in {'JPACK_TEST_BIN': 'jpack', 'JPACK_RETAINED_TEST_BIN': 'jpack-retained',
                                'JPACK_COVERS_TEST_BIN': 'jpack-covers', 'JPACK_CORE050_TEST_BIN': 'jpack-core050',
                                'JPACK_EARLIER_TEST_BIN': 'jpack-earlier', 'JPACK_GATEWAY_TEST_BIN': 'gateway',
                                'JPACK_MCP_ADAPTER_TEST_BIN': 'adapter-mcp', 'JPACK_OLD_RUNNER_TEST_BIN': 'jpack-runner-old'}.items():
                self.assertEqual(call['env'][key], str(ROOT / '.ci/bin' / binary), key)
        self.assertEqual(set().union(*groups), names, 'unit partitions omit cases')
        for index, group in enumerate(groups):
            for other in groups[index + 1:]:
                self.assertFalse(group & other, 'unit partitions repeat cases')
        self.assertEqual(len(groups), 11)
        self.assertEqual(len(rest), 1, 'one unit partition takes what the others leave')
        for name in ('ExampleFuture', 'ExampleColumn', 'FuzzFuture', 'FuzzSelectedLeaf'):
            self.assertIn(name, rest[0])
        conformance = [call for call in calls if '^TestStoreKindsConform$' in call['args']]
        self.assertEqual(len(conformance), 1)
        self.assertEqual(conformance[0]['args'],
                         ['test', '-race', '-count=1', '-v', '-run',
                          '^TestStoreKindsConform$', './internal/runner'],
                         'store conformance invocation changed')

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
            ('run_unit -run "$offline/^release$"', ': # omitted release partition',
             'unit partitions omit cases'),
            (' -skip "$offline/^(release|run)$"', '', 'unit partitions repeat cases'),
            (' -skip "$large_roles"', '', 'unit partitions repeat cases'),
            ('"$offline/^release$"', '"$offline/^release"', 'unit partitions repeat cases'),
            ("run_unit -run '^TestWorkflow'", ': # omitted workflow partition', 'unit partitions omit cases'),
            ("'^(TestColumn|TestSelectedLeaf|TestWorkflow|Test[A-M])'",
             "'^(TestColumn|TestSelectedLeaf|Test[A-M])'", 'unit partitions repeat cases'),
            ('go test -race "$@"', 'go test "$@"', 'unit partition lost race instrumentation'),
            ('go test -race "$@"', 'go test -race -timeout=700s "$@"', 'unit partitions changed default timeout'),
            ('-race -count=1 -v', '-race -v', 'store conformance invocation changed'),
            ('JPACK_TEST_BIN="$bin/jpack"', 'JPACK_TEST_BIN="$bin/wrong"', 'JPACK_TEST_BIN'),
        ]
        for before, after, message in mutations:
            with self.subTest(safeguard=message):
                self.assertEqual(script.count(before), 1, before)
                mutated = script.replace(before, after)
                if after.startswith(': # omitted'):
                    # The script refuses a shard table that names a group it no longer has
                    # (test_unit_script_refuses_a_shard_table_that_does_not_name_every_group_once);
                    # the table leaves out the last group so that the partition check is what fails.
                    mutated = without_unit_group(mutated, 11)
                process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh',
                                                       script_text=mutated)
                self.assertEqual(process.returncode, 0, process.stdout)
                with self.assertRaisesRegex(AssertionError, message):
                    self.assert_unit_partition_calls(calls)

    def test_unit_selections_run_side_by_side_as_they_run_one_after_another(self):
        serial, serial_calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh', jobs='1')
        self.assertEqual(serial.returncode, 0, serial.stdout)
        self.assert_unit_partition_calls(serial_calls)
        self.assertIn('unit-tests: 11 groups, at most 1 at once, ', serial.stdout)
        default = '2' if platform.system() == 'Darwin' else '3'
        for jobs, at_most in ((None, default), ('4', '4'), ('10', '10')):
            with self.subTest(jobs=jobs):
                process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh', jobs=jobs)
                self.assertEqual(process.returncode, 0, process.stdout)
                self.assertEqual(sorted(call['args'] for call in calls), sorted(call['args'] for call in serial_calls))
                self.assertEqual(calls[-1]['args'], serial_calls[-1]['args'], 'conformance did not run last')
                listing = [line for line in process.stdout.splitlines() if line.startswith('unit-tests: 11 groups')]
                self.assertEqual(len(listing), 1, process.stdout)
                self.assertIn(', at most %s at once, ' % at_most, listing[0])
                for group in range(1, 12):
                    self.assertRegex(listing[0], r'(: |, )%d ok [0-9]+s' % group)

    def test_unit_script_refuses_a_job_count_that_is_not_a_whole_number_above_zero(self):
        for jobs in ('0', '00', '03', '-1', '1.5', 'two', ' 2'):
            with self.subTest(jobs=jobs):
                process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh', jobs=jobs)
                self.assertEqual(process.returncode, 2, process.stdout)
                self.assertEqual(calls, [])
                self.assertIn('not a whole number above 0', process.stdout)

    def test_unit_script_propagates_each_partition_and_conformance_failure(self):
        # One at a time, nothing starts after a failure.
        for failed in range(1, 12):
            with self.subTest(call=failed):
                process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh', fail_call=failed,
                                                      jobs='1')
                self.assertNotEqual(process.returncode, 0)
                self.assertEqual(len(calls), failed, 'script continued after Go failed')
                self.assertIn('fake Go failure at call %d' % failed, process.stdout)
        # Side by side, a failed group fails the step, its output is shown whole, and the
        # store conformance run does not start.
        _, everything = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh', jobs='1')
        groups = [call['args'] for call in everything[:11]]
        for number, args in enumerate(groups, 1):
            with self.subTest(group=number):
                process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh', fail_args=args)
                self.assertNotEqual(process.returncode, 0)
                self.assertIn('fake Go failure at call ', process.stdout)
                self.assertIn('== the output of group %d ==' % number, process.stdout)
                self.assertRegex(process.stdout, r'(?m)^group %d FAIL \(7\) in ' % number)
                self.assertRegex(process.stdout, r'(?m)^unit-tests: 11 groups, .*\b%d FAIL \(7\)' % number)
                self.assertNotIn(everything[11]['args'], [call['args'] for call in calls],
                                 'store conformance ran after a group failed')
                self.assertIn(args, [call['args'] for call in calls])

    def assert_unit_store_conformance_required(self, *, script_text=None):
        for kinds, missing in [('sqlite,twice', 'memory'), ('memory,twice', 'sqlite'), ('sqlite,memory', 'twice'),
                               ('', 'sqlite')]:
            process, calls = unit_partition_trace(ROOT / 'scripts/checks/unit-tests.sh',
                                                   script_text=script_text, conformance=kinds)
            self.assertNotEqual(process.returncode, 0, 'script accepted missing store conformance')
            self.assertEqual(len(calls), 12)
            self.assertIn('TestStoreKindsConform/%s did not run and pass' % missing, process.stdout)

    def test_unit_script_requires_every_actual_store_conformance_kind(self):
        self.assert_unit_store_conformance_required()

    def test_unit_store_controls_catch_removed_kind_assertion(self):
        script = read('scripts/checks/unit-tests.sh')
        before = 'for kind in sqlite memory twice; do'
        self.assertEqual(script.count(before), 1)
        for after in ('for kind in memory twice; do', 'for kind in sqlite twice; do', 'for kind in sqlite memory; do'):
            with self.subTest(mutation=after):
                with self.assertRaisesRegex(AssertionError, 'script accepted missing store conformance'):
                    self.assert_unit_store_conformance_required(script_text=script.replace(before, after))

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
