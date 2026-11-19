# Media engine

Independent Go module. No Main internals, PostgreSQL credentials, or balance-admin API.
Main implements the native authentication, immutable price, hold/capture/release and object-storage boundary.

- SQLite WAL + full synchronous commits, encrypted task payloads, one engine owner and multiple admission clients.
- Indexed due queue, bounded workers/backoff, CAS, immutable provider references.
- Durable per-account generation slots, independent of task-record capacity and HTTP concurrency.
- No automatic retry of an uncertain POST. Known pre-submit expiry releases its hold; a separately enabled recovery policy can close customer holds while retaining unknown supplier outcomes.
- Completed/failed payloads: 7 days; idempotency tombstones: 30 days after terminal state. Unresolved funds and automatic unknown-closure receipts are retained.
- S3 object references are owned by Main; reference URLs are refreshed before submit and result delivery.

Implementation, tests, known upstream limitations and deployment gates: [Media engine implementation](../../docs/sources/MEDIA-ENGINE.md).

```sh
go test -race ./...
```

## Namespace, `SOURCES_MEDIA_ENGINE_LEGACY_NAME` and `SOURCES_MEDIA_ENGINE_LEGACY_TASK_PREFIX`

The engine's name is part of values that outlive a process: the sealed
key-check value of the task store (`sources-<name>-v1`), and in Main the
billing request id prefix, internal price models, object-storage key prefixes
and the image-engine registry value. A `Namespace` decides that name in one
place, together with the task id prefix: task ids are `<prefix>_<32 hex>`,
stored, used as ledger batch ids and returned to API clients. The zero value is
the neutral namespace of a fresh install (`media-engine`, registry value
`media`, task prefix `mediatask`). `NewNamespace(legacyName, legacyTaskPrefix)`
selects legacy values instead (both or neither), so a deployment that already
holds data writes exactly what it wrote before; readers accept both the legacy
and the neutral form, including task ids in the store, recovery scopes and the
host's routing.

This module never reads the environment: the host passes the namespace to
`Open` and `OpenProducer`. Main and the Portal read it from
`SOURCES_MEDIA_ENGINE_LEGACY_NAME`; Main also reads
`SOURCES_MEDIA_ENGINE_LEGACY_TASK_PREFIX`. A deployment with existing data
**must** set both on the server, for example
`SOURCES_MEDIA_ENGINE_LEGACY_NAME=<legacy-name>` (`^[a-z0-9]{2,32}$`, not
`sub2api`) and `SOURCES_MEDIA_ENGINE_LEGACY_TASK_PREFIX=<legacy-prefix>`
(`^[a-z0-9]{2,16}$`, without the trailing underscore). Setting only one fails
configuration validation. Without it a store sealed under the legacy
name is refused with `task store identity or key check failed`, as for a wrong
key, so the engine cannot silently start with an empty store; with the name
but another prefix its existing tasks are not found. The values are private and
never belong in this repository.

A passing module suite is not an end-to-end Main/payment/deployment acceptance.

Serving processes open an initialized store with `OpenProducer(dir, key, ns)` and
`NewProducer(store, options)`. A producer can create, read and list tasks, but has
no provider or billing boundary. Admission checks idempotency and per-key record
capacity in the same immediate SQLite transaction, including across processes.
Opening a producer never repairs `submitting` tasks. It verifies the existing
version and encryption key under a short schema lock and cannot initialize an
empty database or perform `Save`, `Retain` or administrator resolution.

The background process retains `Open` and `NewWithOptions`. The unchanged `.lock`
protects the engine for the whole store lifetime; a second engine is rejected,
including on the same store handle. Only this owner repairs, executes, settles,
retains and resolves tasks. Existing `Engine` read/create APIs delegate to the
same producer implementation. The schema version and AEAD envelope stay the
same; the task JSON adds an optional admission hold token, ignored by v0.2.28.

For an orderly handoff call `Engine.CloseGracefully()` before `Store.Close()`.
This stops new steps, lets already claimed provider/billing operations finish
under their existing operation timeout, and waits for persistence before the
engine lock can be released. Leave time for the final SQLite writes as well as
the operation timeout. `Close()` retains immediate cancellation semantics;
neither shutdown path restarts the same engine object. Pending durable phases
are resumed by the next owner, while uncertain submissions remain for review.

