# Sources clean release component scope

The active `Sources clean release` workflow builds and verifies the complete Main/Portal bundle. Publication scope is resolved from the exact source checkout before publication. `.github/release-tools/component-scopes.json` pins **0.2.62 to Main only**; all unlisted versions retain the existing Main + Portal default. The manual `components` input accepts `auto`, `main` or `both`; an explicit value cannot contradict a pinned version.

`clean_components.py` wraps the existing native image publisher and asset uploader. Main-only scope never inspects or builds the Portal registry image. The image receipt records `publication_scope` and a source/version/manifest-hash bound `component_selection`; retries cannot expand or narrow that scope. Release metadata requires exactly the selected, registry-verified component tags and keeps immutable-asset full readback. Image repositories come from the repository-root `.sources-identity.json`, the same file the workflow checks against `github.repository`. A built Portal binary does not imply a published/deployed Portal image. No Latest promotion or production cutover is performed.

Older immutable source refs without this helper retain the original two-component path. The workflow rejects requesting Main-only for such refs rather than silently publishing both.

Read-only selection checks:

```sh
python3 .github/release-tools/clean_components.py resolve --version 0.2.62
# main
python3 .github/release-tools/clean_components.py resolve --version 0.2.63
# main,portal
```

Run `python3 -m unittest discover -s .github/release-tools -p 'test_release_matrix.py'` on Linux/Bash 4+. The suite exercises the real native publisher/uploader with fake Docker/GitHub transports, including Main-only registry calls, both-component defaults, immutable tags/assets, full readback and scope-safe retries. The older matrix shell helper uses Bash 4 lowercase expansion and does not run under macOS's bundled Bash 3.

# Legacy release matrix helpers

The release workflow builds the frontend once, then runs each configured Go target on its own Linux runner. `CGO_ENABLED=0` permits cross-compilation. The target matrix is read from `.goreleaser.yaml`, including its exclusions; simple releases select Linux amd64 only.

Each build uses GoReleaser OSS in snapshot mode with the selected release version and one target. Archive naming, bundled files, Go flags and release templates remain in the existing GoReleaser configurations. Every archive is accompanied by its source commit, target, version and SHA256. The publishing job verifies the complete matrix before building images or publishing. It uses GoReleaser's `extra_files` support to publish existing archives and checksums, with builds disabled. No Pro license is needed.

Go caches are isolated by target and refreshed on each source commit, with fallback to the preceding target cache. Save uses the original restore key, even if a build hook changes `go.sum`. Matrix jobs upload uniquely named artifacts. The publishing job extracts only the regular Linux binary from each verified archive and restores its executable permission before constructing Docker contexts. QEMU remains limited to runtime-image instructions. DockerHub images are omitted when its credentials are absent; GHCR is always retained. Simple mode still publishes only the amd64 GHCR image and the simple release description.

All build jobs use the commit resolved by `prepare`, including a manual release's selected tag. Helper scripts come from the workflow revision and are passed as a run-local artifact, so older application tags do not need to contain the new scripts. The workflow serializes release runs to prevent simultaneous updates to moving image tags.

## Validate without publication

From a branch containing this workflow:

```bash
gh workflow run release.yml --ref <branch> \
  -f tag=<branch> -f dry_run=true -f simple_release=false
```

A dry run builds all selected archives and both runtime images, verifies artifact provenance and produces the final checksum file. It exports images locally as OCI archives instead of pushing them. It skips registry logins, GitHub Release publication, DockerHub description updates, Telegram notifications and VERSION synchronization. Test the simple path separately with `simple_release=true`.

Dry-run artifacts are available in the Actions run, including `release-dry-run-report`. Compare job start/end times, GoReleaser's build duration and cache restore results. Do not present an initial cold-cache run as a warmed-cache benchmark; publishing network time is not measured by dry runs.

Helper checks:

```bash
python -m pip install -r .github/release-tools/requirements-release.txt
python -m unittest discover -s .github/release-tools -p 'test_release_matrix.py'
bash -n .github/release-tools/release-images.sh
```
