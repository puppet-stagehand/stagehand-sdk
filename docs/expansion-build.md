# expansion-build: building a pack's UI

`expansion-build ui` turns your pack's screen source into the bundle the console
loads, and writes the matching fingerprint onto your pack's ID card. This is the
first slice of the pack builder; image build, signing and push come later.

## What it does, for a ten year old

Think of a packing machine. You hand it the recipe for your screen (a few text
files). It checks the recipe against the house rules, bakes it into one ready
file, writes a contents list that names every file in the box with a fingerprint
for each, and then writes one fingerprint of the contents list itself onto your
pack's ID card (`manifest.json`). The console later checks that one fingerprint
before it shows anything, so nobody can swap a file in the box.

The machine refuses to pack if the recipe breaks a house rule: a colour written
as a raw number, an emoji, a font that is not IBM Plex, or the words "converged"
or "no-op" (we say "corrected" and "dry run"). It tells you the file, the line
and how to fix it. When it refuses, it changes nothing.

## Before you start

- A `manifest.json` that passes `pack-check`, with `slots` listing the screens you provide and a **placeholder** digest the build will replace:
  `"ui_digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000"`
- A `node_modules` next to or above your sources. Any package your code imports is bundled from it, except the four the console supplies, which the normal entry leaves alone: `react`, `react/jsx-runtime`, `react-dom` and `@stagehand/console-ui`. The normal entry may not import any other path of those packages (for example `react-dom/client`): that is refused, because it would bundle a second copy of React. The sandbox entry bundles React itself and may import them freely.
- Go (the version in `go.mod`). You do not need Node to run the build.

## Your files

```
ui/src/index.tsx     default export: { contract_version: 1, slots: { nodeDetailTab: HelloTab } }
ui/src/sandbox.tsx   optional: the self-contained version for the sandboxed tier
ui/src/slots.json    { "nodeDetailTab": "Hello" }   (slot name to tab label)
```

The keys in `slots.json` must be exactly the slots in `manifest.slots`. A label is
1 to 32 characters of plain text: no control characters, no `<` or `>`, and no space at the start or end. Types for the entry and the sandbox entry are
in `schema/ts/slots.d.ts` and `schema/ts/sandbox.d.ts`.

## Run it

From the SDK repository root:

```
go run ./cmd/expansion-build ui --src ui/src --out dist/ui --manifest manifest.json
go run ./cmd/pack-check --ui dist/ui manifest.json
```

From anywhere else, point Go at the workspace file and use the full import path:

```
GOWORK=<sdk checkout>/go.work go run github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-build ui
```

Flags: `--src` (default `ui/src`), `--out` (default `dist/ui`), `--manifest`
(default `manifest.json`), `--format json`. Exit 0 ok, 1 findings, 2 usage or
environment error.

What you get in `--out` is exactly what the pack image holds at `/stagehand/ui/`:
`index.js` (and `chunks/chunk-<hash>.js` if your code splits; chunk names never contain your file names), `sandbox.js` if you wrote one,
and `ui.manifest.json`. Only the value of `ui_digest` in `manifest.json` changes.
Building twice gives the same bytes and the same digest.

## What it refuses

Every finding has a `fix` line; do not work around one.

| Code | Meaning |
|---|---|
| `ui_digest_key_missing` | `manifest.json` has no `ui_digest` key; add the zero placeholder shown above |
| `ui_src_entry_missing` | no `index.tsx` / `.ts` / `.jsx` / `.js` in `--src` |
| `ui_slots_json_missing`, `ui_slots_json_invalid` | `slots.json` absent, or not one JSON object of strings |
| `ui_slot_missing`, `ui_slot_not_declared` | `slots.json` and `manifest.slots` disagree |
| `ui_slot_label_invalid` | label empty, over 32 characters, or contains control characters, `<` or `>` |
| `ui_lint_hex_colour` | a hex, `rgb()`, `rgba()`, `hsl()` or `hsla()` colour; use a theme token |
| `ui_lint_emoji` | an emoji character; status is a text glyph plus a label |
| `ui_lint_font` | a font other than IBM Plex, or weight 700 / bold |
| `ui_lint_term` | "converged" (say "corrected") or "no-op" (say "dry run") |
| `ui_build_error` | esbuild could not bundle: a syntax error, a package that is not installed, or a forbidden import of a shim package's subpath from the normal entry |
| `ui_build_output_unsupported` | the build produced CSS or an asset; slice 1 emits `.js` only |
| `ui_out_unsafe` | `--out` is not empty and not a previous build, or contains `--src` or `--manifest` |

Any `manifest.json` problem is reported with `pack-check`'s codes. A finding
whose message starts "builder bug" means the self-check of the finished bundle
failed; that is a bug in `expansion-build`, not in your pack: report it.

The lints read text; they are not a full JavaScript parser. They skip comments.
The hex lint looks inside string literals in `.ts`/`.tsx`/`.js`/`.jsx` and inside
`{ }` blocks in `.css`. A string that is nothing but a hex-letter anchor such as
`"#bad"` is a known false positive; build the value another way.

## What it does not do yet

- CSS or binary assets in a bundle (an ESM entry cannot load CSS by itself).
- The rest of the `/stagehand/` layout (OpenAPI fragment, docs), the container image, cosign signing and push, the conformance suite, and the contrast check that needs a rendered page.
- `go install ...@version`: the builder's `go.mod` has a `replace` pointing at the repo root until the root module is tagged.

## Reproducibility

esbuild is pinned to one exact version in `cmd/expansion-build/go.mod`. A different
esbuild can emit different bytes for the same source, which changes the digest and
means every pack must be rebuilt and republished. Bumping it is a deliberate change.

## Developing the builder

It is its own Go module so that packs importing the SDK never pull in esbuild.
`go test ./...` from the root skips nested modules; run
`go test ./cmd/expansion-build/... -race` (CI does). The golden test builds
`cmd/expansion-build/testdata/hello-src` and requires the output to equal
`uibundle/testdata/hello/ui` byte for byte.