Automatic customer closure is disabled unless both an explicit `RecoveryPolicy`
and a true `RecoveryEnabled` callback are provided. Main reads the immutable
policy from `SOURCES_MEDIA_ENGINE_RECOVERY_POLICY_FILE`. It checks a separate marker at
`SOURCES_MEDIA_ENGINE_RECOVERY_ENABLE_FILE` while scheduling/recovering; the marker must
be a regular file with no group/other write permission and contain exactly the
policy ID, without a newline. Missing or mismatched markers disable new decisions.
The operator can create the marker after normal ownership handoff and remove it
to stop new automatic decisions. Already committed release intents still finish
through the original idempotent ledger.

The sole engine owns separate bounded scheduling loops for uncertain-task
recovery (at most two workers) and release completion (one worker). Slow native
generation and slow supplier polling cannot occupy the release worker. All lanes
share the existing per-task claim and graceful/immediate shutdown contexts;
there is no additional owner, provider submit path or financial writer.

The policy uses `id`, `effective_from_unix`, `timeout_seconds`,
`max_release_amount` (an exact decimal string), `max_closures_per_account_24h`,
and optional `legacy_task_ids`. New tasks are eligible from the effective creation
time; older tasks require explicit IDs. The deadline starts at durable submit
intent, with creation time as the fallback for older records. It cannot be
shorter than the configured generation timeout. Known upstream IDs are queried
without resubmission. Durable success evidence, conflicting ledger claims or
amounts above the policy limit remain for exceptional operator review.

An eligible unknown outcome past its deadline can release only the original
validated hold. This is a customer-service decision, not proof of supplier
failure, cancellation or refund. The engine records operator `0` release intent
and completion events. Explicit historical imports have a separate
`auto_release_historical_unknown` intent and do not consume the new-generation
circuit budget. The circuit counts other current unknown tasks and recent
`auto_release_unknown` intents for that account; it rejects only new queued
generation and releases that unsubmitted hold. It is not a strict total loss
ceiling, because accepted work may finish after the threshold is reached.

Unknown closures erase generation bodies and provider credentials while keeping
the original ticket, supplier reference and audit for reconciliation. Ordinary
terminal retention must not erase those receipts. After enabling this feature,
a serving-only rollback must keep this background owner; an older background
retention implementation does not understand the new receipt exemption.

The first transition from v0.2.28 still needs separate deployment gates. That
version cannot release the engine while keeping its HTTP process serving, and
its admission capacity check is not a cross-process transaction. New producers
may open and read its initialized store, but do not enable old and new media
admission concurrently. Drain old admission before enabling new producers, and
keep exactly one engine owner. Synthetic v0.2.28 record compatibility does not
prove an entire Main cutover or authorize interrupting in-flight requests.

For that first migration only, `Producer.CreateHeld(input, token)` durably queues
new work after all v0.2.28 admission paths have drained. The 32–64 character
lowercase hexadecimal token comes from persistent deployment state. Held tasks
remain `admitting`, version 1, with a token-specific `next_run` in `[2^62, 2^63)`
milliseconds. The unchanged old scheduler skips this range, and the new engine
also rejects direct `Step` on a held task. No money is reserved or upstream POST
started during admission. Idempotent retries retain the original task and token;
held tasks count against normal per-key record capacity.

Holds never expire automatically. `ReleaseAdmissionHold(token)` selects only
that token's indexed sentinel and authenticates the full token, initial phase,
version and untouched state before any change. It updates at most 100 tasks per
transaction using the existing CAS/encryption path. A sentinel collision or
modified record aborts the whole batch. It does not open general producer writes.
First switch the caller to normal admission and wait for every older held
admission to commit; then repeat release until it returns zero. A zero result is
only about this token, not proof that no unrelated hold exists.

Both the new owner and a rollback producer must supply the exact token. Owner
startup does not scan or release unknown holds. The coordinator must persist
mode/token and runtime identity, bound the handoff window and own crash recovery;
if it disappears, existing work continues while held work stays safely queued.
Do not reset creation/review deadlines to hide a long hold. Releasing the same
durable record lets either compatible engine continue without replaying POSTs.
