#!/usr/bin/env python3
"""Hold the archives of a release to the commit they were built from.

Usage: python3 tools/release_archives.py --dist dist --version 0.1.0 [--tree .] [--commit HEAD]

The release workflow runs this on what GoReleaser wrote, before anything is
attested: it is held of the archives themselves, and not of the
configuration that was meant to make them. For every platform a release is
built for, the archive of that name must hold exactly:

  - the documents, byte for byte the commit's;
  - the programs, each one program and not a file of several, built from
    the package of its name, for the archive's operating system and
    architecture and at the processor level a release promises, as the
    executable's own build record says, the very bytes the packer built
    by its own account of what it built, and readable and executable by
    everyone.

An archive holds files and nothing else: no directory of its own, no link,
no device, no name twice, and no name written any way but the plain one
(no "./", no trailing slash, no backslash). A member's header must be of the
one form the packer writes, and any other is refused, though it may be a
lawful archive: a plain regular file with no extended header. The
members' headers are read first and their contents after, one at a time,
and only of members a release holds; a member larger than any a release has
is refused unread. An archive is first read to its end, where its checksum
is: one cut short or changed in passing is refused whole. Every fault found
is reported, and any fault is a failure.

The form is the packer's for what this repository gives it today: short
names in plain letters, files of ordinary size, modes as the checkout has
them. A name too long or not in plain letters, a file of many gigabytes, or
another version of the packer may be written in a form this refuses. That
is a refusal to look into, and not by itself a fault of the archive.

What is compared is the commit, not the working tree: a file changed or
removed in the tree after the commit changes nothing here.

What this is for, and what it is not. It is a check of the workflow's own
packaging: that a pattern did not pass a file over, that a build was made
from the package and for the platform its name says. It reads an archive as
Python's standard library reads one. It is not a defence against an archive
made to be read one way by one program and another way by the next: whoever
could put such an archive where this looks could change this script too.

What this does not establish: that a program behaves, or that the packer
built it well. A program is held to the bytes the packer wrote when it built
it, and those to nothing. A build record says
what an executable was built from and for; it is read here, by whatever
`go` is first on the path, and the program is not run. The release workflow
and CI run this with the toolchain a release is built with.
"""
import argparse
import gzip
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import zlib

ROOT = Path(__file__).resolve().parents[1]
PROJECT = 'judgment-pack-runner'
TARGETS = [f'{goos}_{goarch}' for goos in ('darwin', 'linux') for goarch in ('amd64', 'arm64')]
DOCUMENTS = ['LICENSE', 'README.md', 'THIRD_PARTY_NOTICES', 'openapi.json']
# The level of each architecture a release is built at, which is the lowest:
# an archive named for an architecture runs on every processor of it.
LEVELS = {'amd64': ('GOAMD64', 'v1'), 'arm64': ('GOARM64', 'v8.0')}
# No member of a release is a tenth of this.
LIMIT = 512 << 20
# How a file that holds several programs begins (a universal Mach-O, in either
# byte order and either width). The toolchain reads the first program in one
# and says nothing of the rest.
SEVERAL = (b'\xca\xfe\xba\xbe', b'\xbe\xba\xfe\xca', b'\xca\xfe\xba\xbf', b'\xbf\xba\xfe\xca')


def git(tree, *args):
    return subprocess.run(['git', '-C', str(tree), *args], check=True, capture_output=True).stdout


def programs(tree, commit, scratch):
    """Name -> the package it is built from: every program the commit holds under cmd, in the module its go.mod names."""
    held = scratch / 'go.mod'
    held.write_bytes(git(tree, 'cat-file', 'blob', f'{commit}:go.mod'))
    try:
        # Read by the toolchain, which knows every way a module may be named.
        read = subprocess.run(['go', 'mod', 'edit', '-json', str(held)], check=True, capture_output=True, text=True).stdout
    finally:
        held.unlink()
    module = json.loads(read)['Module']['Path']
    listed = git(tree, 'ls-tree', '-d', '-z', '--name-only', commit, 'cmd/').decode('utf-8')
    names = sorted(path.rsplit('/', 1)[1] for path in listed.split('\0') if path)
    if not names:
        raise SystemExit('the commit holds no program under cmd/')
    return {name: f'{module}/cmd/{name}' for name in names}


