#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
# The source archive and compiled files must refer to the same reviewed tree.
git diff --quiet HEAD -- .
test -z "$(git ls-files --others --exclude-standard)"
OUT=${1:?usage: build.sh NEW_OUTPUT_DIRECTORY}
test ! -e "$OUT"
mkdir -p "$OUT/main" "$OUT/portal" "$OUT/static"
SHA=$(git rev-parse HEAD)
DATE=$(git show -s --format=%cI HEAD)
VERSION=$(tr -d '\r\n' < backend/cmd/server/VERSION)
SOURCE_DATE_EPOCH=$(git show -s --format=%ct HEAD)
export SOURCE_DATE_EPOCH
# Builds use the repo-pinned pnpm; no moving dependency upgrade or production data.
corepack pnpm@9.15.9 --dir frontend install --frozen-lockfile
corepack pnpm@9.15.9 --dir frontend run build
corepack pnpm@9.15.9 --dir extensions/sources-portal/webapp install --frozen-lockfile
corepack pnpm@9.15.9 --dir extensions/sources-portal/webapp run build
(cd extensions/sources-media-engine && go test -race ./... && go vet ./...)
(cd extensions/sources-portal && go test -race ./... && go vet ./...)
python3 scripts/sources_extensions/core_parity.py
python3 scripts/sources_extensions/check_boundaries.py
python3 -m unittest discover -s scripts/sources_release -p 'test_*.py'
(cd backend && go test -json -race -mod=readonly -p "${GO_TEST_P:-2}" -tags embed -count=1 -timeout=3m ./internal/web)
(cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -tags embed -trimpath \
  -ldflags "-s -w -X main.Version=$VERSION -X main.Commit=$SHA -X main.Date=$DATE -X main.BuildType=release" \
  -o "$OUT/main/sub2api" ./cmd/server)
(cd extensions/sources-portal && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -o "$OUT/portal/sources-portal" ./cmd/portal)
cp -a backend/resources "$OUT/main/resources"
# Keep the exact files embedded above for Nginx staging without a second build.
# They are covered by the bundle manifest and stay outside the image context.
cp -a backend/internal/web/dist "$OUT/static/main"
cp -a extensions/sources-portal/internal/portal/homeweb "$OUT/portal/homeweb"
cp deploy/clean/Dockerfile.main "$OUT/main/Dockerfile"
cp deploy/clean/Dockerfile.portal "$OUT/portal/Dockerfile"
git archive --format=tar HEAD | gzip -n > "$OUT/core-source.tar.gz"
python3 - "$OUT" "$SHA" "$VERSION" "$SOURCE_DATE_EPOCH" <<'PY'
import hashlib,json,os,sys
from pathlib import Path
sys.path.insert(0, 'scripts')
from sources_baseline import load_baseline
baseline=load_baseline()
root=Path(sys.argv[1]);files={str(p.relative_to(root)):hashlib.sha256(p.read_bytes()).hexdigest() for p in root.rglob('*') if p.is_file()}
manifest={'schema':1,'component':'sources-core','source_commit':sys.argv[2],'version':sys.argv[3],'platform':baseline['platform'],'source_date_epoch':int(sys.argv[4]),'upstream_commit':baseline['commit'],'upstream':baseline,'ci_run_id':os.environ.get('GITHUB_RUN_ID'),'ci_run_attempt':os.environ.get('GITHUB_RUN_ATTEMPT'),'files':files,'production_access':False}
(root/'build-manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
print('BUILD_BUNDLE_PASS source='+sys.argv[2]+' files='+str(len(files)))
PY
