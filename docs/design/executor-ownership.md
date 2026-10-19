# Inactive executor ownership primitive

`internal/runner/executor_lease*.go` is a foundation for a future trusted host
integration. No current Runner startup, Runtime invocation, Jobs dispatcher,
configuration, HTTP route or capability uses it. A lease is not an execution
permission, policy decision, governance capability or proof that a particular
Runtime ran. Existing portable-store instance locking and recovery behavior are
unchanged.

## Authority and provisioning

The trusted host provisions one existing private directory outside portable
Runner storage, and one existing regular file named `<workspace>.lease` for each
workspace it enrolls. The directory and leaf belong to Runner's effective OS
user, grant no group/other permissions, and the leaf has exactly one hard link.
The host separately retains `ExecutorLeaseIdentity`: the workspace identifier
and the directory's and leaf's device/inode pairs. This retained configuration
must survive independently of portable stores, backups, imports and execution
requests. It is the host's responsibility to select the enrolled workspace and
the expected identity from that trusted configuration.

The package deliberately exposes no provisioning, identity-discovery, reset,
takeover or lock-file recreation operation. Computing a new expected identity
from a caller-selected path, or trusting values restored alongside a store,
would defeat the design. Copied file contents have no authority; the primitive
does not read them. A PID, timestamp, deadline, random identifier or SQLite
record cannot replace the kernel lock. Passing an independently pinned identity
to the primitive does not establish that the caller was authorized to obtain it.

The host must keep the enrolled root and leaf in place and retain the same
trusted pins. Replacing or losing them is a refusal, requiring a separately
designed recovery/enrollment decision. Device/inode pins are local filesystem
identities, not permanent cryptographic identities: deleting an enrollment and
later reusing its inode, changing filesystems or restoring the machine's trusted
configuration is outside this primitive's continuity guarantee. Do not copy the
external enrollment as part of portable Runner state.

## API and lifetime

`AcquireExecutorLease(root, stateDir, workspace, expected)` opens the existing
directories and `<workspace>.lease`; it never creates, repairs, chmods or writes
them. Both directories must be absolute, clean real paths with no observed
symlink component, private to the effective user, and physically disjoint: the
external root cannot contain the portable state directory or be inside it.
`stateDir` must already exist; it supplies placement context and does not supply
enrollment authority. Paths are bounded to 4,096 bytes and 128 components.
Workspace identifiers are 1–128 ASCII letters, digits, underscores or hyphens,
and must equal the independently retained identity's workspace.

Directories are opened one component at a time from an opened `/`, using
`os.Root`. Each component's `Lstat` directory identity must match the opened
directory. Held root and state descriptors pin the original directories. The
leaf is opened relative to the held external root with no-follow and
nonblocking flags, then checked against its observed identity, private custody
and the trusted device/inode pin. A FIFO or another special-file replacement is
refused without waiting for I/O. No leaf contents are read.

Acquisition takes `flock(LOCK_EX | LOCK_NB)` on that existing leaf. An unavailable
or unsupported lock is a refusal. It then revalidates held objects, names and
directory ancestry. Independent opens in the same process also contend. Two
Runner instances naming different copied portable stores but the same enrolled
workspace contend on this one external lease.

`lease.Start(cmd)` revalidates custody, the pinned root/leaf, the held state
directory and their current physical paths. It temporarily appends the lease's
descriptor after `cmd.ExtraFiles`, calls `cmd.Start`, restores the original
slice, and returns the descriptor number seen by the child. Existing entries
keep their order and meaning. The lease mutex stays held through the entire
inheritance boundary; `Close` cannot close the source descriptor before a
successful child inherits it. A failed `Start` leaves the parent's lock held.
The caller must follow `exec.Cmd`'s ordinary restriction against concurrent use
of the same command. `Close` is idempotent and synchronized with `Start`.

Parent and child descriptors refer to the same locked open description. Closing
the parent's copy, or killing the parent, does not release ownership while the
child retains its copy. `Close` **never calls `LOCK_UN`**, because explicitly
unlocking that shared description would also remove the child's protection.
The kernel releases the lock after the last inherited copy closes, including
at process death. The parent must close its own copy when appropriate; a parent
that retains it can continue to exclude competitors after its child exits.

The actual worker must preserve the inherited descriptor until its protected
work ends, must never unlock it, and must pass it through every wrapper that
outlives its parent. A wrapper that closes it before its actual worker exits
breaks the intended lifetime. A child or descendant that accidentally keeps a
copy after finishing work delays reacquisition. The helper neither identifies
trusted programs nor proves descriptor preservation by a Runtime. A future
host integration needs a concrete inherited-descriptor contract and tests for
the exact wrapper and Runtime launch chain before advertising any capability.

## Supported boundary and remaining integration

The implementation supports Linux, macOS and FreeBSD on local filesystems that
provide coherent advisory `flock` locks associated with inherited open file
descriptions. Other platforms fail closed. Network filesystems, emulated or
userspace locking, filesystem snapshots/clones that duplicate device/inode
identities, mount-namespace aliases and concurrent mount replacement have no
support claim. The trusted host must choose a suitable local filesystem and
stable mount namespace. No active filesystem-type probe certifies these
assumptions.

This is cooperative process exclusion under a trusted effective OS user. It
does not resist an administrator, malicious code running as that user, a worker
that closes/unlocks its descriptor, or a writer replacing the machine's trusted
configuration. Ancestor directories can include shared sticky temporary
directories; the pinned identity, private final directories and repeated
descriptor/name checks establish continuity. Checks observe filesystem state;
they do not prevent later mutation. A replacement observed before launch is
refused, and replacing a name does not move an existing holder's kernel lock to
the replacement. There is no automatic fallback to a newly named object.

A future host must still enforce pre-start admission, bind the correct trusted
workspace identity, preserve the descriptor through the full worker lifetime,
and define enrollment/recovery separately. Current Runner versions can execute
without this inactive helper; an old binary or another code path can ignore an
advisory lock. Capability probing must therefore fail closed before any future
execution path relies on this primitive, including when selecting an older
Runner. No downgrade protection, execution capability or Jobs integration is
introduced here.

## Focused verification

The `TestExecutorLease*` tests exercise pinned private custody, missing objects,
symlink and hard-link refusal, special files, disjoint placement, copied roots,
replacement while held, failed launch, and existing `ExtraFiles`. Foreign-owner
fixtures require OS privileges and skip when those are unavailable.

Real test subprocesses exercise normal parent closure and `SIGKILL` of an
intermediate owner while its actual child stays alive. Both the original and a
copied portable store are refused until the child exits. Pipe messages and EOF
synchronize these observations; process IDs and elapsed time never determine
eligibility. A separate race test exercises concurrent `Close` and `Start`.

```sh
GOCACHE=/tmp/jps-policy-coverage-go-cache go test ./internal/runner -run '^TestExecutorLease' -count=1
GOCACHE=/tmp/jps-policy-coverage-go-cache go test -race ./internal/runner -run '^TestExecutorLease' -count=1
```

Running these tests on Linux demonstrates the tested Linux behavior. Compile
checks for macOS/FreeBSD and unsupported-platform stubs do not substitute for
running the process-lifetime suite on those hosts and their chosen filesystems.