def committed(tree, commit):
    """Path -> bytes, as the commit holds them: the documents."""
    return {path: git(tree, 'cat-file', 'blob', f'{commit}:{path}') for path in DOCUMENTS}


def tar_kind(info):
    """What a tar member is, by everything its header says of it."""
    if info.isdir():
        return 'directory'
    if info.issym() or info.islnk():
        return 'link'
    if info.type != tarfile.REGTYPE:
        return 'member of no kind the packer writes'
    if info.pax_headers:
        return 'member whose header says more than a name, a mode and a time'
    return 'file'


def built(dist, target):
    """Name -> bytes of each program the packer built for the target, by the account it leaves of what it built."""
    account = dist / 'artifacts.json'
    if not account.is_file():
        raise SystemExit(f'{account} is not there: the packer leaves it, and the programs are held to what it names')
    goos, _, goarch = target.partition('_')
    found = {}
    for artifact in json.loads(account.read_text(encoding='utf-8')):
        if (artifact.get('type'), artifact.get('goos'), artifact.get('goarch')) != ('Binary', goos, goarch):
            continue
        name = artifact['name']
        if name in found:
            raise SystemExit(f'{target}: the packer built {name} twice')
        # The account names a path from where the packer ran, which begins with its directory of outputs.
        found[name] = (dist / Path(*Path(artifact['path']).parts[1:])).read_bytes()
    return found


def damage(archive):
    """Why an archive cannot be read to its end, or nothing. Its checksum is there, and a reader of members stops short of it."""
    with gzip.open(archive, 'rb') as opened:
        while opened.read(1 << 20):
            pass
    return ''


def headers(archive):
    """Every member's header as (name, kind, mode, size), the name as the archive writes it. Nothing is read."""
    with tarfile.open(archive, 'r:gz') as opened:
        for info in opened:
            # An extended header may give the name, and the reader has tidied it by then.
            yield info.pax_headers.get('path', info.name), tar_kind(info), info.mode & 0o7777, info.size


def contents(archive, names):
    """(name, bytes) for the named members, one at a time, in the archive's order. The names are of sound members."""
    with tarfile.open(archive, 'r:gz') as opened:
        for info in opened:
            if info.name in names:
                yield info.name, opened.extractfile(info).read()


def plain(name):
    """Whether a name is written the one plain way: parts joined by single slashes, none empty, none a dot.

    No name a release holds is written any other way, so a name that fails this is refused without it, as a
    file a release does not hold. This says why.
    """
    return '\\' not in name and all(part not in ('', '.', '..') for part in name.split('/'))


def parse_record(text):
    """What a build record says: (package, os, architecture, level), or why it says nothing usable."""
    packages, settings = [], {}
    for line in text.splitlines():
        fields = line.split()
        if len(fields) < 2:
            continue
        if fields[0] == 'path':
            packages.append(fields[1])
        if fields[0] == 'build':
            key, _, value = fields[1].partition('=')
            settings.setdefault(key, []).append(value)
    if len(packages) != 1:
        return None, f'its build record names {len(packages)} packages'
    for key in ('GOOS', 'GOARCH'):
        if len(settings.get(key, [])) != 1:
            return None, f'its build record states {key} {len(settings.get(key, []))} times'
    arch = settings['GOARCH'][0]
    if arch not in LEVELS:
        return None, f'its build record states an architecture no release is built for: {arch}'
    key = LEVELS[arch][0]
    if len(settings.get(key, [])) != 1:
        return None, f'its build record states {key} {len(settings.get(key, []))} times'
    return (packages[0], settings['GOOS'][0], arch, settings[key][0]), ''


def build_record(data, scratch):
    """What `go version -m` says of an executable's bytes."""
    if data[:4] in SEVERAL:
        return None, 'it holds several programs, and a release holds one under each name'
    held = scratch / 'program'
    held.write_bytes(data)
    try:
        ran = subprocess.run(['go', 'version', '-m', str(held)], capture_output=True, text=True)
    finally:
        held.unlink()
    if ran.returncode != 0 or not ran.stdout.strip():
        return None, 'it carries no build record: ' + (ran.stderr.strip().splitlines() or ['go version -m said nothing'])[-1]
    return parse_record(ran.stdout)


