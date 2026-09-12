# Named caches

Named caches preserve explicitly declared directories on a runner for later
jobs. Cache paths are excluded from uploaded snapshots and retained results,
even when an artifact declaration selects their parent. Caches are disposable:
a cold or evicted cache may require an explicit install or rebuild.

## Using a cache

In `.errand.toml`, declare a name and an exact directory relative to the workspace root:

```toml
[caches]
dependencies = "node_modules"
```

Then run native commands separately on the same runner:

```sh
errand --on mac-mini -- pnpm install
errand --on mac-mini -- pnpm test
```

Replace `mac-mini` with your configured peer. Pinning the peer keeps both commands
on the same runner; `--where` may choose a different runner for each job.
Run the install again when dependencies change or the cache is cold. Errand
does not run installs or invalidate caches when a lockfile changes.

Each ephemeral job gets a real `node_modules` directory populated from the last
saved tree. Directories and symlinks are recreated. macOS uses private native
copy-on-write clones, falling back to byte copies. Linux first tries a complete
hardlinked tree. If any link fails, the entire restore retries with private
reflinks or byte copies. The engine tests filesystem capabilities through the
operations themselves, without detecting filesystem names or tools. Cross-mount
restores use private copies. File contents and symlink targets are preserved
without interpretation.

Trees with no regular files use private-file comparison, since no inodes were
shared and no hardlink capability was exercised. If publication later cannot
create hardlinks, it retries with private copies. This changes the saved copy,
not the comparison mode of an already-linked workspace, whose files may still
share inodes with other jobs.

The same mechanism applies to any declared directory, such as `target`, `.venv`,
`build`, or a package download cache. Errand does not detect tools, inspect their
configuration, set their environment variables, rewrite their metadata, or insert
commands. If a tool needs configuration to use a particular cache location,
configure it yourself using its native options or Errand's existing environment
settings. Errand does not change runner-wide tool configuration or `HOME`.
Commands themselves retain their normal native access to the runner.

Only directories, regular files, and symlinks are supported; sockets, pipes,
and device nodes cannot be saved. The shorthand binds one exact directory:

```toml
[caches]
root-dependencies = "node_modules"
protocol-dependencies = "protocol/node_modules"
```

A new checkout or a different runner starts cold. Existing local cache contents
stay local. To retain an output, put it outside the cache in a declared artifact
path and retrieve it through `fetch`.

For an individual run, use `--cache dependencies=node_modules`. The repeatable
`--cache` flag replaces the configured list; `--no-caches` clears it. These flags
also work with `errand config` and `errand doctor`. Personal config, workspace
config, explicitly selected profiles, and CLI flags have that precedence. An
empty `[profiles.clean.caches]` table clears inherited caches. Inspection reports
bindings without creating checkout identities or contacting a runner.

A destination cannot overlap another cache, name reserved metadata, or replace
an existing submitted entry. Names are limited to 64 ASCII letters, digits,
dots, underscores, or hyphens; a job can declare at most 64 caches. Bindings
sharing a parent must use identical casing for that parent. Exclusion matches
path casing exactly. Case-insensitive filesystems reject existing entries whose
casing differs from the declared path.

## Directory groups for monorepos

Use `roots` to select existing package directories and `path` for the cache
location inside each one:

```toml
[caches.dependencies]
roots = [".", "apps/*", "packages/*"]
path = "node_modules"

[caches.package-builds]
roots = ["packages/*"]
path = "dist"
```

These groups coexist with exact bindings such as `compiler = "target"` under
`[caches]`. Each matching root gets an independent cache. Discovery uses source
package directories, so `node_modules` and `dist` need not exist locally before
installation or compilation. Errand does not discover package-manager manifests,
run installs, or infer which packages produce build outputs. Choose roots that
actually need the declared directory.

