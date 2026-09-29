#!/usr/bin/env python3
"""The archive check holds each thing it says it holds: one negative case each.

Usage: python3 tools/test_release_archives.py

A tree is made in a temporary directory, with the shape of this repository
where the check reads it: the documents and three programs under cmd. The
programs are real ones, built by the toolchain for two platforms, since what the
check reads is the record the toolchain writes. Archives of that tree pass;
then each is spoiled in one way, and the check must refuse it and say why.
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
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parent))
import release_archives as checked  # noqa: E402

PROGRAM = 'package main\n\nfunc main() {}\n'
TARGETS = ['linux_amd64', 'darwin_arm64']
MODULE = 'example.test/runner'
PROGRAMS = ['one', 'two', 'three']


def go_build(module, package, output, goos, goarch, **settings):
    environment = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED='0', GOWORK='off', GOFLAGS='-buildvcs=false', **settings)
    subprocess.run(['go', 'build', '-trimpath', '-ldflags', '-s -w', '-o', str(output), package], cwd=module, env=environment, check=True)


class ArchiveChecks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scratch = Path(tempfile.mkdtemp(prefix='release-archives-test-'))
        tree = cls.tree = cls.scratch / 'tree'
        for name in checked.DOCUMENTS:
            (tree / name).parent.mkdir(parents=True, exist_ok=True)
            (tree / name).write_text(f'{name}\n')
        (tree / 'go.mod').write_text(f'module {MODULE}\n\ngo 1.25.0\n')
        for name in PROGRAMS:
            (tree / 'cmd' / name).mkdir(parents=True)
            (tree / 'cmd' / name / 'main.go').write_text(PROGRAM)

        cls.built = {}
        for target in TARGETS + ['linux_arm64']:
            goos, _, goarch = target.partition('_')
            where = cls.scratch / 'built' / target
            where.mkdir(parents=True)
            for name in PROGRAMS:
                go_build(tree, f'./cmd/{name}', where / name, goos, goarch)
            cls.built[target] = where
        go_build(tree, './cmd/one', cls.scratch / 'built' / 'one-v3', 'linux', 'amd64', GOAMD64='v3')

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.scratch, ignore_errors=True)

    def contents(self, target):
        """What a sound archive of the target holds: name -> (mode, bytes)."""
        held = {name: (0o644, (self.tree / name).read_bytes()) for name in checked.DOCUMENTS}
        for name in PROGRAMS:
            held[name] = (0o755, (self.built[target] / name).read_bytes())
        return held

    def archive(self, target, held, extra=()):
        """Write the archive of the target; extra is tar members or zip entries added as given."""
        dist = Path(tempfile.mkdtemp(dir=self.scratch))
        if target.startswith('windows_'):
            path = dist / f'{checked.PROJECT}_1.0.0_{target}.zip'
            with zipfile.ZipFile(path, 'w') as opened:
                for name, (mode, data) in held.items():
                    info = zipfile.ZipInfo(name)
                    info.external_attr = (0o100000 | mode) << 16
                    opened.writestr(info, data)
                for info, data in extra:
                    opened.writestr(info, data)
            return dist
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
        archive = next(dist.iterdir())
        return checked.check(archive, target, self.tree, dist)

    def refused(self, target, held, saying, extra=()):
        faults = self.faults(target, held, extra)
        self.assertTrue(any(saying in fault for fault in faults), f'no fault says {saying!r}: {faults}')

    def test_sound_archives_pass(self):
        for target in TARGETS:
            self.assertEqual(self.faults(target, self.contents(target)), [], target)

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

    def test_a_directory_a_release_does_not_hold(self):
        held = self.contents('linux_amd64')
        directory = tarfile.TarInfo('extra')
        directory.type, directory.mode = tarfile.DIRTYPE, 0o755
        self.refused('linux_amd64', held, 'extra/: a directory a release does not hold', extra=[(directory, None)])

    def test_a_changed_document(self):
        held = self.contents('darwin_arm64')
        held['openapi.json'] = (0o644, b'{"another": "contract"}\n')
        self.refused('darwin_arm64', held, 'openapi.json: not the bytes the tree holds')

    def test_a_program_built_from_another_package(self):
        held = self.contents('linux_amd64')
        held['one'] = held['two']
        self.refused('linux_amd64', held, f'one: built from {MODULE}/cmd/two')

    def test_a_program_built_for_another_system(self):
        held = self.contents('linux_amd64')
        held['three'] = (0o755, (self.built['darwin_arm64'] / 'three').read_bytes())
        self.refused('linux_amd64', held, f'three: built from {MODULE}/cmd/three for darwin_arm64')

    def test_a_program_built_for_another_architecture(self):
        held = self.contents('linux_amd64')
        held['one'] = (0o755, (self.built['linux_arm64'] / 'one').read_bytes())
        self.refused('linux_amd64', held, f'one: built from {MODULE}/cmd/one for linux_arm64')

    def test_a_program_built_above_the_level_a_release_promises(self):
        held = self.contents('linux_amd64')
        held['one'] = (0o755, (self.scratch / 'built' / 'one-v3').read_bytes())
        self.refused('linux_amd64', held, f'at v3, and not from {MODULE}/cmd/one for linux_amd64 at GOAMD64=v1')

    def test_a_program_that_is_not_a_program(self):
        held = self.contents('linux_amd64')
        held['one'] = (0o755, b'#!/bin/sh\nexit 0\n')
        self.refused('linux_amd64', held, 'one: it carries no build record')

    def test_a_program_without_its_executable_bit(self):
        held = self.contents('darwin_arm64')
        held['two'] = (0o644, held['two'][1])
        self.refused('darwin_arm64', held, 'two: not executable')

    def test_a_directory_of_programs_under_a_program_s_name(self):
        # Two real programs in a directory named for one: `go version -m`
        # reads a directory through, and the last record in it is a sound one.
        held = self.contents('darwin_arm64')
        del held['one']
        held['one/a'] = (0o755, (self.built['linux_amd64'] / 'one').read_bytes())
        held['one/z'] = (0o755, (self.built['darwin_arm64'] / 'one').read_bytes())
        faults = self.faults('darwin_arm64', held)
        self.assertIn('one: not in the archive', faults)
        self.assertTrue(any('one/z: a file a release does not hold' in fault for fault in faults), faults)

    def test_a_link_in_place_of_a_file(self):
        held = self.contents('linux_amd64')
        del held['LICENSE']
        link = tarfile.TarInfo('LICENSE')
        link.type, link.linkname = tarfile.SYMTYPE, '/etc/hostname'
        self.refused('linux_amd64', held, 'LICENSE: a link, and a release holds files', extra=[(link, None)])

    def test_a_path_that_leaves_the_archive(self):
        held = self.contents('linux_amd64')
        held['../outside'] = (0o644, b'x')
        self.refused('linux_amd64', held, 'a path that leaves the archive')

    def test_an_archive_that_is_not_there(self):
        dist = self.archive('linux_amd64', self.contents('linux_amd64'))
        ran = subprocess.run([sys.executable, str(Path(checked.__file__)), '--dist', str(dist), '--version', '1.0.0', '--tree', str(self.tree), '--target', 'linux_amd64', '--target', 'darwin_arm64'], capture_output=True, text=True)
        self.assertEqual(ran.returncode, 1, ran.stderr)
        self.assertIn('linux_amd64: judgment-pack-runner_1.0.0_linux_amd64.tar.gz holds what a release holds', ran.stdout)
        self.assertIn('darwin_arm64: judgment-pack-runner_1.0.0_darwin_arm64.tar.gz is not there', ran.stderr)


if __name__ == '__main__':
    unittest.main(verbosity=2)
