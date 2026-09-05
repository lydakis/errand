# Named caches

Named caches keep disposable build data on a runner for later jobs. They are
separate from retained results: snapshots never upload cache paths, and fetch
never downloads them, even when an artifact declaration selects their parent.
Losing a cache should only make the next build slower.

## Using a cache

Declare a name and an exact directory relative to the workspace root:

```toml
[caches]
compiler = "target"
```

Then run normally:

```sh
errand --on builder -- cargo build
errand --on builder -- cargo test
```

Both jobs use the same runner-side `target` directory. A new checkout or a
different runner starts cold. Existing local `target` contents stay local.
To keep a final binary, copy it outside the cache into a declared artifact path
as part of the command, then retrieve it through `fetch` as usual.

For an individual run, use `--cache compiler=target`. The repeatable `--cache`
flag replaces the configured list; `--no-caches` clears it. These flags also work
with `errand config` and `errand doctor`, using the shared run resolver.
Personal config, workspace config, explicitly selected profiles, and CLI flags
have that precedence. A `[profiles.clean.caches]` table with no entries clears
inherited caches. Inspection reports bindings and their source without creating
project identities or contacting a runner.

Bindings use directory symlinks on Linux and macOS. Commands can create and
modify files through the declared path; they should leave the binding itself
in place. Removing or replacing that symlink does not replace the stored cache.
A cache destination cannot overlap another cache, name reserved metadata, or
replace an existing submitted entry. Paths are exact, without globs, and names
are limited to 64 ASCII letters, digits, dots, underscores, or hyphens.
At most 64 caches may be bound by a job.

Bindings that share a parent must use the same casing for that parent. For
example, `Build/one` and `build/two` are rejected on both platforms because the
created parent directories must also fit the portable retained-result format.

Exclusion matches path casing exactly. On a case-sensitive filesystem, a `build`
cache leaves a separate `Build` directory in source snapshots and retained
results. On a case-insensitive filesystem, existing entries whose casing differs
from a declared cache path are rejected during selection, binding, or collection.

## Storage identity and leases

Each cache belongs to an authenticated owner, a stable checkout identity, and
a name. The client persists a random checkout identity in its private local
state, keyed by filesystem directory identity. It survives ordinary source
edits and directory renames. Separate checkouts and separate clients get
independent identities; project labels and snapshot hashes do not select caches.
Deleting that local identity state starts a fresh set of caches.

The store hashes this structured identity into a directory name. Metadata sits
beside the writable data. The private store root is held by an exclusive
filesystem lock for the daemon's lifetime.

A job acquires durable exclusive leases just before execution. A concurrently
leased cache fails the job before its command starts; Errand does not wait for
that cache. Partially acquired leases are settled on setup failure. Independent
owners, checkouts, and names do not contend for the same cache.

After the entire process scope is confirmed stopped, `Release` measures regular
file bytes without following symlinks and clears the lease. Failed commands keep
usable cache contents too. If the contents cannot be measured, the daemon
attempts to discard that cache. An unresolved storage failure remains a receipt
cleanup failure. The store's byte budget is not a runtime quota.

Closing or restarting the daemon does not clear leases. Restart recovery uses
persisted receipts and process-scope cleanup before settling leases. Missing
receipt identity or unconfirmed process cleanup leaves the cache protected from
reuse and eviction. Process tracking includes the workspace and the canonical
data directories currently leased by the job. Recovery derives these directories
from durable leases, so an old receipt cannot claim a cache another job has since
acquired. Linux recognizes working directories anywhere beneath these roots;
macOS recognizes exact root working directories in addition to the inherited
process marker. Existing limitations of process-scope tracking still apply;
named caches do not add process isolation.

Metadata uses synchronized temporary files and atomic rename. An error after
rename can leave the update visible, so the lifecycle reads back lease state
before retrying or discarding. Mutable cache contents are disposable and are
not transactionally durable.

## Usage and cleanup

`errand df` shows named-cache bytes separately from the shared snapshot cache,
plus the number of protected leases. Named-cache inventory is scoped to the
caller. Active-cache byte counts reflect the last release, not ongoing writes.
JSON output adds `named_caches` with `items`, `bytes`, and `protected`.

`errand gc cache --on builder --dry-run` previews collection of both snapshot
blobs and named caches. Omit `--dry-run` to collect them. This uses the existing
`manage-caches` authorization and may collect idle caches across owners.
`gc all` includes the same operation. Named cache TTL and budget are separate
from the snapshot cache, configured in the runner's `errandd.toml`:

```toml
[named_cache]
max_bytes = 5368709120
ttl_hours = 336
# disabled = true  # refuse new jobs with cache bindings; preserve existing data
```

Zero or omitted size and TTL use the defaults shown above. Collection removes
expired idle caches first, then least recently used idle caches until recorded
bytes meet the budget. Leases remain protected, even over budget. Collection
runs when explicitly requested through `gc`, not on every cache acquisition.
Disabling snapshot caching does not disable named caches, or vice versa.

Dry runs leave data, permissions, modification times, and metadata unchanged.
Actual collection renames entries before deleting them, so interruption cannot
affect a newly created cache with the same identity. Later GC also reclaims
interrupted creations and retirements. The GC response counts removed blobs,
removed named caches, protected caches, and interrupted cleanups separately;
freed bytes sum newly selected snapshot blobs and named cache contents, using
recorded cache sizes. Interrupted cleanup bytes are not included in that total.
