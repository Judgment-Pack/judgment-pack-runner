#!/usr/bin/env python3
"""The archive check refuses what it says it refuses: sound archives, then one fault at a time.

Usage: python3 tools/test_release_archives.py

A repository is made in a temporary directory, with the shape of this one
where the check reads it: the documents, go.mod and three programs under
cmd, all committed. The programs are real ones, built by the toolchain,
since what the check reads is the record the toolchain writes. Archives of
that commit pass; then each is spoiled in one way, and the check must
refuse it and say why.

The cases are not a proof that nothing else gets through. When they were
written, each condition of the script was removed in turn, and at least
one case failed for each.
"""
import io
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
import release_archives as checked  # noqa: E402

PROGRAM = 'package main\n\nfunc main() {}\n'
MODULE = 'example.test/runner'
PROGRAMS = ['one', 'two', 'three']
TARGETS = ['linux_amd64', 'darwin_arm64']
RECORD = f'program: go1.26.5\n\tpath\t{MODULE}/cmd/one\n\tmod\t{MODULE}\t(devel)\t\n\tbuild\tGOARCH=amd64\n\tbuild\tGOOS=linux\n\tbuild\tGOAMD64=v1\n'


def go_build(module, package, output, goos, goarch, **settings):
    # The levels are set here and not taken from whoever runs the tests.
    levels = {'GOAMD64': 'v1', 'GOARM64': 'v8.0'}
    levels.update(settings)
    environment = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED='0', GOWORK='off', GOFLAGS='-buildvcs=false', **levels)
    subprocess.run(['go', 'build', '-trimpath', '-ldflags', '-s -w', '-o', str(output), package], cwd=module, env=environment, check=True)