Roots are relative to the selected workspace root, even in personal config or a
profile. `.` selects that root. Patterns support `*`, `?`, and character classes
within a path component; `*` does not cross `/`, and recursive `**` is unsupported.
Only directories match, including ordinary hidden directories. Wildcards skip
reserved metadata directories (`.git` and root-level `.errand-change-*`); explicitly
naming those roots is an error. Other generated directories remain eligible, so
prefer package-specific patterns over broad roots that also match build output.
Discovery does not follow symlink directories. Every
pattern must match at least one directory; a typo or missing package group fails
with the declaration and pattern in the error. The appended `path` is always
exact and workspace-relative. Duplicate matches within a group are deduplicated;
overlaps between resolved bindings are rejected. The existing 64-cache limit
applies to the expanded list, and discovery stops at the first excess final
match. A separate shared budget of 10,000 filesystem visits (directory entries
and path lookups) bounds discovery across all groups, including intermediate
matches and nonmatching files. Exceeding it reports the group and pattern to
narrow. More than 64 intermediate directories are allowed when the final binding
count and discovery budget remain within their limits.

Generated names use the group name and a digest of the group name plus resolved
path. Their identities do not depend on checkout location, pattern order, or the
number of packages. Adding a package preserves existing caches; renaming a group
or moving a package selects a new cache. Switching an existing exact binding to
a group also selects a new cache, so run installation/build again after migrating.
Old caches remain subject to normal GC.

`errand config` lists bindings ordered by declaration source and path. JSON adds
`cache_sources`, keyed by resolved cache name, including CLI bindings. Expansion happens only for the
winning configuration layer: `--no-caches` or an empty profile table can clear
groups even when their patterns do not match this checkout.

Fresh jobs expand the groups on each invocation. Persistent workspace creation
expands and freezes its bindings. Runs that inherit those bindings and `push`
use the frozen selection without rediscovering cache roots. An explicitly chosen
profile or CLI cache override still has to agree with that selection. Packages
added later retain their files normally in the persistent tree, but gain no new
named-cache binding. Create a new workspace to use a different binding list.

## pnpm installed dependencies and download store

Caching `node_modules` preserves installed dependencies for the next command.
With pnpm's default layout, this includes its `node_modules/.pnpm` virtual store.
Use exact bindings or directory groups for nested `node_modules` directories too.

The pnpm download store is separate. Caching it saves package downloads, but
each fresh workspace still needs an install to create `node_modules`. To manage
that store through Errand, use this binding in `.errand.toml`:

```toml
[caches]
pnpm-store = ".pnpm-store"
```

Point pnpm at that directory with its native option:

```sh
errand --on mac-mini -- pnpm install --store-dir .pnpm-store
```

This example preserves only the download store. For separate install and test
jobs, start with the `node_modules` recipe above and pnpm's default runner-side
store. A workspace-relative custom store moves between ephemeral jobs, so
combining it with saved modules can leave stale absolute paths in pnpm metadata.
Errand does not rewrite those paths. Recreate the installation if pnpm rejects
it, or use a persistent workspace to keep its path stable.

