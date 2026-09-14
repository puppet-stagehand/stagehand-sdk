# Design: `stagehand-sdk` v0.1.0-rc.1 — minimal tracer slice

Status: **Approved** (brainstormed 2026-09-14, this session). Implementation
not yet started. This is a design doc for a **new, separate repository**
(`github.com/puppet-stagehand/stagehand-sdk`), not for `stagehand-console`.
It exists in this repo's spec directory because the Expansion Pack decision
record (ADR 0010, `docs/design/capability-packs.md`, the
`capability-packs-host` seed) already lives here and this doc is the first
concrete slice cut from that larger design.

**Renamed 2026-09-14 (later the same session):** "capability pack" →
**Expansion Pack**; `pack-build` → `expansion-build`. This doc reflects the
new names.

## Why this slice, and why first

`docs/design/capability-packs.md` defines the full Expansion Pack contract:
14 facet services, a manifest schema, an `sdk-ui` npm runtime package, an
`expansion-build` CLI, and a conformance suite. Three repos depend on this
contract existing in some form before they can do real work:

- `stagehand-console`'s host side (`backend/internal/expansions/`) — the
  `capability-packs-host` seed's own trigger condition is explicit: *"When
  stagehand-sdk v0.1.0-rc.1 is tagged."* It is deliberately blocked on this.
- `stagehand-expansion-opentofu` — the design doc's own "tracer" Expansion
  Pack (§9), depends on both the SDK and the host.

Neither exists yet, and neither can start for real without the SDK
existing first. This doc scopes the smallest SDK slice that unblocks the
host repo's own P1 planning while proving the facet design against a real
intended consumer (OpenTofu's state-backend semantics), rather than
speculatively defining all 14 facets before anything consumes them.

**Deliberately deferred to a later SDK slice**, per this session's
decisions: the remaining 10 facet services (Bolt, Inventory, Compliance,
Forge, PuppetDB, Classification, Code, Activity, Metrics, Legacy —
everything OpenTofu's Phase A doesn't need), `@stagehand/sdk-ui` (no
Expansion Pack UI in this slice), the `expansion-build` CLI, and the formal
`conformance.Run` test harness (§3.4 of the design doc). None of these are
wrong to add later — they're just not needed to unblock the host repo or
prove this slice's facet set, and defining them now would be speculative
against zero real consumers.

## Repo & module layout

`github.com/puppet-stagehand/stagehand-sdk`, Apache-2.0, created fresh under
the `puppet-stagehand` GitHub org (confirmed available — no existing repo
of that name; `gh` access to the org confirmed this session). Single Go
module at the repo root — nothing in this slice needs independent
versioning from the rest.

```
proto/stagehand/host/v1/     .proto sources for the four facets in scope
gen/go/stagehand/host/v1/    generated Go + gRPC stubs, checked in (not gitignored)
host/                        Go service interfaces (mirror the generated stubs) + host.Local
manifest/                    Go struct + JSON Schema 2020-12 for manifest.json, plus validation
examples/opentofu-lite/      the facet-sufficiency proof (see below)
```

`gen/go/` is checked in rather than gitignored: Expansion Packs and the
eventual console host both need to build against it without a codegen step
being a precondition of `go build`. `buf generate` regenerates it; CI
verifies the checked-in output matches a fresh generation (drift check).

## Protobuf toolchain: buf

`buf.yaml` + `buf.gen.yaml` over plain `protoc` + manual plugin invocation.
The main reason: `contract_version` (from the design doc §1) is a real
compatibility promise — a console release publishes the set it hosts, an
Expansion Pack's manifest states the one it targets, and a mismatch refuses
the worker at registration. buf's breaking-change detection (`buf breaking`)
gives that promise a real CI gate essentially for free; hand-rolling the
equivalent with plain protoc would be extra work for no benefit in this
slice, and worse, would likely get skipped.

## Facet services in scope

Four services from the design doc's §3.2 table, each as a proto service
definition + generated Go interface:

- **Documents** — `Get`, `Put(if_version)` (compare-and-swap — the host
  never merges), `List(cursor)`, `Query(field, op, value)`,
  `Delete(if_version)`. Always-available permission scope (an Expansion
  Pack's own namespace only, never cross-expansion).
- **Settings** — `Current`, `Subscribe` (server-streaming change notices).
  Always-available.
- **Secrets** — `Store`, `Reveal`, `Seal`, `Open`, `Delete`; refs shaped
  `expansion/<id>/<name>`. Requires the `secrets:rw` permission.
- **Auth** — `IssueToken(scope, ttl, label)`, `Verify`, `Revoke`. Requires
  the `tokens:issue` permission.

Every other facet in the design doc's table (Bolt, Inventory, Compliance,
Forge, PuppetDB, Classification, Code, Activity, Metrics, Legacy) is out of
scope for this slice — added when a real consumer (the host repo's own
work, or a later OpenTofu phase, or the Kubernetes Expansion Pack) actually
needs it.

## Manifest schema

Full shape from the design doc's §2.1 example (`id`, `name`, `version`,
`contract_version`, `publisher`, `publisher_key_id`, `licence`,
`entitlement`, `tier`, `default_enabled`, `nav`, `slots`, `ui_digest`,
`settings_schema`, `permissions`, `extension_points`, `routes`, `jobs`,
`content`, `resources`, `network`) — both a Go struct and the JSON Schema
2020-12 document, with Go-side validation matching the design doc's
registration-time rules (id pattern/uniqueness, contract in the hosted set,
nav group in the seven, every permission/slot known, routes relative with
valid methods and no duplicates, `ui_digest` matches, `forge_slug`
pattern). Kept whole rather than trimmed to this slice's needs — the schema
itself is small, and it's the actual wire contract for every Expansion
Pack, not something worth half-defining even when a given example doesn't
exercise every field.

