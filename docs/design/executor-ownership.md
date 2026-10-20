# Workspace executor ownership

A trusted host can explicitly bind a Runner workspace to an existing external
executor lease. Runner acquires it before opening portable state, then retains it
for the Service lifetime and through every actual Runtime process it launches.
A copied store cannot acquire that same enrolled lease while either its Service
or an inheriting Runtime remains alive. Unconfigured workspaces retain existing
portable-store locking and recovery behavior.

Ownership is cooperative process exclusion. It is not execution permission, a
policy decision, governance admission or authorization of a particular job.
Ownership alone leaves jobs ungoverned. The separate experimental
[manual gate](manual-admission-gate.md) requires retained enrollment, matching
boot and execution demands, and fresh anchored authority admission for the sole
profile `local-admin-manual-pack/1`. A capability response grants no permission.

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


## Trusted boot and Service integration

The private [capability response](runner-capabilities.md) includes
`executor-ownership/1`: supported admission hosts report closed version 3 with
`governanceAdmission: true` scoped solely to `local-admin-manual-pack/1`; hosts
without admission support retain ownership-only closed version 2 with
`governanceAdmission: false`. The trusted host probes an immutable executable
snapshot before sending any boot authority, then repeats the requirement in boot.
The [shared boot fixture](executor-ownership-boot.json) defines the additional
members, alongside the established boot configuration:

- `requiredCapabilities` remains version 1 and explicitly requires
  `executor-ownership/1` if and only if `executorOwnership` is present.
- `executorOwnership` is a closed object with exactly `version: 1`, `root`,
  `machineWorkspace`, `runnerWorkspace`, `rootDevice`, `rootInode`, `fileDevice`
  and `fileInode`. All names are case sensitive; aliases, duplicates and unknown
  members refuse.
- Device/inode values use canonical unsigned decimal strings from `"0"` through
  `"18446744073709551615"`, without signs, whitespace or leading zeroes. Inodes
  must be positive. JSON numbers refuse, avoiding JavaScript rounding.
- `machineWorkspace` selects `<machineWorkspace>.lease` in `root`.
  `runnerWorkspace` must exactly equal the established boot `workspace`. These
  identify distinct host and Runner records; one must not be substituted for the
  other. The root is a clean absolute path within the primitive's bounds.

`runner.Config.ExecutorOwnership` carries this typed trusted binding. `Open`
defensively copies it, validates the exact Runner identity, then acquires the
existing external lease before state creation, configuration/provider processing,
store access/migration, Runtime pinning, recovery or workers. Configured state
must already exist as a private real directory. Runner never creates an enrollment
or discovers replacement pins. Any failed `Open` closes its own copies.

Every Service Runtime call uses the common ownership-aware invocation boundary:
validation, reviewed-set locking, rehearsal, saved tests, mapping coverage,
artifact version checks, Runtime version/binding checks, operational evaluations,
and the private workflow host adapter. Signing inputs, diagnostics, output limits
and subprocess deadlines keep their established behavior. Standalone offline
verification APIs have no Service configuration or trusted ownership authority.

`Close` closes the Service's lease copy after stopping its workers. A request
invocation already running can outlive that close; its inherited descriptor keeps
the external lock until the actual Runtime exits. The configured lease reference
is never cleared. A later call through the stopped Service fails at the inheritance
boundary; it cannot start a process through the unconfigured path. A failed lease
validation or process start likewise never retries without ownership. Configuration
and request data cannot replace or erase the binding after `Open`.

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
trusted programs nor proves descriptor preservation by a Runtime. The
Service launches the exact pinned Runtime directly, with the descriptor inherited
through this boundary. Its real-Runtime tests below verify preservation for the
Runtime under test; any new wrapper or Runtime that closes inherited descriptors
requires equivalent verification before using this capability.

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

The trusted host must enforce exact enrollment and capability negotiation before
boot. An old Runner or another code path can ignore this advisory lock, so the
host must refuse an old/unknown capability response and retain its required
capability on every launch or retry. No reset, takeover, automatic repair or
fallback enrollment is provided.

Pre-start governance admission, authenticated executor epochs, exact release and
adoption registration, durable attempts and authority `commitStart` remain future
work. External ownership does not implement those permissions.

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
GOCACHE=<scratch> go test ./internal/runner -run '^TestExecutorLease' -count=1
GOCACHE=<scratch> go test -race ./internal/runner -run '^TestExecutorLease' -count=1
```

Running these tests on Linux demonstrates the tested Linux behavior. Compile
checks for macOS/FreeBSD and unsupported-platform stubs do not substitute for
running the process-lifetime suite on those hosts and their chosen filesystems.

`TestExecutorOwnedService*` verifies early acquisition, immutable configuration,
failed-start cleanup, real release validation/locking/rehearsal/saved tests, normal
operational dispatch, version checks, private workflow evaluation and refusal after
close. `TestExecutorOwnershipBoot*` checks the strict shared wire before any state
side effect. Existing primitive tests remain the source for custody and Start/Close
race cases; they are not duplicated by Service integration.

On Linux, `TestExecutorOwnedRealRuntimeSurvivesOwnerDeath` runs a real Service in an
isolated subprocess and invokes the exact pinned real Runtime's `audit verify`.
The observer holds an independent audit-file flock, then observes the child's
executable identity, opened audit inode and inherited executor inode through
`/proc`. It closes or kills the Service owner while the real Runtime stays blocked.
Opening both the original and a copied store refuses. After releasing only the
audit blocker and reaping Runtime, actual executor acquisition succeeds. An
isolated Linux subreaper handles descendants on success and failure. Observed
PIDs, process state and deadlines synchronize/bound the fixture; none grants
eligibility. Runtime writes its output only after the blocked audit operation, so
owner death cannot trigger a premature stdout failure at the protected boundary.
The integration requires `JPACK_TEST_BIN` and does not substitute a mock process.

```sh
GOCACHE=<scratch> JPACK_TEST_BIN=/absolute/path/to/jpack \
  go test -race ./cmd/jpack-runner ./internal/runner -run '^TestExecutorOwn' -count=1
```

The Linux process observation fixture is not compiled on macOS/FreeBSD. The real
Service evaluation tests run on supported hosts when their real Runtime is
configured; the primitive's portable process tests remain in place. Compile
checks alone do not establish a filesystem or Runtime lifetime guarantee.