You do not need an Errand cache for pnpm's default runner-side store. pnpm can
reuse that store normally; Errand's `df` and `gc cache` manage only declared
cache directories. See [pnpm's store settings](https://pnpm.io/10.x/settings#storedir)
for store locations and [virtual store settings](https://pnpm.io/10.x/settings#virtualstoredir)
for custom installation layouts.

## Concurrent jobs and saved trees

Every job has a durable holder protecting its caches from Errand GC. Independent
ephemeral and persistent workspaces can use the same named cache concurrently.
Per-cache gates coordinate restoration and publication; they never cover command
execution. Unrelated caches do not share those gates. Restoration is cancellable
staging work, outside the command launch queue.

After a successful command and confirmed process cleanup, Errand saves changed
cache layouts. An ephemeral job can transfer its completed directory directly
into cache storage on the same filesystem, avoiding a second copy. Superseded trees are detached and deleted in tracked
background work; daemon shutdown waits for that work. Unchanged
readers do not publish and cannot replace a newer saved layout. Concurrent
changed workspaces use last-completed-publication wins; their changes are not
merged. Separate named caches are published independently.

A linked tree detects changes to directory entries through names, directory
modes, symlink targets, and regular-file identities. Shared file bytes and
permissions already propagate through the inode; those changes alone do not
publish a directory layout, even if other names for that inode later disappear.
This prevents a reader from republishing an older installation because another
workspace changed a shared file. Use atomic replacement to publish private
file changes. Private clones and copies instead compare file modes, sizes, and
modification times. Neither mode hashes contents; preserving private-file size
and modification time can bypass change detection.

Jobs sharing one persistent workspace use its live directories. Publication
waits until the final member returns, so it never snapshots a directory while a
sibling command is still using it. A successful final member saves accumulated
changes, including changes made by earlier members. Removing or replacing a
cache directory behaves like doing so locally; a replacement must be a real
directory to be saved. A publication error is reported separately from runtime
cleanup: stopped jobs release their cache holders and workspace membership, so
a later job can repair the directory. Unresolved process cleanup or holder
release still retains recovery evidence.

Commands must cooperate with native shared-file behavior. Hardlinked files can
share in-place writes and permission changes across workspaces and saved trees.
Prefer atomic file replacement for private changes, and coordinate operations
that the tools themselves do not make safe together. A failed command does not
publish a new layout, but Errand cannot roll back writes to shared inodes.
Named caches do not provide isolation from uncooperative commands.

Preservation also does not guarantee relocation. Relative links work when their
relative layout is preserved; absolute links, interpreter shebangs, and embedded
workspace paths retain their original values. Tools must support that layout, or
the user must recreate it or use a persistent workspace at its original path.
Python console scripts and configured CMake trees are examples requiring this
care. A package download cache alone does not create an installed environment.

## Identity, recovery, and cleanup

A cache belongs to an authenticated owner, a stable checkout identity, and a
name. The client stores a random checkout identity in private local state keyed
by directory identity. Source edits and directory renames preserve it; separate
checkouts and clients have independent identities. Deleting that identity state
starts a fresh set of caches.

The runner hashes this structured identity into a private storage directory and
holds an exclusive filesystem lock on the store for its lifetime. Metadata and
holder files use atomic replacement and synchronized directory updates. Mutable
cache bytes are disposable, not transactionally durable. Missing published
generations recover as cold caches. Persistent restore records its comparison
mode before exposing the completed directory; a missing directory can be restored
again. Concurrent admissions share a pending preparation state until the first
member finishes preparing caches; later members preserve its live changes.
Recovery removes only the interrupted job's identified staging directory. A
missing or non-directory parent cannot contain that stage and does not prevent
holder release; a symlink parent is never followed for cleanup.

Job startup and settlement inspect only that job's cache keys. Releasing a
holder does not scan its files or release another job's holder. Closing or
restarting the daemon preserves holders. Recovery confirms process cleanup from
durable receipts before releasing them; unresolved identity or processes keep
storage protected. Shared backing storage never identifies process ownership.
The existing process-group, marker, and workspace cleanup rules still apply.

Idle legacy directory caches are adopted without discarding their payload.
Active legacy exclusive leases must settle before adoption. Old-format job and
workspace receipts retain their recovery path. New shared metadata uses v2;
older daemons cannot read it. Stop jobs and preserve or retire these caches before
downgrading a runner.

`errand df` reports named-cache usage separately from snapshot blobs. Sizes
reflect the last completed GC measurement and omit workspace copies. They count
logical regular-file bytes rather than physical disk usage or unique hardlinked
inodes. Never-measured caches appear as `unmeasured`; JSON reports the count and
verbose entries include `bytes_unknown`. Totals mark unmeasured contributions
explicitly. Active writes are not a live disk quota.

`errand gc cache --on builder --dry-run` previews snapshot and named-cache
collection. Omit `--dry-run` to collect. This uses `manage-caches` authorization
and may collect idle caches across owners. `gc all` includes the same operation.
With multiple configured runners, select `--on` or `--url`, including for dry runs.
Named-cache policy is configured independently in the runner's `errandd.toml`:

```toml
[named_cache]
max_bytes = 5368709120
ttl_hours = 336
# disabled = true
```

Zero or omitted limits use the defaults above. Disabling named caches refuses
new bindings while preserving existing data. GC measures idle trees outside the
global metadata lock, rechecks holders, then evicts expired caches followed by
least-recently-used caches until the budget is met. Held caches stay protected
even over budget. Budget enforcement requires an explicit GC command. GC also
reclaims interrupted creations, retirements, and unreferenced generations of idle
caches. Temporary file-descriptor or memory exhaustion does not classify a cache
as damaged. Unreadable or replaced idle data does not prevent collection of healthy entries,
and symlinks are not followed. Deletion runs outside the metadata lock.

Dry runs do not change data, permissions, modification times, or metadata.
Actual collection detaches entries before deleting them so interruption cannot
affect a newly created cache with the same identity. Removing backing storage
does not unlink files already restored into a persistent workspace.
