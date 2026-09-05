# Releases and Homebrew

The release pipeline builds macOS and Linux binaries for amd64 and arm64,
a source archive, SHA-256 checksums, and a Homebrew formula. It creates a
**draft** GitHub release after the same macOS/Linux checks used for pull
requests pass. Publishing the release and updating the tap are separate steps.

## Local rehearsal

Use GoReleaser 2.18.0, Go from `go.mod`, Python 3, and Ruby:

```sh
goreleaser check
python3 -m unittest discover -s scripts -p 'test_*.py'
goreleaser release --snapshot --clean
version=$(python3 -c 'import json; print(json.load(open("dist/metadata.json"))["version"])')
python3 scripts/homebrew_formula.py "$version" "dist/errand_${version}_source.tar.gz" dist/errand.rb
ruby -c dist/errand.rb
(cd dist && shasum -a 256 -c checksums.txt)
```

`--snapshot` never publishes. Its binaries include working-tree changes, but
its source archive comes from Git. For an exact release rehearsal, use a clean
checkout at a version tag and `goreleaser release --clean --skip=publish`.
The generated `dist/` directory is disposable and ignored by Git.

## First release

1. Merge the release machinery and verify CI on the intended commit.
2. Choose a version and push its tag, for example `v0.1.0`. Stable tags use
   `vMAJOR.MINOR.PATCH`; prereleases can use `v0.1.0-rc.1`. Do not move an
   existing release tag. The workflow runs only for pushed `v*` tags.
3. Review the resulting draft on GitHub. It contains four binary archives,
   `errand_VERSION_source.tar.gz`, `checksums.txt`, and `errand.rb`. All archives
   and the formula are covered by `checksums.txt`. Binary archives contain
   `errand`, `LICENSE`, and `README.md`; `errand version` reports the tag version
   without the leading `v`.
4. Download the draft assets and verify checksums and native binaries. Review
   the generated release notes, then publish the draft. Prerelease tags are
   marked as prereleases automatically.

The workflow uses the repository's standard `GITHUB_TOKEN`; no personal token
is required to create the draft. A rerun does not overwrite an existing
release. If a failed run left an incomplete draft, inspect it and delete only
that draft before rerunning. Keep the tag intact.

macOS binaries are not Developer ID signed or notarized. Homebrew builds from
source locally. Signed and notarized direct downloads are a separate release
capability; this pipeline does not disable Gatekeeper or clear quarantine.

## Homebrew tap

The planned tap is `lydakis/homebrew-errand`; it must be created before the
first Homebrew publication. The release workflow generates a ready-to-review
`errand.rb` instead of writing to a second repository with additional credentials.

After publishing a **stable** GitHub release:

1. Download that release's `errand.rb` and `checksums.txt` and verify the
   formula's checksum. Copy the formula to `Formula/errand.rb` in the tap.
2. Run `brew style`, `brew audit --strict`, a source install, and `brew test`
   against the tapped formula. Commit and push the tap update after validation.
3. Users can then install or upgrade with:

   ```sh
   brew install lydakis/errand/errand
   brew upgrade lydakis/errand/errand
   ```

The formula downloads the release's exact source archive, checks its SHA-256,
and uses Go as a build dependency. It supports the host macOS/Linux architecture.
It installs the CLI only; it does not start a runner, modify its authorization,
or remove runner receipts and config on uninstall. Do not publish a prerelease
formula over the stable tap entry.

## Runner upgrades

Install first, then run `errand setup` on machines that should accept jobs.
Setup owns the service; do not also run it through `brew services`.

When invoked through the stable Homebrew path, setup records that path rather
than the versioned Cellar executable. After `brew upgrade`, run `errand setup`
when the runner is idle to restart it on the new version. Setup preserves the
existing runner config and refuses to restart while jobs are active. Callers
need no service restart.

If a service was previously installed from a versioned or temporary executable
path, inspect its definition and update that executable path while preserving
the config path. Do not use `setup --force` solely for an upgrade: it can also
rewrite the runner config.

Packaging references: [GoReleaser source archives](https://goreleaser.com/customization/package/source/),
[Homebrew formula cookbook](https://docs.brew.sh/Formula-Cookbook), and
[maintaining a tap](https://docs.brew.sh/How-to-Create-and-Maintain-a-Tap).
