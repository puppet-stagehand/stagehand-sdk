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

With a UI, also check the built bundle: `go run ./cmd/pack-check --ui path/to/ui path/to/manifest.json`. The bundle contract (index file, digest rule, shims, entry shape) is `docs/ui-bundle-contract.md`.

(Preview 0.0.1: `pack-check` and `expansion-build ui` exist. The rest of `expansion-build` (full image layout, signing, push; formerly called `pack-build`), the Go worker helpers, `host.Local`, the conformance suite, `@stagehand/sdk-ui` runtime helpers (the type contract in `schema/ts` exists) and `expansion-build mcp` are the next phases — see ROADMAP in docs/.)

## expansion-build (UI slice built; image, signing and push are later slices)
`expansion-build` (renamed from `pack-build` on 2026-09-14) is the SDK's pack builder. Today it has one subcommand, `expansion-build ui`, which turns `ui/src` into the `ui/` bundle the console loads and writes `ui_digest` into `manifest.json`. Never build or edit a UI bundle, `ui.manifest.json` or `ui_digest` by hand. Guide: `docs/expansion-build.md`; contract: `docs/ui-bundle-contract.md`.

Setup:
- `manifest.json` carries a placeholder `"ui_digest": "sha256:" + 64 zeros`; the build replaces only that value.
- Sources: `ui/src/index.tsx` (default-exports a `PackEntry`), optional `ui/src/sandbox.tsx` (a `SandboxEntry`), and `ui/src/slots.json` (slot name to label; keys must equal `manifest.slots`). Packages come from the pack's `node_modules`.
- Run (from the SDK repo root, or with `GOWORK=<sdk>/go.work` and the import path `github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-build` elsewhere):
```
go run ./cmd/expansion-build ui --src ui/src --out dist/ui --manifest manifest.json
go run ./cmd/pack-check --ui dist/ui manifest.json
```
- It is its own Go module (`cmd/expansion-build/go.mod`, esbuild pinned inside; a `go.work` joins it to the root). `go test ./...` from the root skips nested modules: test it with `go test ./cmd/expansion-build/...`.
- Fix every finding with its `fix` line, as with `pack-check`; do not work around one. The lints refuse raw colour literals, emoji, non-IBM-Plex fonts and weight 700, and the words "converged" and "no-op".
- Not built yet: CSS or binary assets in a bundle (a finding), the full `/stagehand/` layout, image build, cosign signing, push, the conformance suite.

## Ask the user only about product decisions
What the pack does, its tier and licence, which permissions it should request, what its settings are. Never ask about contract details — they are in the schemas.
