#!/usr/bin/env python3
"""Fail closed when an extension starts depending on Sub's private internals."""
from pathlib import Path
import re
ROOT=Path(__file__).resolve().parents[2]
portal=ROOT/'extensions/sources-portal'
errors=[]
for path in (ROOT/'extensions/sources-pricing-feed').rglob('*.go'):
    if path.name.endswith('_test.go'):continue
    source=path.read_text()
    for forbidden in ('"database/sql"','github.com/Wei-Shaw/sub2api','github.com/jackc/pgx','github.com/lib/pq','entgo.io/ent','/api/v1/admin/','/api/v1/auth/','DATABASE_PASSWORD'):
        if forbidden in source:errors.append(f'{path.relative_to(ROOT)}: pricing feed private dependency {forbidden}')
for path in portal.rglob('*.go'):
    if path.name.endswith('_test.go'):continue
    source=path.read_text()
    for forbidden in ('"database/sql"','github.com/Wei-Shaw/sub2api/internal/','github.com/jackc/pgx','entgo.io/ent','/api/v1/admin/users','/balance-adjust','DATABASE_PASSWORD'):
        if forbidden in source:errors.append(f'{path.relative_to(ROOT)}: private dependency {forbidden}')
# The media engine may use only its own SQLite store, never Main's DB or balance APIs.
for path in (ROOT/'extensions/sources-media-engine').rglob('*.go'):
    if path.name.endswith('_test.go'): continue
    source=path.read_text()
    for forbidden in ('github.com/Wei-Shaw/sub2api/internal/','github.com/jackc/pgx','github.com/lib/pq','entgo.io/ent','/api/v1/admin/users','/balance-adjust','DATABASE_PASSWORD'):
        if forbidden in source:errors.append(f'{path.relative_to(ROOT)}: private dependency {forbidden}')
# Fail the upgrade contract instead of silently continuing with stale auth storage.
auth=(ROOT/'frontend/src/stores/auth.ts').read_text()
if not re.search(r"AUTH_TOKEN_KEY\s*=\s*'auth_token'",auth):errors.append('upstream session storage contract changed')
for config in ('frontend/tailwind.config.js','extensions/sources-portal/webapp/tailwind.config.js'):
    if 'sourcesBrand.primary' not in (ROOT/config).read_text():errors.append(f'{config}: duplicated brand palette')
if 'COPY extensions/sources-brand/' not in (ROOT/'Dockerfile').read_text():errors.append('core image does not carry shared build-time brand tokens')
if errors:raise SystemExit('\n'.join(errors))
print('EXTENSION_BOUNDARIES_PASS: no Sub private imports/Main SQL/balance-admin API; media engine SQLite isolated; native session contract; shared brand tokens')
