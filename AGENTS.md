# stagehand-sdk — brief for code assistants

You are building a **Stagehand Expansion Pack** ("app" for the Stagehand
console). Read this file, then the schema files it points at. Do not read the
console's source; everything a pack may touch is defined here.

## What a pack is
A signed container image with three parts:
- `/stagehand/manifest.json` — the pack's ID card. Schema: `manifest/schema.json`.
- `/stagehand/ui/` — an optional prebuilt UI bundle that the console mounts into named **slots**. Types: `schema/ts/slots.d.ts`.
- the **worker** (container entrypoint) — dials the console over mTLS gRPC and calls **host facets**; the console calls it back for routes, assets, health, jobs. Wire: `schema/proto/stagehand/host/v1/*.proto`.

## Rules that are enforced (by `pack-check` now, by the console at install)
1. The manifest lists every facet the worker uses in `permissions`. Undeclared facets are refused at runtime; the conformance suite fails a worker that reaches for one.
2. UI goes into slots only. A `page` slot needs a `nav` entry in one of the seven console groups: Overview, Inventory, Reporting, Automation, Configuration, Patching & Compliance, Administration. Packs never add groups.
3. Routes are relative (`state/{workspace}`); the console mounts them at `/api/v1/x/<id>/`. Any HTTP method token is allowed. Every route has an `operation_id` present in the pack's OpenAPI fragment.
4. State lives in the `Documents` facet (namespaced JSON documents). Packs never own database tables.
5. Secrets go through the `Secrets` facet (`Store`/`Seal`); never in Documents unsealed, never in the image.
6. Fleet-facing tools (tofu, kubectl, scanners) run on a Puppet-classified **runner node** via the `Bolt` facet, not in the worker, unless the operator opts in per resource.
7. UI: build only from `@stagehand/console-ui`; no raw hex colours, no emoji, IBM Plex only, status is always glyph + label. Terminology: "corrected" not "converged", "dry run" not "no-op".
8. Docs are part of the pack: `docs/USER-GUIDE.md` written ELI10 for someone new to Puppet, and `docs/TESTER-GUIDE.md` with a manual step for every route and slot.

## The loop
```
go run ./cmd/pack-check --format json path/to/manifest.json
```
Fix every finding using its `fix` line; do not work around a finding. When `ok: true`, the manifest is valid for contract_version 1.

(Preview 0.0.1: only `pack-check` exists. `pack-build`, the Go worker helpers, `host.Local`, the conformance suite, `@stagehand/sdk-ui` and `pack-build mcp` are the next phases — see ROADMAP in docs/.)

## Ask the user only about product decisions
What the pack does, its tier and licence, which permissions it should request, what its settings are. Never ask about contract details — they are in the schemas.
