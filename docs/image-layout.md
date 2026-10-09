# Pack image layout

This page is the contract between a pack's container image and the Stagehand
console. If your image follows it, the console can find your manifest, load your
screens and start your worker. If it does not, the console refuses the install
and says why.

## What this is, for a ten year old

A pack ships as a **box** (a container image). The box is built in layers, like
stacking trays one on top of another. The console opens the box and looks for
your pack's **ID card** (`manifest.json`). It starts at the top tray and works
down, and it believes the first ID card it finds. So the ID card must sit in the
top tray, where nothing can slide in on top of it.

Inside the box the console also expects:

- a folder of ready-made screens (only if your pack has any),
- a list of the web addresses your worker answers (only if it has any), and
- the worker program itself, which starts when the box is opened.

Nobody trusts a box just because it looks right. A box is **unsigned** until the
feed that lists it puts its stamp on it (see
[the feed guide](expansion-index.md)). Building the box correctly and getting it
stamped are two separate steps.

## The layout

| Path in the image | Required | What it is |
|-------------------|----------|------------|
| `/stagehand/manifest.json` | always | The pack's manifest (`manifest/schema.json`). It must be in the **topmost layer**. |
| `/stagehand/ui/` | when `manifest.slots` is non-empty | The built UI bundle: `ui.manifest.json` plus the files it lists. Build it with `expansion-build ui` ([guide](expansion-build.md), [contract](ui-bundle-contract.md)). |
| `/stagehand/openapi.json` | when `manifest.routes` is non-empty | The OpenAPI fragment naming every route's `operation_id`. The path is `manifest.openapi_path` (default `/stagehand/openapi.json`). |
| the worker binary | always | Your program, set as the image `ENTRYPOINT`. The console starts the container and the worker dials back ([worker guide](worker.md)). |

The working example is [`examples/hello/Dockerfile`](../examples/hello/Dockerfile).

## Why the manifest must be in the topmost layer

The console reads the manifest straight out of the image's layers, **topmost
layer first**, and uses the first `stagehand/manifest.json` it finds. It never
starts your worker to ask. That makes the topmost layer the one that wins: a
manifest in a lower layer can be hidden by a later layer that carries a
different one. Putting yours in the top layer means what you wrote is what the
console reads.

Rules the console applies while it looks (these are the current limits; they are
checked before anything is trusted):

- The manifest is read from the layer and the **whole layer is verified** (its
  digest, size and diff ID) before the manifest is believed.
- A layer whose manifest entry is a deletion marker (a "whiteout") counts as
  "no manifest".
- `stagehand/manifest.json` must be a regular file of at most **1 MiB**.
- A single layer may be at most **128 MiB** compressed, and the scan stops at
  **256 MiB** compressed (and separately 256 MiB uncompressed) across all
  layers. Oversized layers are refused without being downloaded.
- Layers must be gzip or uncompressed tar. Zstandard layers are not supported.

In a Dockerfile this means: copy the manifest as the **last** `COPY` of the final
stage, after the worker, the OpenAPI fragment and the UI bundle.

```dockerfile
COPY --from=builder /out/hello /hello
COPY examples/hello/openapi.json /stagehand/openapi.json
COPY dist/ui/ /stagehand/ui/                         # only if your pack has a UI
COPY examples/hello/manifest.json /stagehand/manifest.json   # last
ENTRYPOINT ["/hello"]
```

If you build a UI, build it **first**: `expansion-build ui` writes the bundle's
fingerprint (`ui_digest`) into `manifest.json`, and it is that updated manifest
that must go into the image.

## Platform: one image for linux/amd64

Publish a **single-platform `linux/amd64` image**, with provenance and SBOM
attestations switched off, for `contract_version` 1:

```
docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
  -f Dockerfile -t ghcr.io/<org>/<pack>:<version> --push .
```

Why: the console reads the image behind a digest-pinned reference with a
single-image reader. If the reference points at an **image index** (a list of
images, which is what a multi-platform build or the attestation extras produce),
the reader has to pick a platform out of it, and the extra attestation entries
are not images at all. A plain `docker build` on a recent Docker can silently
add an attestation and publish an index. Switching both attestations off keeps
the pushed reference a single image manifest that is exactly what you built.

## Names and references

- Repository names are **lowercase**. Registries refuse uppercase, and the feed
  index only accepts references that match
  `^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+@sha256:[a-f0-9]{64}$`: lowercase,
  pinned by digest, not by tag.
- Refer to a pack by its digest (`ghcr.io/org/pack@sha256:...`). A tag can move;
  a digest cannot.
- The pack `id` in the manifest is separate from the repository name. It
  follows `^[a-z][a-z0-9_]{1,31}$`.

## The image is unsigned until a feed signs it

Building and pushing an image does not make it installable from a feed. A feed
operator reviews the image, copies it into the feed's repository by digest,
signs the digest, and lists it in a signed index. Consoles install only what the
signed index lists, and check the image signature again. The steps are in
[expansion-index](expansion-index.md). Nothing about your Dockerfile changes
that, and nothing in the image can sign itself.

## Check your image before you hand it over

1. Manifest valid: `go run ./cmd/pack-check --format json path/to/manifest.json`
   (add `--ui dist/ui` if you ship a UI).
2. Build as above, then confirm the manifest is the topmost layer. Export the
   image and list the last layer:

   ```
   docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
     -f Dockerfile --output type=oci,dest=pack.tar .
   mkdir pack && tar -xf pack.tar -C pack
   # index.json -> manifest digest -> its "layers" array; the LAST entry is topmost
   tar -tzf pack/blobs/sha256/<last layer digest>     # must list stagehand/manifest.json
   ```
3. The worker starts as the entrypoint and needs nothing from the image's shell:
   distroless "static" images have none.
