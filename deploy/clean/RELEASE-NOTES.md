# Sub2API (nailao fork) release

This release is built from the tagged commit of [code-nailao/sub2api-nailao](https://github.com/code-nailao/sub2api-nailao), a fork of [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) distributed under the LGPL-3.0. Changes since the previous release are listed in the commit history between the two tags.

## Contents

- **Main image**: the Sub2API server binary and its resources, layered on the pinned upstream runtime image.
- **Portal image**: the companion portal service, published together with Main when it changed.
- **Release assets**:
  - `release-images.json`: for each image, the repository, tag, `sha256` digest, platform and source commit.
  - `build-manifest.json`: SHA-256 hashes of every build input and output, including the static web assets.
  - `core-source.tar.gz`: the complete source of the tagged commit.

Images are published only under a fixed version tag and are never promoted to a floating tag. This release is not marked Latest; publishing it does not deploy it anywhere.

## Verifying an image

Always pull and deploy by digest, never by tag:

```bash
gh release download <tag> --repo code-nailao/sub2api-nailao --pattern release-images.json
jq -r '.images[] | "\(.repository)@\(.digest)"' release-images.json
docker pull ghcr.io/<owner>/<image>@sha256:<digest>
docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' ghcr.io/<owner>/<image>@sha256:<digest>
```

The printed revision must equal `source_commit` in `release-images.json` and the commit the release tag points to. `sha256sum core-source.tar.gz` and the hashes in `build-manifest.json` let you compare the published artifacts with a local build of the same commit.