class ArchiveChecks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scratch = Path(tempfile.mkdtemp(prefix='release-archives-test-'))
        cls.addClassCleanup(shutil.rmtree, cls.scratch, ignore_errors=True)
        tree = cls.tree = cls.scratch / 'tree'
        tree.mkdir()
        for name in checked.DOCUMENTS:
            (tree / name).write_text(f'{name}\n')
        # Written as the toolchain also reads it: a tab, and the path quoted.
        (tree / 'go.mod').write_text(f'module\t"{MODULE}"\n\ngo 1.25.0\n')
        for name in PROGRAMS:
            (tree / 'cmd' / name).mkdir(parents=True)
            (tree / 'cmd' / name / 'main.go').write_text(PROGRAM)
        git = ['git', '-C', str(tree), '-c', 'user.name=test', '-c', 'user.email=test@invalid', '-c', 'commit.gpgsign=false']
        subprocess.run(['git', 'init', '-q', str(tree)], check=True)
        subprocess.run(git + ['add', '.'], check=True)
        subprocess.run(git + ['commit', '-q', '-m', 'tree'], check=True)
        cls.files = checked.committed(tree, 'HEAD')
        cls.programs = checked.programs(tree, 'HEAD', cls.scratch)

        cls.built = {}
        for target in TARGETS + ['linux_arm64', 'darwin_amd64']:
            goos, _, goarch = target.partition('_')
            where = cls.scratch / 'built' / target
            where.mkdir(parents=True)
            for name in PROGRAMS:
                go_build(tree, f'./cmd/{name}', where / name, goos, goarch)
            cls.built[target] = where
        go_build(tree, './cmd/one', cls.scratch / 'built' / 'one-amd64-v3', 'linux', 'amd64', GOAMD64='v3')
        go_build(tree, './cmd/one', cls.scratch / 'built' / 'one-arm64-v8.1', 'darwin', 'arm64', GOARM64='v8.1')

    def contents(self, target):
        """What a sound archive of the target holds: name -> (mode, bytes)."""
        held = {name: (0o644, data) for name, data in self.files.items()}
        for name in PROGRAMS:
            held[name] = (0o755, (self.built[target] / name).read_bytes())
        return held

    def archive(self, target, held, extra=()):
        """Write the archive of the target; extra is tar members added after, as given."""
        dist = Path(tempfile.mkdtemp(dir=self.scratch))
        path = dist / f'{checked.PROJECT}_1.0.0_{target}.tar.gz'
        with tarfile.open(path, 'w:gz') as opened:
            for name, (mode, data) in held.items():
                info = tarfile.TarInfo(name)
                info.mode, info.size = mode, len(data)
                opened.addfile(info, io.BytesIO(data))
            for info, data in extra:
                opened.addfile(info, io.BytesIO(data) if data is not None else None)
        return dist

    def faults(self, target, held, extra=()):
        dist = self.archive(target, held, extra)
        return checked.check(next(dist.iterdir()), target, self.files, self.programs, dist)

    def refused(self, target, held, saying, extra=()):
        faults = self.faults(target, held, extra)
        self.assertTrue(any(saying in fault for fault in faults), f'no fault says {saying!r}: {faults}')

    def tar_member(self, name, kind, mode=0o644, link=''):
        info = tarfile.TarInfo(name)
        info.type, info.mode, info.linkname = kind, mode, link
        return info

    # Sound archives, and what they are compared with.

    def test_sound_archives_pass(self):
        for target in TARGETS:
            self.assertEqual(self.faults(target, self.contents(target)), [], target)

    def test_the_module_is_read_as_the_toolchain_reads_it(self):
        self.assertEqual(self.programs, {name: f'{MODULE}/cmd/{name}' for name in PROGRAMS})

    def test_a_commit_that_holds_no_program(self):
        bare = self.scratch / 'bare'
        bare.mkdir()
        (bare / 'go.mod').write_text(f'module {MODULE}\n\ngo 1.25.0\n')
        git = ['git', '-C', str(bare), '-c', 'user.name=test', '-c', 'user.email=test@invalid', '-c', 'commit.gpgsign=false']
        subprocess.run(['git', 'init', '-q', str(bare)], check=True)
        subprocess.run(git + ['add', '.'], check=True)
        subprocess.run(git + ['commit', '-q', '-m', 'no program'], check=True)
        with self.assertRaises(SystemExit) as refused:
            checked.programs(bare, 'HEAD', self.scratch)
        self.assertEqual(str(refused.exception), 'the commit holds no program under cmd/')

    def test_the_commit_is_what_is_compared_and_not_the_tree(self):
        changed, removed = self.tree / 'openapi.json', self.tree / 'LICENSE'
        kept = changed.read_bytes(), removed.read_bytes()
        try:
            changed.write_bytes(b'changed after the commit\n')
            removed.unlink()
            files = checked.committed(self.tree, 'HEAD')
            self.assertEqual(files, self.files)
            held = self.contents('linux_amd64')
            held['openapi.json'] = (0o644, b'changed after the commit\n')
            dist = self.archive('linux_amd64', held)
            faults = checked.check(next(dist.iterdir()), 'linux_amd64', files, self.programs, dist)
            self.assertEqual(faults, ['openapi.json: not the bytes the commit holds'])
        finally:
            changed.write_bytes(kept[0])
            removed.write_bytes(kept[1])

    # What is missing, and what is too much.

    def test_a_missing_program(self):
        held = self.contents('linux_amd64')
        del held['two']
        self.refused('linux_amd64', held, 'two: not in the archive')

    def test_a_missing_document(self):
        held = self.contents('darwin_arm64')
        del held['openapi.json']
        self.refused('darwin_arm64', held, 'openapi.json: not in the archive')

    def test_a_file_a_release_does_not_hold(self):
        held = self.contents('darwin_arm64')
        held['notes.txt'] = (0o644, b'stray\n')
        self.refused('darwin_arm64', held, 'notes.txt: a file a release does not hold')

    def test_a_document_under_another_case(self):
        held = self.contents('linux_amd64')
        held['license'] = held['LICENSE']
        self.refused('linux_amd64', held, 'license: a file a release does not hold')

    def test_a_member_twice(self):
        held = self.contents('linux_amd64')
        again = tarfile.TarInfo('LICENSE')
        again.size = len(held['LICENSE'][1])
        faults = self.faults('linux_amd64', held, extra=[(again, held['LICENSE'][1])])
        self.assertEqual(faults, ['LICENSE: in the archive twice'])

    def test_a_member_larger_than_any_of_a_release(self):
        kept = checked.LIMIT
        try:
            checked.LIMIT = 4
            faults = self.faults('linux_amd64', self.contents('linux_amd64'))
        finally:
            checked.LIMIT = kept
        self.assertIn('LICENSE: 8 bytes, more than any file of a release', faults)
        self.assertFalse(any('not the bytes' in fault or 'build record' in fault for fault in faults), faults)

    # Bytes.

    def test_a_changed_document(self):
        held = self.contents('darwin_arm64')
        held['openapi.json'] = (0o644, b'{"another": "contract"}\n')
        self.refused('darwin_arm64', held, 'openapi.json: not the bytes the commit holds')

    # Names.

    def test_a_name_with_a_leading_dot(self):
        held = self.contents('linux_amd64')
        held['./LICENSE'] = held.pop('LICENSE')
        faults = self.faults('linux_amd64', held)
        self.assertIn('./LICENSE: a name not written the plain way', faults)
        self.assertIn('LICENSE: not in the archive', faults)

    def test_a_program_named_with_a_trailing_slash(self):
        held = self.contents('linux_amd64')
        held['one/'] = held.pop('one')
        faults = self.faults('linux_amd64', held)
        self.assertIn('one/: a name not written the plain way', faults)
        self.assertIn('one: not in the archive', faults)

    def test_a_member_named_for_the_archive_s_own_root(self):
        for kind in (tarfile.REGTYPE, tarfile.FIFOTYPE, tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.CHRTYPE, tarfile.DIRTYPE):
            for name in ('.', './'):
                faults = self.faults('linux_amd64', self.contents('linux_amd64'), extra=[(self.tar_member(name, kind, link='one'), None)])
                self.assertEqual(len(faults), 1, faults)
                self.assertTrue(faults[0].startswith('.'), faults)

    def test_a_path_that_leaves_the_archive(self):
        held = self.contents('linux_amd64')
        held['../outside'] = (0o644, b'x')
        self.refused('linux_amd64', held, '../outside: a name not written the plain way')

    def test_a_name_with_a_backslash(self):
        held = self.contents('linux_amd64')
        held['cmd\\one'] = (0o644, b'x')
        self.refused('linux_amd64', held, 'cmd\\one: a name not written the plain way')

    # Kinds.

    def test_a_directory(self):
        self.refused('linux_amd64', self.contents('linux_amd64'), 'extra: a directory, and a release holds files',
                     extra=[(self.tar_member('extra', tarfile.DIRTYPE, 0o755), None)])

    def test_a_directory_under_the_name_of_a_file_already_there(self):
        # Unpacked, the directory would take the file's place.
        faults = self.faults('linux_amd64', self.contents('linux_amd64'),
                             extra=[(self.tar_member('two', tarfile.DIRTYPE, 0o755), None)])
        self.assertEqual(faults, ['two: in the archive twice'])

    def test_a_directory_of_programs_under_a_program_s_name(self):
        # `go version -m` reads a directory through, and the last record in it is a sound one.
        held = self.contents('darwin_arm64')
        del held['one']
        held['one/a'] = (0o755, (self.built['linux_amd64'] / 'one').read_bytes())
        held['one/z'] = (0o755, (self.built['darwin_arm64'] / 'one').read_bytes())
        faults = self.faults('darwin_arm64', held)
        self.assertIn('one: not in the archive', faults)
        self.assertIn('one/z: a file a release does not hold', faults)

    def test_a_link_in_place_of_a_file(self):
        held = self.contents('linux_amd64')
        del held['LICENSE']
        self.refused('linux_amd64', held, 'LICENSE: a link, and a release holds files',
                     extra=[(self.tar_member('LICENSE', tarfile.SYMTYPE, link='/etc/hostname'), None)])

    def test_a_hard_link_in_place_of_a_file(self):
        held = self.contents('linux_amd64')
        del held['two']
        self.refused('linux_amd64', held, 'two: a link, and a release holds files',
                     extra=[(self.tar_member('two', tarfile.LNKTYPE, 0o755, link='one'), None)])

    def test_a_pipe_or_a_device_in_place_of_a_file(self):
        for kind in (tarfile.FIFOTYPE, tarfile.CHRTYPE, tarfile.BLKTYPE):
            held = self.contents('linux_amd64')
            del held['three']
            self.refused('linux_amd64', held, 'three: a special file, and a release holds files',
                         extra=[(self.tar_member('three', kind, 0o755), None)])

    # Programs.

    def test_a_program_built_from_another_package(self):
        held = self.contents('linux_amd64')
        held['one'] = held['two']
        self.refused('linux_amd64', held, f'one: built from {MODULE}/cmd/two')

    def test_a_program_built_for_another_system_and_the_same_architecture(self):
        held = self.contents('linux_amd64')
        held['three'] = (0o755, (self.built['darwin_amd64'] / 'three').read_bytes())
        self.refused('linux_amd64', held, f'three: built from {MODULE}/cmd/three for darwin_amd64 at v1')

    def test_a_program_built_for_another_architecture_and_the_same_system(self):
        held = self.contents('linux_amd64')
        held['one'] = (0o755, (self.built['linux_arm64'] / 'one').read_bytes())
        self.refused('linux_amd64', held, f'one: built from {MODULE}/cmd/one for linux_arm64')

    def test_a_program_built_above_the_level_a_release_promises(self):
        held = self.contents('linux_amd64')
        held['one'] = (0o755, (self.scratch / 'built' / 'one-amd64-v3').read_bytes())
        self.refused('linux_amd64', held, f'one: built from {MODULE}/cmd/one for linux_amd64 at v3, and not from {MODULE}/cmd/one for linux_amd64 at GOAMD64=v1')
        held = self.contents('darwin_arm64')
        held['one'] = (0o755, (self.scratch / 'built' / 'one-arm64-v8.1').read_bytes())
        self.refused('darwin_arm64', held, f'one: built from {MODULE}/cmd/one for darwin_arm64 at v8.1, and not from {MODULE}/cmd/one for darwin_arm64 at GOARM64=v8.0')

    def test_each_part_of_the_record_is_compared_by_itself(self):
        # The levels of two architectures share no name, so no real program
        # differs from another in its architecture alone: the record is given.
        held = {name: value for name, value in self.contents('linux_amd64').items() if name not in ('two', 'three')}
        package = f'{MODULE}/cmd/one'
        sound = (package, 'linux', 'amd64', 'v1')
        kept = checked.build_record
        try:
            for place, other in enumerate((f'{MODULE}/cmd/two', 'darwin', 'arm64', 'v2')):
                given = sound[:place] + (other,) + sound[place + 1:]
                checked.build_record = lambda data, scratch, given=given: (given, '')
                dist = self.archive('linux_amd64', held)
                faults = checked.check(next(dist.iterdir()), 'linux_amd64', self.files, {'one': package}, dist)
                self.assertEqual(faults, [f'one: built from {given[0]} for {given[1]}_{given[2]} at {given[3]}, and not from {package} for linux_amd64 at GOAMD64=v1'])
            checked.build_record = lambda data, scratch: (sound, '')
            dist = self.archive('linux_amd64', held)
            self.assertEqual(checked.check(next(dist.iterdir()), 'linux_amd64', self.files, {'one': package}, dist), [])
        finally:
            checked.build_record = kept

    def test_a_program_that_is_not_a_program(self):
        held = self.contents('linux_amd64')
        held['one'] = (0o755, b'#!/bin/sh\nexit 0\n')
        self.refused('linux_amd64', held, 'one: it carries no build record')

    def test_a_file_that_holds_several_programs(self):
        # As a universal Mach-O begins; the toolchain would read the first program in it.
        program = (self.built['darwin_arm64'] / 'one').read_bytes()
        for start in checked.SEVERAL:
            held = self.contents('darwin_arm64')
            held['one'] = (0o755, start + program)
            self.refused('darwin_arm64', held, 'one: it holds several programs')

    def test_a_program_not_everyone_can_run(self):
        for mode in (0o644, 0o641, 0o700, 0o751, 0o311):
            held = self.contents('darwin_arm64')
            held['two'] = (mode, held['two'][1])
            self.refused('darwin_arm64', held, f'two: mode {mode:04o}, which not everyone can read and execute')

    # The build record, as text.

    def test_a_sound_record(self):
        self.assertEqual(checked.parse_record(RECORD), ((f'{MODULE}/cmd/one', 'linux', 'amd64', 'v1'), ''))

    def test_a_record_of_two_programs(self):
        self.assertEqual(checked.parse_record(RECORD + RECORD), (None, 'its build record names 2 packages'))

    def test_a_record_that_names_no_package(self):
        self.assertEqual(checked.parse_record(RECORD.replace(f'\tpath\t{MODULE}/cmd/one\n', '')), (None, 'its build record names 0 packages'))

    def test_a_record_without_its_system(self):
        self.assertEqual(checked.parse_record(RECORD.replace('\tbuild\tGOOS=linux\n', '')), (None, 'its build record states GOOS 0 times'))

    def test_a_record_with_its_architecture_twice(self):
        self.assertEqual(checked.parse_record(RECORD + '\tbuild\tGOARCH=arm64\n'), (None, 'its build record states GOARCH 2 times'))

    def test_a_record_without_its_level(self):
        self.assertEqual(checked.parse_record(RECORD.replace('\tbuild\tGOAMD64=v1\n', '')), (None, 'its build record states GOAMD64 0 times'))

    def test_a_record_for_an_architecture_no_release_is_built_for(self):
        self.assertEqual(checked.parse_record(RECORD.replace('GOARCH=amd64', 'GOARCH=riscv64')), (None, 'its build record states an architecture no release is built for: riscv64'))

    # The script, as the workflow runs it.

    def run_script(self, dist, *targets, commit='HEAD'):
        arguments = [sys.executable, str(Path(checked.__file__)), '--dist', str(dist), '--version', '1.0.0', '--tree', str(self.tree), '--commit', commit]
        for target in targets:
            arguments += ['--target', target]
        return subprocess.run(arguments, capture_output=True, text=True)

    def test_an_archive_that_is_not_there(self):
        dist = self.archive('linux_amd64', self.contents('linux_amd64'))
        ran = self.run_script(dist, 'linux_amd64', 'darwin_arm64')
        self.assertEqual(ran.returncode, 1, ran.stderr)
        self.assertIn('linux_amd64: judgment-pack-runner_1.0.0_linux_amd64.tar.gz holds what commit', ran.stdout)
        self.assertIn('darwin_arm64: judgment-pack-runner_1.0.0_darwin_arm64.tar.gz is not there', ran.stderr)

    def test_a_fault_is_a_failure_of_the_script(self):
        held = self.contents('linux_amd64')
        held['notes.txt'] = (0o644, b'stray\n')
        ran = self.run_script(self.archive('linux_amd64', held), 'linux_amd64')
        self.assertEqual(ran.returncode, 1, ran.stdout)
        self.assertIn('linux_amd64: notes.txt: a file a release does not hold', ran.stderr)
        self.assertEqual(self.run_script(self.archive('linux_amd64', self.contents('linux_amd64')), 'linux_amd64').returncode, 0)

    def test_a_commit_that_is_not_there(self):
        ran = self.run_script(self.archive('linux_amd64', self.contents('linux_amd64')), 'linux_amd64', commit='no-such-commit')
        self.assertNotEqual(ran.returncode, 0)


if __name__ == '__main__':
    unittest.main(verbosity=2)
