# Named cache implementation

The storage and lease foundation lives in `internal/namedcache`. Job bindings,
configuration, and the `df` / `gc cache` connection are the next slice. Named
caches are not yet available to jobs through the CLI.

## Storage identity

A cache belongs to an authenticated owner, a stable project identity, and a
name. All three participate in the storage key. The project identity must
survive ordinary source changes; neither the snapshot hash nor a human-readable
project label is suitable. The job integration will derive and persist this
identity on the client and carry it in the job spec.

The store hashes the structured identity into its directory name. Metadata is
kept beside the writable data directory. Names are bounded to 64 ASCII letters,
digits, dots, underscores, or hyphens, excluding `.` and `..`. Owner and project
identities are required and bounded to 512 bytes each. The store root is private
and held by an exclusive filesystem lock for its lifetime.

## Leases and recovery

`Acquire` durably records a job ID before returning the writable cache path.
A second acquisition of the same identity returns `ErrBusy`; independent
owners, projects, and names use independent directories. The store does not
wait for a running job to relinquish a cache.

The job lifecycle must stop the entire process scope before calling `Release`
or `Discard`. `Release` measures regular-file bytes without following symlinks,
records last use, and clears the lease. Measurement errors preserve the lease.
`Discard` removes an unusable cache using that same job ID, including when the
job removed its data directory. A mismatched job ID cannot settle a lease.

Closing or reopening the store does not clear leases. After a daemon restart,
unresolved leases remain unavailable for reuse and protected from eviction.
The integration must reconcile process scopes before explicitly settling them;
if cleanup cannot be confirmed, the lease stays protected. This also applies
to setup failures after acquisition and before a process starts.

Metadata updates use temporary files, file synchronization, and atomic rename.
An I/O error after rename can leave the update visible, so callers must use
`Inventory` for readback before retrying an uncertain lease transition. The
store does not promise transactional durability for the mutable cache contents.

## Eviction and inventory

GC selects idle entries that exceed the configured TTL, then selects the least
recently used idle entries until the recorded byte total meets the budget.
Leased entries are always skipped, even when they are expired or over budget.
The budget is an eviction target, not a runtime disk quota. Active-cache byte
counts reflect their last release and do not measure ongoing writes.

Dry runs select the same entries without changing data, permissions, timestamps,
or metadata. Actual eviction first renames a cache to a unique retirement
directory, so interrupted deletion cannot affect a newly created cache with
the same identity. Later GC also reclaims abandoned creations and retirements.
`Removed` and `FreedBytes` describe newly selected caches using recorded data
sizes; `ReclaimedTemps` separately counts interrupted work being cleaned up.

`Inventory` is internal and includes all owners. The daemon must filter it by
the authenticated caller before exposing usage. Cache-management authorization
and the existing snapshot-cache budget remain separate integration concerns.

## Next slice

Connect declarations to job admission, acquire leases before execution, settle
them after confirmed cleanup, and recover them through the existing receipt
lifecycle. Cache contents must stay out of snapshots and retained changes,
including artifact declarations. Expose usage and eviction through the existing
`df` and `gc cache` commands, with explicit reporting for protected caches.