## `host.Local`

An in-process Go implementation of all four services in scope, constructed
with a manifest's declared permission set. An Expansion Pack author calling
an undeclared facet against `host.Local` gets the same `PERMISSION_DENIED`
a real Expansion Pack would get from the console's gRPC interceptor — this
is what lets an Expansion Pack's own tests (and this slice's example)
exercise realistic permission-denial behavior without a running console.
Backing storage is a plain in-memory map — `host.Local` is a test double
for Expansion Pack authors and for this SDK's own tests, not a preview of
how the console host will actually implement these services (that's
Postgres-backed `expansion_documents` etc., built later in
`stagehand-console`).

## `examples/opentofu-lite` — the facet-sufficiency proof

Not a generic "hello world." Implements Terraform's real HTTP state-backend
protocol (from the design doc's §9 Phase A table) purely by composing the
four in-scope facets, run entirely against `host.Local`:

| Terraform operation | Facet composition |
|---|---|
| `GET state/{ws}` | `Documents.Get("state/{ws}")` |
| `POST state/{ws}?ID=` | requires a held lock (see below); `Secrets.Seal` the body, `Documents.Put(if_version)` the sealed reference, prune older versions |
| `DELETE state/{ws}` | `Documents.Delete(if_version)`; refused (409-equivalent) if locked |
| `LOCK state/{ws}` | `Documents.Put(if_version)` on a `locks/{ws}` document holding the lock ID/metadata — a conflicting lock attempt returns the current holder's info, not a bare error |
| `UNLOCK state/{ws}` | `Documents.Delete(if_version)` on `locks/{ws}`, only when the caller's lock ID matches |
| backend credential mint | `Auth.IssueToken(scope, ttl, label)` |

This is still a facet-sufficiency proof, not a real network-listening HTTP
server and not wire-compatibility tested against an actual `tofu` binary —
that's `stagehand-expansion-opentofu`'s job once the console host exists to
terminate `/api/v1/x/opentofu/*`. What this proves here: the four chosen
facets actually compose into Terraform's real locking/versioning semantics,
not just a toy CRUD example.

**Tests** specifically cover the two behaviors that matter for a real state
backend: a conflicting `LOCK` returns the current holder's info rather than
a generic error, and a `POST` attempted without a held lock is rejected.
Plus ordinary coverage of `host/local_test.go` for each facet, including
the permission-denial path.

## What this does NOT do

- Does not touch `stagehand-console` — no changes to this repo
  (`puppet-console`) as part of this SDK work. The host repo's own
  `backend/internal/expansions/` work is a separate, later effort that
  consumes this SDK once tagged.
- Does not create the `stagehand-expansion-opentofu` repo.
  `examples/opentofu-lite` lives inside `stagehand-sdk` and is explicitly
  not that Expansion Pack — it has no HTTP listener, no worker process, no
  manifest published anywhere.
- Does not implement `expansion-build`, `sdk-ui`, or the conformance suite.
- Does not attempt entitlement/marketplace, Kubernetes Expansion Pack work,
  or any of the design doc's other later phases.

## Open question carried forward — RESOLVED later the same session

The Expansion Pack install write-path (how the console eventually proposes
the Hiera change for a real Expansion Pack) was unresolved when this spec
was first written; **resolved later the same session** (see ADR 0010's
second amendment and `.planning/PROJECT.md`'s "Capability packs: pack
install must not depend on Code Management" decision): there is no Hiera
change at all — the console manages an Expansion Pack's container
lifecycle directly. Still irrelevant to this SDK slice (nothing here
touches install), but the host repo's later P2/P3 work now has a resolved
model to plan `install.go`'s replacement against, not an open question.
