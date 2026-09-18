# Exchange replacement experiment

This candidate is **not enabled in production**. `candidate.patch` applies only
to the frozen `parents` source in
`docs/benchmarks/apply-followup/inputs/parents/inputs.tar.gz`. The matching frozen
`exchange` source includes the patch and tests. Use the input identities in those
directories, rather than a moving checkout, to reproduce it.

The candidate probes exchange support using two private transaction files before
publishing exchange intent. Unsupported syscall/filesystem errors select the
normal multi-parent group protocol. Other errors abort. An error after mutation
uses recovery; it never switches protocols halfway through a transaction.

## Evidence and ordering

An exchange leaves both names present. Before mutation, the journal binds each
item's original and staged inode identities, expected content, and parent identity
under a distinct `group-exchanging` phase. Mixed group/exchange intent and missing
exchange identities are rejected. Staged values retain the production durability
ordering.

For each item, recheck the original and parent, synchronize original file data,
exchange destination and staged value, rename the displaced value to `previous`,
then synchronize and validate the backup and synchronize its item directory.
Complete a barrier on every changed parent before validating installation and
publishing the normal committed outcome. This removes the per-item backup-parent
barrier but does not remove backup-data synchronization.

If a crash occurs between exchange and backup normalization, recovery uses inode
evidence to distinguish untouched staged bytes from displaced original bytes.
Ambiguous evidence is retained. Recovery first normalizes a recognized displaced
value to `previous`, synchronizes it, then uses the existing quarantine, content
check and no-replace restoration protocol. It does **not** exchange backwards:
that could overwrite a concurrent edit between checking and swapping.

The tests cover actual process exits before/after exchanges, before/after backup
normalization, group publication, and commit, for flat and multiple-parent trees.
They also cover unsupported-operation fallback, replaced staged evidence,
intervening edits before installation, after installation, and inside the final
pre-exchange window, plus historical outcome replay after a later edit.

## Adoption boundary

Process exits do not simulate power loss. Before production adoption this needs a
separate review of cross-directory exchange persistence on APFS and Btrfs.
In particular, enumerate the states a power loss before the final parent barrier
can expose when the item-directory fsync has completed but the destination-parent
fsync has not; process exits do not exercise that ordering. Also cover
interrupted rollback/cleanup, corrupted or mixed evidence, and fallback behavior
on unsupported filesystems. The forced `ENOTSUP` test proves dispatch behavior;
it is not a measurement on such a filesystem. Keep this larger recovery-protocol
change separate from the scratch and multi-parent production changes.

Review follow-ups before adoption: assert every sibling and pending-transaction
state in the concurrent-edit tests, cover later deletions and parent replacement
with the shared recovery work, and choose an explicit rollout/capability policy.
The frozen prototype also retains the old fabricated `unused` child in parent
errors; port the production directory helper when rebasing it for adoption.
Keep this patch and its frozen inputs unchanged so its published measurements
remain reproducible.

Run the isolated candidate tests after extracting its verified frozen inputs:

```sh
go test ./internal/changes -run '^Test(Exchange|Merge|Transfer|Apply|CopyToRoot)' -skip '^TestApplySynchronization' -count=1
go test -race ./internal/changes -run '^TestExchange' -count=1
```

Old grouped tests assert the reference rename sequence and intentionally do not
apply to the exchange strategy. Production still runs those tests unchanged.
Native comparison commands and results are in `docs/APPLY_FOLLOWUP.md`.