def check(archive, target, wanted_files, wanted_programs, outputs, scratch):
    """Every fault of one archive, as sentences; none is a pass."""
    faults = []
    shared = sorted(set(wanted_files) & set(wanted_programs))
    if shared:
        # One name cannot be held to a document's bytes and to a program's record.
        return [f'{name}: the name of a document and of a program' for name in shared]
    wanted = set(wanted_files) | set(wanted_programs)

    unread = (tarfile.TarError, zlib.error, OSError, EOFError)
    try:
        damaged = damage(archive)
        listed = list(headers(archive))
    except unread as refused:
        return [f'the archive cannot be read through: {refused}']
    if damaged:
        return [f'the archive cannot be read through: {damaged}']
    sound = {}
    seen = set()
    for name, kind, mode, size in listed:
        shown = repr(name)[1:-1]
        if name in seen:
            faults.append(f'{shown}: in the archive twice')
            # Neither is read: there is no saying which one an unpacking would leave.
            sound.pop(name, None)
            continue
        seen.add(name)
        if kind != 'file':
            faults.append(f'{shown}: a {kind}, and a release holds files')
        elif not plain(name):
            faults.append(f'{shown}: a name not written the plain way')
        elif name not in wanted:
            faults.append(f'{shown}: a file a release does not hold')
        elif size > LIMIT:
            faults.append(f'{shown}: {size} bytes, more than any file of a release')
        else:
            sound[name] = mode
    for name in sorted(wanted - seen):
        faults.append(f'{name}: not in the archive')

    try:
        # One member at a time; a fault in reading ends the reading.
        for name, data in contents(archive, set(sound)):
            faults += member_faults(name, data, sound[name], wanted_files, wanted_programs, outputs, target, scratch)
    except unread as refused:
        faults.append(f'the archive cannot be read through: {refused}')
    return faults


def member_faults(name, data, mode, wanted_files, wanted_programs, outputs, target, scratch):
    """The faults of one sound member's contents and mode."""
    if name in wanted_files:
        return [] if data == wanted_files[name] else [f'{name}: not the bytes the commit holds']
    faults = []
    goos, _, goarch = target.partition('_')
    level_key, level = LEVELS[goarch]
    package = wanted_programs[name]
    record, why = build_record(data, scratch)
    if record is None:
        faults.append(f'{name}: {why}')
    elif record != (package, goos, goarch, level):
        faults.append(f'{name}: built from {record[0]} for {record[1]}_{record[2]} at {record[3]}, and not from {package} for {target} at {level_key}={level}')
    if name not in outputs:
        faults.append(f'{name}: the packer has no record of building it')
    elif data != outputs[name]:
        faults.append(f'{name}: not the bytes the packer built')
    if mode & 0o555 != 0o555:
        faults.append(f'{name}: mode {mode:04o}, which not everyone can read and execute')
    return faults


def main():
    parser = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    parser.add_argument('--dist', type=Path, required=True, help='the directory GoReleaser wrote')
    parser.add_argument('--version', required=True, help='the version in the archive names, without the leading v')
    parser.add_argument('--tree', type=Path, default=ROOT, help='the repository the archives were built from')
    parser.add_argument('--commit', default='HEAD', help='the commit the archives were built from')
    parser.add_argument('--target', action='append', help='check these targets only (for the tests of this script)')
    args = parser.parse_args()
    tree = args.tree.resolve()
    commit = git(tree, 'rev-parse', '--verify', f'{args.commit}^{{commit}}').decode('ascii').strip()
    failed = False
    with tempfile.TemporaryDirectory(prefix='release-archives-') as scratch:
        wanted_files, wanted_programs = committed(tree, commit), programs(tree, commit, Path(scratch))
        for target in args.target or TARGETS:
            archive = args.dist / f'{PROJECT}_{args.version}_{target}.tar.gz'
            if not archive.is_file():
                print(f'{target}: {archive.name} is not there', file=sys.stderr)
                failed = True
                continue
            faults = check(archive, target, wanted_files, wanted_programs, built(args.dist, target), Path(scratch))
            for fault in faults:
                print(f'{target}: {fault}', file=sys.stderr)
            if faults:
                failed = True
            else:
                print(f'{target}: {archive.name} holds what commit {commit[:12]} says a release holds')
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
