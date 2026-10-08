# UI bundle contract (format 1)

This is the contract between a pack's built UI and the console. The console
implements the loader; the SDK defines the files, the digest rule and the
shapes. Source of truth in code: `uibundle/` (rules), `schema/json/ui.manifest.schema.json`
(schema), `schema/ts/slots.d.ts` and `schema/ts/sandbox.d.ts` (types),
`schema/css/sandbox-tokens.css` (sandbox styles).

**Status.** The contract, its checker (`pack-check --ui`) and the builder's UI slice
(`expansion-build ui`, see `docs/expansion-build.md`) exist. A shape change after a
non-hello pack ships means rebuilding and republishing every pack, so treat
everything here as frozen.

## ELI10

A pack's screen is a little box of files. The box has a **contents list**
(`ui.manifest.json`) that says every file in it, how big each is, and a
fingerprint (sha256) of each. The pack's ID card (`manifest.json`) stores one
fingerprint of the contents list itself (`ui_digest`). So one fingerprint
protects the list, and the list protects every file. The console checks the
fingerprint first, then fetches only the files on the list and checks each one.
A file that is not on the list is never shown, whatever else is in the box.

## A. The bundle index: `ui.manifest.json`

The worker serves it at `Assets.Get(path: "ui.manifest.json")`; in the image it
is `/stagehand/ui/ui.manifest.json`.

| Field | Type | Rule |
|---|---|---|
| `format` | integer | Must be `1`. |
| `entry` | path | The in-process ES module. Must be listed in `files` with `content_type` `text/javascript`. |
| `sandbox_entry` | path, optional | One self-contained ES module for the sandboxed tier. Same listing rule. |
| `slots` | map | Slot name to `{ "label": "..." }`. Names are the manifest slots (`page`, `settingsPanel`, `nodeDetailTab`, `deviceDetailTab`, `inventoryKindView`, `complianceSourceCard`, `dashboardCard`). Label: 1 to 32 characters of plain text, no control characters, no `<` or `>`, no leading or trailing space. |
| `files` | map | Path to `{ "sha256", "size", "content_type" }`. `sha256` is lowercase hex. |

Unknown fields are rejected. The slots here must equal `manifest.slots`
(`ui_slot_missing`, `ui_slot_not_declared`).

Labels live here so the console can draw tab names without running any pack code.

## B. The digest rule

`manifest.ui_digest = "sha256:" + lowercase hex of sha256(the exact bytes of ui.manifest.json)`

There is no canonical form. Do not re-serialise the file after the digest is
computed: one added newline changes the digest. The build writes `ui_digest`;
never hand-edit it.

The console: fetches the index, checks the digest, parses it, then fetches every
listed file and checks its size and sha256. It serves only listed files.

## C. Paths, limits, content types

- Path: relative, at most 200 characters from `[A-Za-z0-9._/-]`; no `.` or `..` segment, no empty segment, no leading slash, no backslash, no NUL.
- At most 256 files; 8 MiB per file; 16 MiB for the whole bundle; 256 KiB for `ui.manifest.json`; 1 MiB per `Assets.Get` chunk.
- Allowed `content_type`: `text/javascript`, `text/css`, `application/json`, `image/png`, `image/webp`, `font/woff2`. SVG is excluded because it can carry script.

## D. Runtime shims (in-process tier)

The build marks four modules external and rewrites them to same-origin URLs:

| Import | Rewritten to | Re-exports |
|---|---|---|
| `react` | `/runtime/v1/react.js` | React |
| `react/jsx-runtime` | `/runtime/v1/jsx-runtime.js` | the JSX runtime |
| `react-dom` | `/runtime/v1/react-dom.js` | `createPortal`, `flushSync` |
| `@stagehand/console-ui` | `/runtime/v1/console-ui.js` | `Alert Badge Button Card Checkbox Collapsible Dialog Drawer HelpBubble Input LoadingState Radio SectionHeader Select Spinner StatBlock Switch Tabs Tag Tooltip` |

(20 primitives; the list is `uibundle.ConsoleUIPrimitives`.) Adding an export is
additive; removing one is a contract break. In Rolldown this is `output.paths`
plus `external`.

## E. Entry module (in-process tier)

```ts
export default { contract_version: 1, slots: { nodeDetailTab: HelloTab } } satisfies PackEntry;
```

Every slot component gets `expansionId`; `nodeDetailTab` adds `certname`;
`deviceDetailTab` adds `deviceId`. A pack reaches its own routes at
`/api/v1/x/<expansionId>/`. Types: `schema/ts/slots.d.ts`.

The old `definePackUI` / `./pack` module-federation declarations are gone.

## F. Sandboxed tier

`sandbox_entry` is one self-contained module (React bundled in, no chunks, no
externals). The console sends it to a sandboxed frame over a private
`MessageChannel`; the frame imports it from a blob URL. Its default export is
`{ contract_version: 1, slots: { <slot>: mount } }` where
`mount(element, props, host)` renders into `element` with the bundle's own React
and returns an unmount function. `host` has `api(method, path, body)` (proxied,
restricted to `/api/v1/x/<id>/`), `resize(height)`, `escape()` and
`onTheme(listener)`. The theme arrives as resolved token values; the token names
are in `ThemeTokenName` and may only grow. Base styles that consume them:
`schema/css/sandbox-tokens.css` (no colour literals, ever).

## G. What is not decided here

- **How an in-process pack calls its routes.** The contract says routes are at `/api/v1/x/<id>/` and the session is the console's; the in-process tier is given no API client object. If the console later adds one, it arrives as a new, additive export.
- **Frame framing policy** (`frame-src 'self'` plus `frame-ancestors`) is a console decision and lives in the console, not in the pack contract.
- **`inventoryKindView`, `complianceSourceCard`, `dashboardCard`** are valid slot names but the console does not yet mount them; their props keep the pre-contract shape.

## Checking a bundle

```
go run ./cmd/pack-check --ui path/to/ui path/to/manifest.json
```

Findings (all carry a `fix` line): `ui_manifest_missing`, `ui_manifest_unparseable`,
`ui_manifest_too_large`, `ui_format_unsupported`, `ui_files_empty`, `ui_too_many_files`,
`ui_path_invalid`, `ui_file_sha256_invalid`, `ui_file_too_large`, `ui_bundle_too_large`,
`ui_content_type_not_allowed`, `ui_entry_missing`, `ui_entry_not_listed`,
`ui_entry_not_javascript`, `ui_slots_empty`, `ui_slot_unknown`, `ui_slot_label_invalid`,
`ui_digest_mismatch`, `ui_file_missing`, `ui_file_size_mismatch`, `ui_file_hash_mismatch`,
`ui_file_unlisted`, `ui_slot_missing`, `ui_slot_not_declared`.

Worked example: `uibundle/testdata/hello/ui/`.
