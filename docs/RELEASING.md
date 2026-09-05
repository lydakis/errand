# Releases and Homebrew

The release pipeline builds macOS and Linux binaries for amd64 and arm64,
a source archive, SHA-256 checksums, and a Homebrew formula. It creates a
**draft** GitHub release after the same macOS/Linux checks used for pull
requests pass. Publishing a stable release then triggers a validated Homebrew
tap update automatically.

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

`--snapshot` never publishes or invokes Apple signing/notarization. Its binaries
include working-tree changes, but
its source archive comes from Git. For an exact release rehearsal, use a clean
checkout at a version tag on macOS, configure signing as described below, and
run `goreleaser release --clean --skip=publish --timeout=40m`. That command submits
macOS binaries to Apple's notary service, but does not create a GitHub release.
The generated `dist/` directory is disposable and ignored by Git.

## First release

1. Merge the release machinery and Homebrew publication workflow, verify CI on
   the intended commit, and configure the tap token described below.
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

## macOS signing and notarization

Tagged releases run on macOS. After each Darwin binary is built, the GoReleaser
post-build hook signs it with Developer ID, hardened runtime, and a secure
timestamp, verifies the signature, then submits it with `xcrun notarytool`.
The hook requires an explicit `Accepted` response before GoReleaser packages
the signed binary and calculates checksums. A missing credential, failed
signature, rejection, or timeout fails the job before a draft is created.
Linux binaries and snapshots skip this hook's signing work.

The workflow uses these Errand Actions secrets:

- `APPLE_DEVELOPER_ID_CERTIFICATE_P12_BASE64`: base64-encoded PKCS#12 signing identity.
- `APPLE_DEVELOPER_ID_CERTIFICATE_PASSWORD`: password for that PKCS#12 export.
- `APPLE_DEVELOPER_ID_APPLICATION`: full Developer ID Application identity name.
- `APP_STORE_CONNECT_API_KEY_P8`: original PEM text from the downloaded `.p8` file.
- `APP_STORE_CONNECT_KEY_ID`: the matching App Store Connect key ID.
- `APP_STORE_CONNECT_ISSUER_ID`: the issuer ID for that key's team.

The preparation step imports the identity into a temporary keychain and writes
the API key with owner-only permissions. Cleanup removes both after success or
failure. For a local signed rehearsal, provide the identity name, key ID, and
issuer ID above, plus `ERRAND_SIGNING_KEYCHAIN` pointing to a keychain containing
the identity and `ERRAND_NOTARY_KEY` pointing to the PEM `.p8` file.

Apple submissions may take longer than the 20-minute wait per binary. The job
fails in that case; inspect its submission in Apple's notary history before
retrying the release workflow. No draft should exist from that failed attempt.
The native tools are used because the pinned
[GoReleaser built-in notarization step](https://github.com/goreleaser/goreleaser/blob/v2.18.0/internal/pipe/notary/macos.go)
can finish successfully while an Apple submission is still pending.

Raw executables and `.tar.gz` archives cannot have a notarization ticket stapled
to them; macOS can retrieve the ticket online. Before publishing the first
stable release, test a browser-downloaded archive on a clean Mac with normal
Gatekeeper settings. Homebrew builds from source locally and does not require
this notarization path. The pipeline does not disable Gatekeeper or clear
quarantine.

## Homebrew tap

The public tap is [lydakis/homebrew-errand](https://github.com/lydakis/homebrew-errand).
It can exist without a formula until the first stable release is published.

Set the Errand repository's Actions secret `GORELEASER_TOKEN` to a token with
Contents read/write access to `lydakis/homebrew-errand`. For a fine-grained PAT,
select that repository and the Contents permission. The workflow also accepts
`HOMEBREW_TAP_GITHUB_TOKEN` as a fallback, following the existing IceVault setup.
The ordinary `GITHUB_TOKEN` reads Errand release assets; the tap token is only
exposed to the credential check and final publication step.

The **Publish Homebrew** workflow runs on `release.published`, without scheduled
polling. It verifies that the release is stable and published, checks the source
archive and formula against `checksums.txt`, and compares the formula with the
generator's expected output. It then runs `brew style`, a source install,
`brew audit --strict`, and `brew test` on macOS before updating `Formula/errand.rb`
in the tap through the GitHub Contents API. The previous blob ID guards against
overwriting a concurrent tap edit.

Pushing a tag creates a draft release only. Drafts and prereleases do not update
Homebrew. Publishing the stable draft starts the tap update immediately; installs
become available when that workflow passes. No separate tap release is needed.

If publication fails, fix the reported problem and rerun **Publish Homebrew**
manually with the already published stable tag. The workflow uses the default
branch's validation scripts. Repeating an identical update does nothing, an older
release cannot downgrade the tap, and a changed formula for the same version
requires manual review rather than an overwrite. For token failures, check its
expiration and repository access, then rerun; do not recreate the Errand release.

After the first successful tap update, users can install or upgrade with:

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
